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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

const testMaxScaleImage = "mariadb/maxscale:test"

// newProxyContext builds a controller.Context for an Instance with the given
// topology, engine parameters and proxy parameters. An empty proxyParams omits
// the proxy component entirely.
func newProxyContext(t *testing.T, topology, engineParams, proxyParams string, objs ...client.Object) *controller.Context {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, mariadbv1alpha1.AddToScheme(scheme))

	engine := corev1alpha1.ComponentSpec{Name: common.ComponentEngine, Type: common.ComponentTypeMariaDB}
	if engineParams != "" {
		engine.Parameters = &runtime.RawExtension{Raw: []byte(engineParams)}
	}
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid"},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: engine},
		},
	}
	if topology != "" {
		instance.Spec.Topology = &corev1alpha1.TopologySpec{Type: topology}
	}
	if proxyParams != "" {
		instance.Spec.Components[common.ComponentProxy] = corev1alpha1.ComponentSpec{
			Name:       common.ComponentProxy,
			Type:       "maxscale",
			Image:      testMaxScaleImage,
			Parameters: &runtime.RawExtension{Raw: []byte(proxyParams)},
		}
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append([]client.Object{instance}, objs...)...).
		Build()
	return controller.NewContext(context.Background(), k8sClient, instance, common.ProviderName)
}

func getMaxScale(t *testing.T, c *controller.Context) *mariadbv1alpha1.MaxScale {
	t.Helper()
	mxs := &mariadbv1alpha1.MaxScale{}
	require.NoError(t, c.Get(mxs, maxScaleName(c.Name())))
	return mxs
}

func TestIsProxyEnabled(t *testing.T) {
	tests := []struct {
		name   string
		params string
		want   bool
	}{
		{name: "component absent"},
		{name: "enabled", params: `{"enabled":true}`, want: true},
		{name: "disabled", params: `{"enabled":false}`},
		{name: "UI string boolean", params: `{"enabled":"true"}`, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isProxyEnabled(newProxyContext(t, "galera", "", tt.params))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSyncMaxScale_Creates(t *testing.T) {
	c := newProxyContext(t, "replication", "", `{"enabled":true}`)
	require.NoError(t, SyncMaxScale(c))

	mxs := getMaxScale(t, c)
	require.NotNil(t, mxs.Spec.MariaDBRef)
	assert.Equal(t, "test", mxs.Spec.MariaDBRef.Name)
	assert.Equal(t, testMaxScaleImage, mxs.Spec.Image)
	assert.Equal(t, maxScaleDefaultReplicas, mxs.Spec.Replicas)
	assert.Nil(t, mxs.Spec.TLS, "TLS stays off while the engine does not require it")
	require.NotNil(t, mxs.Spec.Affinity)
	require.NotNil(t, mxs.Spec.Affinity.PodAntiAffinity)
	term := mxs.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].PodAffinityTerm
	assert.Equal(t, []string{"test-maxscale"}, term.LabelSelector.MatchExpressions[0].Values)
	require.Len(t, mxs.OwnerReferences, 1)
	assert.Equal(t, "Instance", mxs.OwnerReferences[0].Kind)
}

func TestSyncMaxScale_OverlayPreservesOperatorDefaults(t *testing.T) {
	existing := &mariadbv1alpha1.MaxScale{
		ObjectMeta: metav1.ObjectMeta{Name: "test-maxscale", Namespace: "default"},
		Spec: mariadbv1alpha1.MaxScaleSpec{
			MariaDBRef: &mariadbv1alpha1.MariaDBRef{ObjectReference: mariadbv1alpha1.ObjectReference{Name: "test"}},
			Servers:    []mariadbv1alpha1.MaxScaleServer{{Name: "test-0", Address: "test-0.test-internal"}},
			Monitor:    mariadbv1alpha1.MaxScaleMonitor{Module: mariadbv1alpha1.MonitorModuleMariadb},
			TLS: &mariadbv1alpha1.MaxScaleTLS{
				ServerCASecretRef: &mariadbv1alpha1.LocalObjectReference{Name: "test-ca-bundle"},
			},
		},
	}
	c := newProxyContext(t, "replication", `{"tls":{"required":true}}`, `{"enabled":true}`, existing)
	proxy := c.Instance().Spec.Components[common.ComponentProxy]
	proxy.Replicas = ptr.To(int32(3))
	proxy.Service = &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer}
	c.Instance().Spec.Components[common.ComponentProxy] = proxy

	require.NoError(t, SyncMaxScale(c))

	mxs := getMaxScale(t, c)
	assert.Equal(t, int32(3), mxs.Spec.Replicas)
	require.NotNil(t, mxs.Spec.KubernetesService)
	assert.Equal(t, corev1.ServiceTypeLoadBalancer, mxs.Spec.KubernetesService.Type)
	assert.Len(t, mxs.Spec.Servers, 1, "operator-defaulted servers must be preserved")
	assert.Equal(t, mariadbv1alpha1.MonitorModuleMariadb, mxs.Spec.Monitor.Module)
	require.NotNil(t, mxs.Spec.TLS)
	assert.True(t, mxs.Spec.TLS.Enabled, "TLS follows the engine's enforcement")
	assert.Equal(t, "test-ca-bundle", mxs.Spec.TLS.ServerCASecretRef.Name)
}

func TestApplyMaxScaleTLSOverlay(t *testing.T) {
	mxs := &mariadbv1alpha1.MaxScale{}
	applyMaxScaleTLSOverlay(mxs, false)
	assert.Nil(t, mxs.Spec.TLS)

	applyMaxScaleTLSOverlay(mxs, true)
	require.NotNil(t, mxs.Spec.TLS)
	assert.True(t, mxs.Spec.TLS.Enabled)

	applyMaxScaleTLSOverlay(mxs, false)
	require.NotNil(t, mxs.Spec.TLS)
	assert.False(t, mxs.Spec.TLS.Enabled)
}

func TestSyncMaxScale_DisabledDeletes(t *testing.T) {
	existing := &mariadbv1alpha1.MaxScale{
		ObjectMeta: metav1.ObjectMeta{Name: "test-maxscale", Namespace: "default"},
	}
	c := newProxyContext(t, "galera", "", `{"enabled":false}`, existing)
	require.NoError(t, SyncMaxScale(c))

	err := c.Get(&mariadbv1alpha1.MaxScale{}, "test-maxscale")
	assert.True(t, apierrors.IsNotFound(err), "expected MaxScale to be deleted, got %v", err)

	// Idempotent when already gone.
	require.NoError(t, SyncMaxScale(c))
}

func TestDesiredMaxScaleRef(t *testing.T) {
	ref, err := desiredMaxScaleRef(newProxyContext(t, "galera", "", `{"enabled":true}`))
	require.NoError(t, err)
	assert.Nil(t, ref, "no reference until the MaxScale CR exists")

	existing := &mariadbv1alpha1.MaxScale{
		ObjectMeta: metav1.ObjectMeta{Name: "test-maxscale", Namespace: "default"},
	}
	ref, err = desiredMaxScaleRef(newProxyContext(t, "galera", "", `{"enabled":true}`, existing))
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, "test-maxscale", ref.Name)

	ref, err = desiredMaxScaleRef(newProxyContext(t, "galera", "", `{"enabled":false}`, existing))
	require.NoError(t, err)
	assert.Nil(t, ref, "reference is cleared when the proxy is disabled")
}

