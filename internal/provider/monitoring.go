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
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition/components"
	"github.com/openeverest/provider-mariadb/internal/common"
)

// monitoringConfigPath is the field index path used to look up Instances by the
// name of the MonitoringConfig their monitoring component references.
const monitoringConfigPath = ".spec.components.monitoring.monitoringConfigName"

// resolveMonitoringConfig looks up the MonitoringConfig referenced by the
// instance's monitoring component. It returns nil without error when the
// monitoring component is absent, making monitoring opt-in.
func resolveMonitoringConfig(c *controller.Context) (*monitoringv1alpha1.MonitoringConfig, error) {
	monitoring, ok := c.Instance().Spec.Components[common.ComponentMonitoring]
	if !ok {
		return nil, nil
	}

	var params components.MonitoringParameters
	if err := c.DecodeComponentParameters(monitoring, &params); err != nil {
		return nil, fmt.Errorf("decode monitoring parameters: %w", err)
	}

	if params.MonitoringConfigName == nil || *params.MonitoringConfigName == "" {
		return nil, fmt.Errorf("monitoringConfigName is required when the monitoring component is set")
	}

	mc := &monitoringv1alpha1.MonitoringConfig{}
	if err := c.Get(mc, *params.MonitoringConfigName); err != nil {
		return nil, fmt.Errorf("get MonitoringConfig %q: %w", *params.MonitoringConfigName, err)
	}

	return mc, nil
}

// extractMonitoringConfigName extracts the MonitoringConfig name referenced by
// an Instance's monitoring component, for use as a field index value.
func extractMonitoringConfigName(obj client.Object) []string {
	in, ok := obj.(*corev1alpha1.Instance)
	if !ok {
		return nil
	}

	monitoring, ok := in.Spec.Components[common.ComponentMonitoring]
	if !ok || monitoring.Parameters == nil || monitoring.Parameters.Raw == nil {
		return nil
	}

	var params components.MonitoringParameters
	if err := json.Unmarshal(monitoring.Parameters.Raw, &params); err != nil {
		return nil
	}

	if params.MonitoringConfigName == nil || *params.MonitoringConfigName == "" {
		return nil
	}

	return []string{*params.MonitoringConfigName}
}
