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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	mariadbbackup "github.com/openeverest/provider-mariadb/definition/backupclasses/mariadb"
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
	// typeLabel identifies the backup strategy (logical|physical) of a scheduled
	// backup, so Mirror can reconstruct the run's parameters.type.
	typeLabel = "backup.provider-mariadb.openeverest.io/type"

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

// scheduledBackupLabels returns the labels stamped on a scheduled operator
// backup CR and propagated (via inheritMetadata) onto every run Job, so Mirror
// can recognize and reconstruct scheduled runs.
func scheduledBackupLabels(c *controller.Context, scheduleName, storageName, backupType string) map[string]string {
	return map[string]string{
		scheduleLabel: scheduleName,
		instanceLabel: c.Name(),
		storageLabel:  storageName,
		classLabel:    c.Instance().Spec.Backup.ClassRef.Name,
		typeLabel:     backupType,
	}
}

// deleteScheduledBackup removes a logical scheduled Backup with the given name,
// used when a schedule switches strategy to physical. Absence is not an error.
func deleteScheduledBackup(c *controller.Context, name string) error {
	b := &mariadbv1alpha1.Backup{}
	if err := c.Get(b, name); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get scheduled backup %q: %w", name, err)
	}
	if err := c.Delete(b); err != nil {
		return fmt.Errorf("delete scheduled backup %q: %w", name, err)
	}
	return nil
}

// deleteScheduledPhysicalBackup removes a physical scheduled PhysicalBackup with
// the given name, used when a schedule switches strategy to logical. Absence is
// not an error.
func deleteScheduledPhysicalBackup(c *controller.Context, name string) error {
	b := &mariadbv1alpha1.PhysicalBackup{}
	if err := c.Get(b, name); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get scheduled physical backup %q: %w", name, err)
	}
	if err := c.Delete(b); err != nil {
		return fmt.Errorf("delete scheduled physical backup %q: %w", name, err)
	}
	return nil
}

// SyncScheduledBackups reconciles one operator backup CR (Backup or
// PhysicalBackup, per each schedule's type) with spec.schedule per
// InstanceBackupSchedule declared on the Instance, and prunes operator backups
// for schedules that no longer exist. Individual runs are surfaced as Backup
// CRs by Mirror.
func SyncScheduledBackups(c *controller.Context) error {
	backupCfg := c.Instance().Spec.Backup

	desiredLogical := map[string]bool{}
	desiredPhysical := map[string]bool{}
	if backupCfg != nil && backupCfg.Enabled {
		var mdb *mariadbv1alpha1.MariaDB
		for i := range backupCfg.Storages {
			storage := &backupCfg.Storages[i]
			for j := range storage.Schedules {
				schedule := &storage.Schedules[j]
				name := scheduledBackupName(c.Name(), schedule.Name)
				params, err := parseBackupParams(rawParameters(schedule.Parameters))
				if err != nil {
					return err
				}

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
				if isPhysical(params) {
					desiredPhysical[name] = true
					if err := reconcileScheduledPhysicalBackup(c, mdb, storage.StorageRef.Name, schedule, name, params); err != nil {
						return err
					}
				} else {
					desiredLogical[name] = true
					if err := reconcileScheduledBackup(c, mdb, storage.StorageRef.Name, schedule, name, params); err != nil {
						return err
					}
				}
			}
		}
	}

	if err := pruneScheduledBackups(c, desiredLogical); err != nil {
		return err
	}
	return pruneScheduledPhysicalBackups(c, desiredPhysical)
}

func reconcileScheduledBackup(
	c *controller.Context,
	mdb *mariadbv1alpha1.MariaDB,
	storageName string,
	schedule *corev1alpha1.InstanceBackupSchedule,
	name string,
	params mariadbbackup.MariadbBackupParameters,
) error {
	storage, err := buildOperatorS3Storage(c, storageName)
	if err != nil {
		return err
	}
	retention, err := deriveMaxRetention(schedule.Cron, schedule.RetentionCopies)
	if err != nil {
		return &controller.BackupConfigError{Reason: "InvalidSchedule", Message: err.Error()}
	}

	// A physical PhysicalBackup with the same name may exist if this schedule's
	// type was switched; remove it so the logical CR can take over.
	if err := deleteScheduledPhysicalBackup(c, name); err != nil {
		return err
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

	managedLabels := scheduledBackupLabels(c, schedule.Name, storageName, backupTypeLogical)
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
		applyLogicalBackupParameters(&opBackup.Spec, params)
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
	spec := backupv1alpha1.BackupSpec{
		InstanceRef:  commonv1alpha1.ObjectRef{Name: instanceName},
		ClassRef:     commonv1alpha1.ObjectRef{Name: className},
		StorageRef:   commonv1alpha1.ObjectRef{Name: storageName},
		ScheduleName: scheduleName,
	}
	// Carry the run's strategy so restore/seeding can tell logical from
	// physical from the mirrored Backup alone.
	if backupType := labels[typeLabel]; backupType != "" {
		raw, err := json.Marshal(map[string]string{"type": backupType})
		if err != nil {
			return nil, fmt.Errorf("marshal mirrored backup parameters: %w", err)
		}
		spec.Parameters = &runtime.RawExtension{Raw: raw}
	}
	return &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: job.Namespace,
		},
		Spec: spec,
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

