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
	"os"

	goversion "github.com/hashicorp/go-version"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// minDataPlaneVersion is the oldest data plane the bundled operator manages.
// Bump it with the operator when an upgrade guide says the data plane "must
// be updated" (docs/releases/UPGRADE_<version>.md upstream); keep it when the
// update is optional. 26.10.1 changed the mariadb container env and 26.10.2
// works with its data plane.
const minDataPlaneVersion = "26.10.1"

// targetOperatorVersionEnv is set by the chart's pre-upgrade hook Job to the
// operator version the target release installs.
const targetOperatorVersionEnv = "TARGET_OPERATOR_VERSION"

// Reasons reported on upgrade issues.
const (
	reasonSkewWindowExceeded = "SkewWindowExceeded"
	reasonSkewUnverified     = "SkewUnverified"
	reasonMaintenancePending = "MaintenancePending"
)

var _ controller.UpgradeProvider = (*MariaDBProvider)(nil)

// CheckUpgrade blocks a provider upgrade that would leave a cluster with a
// held data-plane update outside the target operator's support window: a
// data-plane update may be deferred on the current operator, but not across
// an upgrade that would leave the cluster unmanageable. It also warns about
// held maintenance, which is requested again under new tokens afterwards.
func (p *MariaDBProvider) CheckUpgrade(h *controller.HookContext, _ *corev1alpha1.ProviderSpec, instances []corev1alpha1.Instance) []controller.UpgradeIssue {
	target := os.Getenv(targetOperatorVersionEnv)
	if target == "" {
		// The hook's Job template sets it; absence means the chart drifted.
		return []controller.UpgradeIssue{{
			Severity: controller.UpgradeWarning,
			Reason:   reasonSkewUnverified,
			Message:  "cannot verify the data-plane skew: " + targetOperatorVersionEnv + " is not set on the preflight job",
		}}
	}
	if _, err := goversion.NewVersion(target); err != nil {
		return []controller.UpgradeIssue{{
			Severity: controller.UpgradeWarning,
			Reason:   reasonSkewUnverified,
			Message:  fmt.Sprintf("cannot verify the data-plane skew: target version %q is not a valid version", target),
		}}
	}

	var issues []controller.UpgradeIssue
	for i := range instances {
		in := &instances[i]
		if in.DeletionTimestamp != nil {
			continue
		}
		if issue := checkDataPlaneSkew(h, in, target); issue != nil {
			issues = append(issues, *issue)
		}
		if n := len(in.Status.PendingMaintenance); n > 0 {
			issues = append(issues, controller.UpgradeIssue{
				Severity:     controller.UpgradeWarning,
				InstanceName: in.Name,
				Namespace:    in.Namespace,
				Reason:       reasonMaintenancePending,
				Message: fmt.Sprintf(
					"%d maintenance action(s) are waiting for approval; after the upgrade they may be listed again with new approval tokens",
					n),
			})
		}
	}
	return issues
}

func checkDataPlaneSkew(h *controller.HookContext, in *corev1alpha1.Instance, target string) *controller.UpgradeIssue {
	issue := &controller.UpgradeIssue{
		InstanceName: in.Name,
		Namespace:    in.Namespace,
		Reason:       reasonSkewUnverified,
		Severity:     controller.UpgradeWarning,
	}
	mdb := &mariadbv1alpha1.MariaDB{}
	err := h.Client().Get(h.Context(), client.ObjectKey{Namespace: in.Namespace, Name: in.Name}, mdb)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		issue.Message = fmt.Sprintf("cannot verify the data-plane skew: fetching the database failed: %v", err)
		return issue
	}
	current := dataPlaneVersion(mdb)
	if current == "" {
		return nil
	}
	if _, err := goversion.NewVersion(current); err != nil {
		issue.Message = fmt.Sprintf("cannot verify the data-plane skew: version %q is not a valid version", current)
		return issue
	}

	switch {
	case versionLessThan(current, minDataPlaneVersion):
		issue.Severity = controller.UpgradeError
		issue.Reason = reasonSkewWindowExceeded
		issue.Message = fmt.Sprintf(
			"this database runs management sidecars %s, older than the %s the target provider release supports; approve the pending sidecar update (spec.maintenance.approved) and let it roll out before upgrading the provider, or upgrade through an intermediate provider release",
			current, minDataPlaneVersion)
		return issue
	case versionLessThan(target, current):
		// A chart rollback: the older operator keeps the newer sidecars.
		issue.Reason = reasonSkewWindowExceeded
		issue.Message = fmt.Sprintf(
			"this database runs management sidecars %s, newer than the target provider release's %s; a downgrade keeps them as they are",
			current, target)
		return issue
	}
	return nil
}
