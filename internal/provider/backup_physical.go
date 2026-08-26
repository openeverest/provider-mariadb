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
	"reflect"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	apicommon "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	mariadbbackup "github.com/openeverest/provider-mariadb/definition/backupclasses/mariadb"
)

// buildPhysicalS3Storage wraps the resolved S3 spec in the operator's
// PhysicalBackup storage type.
func buildPhysicalS3Storage(c *controller.Context, storageName string) (mariadbv1alpha1.PhysicalBackupStorage, error) {
	s3, err := buildOperatorS3(c, storageName, c.Name())
	if err != nil {
		return mariadbv1alpha1.PhysicalBackupStorage{}, err
	}
	return mariadbv1alpha1.PhysicalBackupStorage{S3: s3}, nil
}

// applyPhysicalBackupParameters applies the decoded parameters onto the operator
// PhysicalBackup spec. Target defaults to PreferReplica (not the operator's own
// Replica default) so single-node/standalone instances — which have no replica —
// still take the backup from the primary instead of never scheduling it.
func applyPhysicalBackupParameters(spec *mariadbv1alpha1.PhysicalBackupSpec, params mariadbbackup.MariadbBackupParameters) {
	if params.Compression != "" {
		spec.Compression = mariadbv1alpha1.CompressAlgorithm(params.Compression)
	}
	target := mariadbv1alpha1.PhysicalBackupTargetPreferReplica
	if params.Target != "" {
		target = mariadbv1alpha1.PhysicalBackupTarget(params.Target)
	}
	spec.Target = &target
}

// syncPhysicalBackup creates or updates a mariadb-operator PhysicalBackup CR
// that snapshots the Instance's MariaDB data directory to the S3 storage
// referenced by backup.spec.storageRef, then maps the operator's Complete
// condition into the BackupExecutionStatus the runtime reflects onto the Backup.
func syncPhysicalBackup(
	c *controller.Context,
	backup *backupv1alpha1.Backup,
	params mariadbbackup.MariadbBackupParameters,
) (controller.BackupExecutionStatus, error) {
	mdb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(mdb, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.BackupExecutionStatus{
				State:   backupv1alpha1.BackupStatePending,
				Message: "Waiting for MariaDB cluster to exist",
			}, nil
		}
		return controller.BackupExecutionStatus{}, fmt.Errorf("get MariaDB: %w", err)
	}

	opBackup := &mariadbv1alpha1.PhysicalBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backup.Name,
			Namespace: backup.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), opBackup, func() error {
		// The operator PhysicalBackup spec is immutable (spec.storage,
		// spec.mariaDbRef) and on-demand backups are run-once, so only populate
		// it on creation (empty ResourceVersion means the object does not exist).
		if opBackup.ResourceVersion == "" {
			storage, err := buildPhysicalS3Storage(c, backup.Spec.StorageRef.Name)
			if err != nil {
				return err
			}
			opBackup.Spec.MariaDBRef = mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: c.Name()},
				WaitForIt:       true,
			}
			opBackup.Spec.Storage = storage
			applyPhysicalBackupParameters(&opBackup.Spec, params)
		}
		return controllerutil.SetControllerReference(backup, opBackup, c.Client().Scheme())
	}); err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("create or update MariaDB physical backup: %w", err)
	}

	exec := mapBackupState(opBackup.Status.Conditions)
	exec.OperatorBackupRef = &apicommon.TypedObjectRef{
		Group: mariadbv1alpha1.GroupVersion.Group,
		Kind:  "PhysicalBackup",
		Name:  opBackup.Name,
	}
	return exec, nil
}

// cleanupPhysicalBackup deletes the operator PhysicalBackup CR. As with logical
// backups, the operator does not purge S3 objects on deletion (retention is
// time-based via maxRetention), so this removes only the CR.
func cleanupPhysicalBackup(c *controller.Context, backup *backupv1alpha1.Backup) (bool, error) {
	opBackup := &mariadbv1alpha1.PhysicalBackup{}
	err := c.Get(opBackup, backup.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get MariaDB physical backup for cleanup: %w", err)
	}
	if opBackup.DeletionTimestamp.IsZero() {
		if err := c.Delete(opBackup); err != nil {
			return false, fmt.Errorf("delete MariaDB physical backup: %w", err)
		}
	}
	return false, nil
}

