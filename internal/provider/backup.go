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
	"encoding/json"
	"fmt"
	"strings"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	apicommon "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	mariadbdump "github.com/openeverest/provider-mariadb/definition/backupclasses/mariadb-dump"
)

const (
	// s3AccessKeyIDSecretKey is the Secret key holding the S3 access key id in
	// a BackupStorage credentials Secret.
	s3AccessKeyIDSecretKey = "AWS_ACCESS_KEY_ID"
	// s3SecretAccessKeySecretKey is the Secret key holding the S3 secret access
	// key in a BackupStorage credentials Secret.
	s3SecretAccessKeySecretKey = "AWS_SECRET_ACCESS_KEY"
)

// buildOperatorS3Storage resolves the named BackupStorage into the operator's
// S3 storage spec. Only S3-compatible storages are supported; anything else is
// rejected as a configuration error.
func buildOperatorS3Storage(c *controller.Context, storageName string) (mariadbv1alpha1.BackupStorage, error) {
	bs, err := c.BackupStorage(storageName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return mariadbv1alpha1.BackupStorage{}, controller.WaitFor(
				fmt.Sprintf("BackupStorage %q not yet present", storageName))
		}
		return mariadbv1alpha1.BackupStorage{}, err
	}
	if bs.Spec.S3 == nil {
		return mariadbv1alpha1.BackupStorage{}, &controller.BackupConfigError{
			Reason:  "UnsupportedStorage",
			Message: fmt.Sprintf("BackupStorage %q is not S3-compatible", storageName),
		}
	}
	s3 := bs.Spec.S3

	// The operator expects the endpoint without a scheme and derives TLS from
	// the dedicated flag below.
	endpoint := s3.EndpointURL
	useTLS := strings.HasPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")

	credentialsSecret := s3.CredentialsSecretRef.Name
	return mariadbv1alpha1.BackupStorage{
		S3: &mariadbv1alpha1.S3{
			Bucket:   s3.Bucket,
			Endpoint: endpoint,
			Region:   s3.Region,
			// Isolate an Instance's backups within a shared bucket.
			Prefix: c.Name(),
			AccessKeyIdSecretKeyRef: &mariadbv1alpha1.SecretKeySelector{
				LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: credentialsSecret},
				Key:                  s3AccessKeyIDSecretKey,
			},
			SecretAccessKeySecretKeyRef: &mariadbv1alpha1.SecretKeySelector{
				LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: credentialsSecret},
				Key:                  s3SecretAccessKeySecretKey,
			},
			TLS: &mariadbv1alpha1.TLSConfig{Enabled: useTLS},
		},
	}, nil
}

// parseBackupParameters decodes the Backup CR's structured parameters into the
// mariadb-dump backup parameters. An absent payload yields the zero value.
func parseBackupParameters(backup *backupv1alpha1.Backup) (mariadbdump.MariadbDumpBackupParameters, error) {
	var params mariadbdump.MariadbDumpBackupParameters
	if backup.Spec.Parameters == nil || len(backup.Spec.Parameters.Raw) == 0 {
		return params, nil
	}
	if err := json.Unmarshal(backup.Spec.Parameters.Raw, &params); err != nil {
		return params, &controller.BackupConfigError{
			Reason:  "InvalidParameters",
			Message: fmt.Sprintf("decode backup parameters: %v", err),
		}
	}
	return params, nil
}

