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

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	mariadbbackup "github.com/openeverest/provider-mariadb/definition/backupclasses/mariadb"
)

const (
	// pitrBaseLabel marks the operator PhysicalBackup that serves as the PITR
	// full base backup, so it is excluded from scheduled-run mirroring.
	pitrBaseLabel = "backup.provider-mariadb.openeverest.io/pitr-base"

	// binlogPrefixSuffix isolates archived binary logs under a dedicated prefix
	// in the storage bucket, separate from the base backup files.
	binlogPrefixSuffix = "-binlog"
)

// pitrCRName is the name of the PointInTimeRecovery CR managed for an Instance.
func pitrCRName(instance string) string {
	return instance + "-pitr"
}

// pitrBaseBackupName derives a stable, short (<=52 char) name for the operator
// PhysicalBackup used as the PITR full base backup. The operator turns a
// scheduled PhysicalBackup into a CronJob, whose name Kubernetes bounds to 52
// characters, so an opaque hash is used rather than "<instance>-pitr-base".
func pitrBaseBackupName(instance string) string {
	sum := sha1.Sum([]byte(instance + "\x00pitr-base")) //nolint:gosec
	return "mdb-pitr-" + hex.EncodeToString(sum[:])[:12]
}

// pitrEnabledStorage returns the single storage with PITR enabled, or nil when
// none is enabled. The mariadb-operator archives binary logs from the single
// primary of the asynchronous replication topology, so configuring more than
// one PITR-enabled storage is rejected as a configuration error.
func pitrEnabledStorage(backupCfg *corev1alpha1.InstanceBackupSpec) (*corev1alpha1.InstanceBackupStorage, error) {
	if backupCfg == nil || !backupCfg.Enabled {
		return nil, nil
	}
	var found *corev1alpha1.InstanceBackupStorage
	var names []string
	for i := range backupCfg.Storages {
		s := &backupCfg.Storages[i]
		if s.PITR != nil && s.PITR.Enabled {
			names = append(names, s.StorageRef.Name)
			if found == nil {
				found = s
			}
		}
	}
	if len(names) > 1 {
		return nil, &controller.BackupConfigError{
			Reason:  "PITRConfigInvalid",
			Message: fmt.Sprintf("at most one PITR-enabled storage is supported, got %d: %v", len(names), names),
		}
	}
	return found, nil
}

// firstPhysicalSchedule returns the first physical backup schedule declared on
// the storage. Point-in-time recovery replays binary logs on top of a full
// physical base backup, so a PITR-enabled storage must schedule at least one
// physical backup; its cadence and retention drive the base backup.
func firstPhysicalSchedule(storage *corev1alpha1.InstanceBackupStorage) (*corev1alpha1.InstanceBackupSchedule, error) {
	for i := range storage.Schedules {
		schedule := &storage.Schedules[i]
		params, err := parseBackupParams(rawParameters(schedule.Parameters))
		if err != nil {
			return nil, err
		}
		if isPhysical(params) {
			return schedule, nil
		}
	}
	return nil, &controller.BackupConfigError{
		Reason: "PITRConfigInvalid",
		Message: fmt.Sprintf(
			"PITR-enabled storage %q must declare at least one physical backup schedule to serve as the full base backup",
			storage.StorageRef.Name,
		),
	}
}

// SyncPITR reconciles point-in-time recovery for the Instance: it manages the
// full base PhysicalBackup and the PointInTimeRecovery CR when a storage has
// PITR enabled, and tears them down otherwise. The MariaDB.spec
// .pointInTimeRecoveryRef overlay that actually turns on binary log archival is
// applied in SyncMariaDB.
func SyncPITR(c *controller.Context) error {
	storage, err := pitrEnabledStorage(c.Instance().Spec.Backup)
	if err != nil {
		return err
	}
	if storage == nil {
		return teardownPITR(c)
	}

	// The base backup and PITR CR are owned by the MariaDB so they are
	// garbage-collected with it; wait until it exists.
	mdb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(mdb, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.WaitFor("Waiting for MariaDB cluster to exist")
		}
		return fmt.Errorf("get MariaDB: %w", err)
	}

	schedule, err := firstPhysicalSchedule(storage)
	if err != nil {
		return err
	}

	baseName := pitrBaseBackupName(c.Name())
	if err := reconcilePITRBaseBackup(c, mdb, storage.StorageRef.Name, schedule, baseName); err != nil {
		return err
	}
	return reconcilePITR(c, storage.StorageRef.Name, baseName)
}

