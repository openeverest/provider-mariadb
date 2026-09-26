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
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// Reasons published on InstanceBackupStoragePITRStatus.
const (
	pitrReasonWindowAvailable = "WindowAvailable"
	pitrReasonNoBaseBackup    = "NoRecoverableTime"
)

var _ controller.InstanceBackupStatusReporter = (*MariaDBProvider)(nil)

// BackupStorageStatuses publishes the point-in-time recovery window observed on
// the Instance's PITR-enabled storage. The window end is the operator's
// lastRecoverableTime; the operator does not expose a window start, so
// EarliestRestorableTime is left unset.
func (p *MariaDBProvider) BackupStorageStatuses(c *controller.Context) ([]corev1alpha1.InstanceBackupStorageStatus, error) {
	storage, err := pitrEnabledStorage(c.Instance().Spec.Backup)
	if err != nil {
		// Misconfiguration is surfaced by validation and Sync; do not fail the
		// observability path over it.
		return nil, nil //nolint:nilerr
	}
	if storage == nil {
		return nil, nil
	}

	pitr := &mariadbv1alpha1.PointInTimeRecovery{}
	if err := c.Get(pitr, pitrCRName(c.Name())); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get PointInTimeRecovery: %w", err)
	}

	return []corev1alpha1.InstanceBackupStorageStatus{
		{
			Name: storage.StorageRef.Name,
			PITR: pitrWindow(pitr.Status.LastRecoverableTime),
		},
	}, nil
}

// pitrWindow maps the operator's lastRecoverableTime (RFC3339) into the recovery
// window status. Until a full base backup has completed and binary logs have
// been archived, the operator publishes no recoverable time and the window is
// Unavailable.
func pitrWindow(lastRecoverableTime *string) *corev1alpha1.InstanceBackupStoragePITRStatus {
	if lastRecoverableTime == nil || *lastRecoverableTime == "" {
		return &corev1alpha1.InstanceBackupStoragePITRStatus{
			State:   corev1alpha1.PITRStateUnavailable,
			Reason:  pitrReasonNoBaseBackup,
			Message: "Waiting for a full base backup and archived binary logs",
		}
	}
	parsed, err := time.Parse(time.RFC3339, *lastRecoverableTime)
	if err != nil {
		return &corev1alpha1.InstanceBackupStoragePITRStatus{
			State:   corev1alpha1.PITRStateUnavailable,
			Reason:  pitrReasonNoBaseBackup,
			Message: fmt.Sprintf("Invalid lastRecoverableTime %q: %v", *lastRecoverableTime, err),
		}
	}
	latest := metav1.NewTime(parsed)
	return &corev1alpha1.InstanceBackupStoragePITRStatus{
		LatestRestorableTime: &latest,
		State:                corev1alpha1.PITRStateAvailable,
		Reason:               pitrReasonWindowAvailable,
	}
}