// SyncBackup creates or updates a mariadb-operator Backup CR that dumps the
// Instance's MariaDB to the S3 storage referenced by backup.spec.storageRef,
// then maps the operator's Complete condition into the BackupExecutionStatus
// the runtime reflects onto the Backup CR.
func (p *MariaDBProvider) SyncBackup(
	c *controller.Context,
	backup *backupv1alpha1.Backup,
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

	opBackup := &mariadbv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backup.Name,
			Namespace: backup.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), opBackup, func() error {
		// The operator Backup spec is immutable (spec.storage, spec.mariaDbRef)
		// and backups are run-once, so only populate it on creation (empty
		// ResourceVersion means the object does not exist yet).
		if opBackup.ResourceVersion == "" {
			storage, err := buildOperatorS3Storage(c, backup.Spec.StorageRef.Name)
			if err != nil {
				return err
			}
			params, err := parseBackupParameters(backup)
			if err != nil {
				return err
			}
			opBackup.Spec.MariaDBRef = mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: c.Name()},
				WaitForIt:       true,
			}
			opBackup.Spec.Storage = storage
			opBackup.Spec.Databases = params.Databases
			opBackup.Spec.IgnoreGlobalPriv = params.IgnoreGlobalPriv
			if params.Compression != "" {
				opBackup.Spec.Compression = mariadbv1alpha1.CompressAlgorithm(params.Compression)
			}
		}
		return controllerutil.SetControllerReference(backup, opBackup, c.Client().Scheme())
	}); err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("create or update MariaDB backup: %w", err)
	}

	exec := mapBackupState(opBackup.Status.Conditions)
	exec.OperatorBackupRef = &apicommon.TypedObjectRef{
		Group: mariadbv1alpha1.GroupVersion.Group,
		Kind:  "Backup",
		Name:  opBackup.Name,
	}
	return exec, nil
}

// SyncRestore creates or updates a mariadb-operator Restore CR that restores
// the Instance's MariaDB from the operator Backup mirrored by the Backup CR
// named in restore.spec.dataSource.backup.backupRef.
func (p *MariaDBProvider) SyncRestore(
	c *controller.Context,
	restore *backupv1alpha1.Restore,
) (controller.RestoreExecutionStatus, error) {
	if restore.Spec.DataSource.Type != backupv1alpha1.DataSourceTypeBackup {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: fmt.Sprintf("Unsupported dataSource type %q", restore.Spec.DataSource.Type),
		}, nil
	}
	ref := restore.Spec.DataSource.Backup
	if ref == nil || ref.BackupRef.Name == "" {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: "Restore dataSource.backup.backupRef.name is required",
		}, nil
	}

	sourceBackup := &backupv1alpha1.Backup{}
	if err := c.Get(sourceBackup, ref.BackupRef.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.RestoreExecutionStatus{
				State:   backupv1alpha1.RestoreStatePending,
				Message: "Waiting for source Backup",
			}, nil
		}
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get source Backup: %w", err)
	}
	if sourceBackup.Status.State == backupv1alpha1.BackupStateFailed {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: "Source Backup failed; cannot restore",
		}, nil
	}
	if sourceBackup.Status.State != backupv1alpha1.BackupStateSucceeded {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStatePending,
			Message: "Waiting for source Backup to succeed",
		}, nil
	}

	// SyncBackup names the operator Backup after the Backup CR, so the same
	// name resolves the source; prefer the recorded ref when present.
	operatorBackupName := sourceBackup.Name
	if sourceBackup.Status.OperatorBackupRef != nil && sourceBackup.Status.OperatorBackupRef.Name != "" {
		operatorBackupName = sourceBackup.Status.OperatorBackupRef.Name
	}

	opRestore := &mariadbv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restore.Name,
			Namespace: restore.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), opRestore, func() error {
		// The operator Restore spec is immutable and restores are run-once, so
		// only populate it on creation (empty ResourceVersion means the object
		// does not exist yet).
		if opRestore.ResourceVersion == "" {
			opRestore.Spec.MariaDBRef = mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: c.Name()},
				WaitForIt:       true,
			}
			opRestore.Spec.BackupRef = &mariadbv1alpha1.LocalObjectReference{Name: operatorBackupName}
		}
		return controllerutil.SetControllerReference(restore, opRestore, c.Client().Scheme())
	}); err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("create or update MariaDB restore: %w", err)
	}

	out := mapRestoreState(opRestore.Status.Conditions)
	out.OperatorRestoreRef = &apicommon.TypedObjectRef{
		Group: mariadbv1alpha1.GroupVersion.Group,
		Kind:  "Restore",
		Name:  opRestore.Name,
	}
	return out, nil
}

