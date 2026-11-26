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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-mariadb/internal/common"
)

const providerNamespace = "everest-system"

func withProviderNamespace(t *testing.T) {
	t.Helper()
	orig := currentNamespace
	currentNamespace = func() (string, error) { return providerNamespace, nil }
	t.Cleanup(func() { currentNamespace = orig })
}

func operatorDeployment(name, namespace, image string, args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "controller", Image: image, Args: args}},
			}},
		},
	}
}

// dataPlaneMariaDB is a MariaDB of the given topology whose data plane the
// operator has defaulted to version; an empty version is not defaulted yet.
func dataPlaneMariaDB(topology, version string) *mariadbv1alpha1.MariaDB {
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"}}
	agent := mariadbv1alpha1.Agent{}
	if version != "" {
		agent.Image = "ghcr.io/mariadb-operator/mariadb-operator:" + version
	}
	switch topology {
	case "galera":
		mdb.Spec.Galera = &mariadbv1alpha1.Galera{Enabled: true, GaleraSpec: mariadbv1alpha1.GaleraSpec{Agent: agent}}
	case "replication":
		mdb.Spec.Replication = &mariadbv1alpha1.Replication{Enabled: true, ReplicationSpec: mariadbv1alpha1.ReplicationSpec{Agent: agent}}
	}
	return mdb
}

func TestSplitImage(t *testing.T) {
	tests := map[string][2]string{
		"ghcr.io/mariadb-operator/mariadb-operator:26.10.2":             {"ghcr.io/mariadb-operator/mariadb-operator", "26.10.2"},
		"reg.example.com:5000/mariadb-operator/mariadb-operator:v26.10": {"reg.example.com:5000/mariadb-operator/mariadb-operator", "26.10"},
		"mariadb-operator/mariadb-operator:26.10.2@sha256:abc":          {"mariadb-operator/mariadb-operator", "26.10.2"},
		"reg.example.com:5000/mariadb-operator/mariadb-operator":        {"reg.example.com:5000/mariadb-operator/mariadb-operator", ""},
		"mariadb-operator/mariadb-operator@sha256:abc":                  {"mariadb-operator/mariadb-operator", ""},
	}
	for image, want := range tests {
		repo, tag := splitImage(image)
		assert.Equal(t, want, [2]string{repo, tag}, image)
	}
}

