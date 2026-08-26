// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// resolvePhysicalBootstrapFrom builds the operator MariaDB.spec.bootstrapFrom
// used to seed a brand-new Instance from a physical backup referenced by
// spec.dataSource. It returns:
//   - (nil, "", nil) when the Instance has no DataSource or the source is not a
//     physical backup (logical seeding is handled elsewhere / unsupported here);
//   - a WaitFor error while the source Backup is not yet resolvable or ready;
//   - a populated BootstrapFrom plus the source Instance name once the source
//     physical backup has succeeded.
//
// Physical backups can only be restored into a fresh MariaDB via bootstrapFrom,
// so this is only consulted when the MariaDB CR is first created.
func resolvePhysicalBootstrapFrom(c *controller.Context) (*mariadbv1alpha1.BootstrapFrom, string, error) {
	ds := c.Instance().Spec.DataSource
	// Only backup-sourced seeding is supported for physical backups;
	// point-in-time recovery is out of scope.
	if ds == nil || ds.Type != backupv1alpha1.DataSourceTypeBackup || ds.Backup == nil {
		return nil, "", nil
	}

	sourceBackup := &backupv1alpha1.Backup{}
	if err := c.Get(sourceBackup, ds.Backup.BackupRef.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", controller.WaitFor(
				fmt.Sprintf("Waiting for source Backup %q", ds.Backup.BackupRef.Name))
		}
		return nil, "", fmt.Errorf("get source Backup: %w", err)
	}
	params, err := parseBackupParams(rawParameters(sourceBackup.Spec.Parameters))
	if err != nil {
		return nil, "", err
	}
	if !isPhysical(params) {
		// Logical seeding is not driven through bootstrapFrom here.
		return nil, "", nil
	}
	if sourceBackup.Status.State == backupv1alpha1.BackupStateFailed {
		return nil, "", &controller.DataSourceConfigError{
			Reason:  corev1alpha1.ReasonDataSourceSourceBackupNotSucceeded,
			Message: fmt.Sprintf("source Backup %q failed; cannot seed", sourceBackup.Name),
		}
	}
	if sourceBackup.Status.State != backupv1alpha1.BackupStateSucceeded {
		return nil, "", controller.WaitFor(
			fmt.Sprintf("Waiting for source Backup %q to succeed", sourceBackup.Name))
	}

	sourceInstance := sourceBackup.Spec.InstanceRef.Name
	// Read the physical backup files from the source Instance's prefix in the
	// storage it was written to.
	s3, err := buildOperatorS3(c, sourceBackup.Spec.StorageRef.Name, sourceInstance)
	if err != nil {
		return nil, "", err
	}
	bootstrap := &mariadbv1alpha1.BootstrapFrom{
		S3:                s3,
		BackupContentType: mariadbv1alpha1.BackupContentTypePhysical,
	}
	// Pin selection to the specific source backup: the operator matches the
	// closest backup at or before this time.
	if sourceBackup.Status.CompletedAt != nil {
		bootstrap.TargetRecoveryTime = sourceBackup.Status.CompletedAt
	}
	return bootstrap, sourceInstance, nil
}

// ensurePhysicalRestoreCredentials copies the source Instance's root and user
// credential Secrets onto the restored Instance's Secret names before the
// operator creates the MariaDB CR. A physical backup embeds the source's
// mysql.user/global_priv tables, so the restored cluster must reuse the source
// credentials; otherwise the operator would generate fresh random passwords and
// the liveness/readiness probes (which authenticate as root) would fail,
// crash-looping the Pods. It is idempotent — existing target Secrets are left
// untouched — and must run before the CR is applied.
func ensurePhysicalRestoreCredentials(c *controller.Context, sourceInstance string) error {
	if err := copyInstanceSecret(c, rootSecretName(sourceInstance), rootSecretName(c.Name())); err != nil {
		return err
	}
	return copyInstanceSecret(c, userSecretName(sourceInstance), userSecretName(c.Name()))
}

// copyInstanceSecret creates dstName as a copy of srcName's data, owned by the
// Instance so it is garbage-collected with it. Existing destinations are left
// as-is. A missing source is treated as a terminal seeding failure since the
// restored data cannot be authenticated without it.
func copyInstanceSecret(c *controller.Context, srcName, dstName string) error {
	dst := &corev1.Secret{}
	if err := c.Get(dst, dstName); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get target Secret %q: %w", dstName, err)
	}

	src := &corev1.Secret{}
	if err := c.Get(src, srcName); err != nil {
		if apierrors.IsNotFound(err) {
			return &controller.DataSourceConfigError{
				Reason: corev1alpha1.ReasonDataSourceFailed,
				Message: fmt.Sprintf(
					"source Instance credentials Secret %q not found; the source Instance may have been deleted", srcName),
			}
		}
		return fmt.Errorf("get source Secret %q: %w", srcName, err)
	}

	newSecret := &corev1.Secret{
		ObjectMeta: c.ObjectMeta(dstName),
		Type:       src.Type,
		Data:       src.Data,
	}
	if err := controllerutil.SetControllerReference(c.Instance(), newSecret, c.Client().Scheme()); err != nil {
		return fmt.Errorf("set owner reference on Secret %q: %w", dstName, err)
	}
	if err := c.Client().Create(c.Context(), newSecret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create target Secret %q: %w", dstName, err)
	}
	return nil
}

// syncPhysicalDataSourceStatus stages the DataSourceReady status for an Instance
// seeded from a physical backup, translating the seeded MariaDB's readiness into
// the runtime's DataSourceStatus. It is a no-op for Instances that are not
// physically seeded.
func syncPhysicalDataSourceStatus(c *controller.Context) error {
	ds := c.Instance().Spec.DataSource
	if ds == nil {
		return nil
	}

	mdb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(mdb, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get MariaDB: %w", err)
	}
	// Only manage the condition for physically-seeded clusters; the Physical
	// content type on bootstrapFrom is the authoritative marker.
	if mdb.Spec.BootstrapFrom == nil ||
		mdb.Spec.BootstrapFrom.BackupContentType != mariadbv1alpha1.BackupContentTypePhysical {
		return nil
	}

	if mdb.IsReady() {
		c.SetDataSourceStatus(controller.DataSourceStatus{
			Done:    true,
			State:   controller.DataSourceStateSucceeded,
			Reason:  corev1alpha1.ReasonDataSourceSucceeded,
			Message: "Instance seeded from physical backup",
		})
		return nil
	}
	c.SetDataSourceStatus(controller.DataSourceStatus{
		Done:    false,
		State:   controller.DataSourceStateRestoring,
		Reason:  corev1alpha1.ReasonDataSourceRestoring,
		Message: "Restoring physical backup into new MariaDB cluster",
	})
	return nil
}
