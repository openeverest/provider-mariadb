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
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

func TestCheckUpgrade(t *testing.T) {
	instance := func(mutate ...func(*corev1alpha1.Instance)) corev1alpha1.Instance {
		in := corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"}}
		for _, m := range mutate {
			m(&in)
		}
		return in
	}
	tests := []struct {
		name       string
		target     string
		existing   *mariadbv1alpha1.MariaDB
		instance   corev1alpha1.Instance
		wantReason string
		wantSev    controller.UpgradeSeverity
	}{
		{name: "data plane within the window", target: "26.10.2", existing: dataPlaneMariaDB("galera", "26.10.1"), instance: instance()},
		{name: "data plane current", target: "26.10.2", existing: dataPlaneMariaDB("replication", "26.10.2"), instance: instance()},
		{name: "standalone", target: "26.10.2", existing: dataPlaneMariaDB("standalone", ""), instance: instance()},
		{name: "no MariaDB yet", target: "26.10.2", instance: instance()},
		{
			name: "data plane below the window blocks", target: "26.10.2",
			existing: dataPlaneMariaDB("replication", "26.10.0"), instance: instance(),
			wantReason: reasonSkewWindowExceeded, wantSev: controller.UpgradeError,
		},
		{
			name: "deleting Instance is skipped", target: "26.10.2", existing: dataPlaneMariaDB("replication", "26.10.0"),
			instance: instance(func(in *corev1alpha1.Instance) { in.DeletionTimestamp = &metav1.Time{} }),
		},
		{
			name: "rollback below the data plane warns", target: "26.10.1",
			existing: dataPlaneMariaDB("galera", "26.10.2"), instance: instance(),
			wantReason: reasonSkewWindowExceeded, wantSev: controller.UpgradeWarning,
		},
		{
			name: "unparseable data plane warns", target: "26.10.2",
			existing: dataPlaneMariaDB("galera", "latest"), instance: instance(),
			wantReason: reasonSkewUnverified, wantSev: controller.UpgradeWarning,
		},
		{
			name: "missing target version warns", existing: dataPlaneMariaDB("galera", "26.10.0"), instance: instance(),
			wantReason: reasonSkewUnverified, wantSev: controller.UpgradeWarning,
		},
		{
			name: "held maintenance warns", target: "26.10.2", existing: dataPlaneMariaDB("galera", "26.10.2"),
			instance: instance(func(in *corev1alpha1.Instance) {
				in.Status.PendingMaintenance = []corev1alpha1.PendingMaintenanceAction{{
					Description: "restart", Severity: corev1alpha1.MaintenanceRollingRestart, ApprovalToken: "t",
				}}
			}),
			wantReason: reasonMaintenancePending, wantSev: controller.UpgradeWarning,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(targetOperatorVersionEnv, tt.target)
			scheme := runtime.NewScheme()
			require.NoError(t, mariadbv1alpha1.AddToScheme(scheme))
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.existing != nil {
				builder = builder.WithObjects(tt.existing)
			}
			h := controller.NewHookContext(context.Background(), builder.Build(), common.ProviderName)

			issues := New().CheckUpgrade(h, nil, []corev1alpha1.Instance{tt.instance})

			if tt.wantReason == "" {
				assert.Empty(t, issues)
				return
			}
			require.Len(t, issues, 1)
			assert.Equal(t, tt.wantReason, issues[0].Reason)
			assert.Equal(t, tt.wantSev, issues[0].Severity)
		})
	}
}
