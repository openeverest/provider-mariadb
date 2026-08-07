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
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition/components"
	"github.com/openeverest/provider-mariadb/internal/common"
)

// newMonitoringContext builds a controller.Context for an Instance whose
// monitoring component carries the given parameters and image override. A nil
// params map means the monitoring component is omitted entirely.
func newMonitoringContext(t *testing.T, params *components.MonitoringParameters, image string) *controller.Context {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{},
		},
	}

	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		instance.Spec.Components[common.ComponentMonitoring] = corev1alpha1.ComponentSpec{
			Name:       common.ComponentMonitoring,
			Type:       common.ComponentMonitoring,
			Image:      image,
			Parameters: &runtime.RawExtension{Raw: raw},
		}
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance).Build()
	return controller.NewContext(context.Background(), c, instance, common.ProviderName)
}

func TestBuildMetrics_MonitoringAbsent(t *testing.T) {
	ctx := newMonitoringContext(t, nil, "")

	metrics, err := buildMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics != nil {
		t.Fatalf("expected nil metrics when monitoring absent, got %+v", metrics)
	}
}

func TestBuildMetrics_Disabled(t *testing.T) {
	ctx := newMonitoringContext(t, &components.MonitoringParameters{Enabled: false}, "exporter:latest")

	metrics, err := buildMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics != nil {
		t.Fatalf("expected nil metrics when disabled, got %+v", metrics)
	}
}

func TestBuildMetrics_EnabledWithImageOverride(t *testing.T) {
	params := &components.MonitoringParameters{
		Enabled: true,
		ServiceMonitor: components.ServiceMonitorParameters{
			PrometheusRelease: "kube-prometheus-stack",
			JobLabel:          "app.kubernetes.io/instance",
			Interval:          "30s",
			ScrapeTimeout:     "10s",
		},
	}
	ctx := newMonitoringContext(t, params, "custom/mysqld-exporter:v1")

	metrics, err := buildMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics == nil {
		t.Fatal("expected metrics, got nil")
	}
	if !metrics.Enabled {
		t.Error("expected metrics.Enabled to be true")
	}
	if metrics.Exporter.Image != "custom/mysqld-exporter:v1" {
		t.Errorf("expected exporter image override, got %q", metrics.Exporter.Image)
	}
	if metrics.ServiceMonitor.PrometheusRelease != "kube-prometheus-stack" {
		t.Errorf("unexpected prometheusRelease: %q", metrics.ServiceMonitor.PrometheusRelease)
	}
	if metrics.ServiceMonitor.JobLabel != "app.kubernetes.io/instance" {
		t.Errorf("unexpected jobLabel: %q", metrics.ServiceMonitor.JobLabel)
	}
	if metrics.ServiceMonitor.Interval != "30s" {
		t.Errorf("unexpected interval: %q", metrics.ServiceMonitor.Interval)
	}
	if metrics.ServiceMonitor.ScrapeTimeout != "10s" {
		t.Errorf("unexpected scrapeTimeout: %q", metrics.ServiceMonitor.ScrapeTimeout)
	}
}

func TestBuildMetrics_ExporterResources(t *testing.T) {
	params := &components.MonitoringParameters{Enabled: true}
	ctx := newMonitoringContext(t, params, "exporter:latest")

	// Attach resources to the monitoring component.
	monitoring := ctx.Instance().Spec.Components[common.ComponentMonitoring]
	monitoring.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		},
	}
	ctx.Instance().Spec.Components[common.ComponentMonitoring] = monitoring

	metrics, err := buildMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics == nil || metrics.Exporter.Resources == nil {
		t.Fatal("expected exporter resources to be set")
	}
	if got := metrics.Exporter.Resources.Requests.Cpu().String(); got != "100m" {
		t.Errorf("unexpected cpu request: %q", got)
	}
}

// The OpenEverest UI writes select values as strings, so `enabled` can arrive as
// the quoted string "true" rather than a JSON bool. FlexBool must decode it.
func TestBuildMetrics_EnabledAsString(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentMonitoring: {
					Name:       common.ComponentMonitoring,
					Type:       common.ComponentMonitoring,
					Image:      "exporter:latest",
					Parameters: &runtime.RawExtension{Raw: []byte(`{"enabled":"true"}`)},
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance).Build()
	ctx := controller.NewContext(context.Background(), c, instance, common.ProviderName)

	metrics, err := buildMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error decoding string-valued enabled: %v", err)
	}
	if metrics == nil || !metrics.Enabled {
		t.Fatal("expected metrics enabled when enabled is the string \"true\"")
	}
}

