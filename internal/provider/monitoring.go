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

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition/components"
	"github.com/openeverest/provider-mariadb/internal/common"
)

// monitoringParams decodes the monitoring component parameters from the
// Instance. It returns nil when the monitoring component is absent, making
// monitoring opt-in.
func monitoringParams(c *controller.Context) (*components.MonitoringParameters, error) {
	monitoring, ok := c.Instance().Spec.Components[common.ComponentMonitoring]
	if !ok {
		return nil, nil
	}

	var params components.MonitoringParameters
	if err := c.DecodeComponentParameters(monitoring, &params); err != nil {
		return nil, fmt.Errorf("decode monitoring parameters: %w", err)
	}
	return &params, nil
}

// buildMetrics translates the monitoring component into the mariadb-operator's
// spec.metrics. It returns nil when metrics are not enabled, so the operator
// deploys no exporter and creates no Prometheus objects.
//
// The mariadb-operator couples the exporter and the ServiceMonitor: whenever
// metrics are enabled it always reconciles a ServiceMonitor (and requires the
// ServiceMonitor CRD to be present). We therefore treat "metrics enabled" as
// implying ServiceMonitor creation, while still honoring the ServiceMonitor
// scrape settings the user provides.
func buildMetrics(c *controller.Context) (*mariadbv1alpha1.MariadbMetrics, error) {
	params, err := monitoringParams(c)
	if err != nil {
		return nil, err
	}
	if params == nil || !params.Enabled {
		return nil, nil
	}

	image, err := resolveExporterImage(c)
	if err != nil {
		return nil, err
	}

	metrics := &mariadbv1alpha1.MariadbMetrics{
		Enabled: true,
		Exporter: mariadbv1alpha1.Exporter{
			Image: image,
		},
		ServiceMonitor: mariadbv1alpha1.ServiceMonitor{
			PrometheusRelease: params.ServiceMonitor.PrometheusRelease,
			JobLabel:          params.ServiceMonitor.JobLabel,
			Interval:          params.ServiceMonitor.Interval,
			ScrapeTimeout:     params.ServiceMonitor.ScrapeTimeout,
		},
	}

	monitoring := c.Instance().Spec.Components[common.ComponentMonitoring]
	if monitoring.Resources != nil && (monitoring.Resources.Limits != nil || monitoring.Resources.Requests != nil) {
		metrics.Exporter.Resources = &mariadbv1alpha1.ResourceRequirements{
			Limits:   monitoring.Resources.Limits,
			Requests: monitoring.Resources.Requests,
		}
	}

	return metrics, nil
}

// applyMetricsOverlay writes the provider-managed metrics fields onto the
// operator's existing MariaDB spec without discarding the fields the operator
// defaults on its own (Exporter.Port, Username, PasswordSecretKeyRef, ...).
// Replacing spec.metrics wholesale would reset those defaults every reconcile,
// producing an endless diff/update loop (and resourceVersion conflicts) against
// the operator. When metrics is nil the whole block is cleared so the operator
// tears the exporter down.
func applyMetricsOverlay(mariadb *mariadbv1alpha1.MariaDB, metrics *mariadbv1alpha1.MariadbMetrics) {
	if metrics == nil {
		mariadb.Spec.Metrics = nil
		return
	}
	if mariadb.Spec.Metrics == nil {
		mariadb.Spec.Metrics = metrics
		return
	}
	mariadb.Spec.Metrics.Enabled = metrics.Enabled
	mariadb.Spec.Metrics.Exporter.Image = metrics.Exporter.Image
	mariadb.Spec.Metrics.Exporter.Resources = metrics.Exporter.Resources
	mariadb.Spec.Metrics.ServiceMonitor = metrics.ServiceMonitor
}

// component override, its selected version, or the provider default — mirroring
// the engine image resolution precedence.
func resolveExporterImage(c *controller.Context) (string, error) {
	monitoring := c.Instance().Spec.Components[common.ComponentMonitoring]
	if monitoring.Image != "" {
		return monitoring.Image, nil
	}

	spec, err := c.ProviderSpec()
	if err != nil {
		return "", fmt.Errorf("get provider spec: %w", err)
	}

	image := ""
	if monitoring.Version != "" {
		image = controller.GetImageForVersion(spec, common.ComponentMonitoring, monitoring.Version)
	}
	if image == "" {
		image = controller.GetDefaultImageForComponent(spec, common.ComponentMonitoring)
	}
	return image, nil
}

