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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition/components"
	"github.com/openeverest/provider-mariadb/internal/common"
)

// maxScaleDefaultReplicas avoids making a single proxy Pod a single point of
// failure in front of an HA cluster.
const maxScaleDefaultReplicas int32 = 2

// maxScaleName returns the name of the MaxScale CR (and its client Service)
// managed for the given instance.
func maxScaleName(instanceName string) string {
	return instanceName + "-maxscale"
}

// isProxyEnabled reports whether the proxy component requests MaxScale. A proxy
// component without parameters is treated as disabled, making MaxScale opt-in.
func isProxyEnabled(c *controller.Context) (bool, error) {
	proxy, ok := c.Instance().Spec.Components[common.ComponentProxy]
	if !ok || proxy.Parameters == nil || proxy.Parameters.Raw == nil {
		return false, nil
	}
	var params components.MaxScaleParameters
	if err := c.DecodeComponentParameters(proxy, &params); err != nil {
		return false, fmt.Errorf("decode proxy parameters: %w", err)
	}
	return bool(params.Enabled), nil
}

// SyncMaxScale creates or updates the MaxScale CR in front of the MariaDB when
// the proxy is enabled, and deletes it otherwise.
func SyncMaxScale(c *controller.Context) error {
	enabled, err := isProxyEnabled(c)
	if err != nil {
		return err
	}
	if !enabled {
		return teardownMaxScale(c)
	}

	proxy := c.Instance().Spec.Components[common.ComponentProxy]
	name := maxScaleName(c.Name())

	image, err := resolveMaxScaleImage(c)
	if err != nil {
		return err
	}

	replicas := maxScaleDefaultReplicas
	if proxy.Replicas != nil {
		replicas = *proxy.Replicas
	}

	var resources *mariadbv1alpha1.ResourceRequirements
	if proxy.Resources != nil && (proxy.Resources.Limits != nil || proxy.Resources.Requests != nil) {
		resources = &mariadbv1alpha1.ResourceRequirements{
			Limits:   proxy.Resources.Limits,
			Requests: proxy.Resources.Requests,
		}
	}

	var rawAffinity *corev1.Affinity
	if proxy.SchedulingPolicy != nil {
		rawAffinity = proxy.SchedulingPolicy.Affinity
	}
	affinity, err := buildAffinity(rawAffinity, "", true, name)
	if err != nil {
		return fmt.Errorf("build proxy affinity: %w", err)
	}

	tlsSettings, err := resolveTLSSettings(c)
	if err != nil {
		return fmt.Errorf("resolve TLS settings: %w", err)
	}

	// Read-modify-write, as for the MariaDB: the operator persists its defaults
	// (servers, monitor, services, auth, ...) into the spec, and c.Apply
	// performs a full Update.
	mxs := &mariadbv1alpha1.MaxScale{}
	if err := c.Get(mxs, name); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get MaxScale: %w", err)
		}
		mxs = &mariadbv1alpha1.MaxScale{
			ObjectMeta: c.ObjectMeta(name),
			Spec: mariadbv1alpha1.MaxScaleSpec{
				// Immutable; the operator infers servers, the monitor module
				// (galeramon / mariadbmon) and credentials from it.
				MariaDBRef: &mariadbv1alpha1.MariaDBRef{
					ObjectReference: mariadbv1alpha1.ObjectReference{Name: c.Name()},
					WaitForIt:       true,
				},
			},
		}
	}

	mxs.Spec.Image = image
	mxs.Spec.Replicas = replicas
	mxs.Spec.Resources = resources
	mxs.Spec.Affinity = affinity
	mxs.Spec.KubernetesService = configureService(proxy.Service)
	// MaxScale listeners only accept TLS clients once TLS is on, so it mirrors
	// the engine's enforcement rather than its enablement.
	applyMaxScaleTLSOverlay(mxs, tlsSettings.Enabled && tlsSettings.Required)
	if isReplicationTopology(c) {
		applyMaxScaleMonitorOverlay(mxs)
	}

	if err := c.Apply(mxs); err != nil {
		return fmt.Errorf("apply MaxScale: %w", err)
	}
	return nil
}

