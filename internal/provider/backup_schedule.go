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
	"context"
	"crypto/sha1" //nolint:gosec // short, non-cryptographic name derivation
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	mariadbdump "github.com/openeverest/provider-mariadb/definition/backupclasses/mariadb-dump"
)

const (
	// scheduleLabel identifies the InstanceBackupSchedule a resource belongs to.
	// It is stamped on the operator Backup CR and, via inheritMetadata, on every
	// run Job so the mirror can recognize scheduled runs.
	scheduleLabel = "backup.provider-mariadb.openeverest.io/schedule"
	// instanceLabel identifies the owning Instance.
	instanceLabel = "backup.provider-mariadb.openeverest.io/instance"
	// storageLabel identifies the BackupStorage a scheduled backup targets.
	storageLabel = "backup.provider-mariadb.openeverest.io/storage"
	// classLabel identifies the BackupClass a scheduled backup was taken with.
	classLabel = "backup.provider-mariadb.openeverest.io/class"

	// keepAllRetention approximates "keep every backup" for schedules that set
	// retentionCopies to 0, since the operator only supports duration retention.
	keepAllRetention = 100 * 365 * 24 * time.Hour
)

// scheduledBackupName derives a stable, short (<=52 char) name for the operator
// Backup CR backing a schedule. The operator turns this into a CronJob, whose
// name is bounded by Kubernetes to 52 characters, so an opaque hash is used
// rather than "<instance>-<schedule>".
func scheduledBackupName(instance, schedule string) string {
	sum := sha1.Sum([]byte(instance + "\x00" + schedule)) //nolint:gosec
	return "mdb-sched-" + hex.EncodeToString(sum[:])[:12]
}

// deriveMaxRetention approximates a retention duration from a schedule's cron
// cadence and desired copy count, since the operator enforces retention by age
// rather than by count. retentionCopies<=0 means "keep all".
func deriveMaxRetention(cronExpr string, copies int32) (metav1.Duration, error) {
	if copies <= 0 {
		return metav1.Duration{Duration: keepAllRetention}, nil
	}
	sched, err := cron.ParseStandard(cronExpr)
	if err != nil {
		return metav1.Duration{}, fmt.Errorf("parse cron %q: %w", cronExpr, err)
	}
	now := time.Now()
	next := sched.Next(now)
	interval := sched.Next(next).Sub(next)
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	// Retain the desired copies plus one interval of slack so a fresh backup is
	// never pruned before its predecessor count is met.
	return metav1.Duration{Duration: interval*time.Duration(copies) + interval}, nil
}

// parseScheduleParameters decodes a schedule's structured parameters into the
// mariadb-dump backup parameters. An absent payload yields the zero value.
func parseScheduleParameters(schedule *corev1alpha1.InstanceBackupSchedule) (mariadbdump.MariadbDumpBackupParameters, error) {
	var params mariadbdump.MariadbDumpBackupParameters
	if schedule.Parameters == nil || len(schedule.Parameters.Raw) == 0 {
		return params, nil
	}
	if err := json.Unmarshal(schedule.Parameters.Raw, &params); err != nil {
		return params, &controller.BackupConfigError{
			Reason:  "InvalidParameters",
			Message: fmt.Sprintf("decode schedule %q parameters: %v", schedule.Name, err),
		}
	}
	return params, nil
}

// SyncScheduledBackups reconciles one operator Backup CR (with spec.schedule)
// per InstanceBackupSchedule declared on the Instance, and prunes operator
// Backups for schedules that no longer exist. Individual runs are surfaced as
// Backup CRs by Mirror.
func SyncScheduledBackups(c *controller.Context) error {
	backupCfg := c.Instance().Spec.Backup

	desired := map[string]bool{}
	if backupCfg != nil && backupCfg.Enabled {
		var mdb *mariadbv1alpha1.MariaDB
		for i := range backupCfg.Storages {
			storage := &backupCfg.Storages[i]
			for j := range storage.Schedules {
				schedule := &storage.Schedules[j]
				name := scheduledBackupName(c.Name(), schedule.Name)
				desired[name] = true

				// Fetch the owner MariaDB lazily, only when a schedule exists.
				if mdb == nil {
					mdb = &mariadbv1alpha1.MariaDB{}
					if err := c.Get(mdb, c.Name()); err != nil {
						if apierrors.IsNotFound(err) {
							return controller.WaitFor("Waiting for MariaDB cluster to exist")
						}
						return fmt.Errorf("get MariaDB: %w", err)
					}
				}
				if err := reconcileScheduledBackup(c, mdb, storage.StorageRef.Name, schedule, name); err != nil {
					return err
				}
			}
		}
	}

	return pruneScheduledBackups(c, desired)
}