func TestOperatorVersion(t *testing.T) {
	withProviderNamespace(t)
	tests := []struct {
		name    string
		objs    []client.Object
		want    string
		wantErr bool
	}{
		{
			name: "webhook and cert-controller do not count",
			objs: []client.Object{
				operatorDeployment("provider-mariadb", providerNamespace, "ghcr.io/openeverest/provider-mariadb:0.4.0"),
				operatorDeployment("mariadb-operator", providerNamespace, "ghcr.io/mariadb-operator/mariadb-operator:26.10.1", "--metrics-addr=:8080"),
				operatorDeployment("mariadb-operator-webhook", providerNamespace, "ghcr.io/mariadb-operator/mariadb-operator:26.10.2", "webhook", "--port=9443"),
				operatorDeployment("mariadb-operator-cert-controller", providerNamespace, "ghcr.io/mariadb-operator/mariadb-operator:26.10.2", "cert-controller"),
			},
			want: "26.10.1",
		},
		{
			name: "only the provider namespace counts",
			objs: []client.Object{
				operatorDeployment("mariadb-operator", "other", "ghcr.io/mariadb-operator/mariadb-operator:26.10.1"),
				operatorDeployment("mariadb-operator", providerNamespace, "ghcr.io/mariadb-operator/mariadb-operator:26.10.2"),
			},
			want: "26.10.2",
		},
		{
			name: "conflicting operators",
			objs: []client.Object{
				operatorDeployment("mariadb-operator", providerNamespace, "ghcr.io/mariadb-operator/mariadb-operator:26.10.2"),
				operatorDeployment("mariadb-operator-2", providerNamespace, "ghcr.io/mariadb-operator/mariadb-operator:26.10.1"),
			},
			wantErr: true,
		},
		{
			name:    "no operator",
			objs:    []client.Object{operatorDeployment("provider-mariadb", providerNamespace, "ghcr.io/openeverest/provider-mariadb:0.4.0")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSyncHarness(t, "replication", nil, tt.objs...)
			got, err := operatorVersion(h.Context)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSyncMariaDB_DataPlaneUpdate(t *testing.T) {
	withProviderNamespace(t)
	const token = dataPlaneTokenPrefix + "26.10.2"
	operator := operatorDeployment("mariadb-operator", providerNamespace, "ghcr.io/mariadb-operator/mariadb-operator:26.10.2")
	tests := []struct {
		name        string
		topology    string
		existing    *mariadbv1alpha1.MariaDB
		noOperator  bool
		maintenance *corev1alpha1.MaintenanceSpec
		wantUpdate  bool
		wantPending bool
	}{
		{name: "new cluster gets the operator's data plane on creation", topology: "replication"},
		{name: "data plane not defaulted yet", topology: "replication", existing: dataPlaneMariaDB("replication", "")},
		{name: "data plane current", topology: "galera", existing: dataPlaneMariaDB("galera", "26.10.2")},
		{name: "data plane newer after a rollback", topology: "galera", existing: dataPlaneMariaDB("galera", "26.10.3")},
		{name: "standalone has no data plane", topology: "", existing: dataPlaneMariaDB("standalone", "")},
		{
			name: "operator version unknown holds the update", topology: "replication",
			existing: dataPlaneMariaDB("replication", "26.10.1"), noOperator: true,
		},
		{
			name: "replication update waits for approval", topology: "replication",
			existing: dataPlaneMariaDB("replication", "26.10.1"), wantPending: true,
		},
		{
			name: "galera update waits for approval", topology: "galera",
			existing: dataPlaneMariaDB("galera", "26.10.1"), wantPending: true,
		},
		{
			name: "approved update", topology: "galera", existing: dataPlaneMariaDB("galera", "26.10.1"),
			maintenance: &corev1alpha1.MaintenanceSpec{Approved: token}, wantUpdate: true,
		},
		{
			name: "approval for another update", topology: "galera", existing: dataPlaneMariaDB("galera", "26.10.1"),
			maintenance: &corev1alpha1.MaintenanceSpec{Approved: dataPlaneTokenPrefix + "26.10.1"}, wantPending: true,
		},
		{
			name: "rolling restarts approved up front", topology: "replication", existing: dataPlaneMariaDB("replication", "26.10.1"),
			maintenance: &corev1alpha1.MaintenanceSpec{AutoApproveUpTo: corev1alpha1.MaintenanceRollingRestart}, wantUpdate: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []client.Object
			if !tt.noOperator {
				objs = append(objs, operator)
			}
			if tt.existing != nil {
				objs = append(objs, tt.existing)
			}
			h := newSyncHarness(t, tt.topology, nil, objs...)
			h.Instance().Spec.Maintenance = tt.maintenance

			require.NoError(t, SyncMariaDB(h.Context))

			strategy, ok := h.lastSpec(t)["updateStrategy"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tt.wantUpdate, strategy["autoUpdateDataPlane"])
			pending := h.GetPendingMaintenance()
			if !tt.wantPending {
				assert.Empty(t, pending)
				return
			}
			require.Len(t, pending, 1)
			assert.Equal(t, token, pending[0].ApprovalToken)
			assert.Equal(t, corev1alpha1.MaintenanceRollingRestart, pending[0].Severity)
			assert.Contains(t, pending[0].Description, "from 26.10.1 to 26.10.2")
		})
	}
}

func TestHoldDataPlaneUpdates(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, mariadbv1alpha1.AddToScheme(scheme))

	instance := func(name, provider string) *corev1alpha1.Instance {
		in := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		in.Spec.ProviderRef.Name = provider
		return in
	}
	mariaDB := func(name string, auto bool) *mariadbv1alpha1.MariaDB {
		mdb := dataPlaneMariaDB("galera", "26.10.1")
		mdb.Name = name
		mdb.Spec.UpdateStrategy.AutoUpdateDataPlane = ptr.To(auto)
		return mdb
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		instance("auto", common.ProviderName), mariaDB("auto", true),
		instance("held", common.ProviderName), mariaDB("held", false),
		instance("no-mariadb", common.ProviderName),
		instance("other", "other-provider"), mariaDB("other", true),
	).Build()

	require.NoError(t, HoldDataPlaneUpdates(context.Background(), cl))

	for name, want := range map[string]bool{"auto": false, "held": false, "other": true} {
		mdb := &mariadbv1alpha1.MariaDB{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, mdb))
		assert.Equal(t, want, ptr.Deref(mdb.Spec.UpdateStrategy.AutoUpdateDataPlane, true), name)
	}
}