func TestValidateProxy(t *testing.T) {
	tests := []struct {
		name     string
		topology string
		params   string
		replicas *int32
		wantErr  bool
	}{
		{name: "absent on standalone"},
		{name: "disabled on standalone", params: `{"enabled":false}`},
		{name: "enabled on standalone", params: `{"enabled":true}`, wantErr: true},
		{name: "enabled on galera", topology: "galera", params: `{"enabled":true}`},
		{name: "enabled on replication", topology: "replication", params: `{"enabled":true}`},
		{name: "zero replicas", topology: "galera", params: `{"enabled":true}`, replicas: ptr.To(int32(0)), wantErr: true},
		{name: "malformed parameters", topology: "galera", params: `{"enabled":"maybe"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newProxyContext(t, tt.topology, "", tt.params)
			if tt.replicas != nil {
				proxy := c.Instance().Spec.Components[common.ComponentProxy]
				proxy.Replicas = tt.replicas
				c.Instance().Spec.Components[common.ComponentProxy] = proxy
			}
			err := validateProxy(c)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestBuildConnectionDetails_ThroughMaxScale(t *testing.T) {
	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: userSecretName("test"), Namespace: "default"},
		Data:       map[string][]byte{userPasswordSecretKey: []byte("password")},
	}
	mxsCABundle := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-maxscale-ca-bundle", Namespace: "default"},
		Data:       map[string][]byte{tlsCAKey: []byte("maxscale-ca")},
	}
	c := newProxyContext(t, "galera", "", `{"enabled":true}`, credentials, mxsCABundle)

	mxs := &mariadbv1alpha1.MaxScale{ObjectMeta: metav1.ObjectMeta{Name: "test-maxscale", Namespace: "default"}}
	details, err := buildConnectionDetails(c, mxs)
	require.NoError(t, err)
	assert.Equal(t, "test-maxscale.default.svc", details.Host)
	assert.Equal(t, "3306", details.Port)
	assert.Empty(t, details.AdditionalProperties, "no TLS while MaxScale TLS is off")

	mxs.Spec.TLS = &mariadbv1alpha1.MaxScaleTLS{Enabled: true}
	details, err = buildConnectionDetails(c, mxs)
	require.NoError(t, err)
	assert.Equal(t, "true", details.AdditionalProperties[tlsConnectionKey])
	assert.Equal(t, "maxscale-ca", details.AdditionalProperties[tlsCAKey])
}

func TestStatusMariaDB_WaitsForMaxScale(t *testing.T) {
	readyCondition := metav1.Condition{
		Type:               mariadbv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		LastTransitionTime: metav1.Now(),
	}
	mdb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Status:     mariadbv1alpha1.MariaDBStatus{Conditions: []metav1.Condition{readyCondition}},
	}
	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: userSecretName("test"), Namespace: "default"},
		Data:       map[string][]byte{userPasswordSecretKey: []byte("password")},
	}

	status, err := StatusMariaDB(newProxyContext(t, "galera", `{"tls":{"enabled":false}}`, `{"enabled":true}`, mdb, credentials))
	require.NoError(t, err)
	assert.Equal(t, controller.Provisioning("Waiting for MaxScale proxy to be created"), status)

	mxs := &mariadbv1alpha1.MaxScale{
		ObjectMeta: metav1.ObjectMeta{Name: "test-maxscale", Namespace: "default"},
		Status:     mariadbv1alpha1.MaxScaleStatus{Conditions: []metav1.Condition{readyCondition}},
	}
	status, err = StatusMariaDB(newProxyContext(t, "galera", `{"tls":{"enabled":false}}`, `{"enabled":true}`, mdb, credentials, mxs))
	require.NoError(t, err)
	assert.Equal(t, corev1alpha1.InstancePhaseReady, status.Phase)
	assert.Equal(t, "test-maxscale.default.svc", status.ConnectionDetails.Host)
}