// reconcileScheduledPhysicalBackup reconciles one operator PhysicalBackup CR
// (with spec.schedule) for an InstanceBackupSchedule. It mirrors
// reconcileScheduledBackup but drives the PhysicalBackup CRD.
func reconcileScheduledPhysicalBackup(
	c *controller.Context,
	mdb *mariadbv1alpha1.MariaDB,
	storageName string,
	schedule *corev1alpha1.InstanceBackupSchedule,
	name string,
	params mariadbbackup.MariadbBackupParameters,
) error {
	storage, err := buildPhysicalS3Storage(c, storageName)
	if err != nil {
		return err
	}
	retention, err := deriveMaxRetention(schedule.Cron, schedule.RetentionCopies)
	if err != nil {
		return &controller.BackupConfigError{Reason: "InvalidSchedule", Message: err.Error()}
	}

	// A logical Backup with the same name may exist if this schedule's type was
	// switched; remove it so the physical CR can take over.
	if err := deleteScheduledBackup(c, name); err != nil {
		return err
	}

	// spec.storage is immutable on the operator PhysicalBackup; if it changed,
	// recreate the CR so the new value takes effect.
	existing := &mariadbv1alpha1.PhysicalBackup{}
	if getErr := c.Get(existing, name); getErr == nil {
		if !reflect.DeepEqual(existing.Spec.Storage, storage) {
			if err := c.Delete(existing); err != nil {
				return fmt.Errorf("delete outdated scheduled physical backup %q: %w", name, err)
			}
			return controller.WaitFor(fmt.Sprintf("Recreating scheduled physical backup %q", name))
		}
	} else if !apierrors.IsNotFound(getErr) {
		return fmt.Errorf("get scheduled physical backup %q: %w", name, getErr)
	}

	managedLabels := scheduledBackupLabels(c, schedule.Name, storageName, backupTypePhysical)
	opBackup := &mariadbv1alpha1.PhysicalBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace()},
	}
	_, err = controllerutil.CreateOrUpdate(c.Context(), c.Client(), opBackup, func() error {
		if opBackup.ResourceVersion == "" {
			opBackup.Labels = managedLabels
			opBackup.Spec.MariaDBRef = mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: c.Name()},
				WaitForIt:       true,
			}
			opBackup.Spec.Storage = storage
			// Propagated onto each run Job so Mirror can identify scheduled runs.
			opBackup.Spec.InheritMetadata = &mariadbv1alpha1.Metadata{Labels: managedLabels}
		}
		opBackup.Spec.MaxRetention = retention
		opBackup.Spec.Schedule = &mariadbv1alpha1.PhysicalBackupSchedule{
			Cron:    schedule.Cron,
			Suspend: !schedule.Enabled,
		}
		applyPhysicalBackupParameters(&opBackup.Spec, params)
		return controllerutil.SetControllerReference(mdb, opBackup, c.Client().Scheme())
	})
	if err != nil {
		return fmt.Errorf("reconcile scheduled physical backup %q: %w", name, err)
	}
	return nil
}

// pruneScheduledPhysicalBackups deletes operator PhysicalBackups managed by this
// provider whose schedule is no longer declared on the Instance.
func pruneScheduledPhysicalBackups(c *controller.Context, desired map[string]bool) error {
	list := &mariadbv1alpha1.PhysicalBackupList{}
	if err := c.List(list, client.MatchingLabels{instanceLabel: c.Name()}); err != nil {
		return fmt.Errorf("list scheduled physical backups: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if _, ok := item.Labels[scheduleLabel]; !ok {
			continue
		}
		if desired[item.Name] {
			continue
		}
		if err := c.Delete(item); err != nil {
			return fmt.Errorf("delete stale scheduled physical backup %q: %w", item.Name, err)
		}
	}
	return nil
}
