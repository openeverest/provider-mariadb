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
	"fmt"
	"os"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

const providerSpecPath = "../../charts/provider-mariadb/generated/provider-spec.yaml"

// generatedProvider is the Provider CR the chart installs, so tests see the
// monitoring contract and exporter image of definition/.
func generatedProvider(t *testing.T) *corev1alpha1.Provider {
	t.Helper()
	raw, err := os.ReadFile(providerSpecPath)
	require.NoError(t, err)
	var doc struct {
		Spec corev1alpha1.ProviderSpec `json:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	return &corev1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: common.ProviderName}, Spec: doc.Spec}
}

func monitoringTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, monitoringv1alpha1.AddToScheme(scheme))
	require.NoError(t, mariadbv1alpha1.AddToScheme(scheme))
	return scheme
}

// monitoringContext builds a context for the Instance "test" whose
// spec.monitoring lists entries; objs are the bindings, classes and live
// objects that make them resolvable. The REST mapper serves the ServiceMonitor
// and VMServiceScrape CRDs only when crds is set.
func monitoringContext(t *testing.T, entries []corev1alpha1.InstanceMonitoringDestination, crds bool, objs ...client.Object) *controller.Context {
	t.Helper()
	scheme := monitoringTestScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	for gvk := range scheme.AllKnownTypes() {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	if crds {
		mapper.Add(serviceMonitorGVK, meta.RESTScopeNamespace)
		mapper.Add(vmServiceScrapeGVK, meta.RESTScopeNamespace)
	}

	in := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid"},
		Spec: corev1alpha1.InstanceSpec{Components: map[string]corev1alpha1.ComponentSpec{
			common.ComponentEngine: {Type: common.ComponentTypeMariaDB},
		}},
	}
	if entries != nil {
		in.Spec.Monitoring = &corev1alpha1.InstanceMonitoringSpec{Destinations: entries}
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(mapper).
		WithObjects(append(objs, in, generatedProvider(t))...).
		Build()
	return controller.NewContext(context.Background(), cl, in, common.ProviderName)
}

func entry(name, destination string) corev1alpha1.InstanceMonitoringDestination {
	return corev1alpha1.InstanceMonitoringDestination{
		Name:           name,
		DestinationRef: commonv1alpha1.MonitoringDestinationRef{Name: destination},
	}
}

// acceptedBinding is what core materialises for an entry of the Instance
// "test" once it has resolved destination, class and mode.
func acceptedBinding(entryName, destination, class string, mode corev1alpha1.MonitoringExecutionMode) *monitoringv1alpha1.MonitoringBinding {
	return &monitoringv1alpha1.MonitoringBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-" + entryName,
			Namespace:  "default",
			Generation: 1,
			Labels: map[string]string{
				monitoringv1alpha1.BindingInstanceLabel: "test",
				monitoringv1alpha1.BindingEntryLabel:    entryName,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(),
				Kind:       "Instance",
				Name:       "test",
				UID:        "uid",
				Controller: ptr.To(true),
			}},
		},
		Spec: monitoringv1alpha1.MonitoringBindingSpec{
			InstanceRef:    commonv1alpha1.ObjectRef{Name: "test"},
			DestinationRef: commonv1alpha1.MonitoringDestinationRef{Name: destination},
			ClassRef:       &commonv1alpha1.ObjectRef{Name: class},
			ExecutionMode:  mode,
		},
		Status: monitoringv1alpha1.MonitoringBindingStatus{Conditions: []metav1.Condition{{
			Type: monitoringv1alpha1.MonitoringBindingConditionAccepted, Status: metav1.ConditionTrue,
			Reason: monitoringv1alpha1.ReasonAccepted, ObservedGeneration: 1,
		}}},
	}
}

// metricsClass is an ExtensionManaged class, such as monitoring-prometheus,
// that scrapes the published endpoints when requiresMetrics is set.
func metricsClass(name string, requiresMetrics bool) *monitoringv1alpha1.MonitoringClass {
	return &monitoringv1alpha1.MonitoringClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: monitoringv1alpha1.MonitoringClassSpec{
			AuthModes: []commonv1alpha1.AuthMode{{Name: commonv1alpha1.AuthModeAnonymous}},
			ExtensionManaged: &monitoringv1alpha1.ExtensionManagedFulfilment{
				Requires: monitoringv1alpha1.SourceRequirements{Metrics: requiresMetrics},
			},
		},
	}
}

// metricsMonitoring binds the Instance "test" to a class that requires
// metrics: the spec.monitoring to set and the objects core materialises.
func metricsMonitoring() (*corev1alpha1.InstanceMonitoringSpec, []client.Object) {
	return &corev1alpha1.InstanceMonitoringSpec{Destinations: []corev1alpha1.InstanceMonitoringDestination{entry("prom", "prom")}},
		[]client.Object{
			acceptedBinding("prom", "prom", "prometheus", corev1alpha1.MonitoringExecutionModeExtensionManaged),
			metricsClass("prometheus", true),
		}
}

// liveMetricsMariaDB is the MariaDB as the operator sees it, running the
// exporter when metrics is set.
func liveMetricsMariaDB(metrics bool) *mariadbv1alpha1.MariaDB {
	mdb := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "mdb-uid"},
		Spec:       mariadbv1alpha1.MariaDBSpec{Replicas: 3, Port: 3306},
	}
	if metrics {
		mdb.Spec.Metrics = &mariadbv1alpha1.MariadbMetrics{
			Enabled:        true,
			ServiceMonitor: mariadbv1alpha1.ServiceMonitor{Enabled: ptr.To(false)},
		}
	}
	return mdb
}

func TestMetricsExporterRequired(t *testing.T) {
	prom := acceptedBinding("prom", "prom", "prometheus", corev1alpha1.MonitoringExecutionModeExtensionManaged)
	tests := []struct {
		name    string
		entries []corev1alpha1.InstanceMonitoringDestination
		objs    []client.Object
		want    bool
	}{
		{
			name:    "a bound class requiring metrics runs the exporter",
			entries: []corev1alpha1.InstanceMonitoringDestination{entry("prom", "prom")},
			objs:    []client.Object{prom, metricsClass("prometheus", true)},
			want:    true,
		},
		{
			name:    "a bound class without metrics stops a live exporter",
			entries: []corev1alpha1.InstanceMonitoringDestination{entry("prom", "prom")},
			objs:    []client.Object{prom, metricsClass("prometheus", false), liveMetricsMariaDB(true)},
		},
		{
			name: "no entries stop a live exporter",
			objs: []client.Object{liveMetricsMariaDB(true)},
		},
		{
			name:    "an entry without a binding yet holds the live exporter",
			entries: []corev1alpha1.InstanceMonitoringDestination{entry("prom", "prom")},
			objs:    []client.Object{liveMetricsMariaDB(true)},
			want:    true,
		},
		{
			name:    "an entry without a binding yet starts nothing",
			entries: []corev1alpha1.InstanceMonitoringDestination{entry("prom", "prom")},
			objs:    []client.Object{liveMetricsMariaDB(false)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := metricsExporterRequired(monitoringContext(t, tt.entries, true, tt.objs...))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildMetrics(t *testing.T) {
	spec, objs := metricsMonitoring()
	c := monitoringContext(t, spec.Destinations, true, objs...)

	metrics, err := buildMetrics(c)
	require.NoError(t, err)
	require.NotNil(t, metrics)
	assert.True(t, metrics.Enabled)
	assert.Equal(t, "prom/mysqld-exporter:v0.15.1", metrics.Exporter.Image, "the provider spec pins the exporter image")
	assert.Equal(t, map[string]string{
		controller.ProviderLabel: common.ProviderName,
		controller.InstanceLabel: "test",
	}, metrics.Exporter.PodMetadata.Labels)
	assert.Equal(t, ptr.To(false), metrics.ServiceMonitor.Enabled, "class controllers create the scrape objects")
	assert.Empty(t, c.LabelledComponents(), "the exporter is no component of the Instance")

	metrics, err = buildMetrics(monitoringContext(t, nil, true))
	require.NoError(t, err)
	assert.Nil(t, metrics, "without a class requiring metrics the operator deploys no exporter")
}

func TestMonitoringSources(t *testing.T) {
	p := New()

	t.Run("one probe endpoint per MariaDB pod while the exporter runs", func(t *testing.T) {
		s, err := p.MonitoringSources(monitoringContext(t, nil, true, liveMetricsMariaDB(true)))
		require.NoError(t, err)
		require.Len(t, s.Metrics, 3)
		for i, ep := range s.Metrics {
			assert.Equal(t, corev1alpha1.MetricsEndpoint{
				Component: common.ComponentEngine,
				Kind:      "mariadb",
				PodSelector: map[string]string{
					controller.InstanceLabel:     "test",
					"app.kubernetes.io/name":     "exporter",
					"app.kubernetes.io/instance": "test-metrics",
				},
				Port:   intstr.FromString("metrics"),
				Path:   "/probe",
				Scheme: "http",
				Params: map[string][]string{
					"target": {fmt.Sprintf("test-%d.test-internal.default.svc.cluster.local:3306", i)},
				},
			}, ep)
		}
	})

	for name, objs := range map[string][]client.Object{
		"before the MariaDB exists": nil,
		"without the exporter":      {liveMetricsMariaDB(false)},
	} {
		t.Run("nothing is published "+name, func(t *testing.T) {
			s, err := p.MonitoringSources(monitoringContext(t, nil, true, objs...))
			require.NoError(t, err)
			require.NotNil(t, s, "nil would keep stale sources")
			assert.Empty(t, s.Metrics)
		})
	}
}

// controlledBy returns an unstructured object of gvk named test-metrics,
// controlled by owner when it is set.
func controlledBy(gvk schema.GroupVersionKind, owner client.Object) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName("test-metrics")
	u.SetNamespace("default")
	if owner != nil {
		u.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "v1", Kind: "Owner", Name: owner.GetName(), UID: owner.GetUID(), Controller: ptr.To(true),
		}})
	}
	return u
}

func exporterDeployment(owner client.Object) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-metrics", Namespace: "default"}}
	if owner != nil {
		d.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "v1", Kind: "Owner", Name: owner.GetName(), UID: owner.GetUID(), Controller: ptr.To(true),
		}}
	}
	return d
}

func exists(t *testing.T, c *controller.Context, gvk schema.GroupVersionKind) bool {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(u, "test-metrics")
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestSyncMetricsCleanup(t *testing.T) {
	instance := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "test", UID: "uid"}}

	t.Run("once metrics are off the exporter and scrape objects are deleted", func(t *testing.T) {
		mdb := liveMetricsMariaDB(false)
		c := monitoringContext(t, nil, true, mdb, exporterDeployment(mdb),
			controlledBy(serviceMonitorGVK, mdb), controlledBy(vmServiceScrapeGVK, instance))
		require.NoError(t, SyncMetricsCleanup(c))
		assert.False(t, exists(t, c, deploymentGVK))
		assert.False(t, exists(t, c, serviceMonitorGVK))
		assert.False(t, exists(t, c, vmServiceScrapeGVK))
	})

	t.Run("while metrics are on only the ServiceMonitor and VMServiceScrape go", func(t *testing.T) {
		mdb := liveMetricsMariaDB(true)
		c := monitoringContext(t, nil, true, mdb, exporterDeployment(mdb),
			controlledBy(serviceMonitorGVK, mdb), controlledBy(vmServiceScrapeGVK, instance))
		require.NoError(t, SyncMetricsCleanup(c))
		assert.True(t, exists(t, c, deploymentGVK))
		assert.False(t, exists(t, c, serviceMonitorGVK))
		assert.False(t, exists(t, c, vmServiceScrapeGVK))
	})

	t.Run("objects the owners do not control are left alone", func(t *testing.T) {
		c := monitoringContext(t, nil, true, liveMetricsMariaDB(false), exporterDeployment(nil),
			controlledBy(serviceMonitorGVK, nil), controlledBy(vmServiceScrapeGVK, nil))
		require.NoError(t, SyncMetricsCleanup(c))
		assert.True(t, exists(t, c, deploymentGVK))
		assert.True(t, exists(t, c, serviceMonitorGVK))
		assert.True(t, exists(t, c, vmServiceScrapeGVK))
	})

	t.Run("missing CRDs and a missing MariaDB are nothing to clean up", func(t *testing.T) {
		require.NoError(t, SyncMetricsCleanup(monitoringContext(t, nil, false, liveMetricsMariaDB(false))))
		require.NoError(t, SyncMetricsCleanup(monitoringContext(t, nil, true)))
	})
}