// applyMaxScaleTLSOverlay sets only the TLS switch and preserves the CA and
// certificate references the operator defaults from the MariaDB.
func applyMaxScaleTLSOverlay(mxs *mariadbv1alpha1.MaxScale, enabled bool) {
	if mxs.Spec.TLS == nil {
		if !enabled {
			return
		}
		mxs.Spec.TLS = &mariadbv1alpha1.MaxScaleTLS{}
	}
	mxs.Spec.TLS.Enabled = enabled
}

// maxScaleTopologyParams are the mariadbmon operations that change the
// replication topology. The operator stays the single owner of failover,
// rejoin and read_only: running them in MaxScale as well races with the
// operator, which reconfigures a node MaxScale just promoted back into a
// read-only replica of the failed primary (#37).
var maxScaleTopologyParams = []string{"auto_failover", "auto_rejoin", "switchover_on_low_disk_space"}

// applyMaxScaleMonitorOverlay turns the mariadbmon topology operations off and
// preserves any other monitor parameter. It must be set before the operator
// defaults the monitor, which it only does while the parameters are unset.
func applyMaxScaleMonitorOverlay(mxs *mariadbv1alpha1.MaxScale) {
	if mxs.Spec.Monitor.Params == nil {
		mxs.Spec.Monitor.Params = map[string]string{}
	}
	for _, param := range maxScaleTopologyParams {
		mxs.Spec.Monitor.Params[param] = "false"
	}
}

// teardownMaxScale deletes the MaxScale CR when the proxy is disabled. Absence
// is not an error.
func teardownMaxScale(c *controller.Context) error {
	mxs := &mariadbv1alpha1.MaxScale{}
	if err := c.Get(mxs, maxScaleName(c.Name())); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get MaxScale for teardown: %w", err)
	}
	if !mxs.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := c.Delete(mxs); err != nil {
		return fmt.Errorf("delete MaxScale %q: %w", mxs.Name, err)
	}
	return nil
}

// readyMaxScale returns the MaxScale CR once it is ready to serve clients.
// While it is not, the returned message describes what is being waited for.
func readyMaxScale(c *controller.Context) (*mariadbv1alpha1.MaxScale, string, error) {
	mxs := &mariadbv1alpha1.MaxScale{}
	if err := c.Get(mxs, maxScaleName(c.Name())); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "Waiting for MaxScale proxy to be created", nil
		}
		return nil, "", fmt.Errorf("get MaxScale: %w", err)
	}
	if !mxs.IsReady() {
		for _, cond := range mxs.Status.Conditions {
			if cond.Type == mariadbv1alpha1.ConditionTypeReady && cond.Message != "" {
				return nil, "MaxScale: " + cond.Message, nil
			}
		}
		return nil, "Waiting for MaxScale proxy to be ready", nil
	}
	return mxs, "", nil
}

// resolveMaxScaleImage returns the MaxScale image from the proxy component
// override, its selected version, or the provider default — mirroring the
// engine image resolution precedence.
func resolveMaxScaleImage(c *controller.Context) (string, error) {
	proxy := c.Instance().Spec.Components[common.ComponentProxy]
	if proxy.Image != "" {
		return proxy.Image, nil
	}

	spec, err := c.ProviderSpec()
	if err != nil {
		return "", fmt.Errorf("get provider spec: %w", err)
	}

	image := ""
	if proxy.Version != "" {
		image = controller.GetImageForVersion(spec, common.ComponentProxy, proxy.Version)
	}
	if image == "" {
		image = controller.GetDefaultImageForComponent(spec, common.ComponentProxy)
	}
	return image, nil
}
