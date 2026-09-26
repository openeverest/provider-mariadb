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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

// newTopologyContext builds a controller.Context for an Instance with the given
// topology type and optional engine replica count. Extra objects (e.g. an
// existing MariaDB CR) are seeded into the fake client.
func newTopologyContext(t *testing.T, topology string, replicas *int32, objs ...client.Object) *controller.Context {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := mariadbv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add mariadb scheme: %v", err)
	}

	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentEngine: {Name: common.ComponentEngine, Type: common.ComponentTypeMariaDB, Replicas: replicas},
			},
		},
	}
	if topology != "" {
		instance.Spec.Topology = &corev1alpha1.TopologySpec{Type: topology}
	}

	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance)
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	return controller.NewContext(context.Background(), builder.Build(), instance, common.ProviderName)
}

func existingMariaDB(galera bool) *mariadbv1alpha1.MariaDB {
	mdb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}
	if galera {
		mdb.Spec.Galera = &mariadbv1alpha1.Galera{Enabled: true}
	}
	return mdb
}

func TestDefaultReplicas(t *testing.T) {
	if got := defaultReplicas(newTopologyContext(t, "", nil)); got != 1 {
		t.Errorf("standalone default = %d, want 1", got)
	}
	if got := defaultReplicas(newTopologyContext(t, "galera", nil)); got != 3 {
		t.Errorf("galera default = %d, want 3", got)
	}
	if got := defaultReplicas(newTopologyContext(t, "replication", nil)); got != 3 {
		t.Errorf("replication default = %d, want 3", got)
	}
}

func TestApplyGaleraOverlay(t *testing.T) {
	// Standalone: never touches galera.
	mdb := &mariadbv1alpha1.MariaDB{}
	applyGaleraOverlay(mdb, false)
	if mdb.Spec.Galera != nil {
		t.Errorf("expected nil galera for standalone, got %+v", mdb.Spec.Galera)
	}

	// Galera on a fresh object: enables it.
	applyGaleraOverlay(mdb, true)
	if mdb.Spec.Galera == nil || !mdb.Spec.Galera.Enabled {
		t.Fatalf("expected galera enabled, got %+v", mdb.Spec.Galera)
	}

	// Existing galera with operator-defaulted sub-fields: preserved, stays enabled.
	mdb.Spec.Galera.SST = mariadbv1alpha1.SSTMariaBackup
	applyGaleraOverlay(mdb, true)
	if !mdb.Spec.Galera.Enabled || mdb.Spec.Galera.SST != mariadbv1alpha1.SSTMariaBackup {
		t.Errorf("overlay clobbered operator defaults: %+v", mdb.Spec.Galera)
	}
}

func TestDefaultHAAffinity(t *testing.T) {
	aff := defaultHAAffinity("my-instance")
	if aff == nil || aff.PodAntiAffinity == nil {
		t.Fatalf("expected pod anti-affinity, got %+v", aff)
	}
	preferred := aff.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(preferred) != 1 {
		t.Fatalf("expected 1 preferred term (soft), got %d", len(preferred))
	}
	if len(aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
		t.Errorf("expected no required terms (must stay soft)")
	}
	term := preferred[0].PodAffinityTerm
	if term.TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("unexpected topology key: %q", term.TopologyKey)
	}
	if term.LabelSelector == nil || len(term.LabelSelector.MatchExpressions) != 1 ||
		term.LabelSelector.MatchExpressions[0].Values[0] != "my-instance" {
		t.Errorf("label selector not scoped to the instance: %+v", term.LabelSelector)
	}
}

func newEngineParamsContext(t *testing.T, params string, affinity *corev1.Affinity) *controller.Context {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := mariadbv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add mariadb scheme: %v", err)
	}

	engine := corev1alpha1.ComponentSpec{
		Name:             common.ComponentEngine,
		Type:             common.ComponentTypeMariaDB,
		SchedulingPolicy: &commonv1alpha1.SchedulingPolicy{Affinity: affinity},
	}
	if params != "" {
		engine.Parameters = &runtime.RawExtension{Raw: []byte(params)}
	}

	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: engine},
		},
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance)
	return controller.NewContext(context.Background(), builder.Build(), instance, common.ProviderName)
}

func TestValidateNodeAffinity(t *testing.T) {
	tests := []struct {
		name     string
		params   string
		affinity *corev1.Affinity
		wantErr  bool
	}{
		{name: "no params", params: "", wantErr: false},
		{name: "valid rules", params: `{"nodeAffinity":"disktype In ssd"}`, wantErr: false},
		{name: "invalid syntax", params: `{"nodeAffinity":"disktype Equals ssd"}`, wantErr: true},
		{
			name:     "conflict with raw node affinity",
			params:   `{"nodeAffinity":"disktype In ssd"}`,
			affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}},
			wantErr:  true,
		},
		{
			name:     "raw pod anti-affinity does not conflict",
			params:   `{"nodeAffinity":"disktype In ssd"}`,
			affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}},
			wantErr:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newEngineParamsContext(t, tt.params, tt.affinity)
			engine := c.Instance().Spec.Components[common.ComponentEngine]
			err := validateNodeAffinity(c, engine)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateNodeAffinity() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateTopology_Unsupported(t *testing.T) {
	c := newTopologyContext(t, "sharded", nil)
	if err := validateTopology(c); err == nil {
		t.Fatal("expected error for unsupported topology")
	}
}

func TestValidateTopology_GaleraReplicas(t *testing.T) {
	tests := []struct {
		name     string
		replicas *int32
		wantErr  bool
	}{
		{name: "default (unset) is 3", replicas: nil, wantErr: false},
		{name: "odd >= 3 ok", replicas: ptr.To(int32(5)), wantErr: false},
		{name: "too few", replicas: ptr.To(int32(1)), wantErr: true},
		{name: "even rejected", replicas: ptr.To(int32(4)), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTopologyContext(t, "galera", tt.replicas)
			err := validateTopology(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTopology() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateTopology_StandaloneReplicas(t *testing.T) {
	tests := []struct {
		name     string
		replicas *int32
		wantErr  bool
	}{
		{name: "default (unset) is 1", replicas: nil, wantErr: false},
		{name: "single node ok", replicas: ptr.To(int32(1)), wantErr: false},
		{name: "two nodes rejected", replicas: ptr.To(int32(2)), wantErr: true},
		{name: "three nodes rejected", replicas: ptr.To(int32(3)), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTopologyContext(t, "standalone", tt.replicas)
			err := validateTopology(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTopology() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateTopology_Immutable(t *testing.T) {
	tests := []struct {
		name     string
		topology string
		existing *mariadbv1alpha1.MariaDB
		wantErr  bool
	}{
		{name: "no existing MariaDB is fine", topology: "galera", existing: nil, wantErr: false},
		{name: "galera stays galera", topology: "galera", existing: existingMariaDB(true), wantErr: false},
		{name: "standalone stays standalone", topology: "", existing: existingMariaDB(false), wantErr: false},
		{name: "standalone -> galera rejected", topology: "galera", existing: existingMariaDB(false), wantErr: true},
		{name: "galera -> standalone rejected", topology: "", existing: existingMariaDB(true), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			galera := tt.topology == "galera"
			replicas := ptr.To(int32(1))
			if galera {
				replicas = ptr.To(int32(3))
			}
			var c *controller.Context
			if tt.existing != nil {
				c = newTopologyContext(t, tt.topology, replicas, tt.existing)
			} else {
				c = newTopologyContext(t, tt.topology, replicas)
			}
			err := validateTopology(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTopology() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
