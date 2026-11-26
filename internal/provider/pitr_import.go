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
	"crypto/sha1" //nolint:gosec // short, non-cryptographic name derivation
	"encoding/hex"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// importedPITRName derives the name of the operator PhysicalBackup and
// PointInTimeRecovery that describe, to the operator, the imported stream an
// Instance is recovered from. Like pitrBaseBackupName it is short and opaque.
func importedPITRName(instance string) string {
	sum := sha1.Sum([]byte(instance + "\x00pitr-import")) //nolint:gosec
	return "mdb-dr-" + hex.EncodeToString(sum[:])[:12]
}

// importedPointInTimeRecovery recovers pit from the stream of a source
// Instance that no longer exists, as imported by a BackupImport: it picks the
// imported physical Backup of the source Instance on the source storage that
// the target time recovers from, and points an operator PointInTimeRecovery at
// the directory holding it and its binary logs. It returns nil when no such
// Backup has been imported.
//
// The operator only reads storage locations from the PhysicalBackup and
// PointInTimeRecovery a bootstrap names, so both are created inert: the
// PhysicalBackup's schedule is suspended and nothing archives into the
// PointInTimeRecovery. They are owned by the Instance.
func importedPointInTimeRecovery(
	c *controller.Context,
	pit *backupv1alpha1.DataSourcePointInTime,
) (*mariadbv1alpha1.LocalObjectReference, error) {
	base, err := importedPITRBase(c, pit)
	if err != nil || base == nil {
		return nil, err
	}
	s3, err := backupS3(c, base)
	if err != nil {
		return nil, err
	}
	binlogs := s3.DeepCopy()
	binlogs.Prefix = path.Join(s3.Prefix, binlogPathSegment)

	name := importedPITRName(c.Name())
	physical := &mariadbv1alpha1.PhysicalBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace()},
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), physical, func() error {
		// spec.storage is immutable; a different base later in the same
		// directory is still picked by the target time.
		if physical.ResourceVersion == "" {
			physical.Spec.MariaDBRef = mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: c.Name()},
			}
			physical.Spec.Storage = mariadbv1alpha1.PhysicalBackupStorage{S3: s3}
		}
		physical.Spec.Schedule = &mariadbv1alpha1.PhysicalBackupSchedule{Suspend: true, Immediate: ptr.To(false)}
		return controllerutil.SetControllerReference(c.Instance(), physical, c.Client().Scheme())
	}); err != nil {
		return nil, fmt.Errorf("reconcile imported PITR base %q: %w", name, err)
	}

	pitr := &mariadbv1alpha1.PointInTimeRecovery{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace()},
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), pitr, func() error {
		pitr.Spec.PhysicalBackupRef = mariadbv1alpha1.LocalObjectReference{Name: name}
		pitr.Spec.PointInTimeRecoveryStorage = mariadbv1alpha1.PointInTimeRecoveryStorage{S3: binlogs}
		return controllerutil.SetControllerReference(c.Instance(), pitr, c.Client().Scheme())
	}); err != nil {
		return nil, fmt.Errorf("reconcile imported PointInTimeRecovery %q: %w", name, err)
	}
	return &mariadbv1alpha1.LocalObjectReference{Name: name}, nil
}

// importedPITRBase returns the imported physical Backup of pit's source
// Instance on its storage that the target time recovers from: the latest one
// started at or before the date, or the latest one for the "latest" target. It
// returns nil when none is imported, and an error when none precedes the date.
//
// Instances of the same name in different namespaces may share a storage, and
// their streams overlap in time, so only one namespace's backups are
// considered: the Instance's own when it has any, otherwise the only one
// there is. Several other namespaces are ambiguous and rejected.
func importedPITRBase(
	c *controller.Context,
	pit *backupv1alpha1.DataSourcePointInTime,
) (*backupv1alpha1.Backup, error) {
	list := &backupv1alpha1.BackupList{}
	if err := c.List(list, client.MatchingLabels{sourceInstanceLabel: pit.Source.InstanceRef.Name}); err != nil {
		return nil, fmt.Errorf("list imported Backups: %w", err)
	}
	byNamespace := map[string][]*backupv1alpha1.Backup{}
	for i := range list.Items {
		b := &list.Items[i]
		if !isImported(b) || b.Spec.StorageRef.Name != pit.Source.StorageRef.Name {
			continue
		}
		if params, err := parseBackupParams(rawParameters(b.Spec.Parameters)); err != nil || !isPhysical(params) {
			continue
		}
		ns := b.Labels[sourceNamespaceLabel]
		byNamespace[ns] = append(byNamespace[ns], b)
	}
	candidates, ok := byNamespace[c.Namespace()]
	if !ok && len(byNamespace) > 1 {
		namespaces := slices.Sorted(maps.Keys(byNamespace))
		return nil, &controller.DataSourceConfigError{
			Reason: corev1alpha1.ReasonDataSourceFailed,
			Message: fmt.Sprintf("imported physical Backups of Instance %q on storage %q come from several namespaces (%s); "+
				"recover in the namespace the source Instance ran in",
				pit.Source.InstanceRef.Name, pit.Source.StorageRef.Name, strings.Join(namespaces, ", ")),
		}
	}
	if !ok {
		for _, backups := range byNamespace {
			candidates = backups
		}
	}
	var base *backupv1alpha1.Backup
	found := len(candidates) > 0
	for _, b := range candidates {
		started := b.Spec.Origin.External.StartedAt
		if pit.RecoveryTarget == backupv1alpha1.RecoveryTargetDate && started.After(pit.Date.Time) {
			continue
		}
		if base == nil || started.After(base.Spec.Origin.External.StartedAt.Time) {
			base = b
		}
	}
	if found && base == nil {
		return nil, &controller.DataSourceConfigError{
			Reason: corev1alpha1.ReasonDataSourceFailed,
			Message: fmt.Sprintf("no imported physical Backup of Instance %q on storage %q started at or before %s",
				pit.Source.InstanceRef.Name, pit.Source.StorageRef.Name, pit.Date.UTC().Format("2006-01-02T15:04:05Z")),
		}
	}
	return base, nil
}