// CleanupBackup deletes the operator Backup CR. The mariadb-operator does not
// purge S3 objects on Backup deletion (retention is time-based via
// maxRetention), so DeletionPolicy=Delete removes only the CR; the underlying
// data is left to the storage's retention policy.
func (p *MariaDBProvider) CleanupBackup(c *controller.Context, backup *backupv1alpha1.Backup) (bool, error) {
	opBackup := &mariadbv1alpha1.Backup{}
	err := c.Get(opBackup, backup.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get MariaDB backup for cleanup: %w", err)
	}
	if opBackup.DeletionTimestamp.IsZero() {
		if err := c.Delete(opBackup); err != nil {
			return false, fmt.Errorf("delete MariaDB backup: %w", err)
		}
	}
	return false, nil
}

// CleanupRestore deletes the operator Restore CR. The Restore is
// run-to-completion and carries no protective finalizer, so a single delete is
// sufficient.
func (p *MariaDBProvider) CleanupRestore(c *controller.Context, restore *backupv1alpha1.Restore) (bool, error) {
	opRestore := &mariadbv1alpha1.Restore{}
	err := c.Get(opRestore, restore.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get MariaDB restore for cleanup: %w", err)
	}
	if opRestore.DeletionTimestamp.IsZero() {
		if err := c.Delete(opRestore); err != nil {
			return false, fmt.Errorf("delete MariaDB restore: %w", err)
		}
	}
	return false, nil
}

// mapBackupState translates the operator Backup's Complete condition into a
// BackupExecutionStatus. The operator reports success and terminal failure both
// as Complete=True, distinguished only by the condition reason, so the mapping
// keys on the reason rather than the status.
func mapBackupState(conditions []metav1.Condition) controller.BackupExecutionStatus {
	cond := meta.FindStatusCondition(conditions, mariadbv1alpha1.ConditionTypeComplete)
	if cond == nil {
		return controller.BackupExecutionStatus{State: backupv1alpha1.BackupStatePending}
	}
	switch cond.Reason {
	case mariadbv1alpha1.ConditionReasonJobComplete, mariadbv1alpha1.ConditionReasonCronJobSuccess:
		completedAt := cond.LastTransitionTime
		return controller.BackupExecutionStatus{
			State:       backupv1alpha1.BackupStateSucceeded,
			CompletedAt: &completedAt,
		}
	case mariadbv1alpha1.ConditionReasonJobFailed, mariadbv1alpha1.ConditionReasonFailed:
		return controller.BackupExecutionStatus{
			State:   backupv1alpha1.BackupStateFailed,
			Message: cond.Message,
		}
	default:
		return controller.BackupExecutionStatus{
			State:   backupv1alpha1.BackupStateRunning,
			Message: cond.Message,
		}
	}
}

// mapRestoreState translates the operator Restore's Complete condition into a
// RestoreExecutionStatus, keying on the reason for the same reason as
// mapBackupState.
func mapRestoreState(conditions []metav1.Condition) controller.RestoreExecutionStatus {
	cond := meta.FindStatusCondition(conditions, mariadbv1alpha1.ConditionTypeComplete)
	if cond == nil {
		return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStatePending}
	}
	switch cond.Reason {
	case mariadbv1alpha1.ConditionReasonJobComplete, mariadbv1alpha1.ConditionReasonCronJobSuccess:
		completedAt := cond.LastTransitionTime
		return controller.RestoreExecutionStatus{
			State:       backupv1alpha1.RestoreStateSucceeded,
			CompletedAt: &completedAt,
		}
	case mariadbv1alpha1.ConditionReasonJobFailed, mariadbv1alpha1.ConditionReasonFailed:
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: cond.Message,
		}
	default:
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateRunning,
			Message: cond.Message,
		}
	}
}