func reconcileScheduledBackup(
	c *controller.Context,
	mdb *mariadbv1alpha1.MariaDB,
	storageName string,
	schedule *corev1alpha1.InstanceBackupSchedule,
	name string,
) error {
	storage, err := buildOperatorS3Storage(c, storageName)
	if err != nil {
		return err
	}
	params, err := parseScheduleParameters(schedule)
	if err != nil {
		return err
	}
	retention, err := deriveMaxRetention(schedule.Cron, schedule.RetentionCopies)
	if err != nil {
		return &controller.BackupConfigError{Reason: "InvalidSchedule", Message: err.Error()}
	}

	// spec.storage and spec.maxRetention are immutable on the operator Backup;
	// if either changed, recreate the CR so the new values take effect.
	existing := &mariadbv1alpha1.Backup{}
	if getErr := c.Get(existing, name); getErr == nil {
		if !reflect.DeepEqual(existing.Spec.Storage, storage) || existing.Spec.MaxRetention != retention {
			if err := c.Delete(existing); err != nil {
				return fmt.Errorf("delete outdated scheduled backup %q: %w", name, err)
			}
			return controller.WaitFor(fmt.Sprintf("Recreating scheduled backup %q", name))
		}
	} else if !apierrors.IsNotFound(getErr) {
		return fmt.Errorf("get scheduled backup %q: %w", name, getErr)
	}

	managedLabels := map[string]string{
		scheduleLabel: schedule.Name,
		instanceLabel: c.Name(),
		storageLabel:  storageName,
		classLabel:    c.Instance().Spec.Backup.ClassRef.Name,
	}
	opBackup := &mariadbv1alpha1.Backup{
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
			opBackup.Spec.MaxRetention = retention
			// Propagated onto each run Job so Mirror can identify scheduled runs.
			opBackup.Spec.InheritMetadata = &mariadbv1alpha1.Metadata{Labels: managedLabels}
		}
		opBackup.Spec.Schedule = &mariadbv1alpha1.Schedule{
			Cron:    schedule.Cron,
			Suspend: !schedule.Enabled,
		}
		opBackup.Spec.Databases = params.Databases
		opBackup.Spec.IgnoreGlobalPriv = params.IgnoreGlobalPriv
		if params.Compression != "" {
			opBackup.Spec.Compression = mariadbv1alpha1.CompressAlgorithm(params.Compression)
		}
		return controllerutil.SetControllerReference(mdb, opBackup, c.Client().Scheme())
	})
	if err != nil {
		return fmt.Errorf("reconcile scheduled backup %q: %w", name, err)
	}
	return nil
}

// pruneScheduledBackups deletes operator Backups managed by this provider whose
// schedule is no longer declared on the Instance.
func pruneScheduledBackups(c *controller.Context, desired map[string]bool) error {
	list := &mariadbv1alpha1.BackupList{}
	if err := c.List(list, client.MatchingLabels{instanceLabel: c.Name()}); err != nil {
		return fmt.Errorf("list scheduled backups: %w", err)
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
			return fmt.Errorf("delete stale scheduled backup %q: %w", item.Name, err)
		}
	}
	return nil
}

// Mirror implements controller.BackupMirror. It surfaces each scheduled backup
// run (a Job produced by an operator Backup CronJob) as a first-class Backup CR
// named after the Job. Jobs that are not scheduled backup runs are skipped.
func (p *MariaDBProvider) Mirror(_ context.Context, _ client.Client, obj client.Object) (*backupv1alpha1.Backup, error) {
	job, ok := obj.(*batchv1.Job)
	if !ok {
		return nil, nil
	}
	labels := job.GetLabels()
	scheduleName := labels[scheduleLabel]
	className := labels[classLabel]
	storageName := labels[storageLabel]
	instanceName := labels[instanceLabel]
	if scheduleName == "" || className == "" || storageName == "" || instanceName == "" {
		return nil, nil
	}
	return &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: job.Namespace,
		},
		Spec: backupv1alpha1.BackupSpec{
			InstanceRef:  commonv1alpha1.ObjectRef{Name: instanceName},
			ClassRef:     commonv1alpha1.ObjectRef{Name: className},
			StorageRef:   commonv1alpha1.ObjectRef{Name: storageName},
			ScheduleName: scheduleName,
		},
	}, nil
}

// OperatorBackupType implements controller.BackupMirror.
func (p *MariaDBProvider) OperatorBackupType() client.Object {
	return &batchv1.Job{}
}

// syncScheduledRun reports the status of a mirrored scheduled backup from its
// underlying Job, which shares the Backup CR's name. It never creates operator
// resources: the run already happened, driven by the operator's CronJob.
func syncScheduledRun(c *controller.Context, backup *backupv1alpha1.Backup) (controller.BackupExecutionStatus, error) {
	job := &batchv1.Job{}
	if err := c.Get(job, backup.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.BackupExecutionStatus{
				State:   backupv1alpha1.BackupStatePending,
				Message: "Waiting for scheduled backup Job",
			}, nil
		}
		return controller.BackupExecutionStatus{}, fmt.Errorf("get scheduled backup Job: %w", err)
	}
	exec := mapJobState(job)
	exec.OperatorBackupRef = &commonv1alpha1.TypedObjectRef{
		Group: batchv1.SchemeGroupVersion.Group,
		Kind:  "Job",
		Name:  job.Name,
	}
	return exec, nil
}

// mapJobState translates a run Job's conditions into a BackupExecutionStatus.
func mapJobState(job *batchv1.Job) controller.BackupExecutionStatus {
	for i := range job.Status.Conditions {
		cond := &job.Status.Conditions[i]
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			completedAt := cond.LastTransitionTime
			return controller.BackupExecutionStatus{
				State:       backupv1alpha1.BackupStateSucceeded,
				CompletedAt: &completedAt,
			}
		case batchv1.JobFailed:
			return controller.BackupExecutionStatus{
				State:   backupv1alpha1.BackupStateFailed,
				Message: cond.Message,
			}
		}
	}
	return controller.BackupExecutionStatus{State: backupv1alpha1.BackupStateRunning}
}