// reconcilePITRBaseBackup reconciles the scheduled operator PhysicalBackup that
// serves as the PITR full base backup. Its schedule and retention are driven by
// the storage's first physical schedule; Immediate ensures a base exists as
// soon as possible so point-in-time restoration becomes available quickly.
func reconcilePITRBaseBackup(
	c *controller.Context,
	mdb *mariadbv1alpha1.MariaDB,
	storageName string,
	schedule *corev1alpha1.InstanceBackupSchedule,
	name string,
) error {
	storage, err := buildPhysicalS3Storage(c, storageName)
	if err != nil {
		return err
	}
	retention, err := deriveMaxRetention(schedule.Cron, schedule.RetentionCopies)
	if err != nil {
		return &controller.BackupConfigError{Reason: "InvalidSchedule", Message: err.Error()}
	}

	managedLabels := map[string]string{
		instanceLabel: c.Name(),
		pitrBaseLabel: "true",
	}
	opBackup := &mariadbv1alpha1.PhysicalBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace()},
	}
	_, err = controllerutil.CreateOrUpdate(c.Context(), c.Client(), opBackup, func() error {
		// spec.storage is immutable on the operator PhysicalBackup, so only set
		// it (and the labels) on creation.
		if opBackup.ResourceVersion == "" {
			opBackup.Labels = managedLabels
			opBackup.Spec.MariaDBRef = mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: c.Name()},
				WaitForIt:       true,
			}
			opBackup.Spec.Storage = storage
		}
		opBackup.Spec.MaxRetention = retention
		opBackup.Spec.Schedule = &mariadbv1alpha1.PhysicalBackupSchedule{
			Cron:      schedule.Cron,
			Suspend:   !schedule.Enabled,
			Immediate: ptr.To(true),
		}
		// The base backup is always physical; an empty parameter set defaults the
		// target to PreferReplica so single-node instances still take a backup.
		applyPhysicalBackupParameters(&opBackup.Spec, mariadbbackup.MariadbBackupParameters{})
		return controllerutil.SetControllerReference(mdb, opBackup, c.Client().Scheme())
	})
	if err != nil {
		return fmt.Errorf("reconcile PITR base backup %q: %w", name, err)
	}
	return nil
}

// reconcilePITR reconciles the PointInTimeRecovery CR that configures binary log
// archival to the storage and references the full base backup. Compression,
// archiveTimeout and strictMode use the operator defaults.
func reconcilePITR(
	c *controller.Context,
	storageName string,
	baseName string,
) error {
	s3, err := buildOperatorS3(c, storageName, c.Name()+binlogPrefixSuffix)
	if err != nil {
		return err
	}
	pitr := &mariadbv1alpha1.PointInTimeRecovery{
		ObjectMeta: metav1.ObjectMeta{Name: pitrCRName(c.Name()), Namespace: c.Namespace()},
	}
	_, err = controllerutil.CreateOrUpdate(c.Context(), c.Client(), pitr, func() error {
		// spec.compression is immutable; the base backup reference is set once
		// so the timeline is not silently rebased.
		if pitr.ResourceVersion == "" {
			pitr.Spec.PhysicalBackupRef = mariadbv1alpha1.LocalObjectReference{Name: baseName}
		}
		pitr.Spec.PointInTimeRecoveryStorage = mariadbv1alpha1.PointInTimeRecoveryStorage{S3: s3}
		// Owned by the Instance (not the MariaDB) so the Instance reconciler's
		// WatchOwned re-enqueues on PITR status changes; the Instance still GCs
		// it on deletion.
		return controllerutil.SetControllerReference(c.Instance(), pitr, c.Client().Scheme())
	})
	if err != nil {
		return fmt.Errorf("reconcile PointInTimeRecovery %q: %w", pitrCRName(c.Name()), err)
	}
	return nil
}

// teardownPITR deletes the PointInTimeRecovery CR and the base PhysicalBackup
// when PITR is no longer enabled. Absence is not an error.
func teardownPITR(c *controller.Context) error {
	pitr := &mariadbv1alpha1.PointInTimeRecovery{}
	if err := c.Get(pitr, pitrCRName(c.Name())); err == nil {
		if pitr.DeletionTimestamp.IsZero() {
			if err := c.Delete(pitr); err != nil {
				return fmt.Errorf("delete PointInTimeRecovery %q: %w", pitr.Name, err)
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get PointInTimeRecovery for teardown: %w", err)
	}

	base := &mariadbv1alpha1.PhysicalBackup{}
	if err := c.Get(base, pitrBaseBackupName(c.Name())); err == nil {
		if base.DeletionTimestamp.IsZero() {
			if err := c.Delete(base); err != nil {
				return fmt.Errorf("delete PITR base backup %q: %w", base.Name, err)
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get PITR base backup for teardown: %w", err)
	}
	return nil
}

// desiredPITRRef returns the MariaDB.spec.pointInTimeRecoveryRef to set for the
// Instance, or nil when PITR is disabled or its CR does not exist yet. Gating on
// existence avoids pointing the MariaDB at a PointInTimeRecovery that has not
// been created, which would leave the operator waiting.
func desiredPITRRef(c *controller.Context) (*mariadbv1alpha1.LocalObjectReference, error) {
	storage, err := pitrEnabledStorage(c.Instance().Spec.Backup)
	if err != nil || storage == nil {
		return nil, err
	}
	name := pitrCRName(c.Name())
	pitr := &mariadbv1alpha1.PointInTimeRecovery{}
	if err := c.Get(pitr, name); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get PointInTimeRecovery: %w", err)
	}
	return &mariadbv1alpha1.LocalObjectReference{Name: name}, nil
}
