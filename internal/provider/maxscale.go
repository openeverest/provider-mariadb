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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
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

// SyncMaxScale server-side applies the MaxScale CR in front of the MariaDB when
// the proxy is enabled, and deletes it otherwise. Only the fields the provider
// owns are declared; the operator keeps owning the ones it defaults (servers,
// monitor module, services, auth, TLS certificates, ...).
func SyncMaxScale(c *controller.Context) error {
	enabled, err := isProxyEnabled(c)
	if err != nil {
		return err
	}
	if !enabled {
		return teardownMaxScale(c)
	}

	desired, err := desiredMaxScale(c)
	if err != nil {
		return err
	}

	existing := &mariadbv1alpha1.MaxScale{}
	err = c.Get(existing, desired.Name)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("get MaxScale: %w", err)
	default:
		if err := migrateFieldOwnership(c, existing); err != nil {
			return err
		}
	}

	obj, err := toApplyObject(c, desired)
	if err != nil {
		return err
	}
	pruneMaxScaleZeroDefaults(obj.Object)
	if err := c.Apply(obj); err != nil {
		return fmt.Errorf("apply MaxScale: %w", err)
	}
	return nil
}

// pruneMaxScaleZeroDefaults drops the zero values of non-omitempty fields the
// operator defaults, which toApplyObject keeps as explicit 0 / "0s".
func pruneMaxScaleZeroDefaults(obj map[string]any) {
	unstructured.RemoveNestedField(obj, "spec", "admin", "port")
	unstructured.RemoveNestedField(obj, "spec", "monitor", "interval")
	pruneUnset(obj)
}

// desiredMaxScale builds the MaxScale fields the provider owns from the proxy
// component.
func desiredMaxScale(c *controller.Context) (*mariadbv1alpha1.MaxScale, error) {
	proxy := c.Instance().Spec.Components[common.ComponentProxy]
	name := maxScaleName(c.Name())

	image, err := resolveMaxScaleImage(c)
	if err != nil {
		return nil, err
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

	scheduling := ptr.Deref(proxy.SchedulingPolicy, commonv1alpha1.SchedulingPolicy{})

	tlsSettings, err := resolveTLSSettings(c)
	if err != nil {
		return nil, fmt.Errorf("resolve TLS settings: %w", err)
	}

	mxs := &mariadbv1alpha1.MaxScale{
		ObjectMeta: c.ObjectMeta(name),
		Spec: mariadbv1alpha1.MaxScaleSpec{
			// Immutable; the operator infers servers, the monitor module
			// (galeramon / mariadbmon) and credentials from it.
			MariaDBRef: &mariadbv1alpha1.MariaDBRef{
				ObjectReference: mariadbv1alpha1.ObjectReference{Name: c.Name()},
				WaitForIt:       true,
			},
			Image:             image,
			Replicas:          replicas,
			KubernetesService: configureService(proxy.Service),
			// MaxScale listeners only accept TLS clients once TLS is on, so it
			// mirrors the engine's enforcement rather than its enablement. Only
			// the switch is declared; the certificate references stay with the
			// operator.
			TLS: &mariadbv1alpha1.MaxScaleTLS{Enabled: tlsSettings.Enabled && tlsSettings.Required},
		},
	}
	mxs.Spec.Resources = resources
	// MaxScale pods may share nodes with each other only as a last resort.
	mxs.Spec.Affinity = buildAffinity(scheduling.Affinity, true, name)
	mxs.Spec.NodeSelector = scheduling.NodeSelector
	mxs.Spec.Tolerations = scheduling.Tolerations
	mxs.Spec.TopologySpreadConstraints = convertTopologySpreadConstraints(
		controller.TopologySpreadConstraints(&scheduling, maxScalePodLabels(name)),
	)
	mxs.Spec.PodMetadata = &mariadbv1alpha1.Metadata{Labels: c.PodLabels(common.ComponentProxy)}
	if isReplicationTopology(c) {
		mxs.Spec.Monitor.Params = maxScaleMonitorParams()
	}
	return mxs, nil
}

// maxScaleTopologyParams are the mariadbmon operations that change the
// replication topology. The operator stays the single owner of failover,
// rejoin and read_only: running them in MaxScale as well races with the
// operator, which reconfigures a node MaxScale just promoted back into a
// read-only replica of the failed primary (#37).
var maxScaleTopologyParams = []string{"auto_failover", "auto_rejoin", "switchover_on_low_disk_space"}

// maxScaleMonitorParams turns the mariadbmon topology operations off. Applied
// on creation, they keep the operator from defaulting the monitor parameters,
// which it only does while they are unset; other parameters stay untouched.
func maxScaleMonitorParams() map[string]string {
	params := make(map[string]string, len(maxScaleTopologyParams))
	for _, param := range maxScaleTopologyParams {
		params[param] = "false"
	}
	return params
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
