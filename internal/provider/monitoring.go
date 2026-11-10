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
	"cmp"
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/mariadb-operator/mariadb-operator/v26/pkg/builder"
	labels "github.com/mariadb-operator/mariadb-operator/v26/pkg/builder/labels"
	"github.com/mariadb-operator/mariadb-operator/v26/pkg/statefulset"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

const (
	// metricsExporterComponentType is the component type in the provider spec
	// that pins the mysqld-exporter image.
	metricsExporterComponentType = "mysqld-exporter"

	// metricsKind is the kind of the mariadb component type, as declared in
	// definition/provider.yaml.
	metricsKind = "mariadb"

	// metricsProbePath is the mysqld-exporter multi-target endpoint: it
	// scrapes the MariaDB named by the target query parameter.
	metricsProbePath = "/probe"

	defaultMariaDBPort = 3306
)

var (
	// The GVKs below are read as unstructured: the client does not cache
	// unstructured objects, so a get needs no list/watch RBAC and no
	// cluster-wide informer, and an absent CRD is a NoMatch error.
	deploymentGVK      = appsv1.SchemeGroupVersion.WithKind("Deployment")
	serviceMonitorGVK  = monitoringv1.SchemeGroupVersion.WithKind(monitoringv1.ServiceMonitorsKind)
	vmServiceScrapeGVK = schema.GroupVersionKind{Group: "operator.victoriametrics.com", Version: "v1beta1", Kind: "VMServiceScrape"}
)

// buildMetrics returns the mariadb-operator's spec.metrics while a monitoring
// class bound to the Instance requires metrics, and nil otherwise, so the
// operator deploys no exporter. Scrape objects come from the class
// controllers, from the endpoints MonitoringSources publishes, so the
// operator's ServiceMonitor stays off.
func buildMetrics(c *controller.Context) (*mariadbv1alpha1.MariadbMetrics, error) {
	required, err := metricsExporterRequired(c)
	if err != nil || !required {
		return nil, err
	}

	spec, err := c.ProviderSpec()
	if err != nil {
		return nil, fmt.Errorf("get provider spec: %w", err)
	}
	image := controller.GetDefaultImage(spec, metricsExporterComponentType)
	if image == "" {
		return nil, fmt.Errorf("provider spec has no image for component type %q", metricsExporterComponentType)
	}

	return &mariadbv1alpha1.MariadbMetrics{
		Enabled: true,
		Exporter: mariadbv1alpha1.Exporter{
			Image: image,
			// Not c.PodLabels: the exporter is no component of the Instance.
			PodMetadata: &mariadbv1alpha1.Metadata{Labels: map[string]string{
				controller.ProviderLabel: common.ProviderName,
				controller.InstanceLabel: c.Name(),
			}},
		},
		ServiceMonitor: mariadbv1alpha1.ServiceMonitor{Enabled: ptr.To(false)},
	}, nil
}

// metricsExporterRequired says whether the exporter runs this pass: when a
// bound class requires metrics or, while that is unresolved, when the live
// MariaDB already runs it (removing and re-adding it would flap the scrape
// for nothing).
func metricsExporterRequired(c *controller.Context) (bool, error) {
	// Without entries nothing is required: bindings of removed entries are
	// ignored, so RequiredSources would answer NotRequired.
	if m := c.Instance().Spec.Monitoring; m == nil || len(m.Destinations) == 0 {
		return false, nil
	}
	req, err := c.RequiredSources()
	if err != nil {
		return false, err
	}
	live, err := liveMariaDB(c)
	if err != nil {
		return false, err
	}
	return req.Metrics.Else(live.AreMetricsEnabled()), nil
}

// MonitoringSources implements controller.MonitoringSourcesProvider. The
// operator's mysqld-exporter is a single multi-target Deployment, so one
// endpoint is published per MariaDB pod: the exporter pods' /probe with the
// pod as target, which the class controllers turn into the instance label.
// Endpoints are published while the MariaDB runs the exporter: they select the
// exporter pods, so they yield no scrape target before the operator has
// started them, and they stay published through rolling restarts.
func (p *MariaDBProvider) MonitoringSources(c *controller.Context) (*corev1alpha1.MonitoringSources, error) {
	mdb, err := liveMariaDB(c)
	if err != nil {
		return nil, err
	}
	s := &corev1alpha1.MonitoringSources{}
	if !mdb.AreMetricsEnabled() {
		return s, nil
	}

	selector := labels.NewLabelsBuilder().WithMetricsSelectorLabels(mdb.MetricsKey()).Build()
	selector[controller.InstanceLabel] = c.Name()
	service := mdb.InternalServiceKey().Name
	port := cmp.Or(mdb.Spec.Port, defaultMariaDBPort)
	for i := range int(mdb.Spec.Replicas) {
		target := fmt.Sprintf("%s:%d", statefulset.PodFQDNWithService(mdb.ObjectMeta, i, service), port)
		s.Metrics = append(s.Metrics, corev1alpha1.MetricsEndpoint{
			Component:   common.ComponentEngine,
			Kind:        metricsKind,
			PodSelector: selector,
			Port:        intstr.FromString(builder.MetricsPortName),
			Path:        metricsProbePath,
			Scheme:      "http",
			Params:      map[string][]string{"target": {target}},
		})
	}
	return s, nil
}

// SyncMetricsCleanup deletes what the mariadb-operator leaves behind: it
// stops reconciling the exporter Deployment and the ServiceMonitor once they
// are turned off, but never deletes them. It also deletes the VMServiceScrape
// earlier provider versions created. Each object is deleted only once the
// change is visible on the live MariaDB, so the operator does not recreate it
// from a stale spec; the MariaDB watch re-enqueues the Instance once it is.
func SyncMetricsCleanup(c *controller.Context) error {
	mdb, err := liveMariaDB(c)
	if err != nil || mdb.UID == "" {
		return err
	}
	name := mdb.MetricsKey().Name
	if !mdb.IsServiceMonitorEnabled() {
		if err := deleteControlled(c, serviceMonitorGVK, name, mdb); err != nil {
			return err
		}
	}
	if !mdb.AreMetricsEnabled() {
		if err := deleteControlled(c, deploymentGVK, name, mdb); err != nil {
			return err
		}
	}
	return deleteControlled(c, vmServiceScrapeGVK, name, c.Instance())
}

// deleteControlled deletes the named object when owner controls it. Objects
// it does not control are left alone, and a missing CRD means there is
// nothing to delete.
func deleteControlled(c *controller.Context, gvk schema.GroupVersionKind, name string, owner metav1.Object) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := c.Get(obj, name); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("get %s %q: %w", gvk.Kind, name, err)
	}
	if !metav1.IsControlledBy(obj, owner) || obj.GetDeletionTimestamp() != nil {
		return nil
	}
	if err := c.Delete(obj); err != nil {
		return fmt.Errorf("delete %s %q: %w", gvk.Kind, name, err)
	}
	return nil
}

// liveMariaDB returns the MariaDB as it is now, empty before it exists.
func liveMariaDB(c *controller.Context) (*mariadbv1alpha1.MariaDB, error) {
	live := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(live, c.Name()); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get live MariaDB: %w", err)
	}
	return live, nil
}

var _ controller.MonitoringSourcesProvider = (*MariaDBProvider)(nil)
