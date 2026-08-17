// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
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
	"k8s.io/utils/ptr"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition/components"
	"github.com/openeverest/provider-mariadb/internal/common"
)

const (
	tlsCAKey             = "ca.crt"
	tlsConnectionKey     = "tls"
	tlsCABundleSuffix    = "-ca-bundle"
	tlsEnabledByDefault  = true
	tlsRequiredByDefault = false
)

type tlsSettings struct {
	Enabled          bool
	Required         bool
	GaleraSSTEnabled bool
}

// resolveTLSSettings decodes engine TLS parameters and applies provider
// defaults. TLS is enabled by default; enforcement remains opt-in so existing
// password-based clients can migrate without an abrupt connection failure.
func resolveTLSSettings(c *controller.Context) (tlsSettings, error) {
	settings := tlsSettings{
		Enabled:  tlsEnabledByDefault,
		Required: tlsRequiredByDefault,
	}

	engine := c.Instance().Spec.Components[common.ComponentEngine]
	var params components.MariadbParameters
	if engine.Parameters != nil && engine.Parameters.Raw != nil {
		if err := c.DecodeComponentParameters(engine, &params); err != nil {
			return tlsSettings{}, fmt.Errorf("decode engine parameters: %w", err)
		}
	}

	if params.TLS != nil {
		if params.TLS.Enabled != nil {
			settings.Enabled = bool(*params.TLS.Enabled)
		}
		if params.TLS.Required != nil {
			settings.Required = bool(*params.TLS.Required)
		}
	}

	// Encrypt Galera SSTs by default for new TLS-enabled Galera instances.
	settings.GaleraSSTEnabled = isGaleraTopology(c) && settings.Enabled
	if params.TLS != nil && params.TLS.GaleraSSTEnabled != nil {
		settings.GaleraSSTEnabled = bool(*params.TLS.GaleraSSTEnabled)
	}

	return settings, nil
}

func validateTLS(c *controller.Context) error {
	settings, err := resolveTLSSettings(c)
	if err != nil {
		return err
	}
	if !settings.Enabled && settings.Required {
		return fmt.Errorf("spec.components.%s.parameters.tls.required cannot be true when TLS is disabled", common.ComponentEngine)
	}
	if !settings.Enabled && settings.GaleraSSTEnabled {
		return fmt.Errorf("spec.components.%s.parameters.tls.galeraSSTEnabled cannot be true when TLS is disabled", common.ComponentEngine)
	}
	if !isGaleraTopology(c) && settings.GaleraSSTEnabled {
		return fmt.Errorf("spec.components.%s.parameters.tls.galeraSSTEnabled is only supported by the galera topology", common.ComponentEngine)
	}
	return nil
}

func buildTLS(c *controller.Context) (*mariadbv1alpha1.TLS, error) {
	settings, err := resolveTLSSettings(c)
	if err != nil {
		return nil, err
	}

	tls := &mariadbv1alpha1.TLS{
		Enabled:  settings.Enabled,
		Required: ptr.To(settings.Required),
	}
	if isGaleraTopology(c) {
		tls.GaleraSSTEnabled = ptr.To(settings.GaleraSSTEnabled)
	}
	return tls, nil
}

// applyTLSOverlay changes only the fields owned by this provider and preserves
// operator-defaulted or user-managed CA, certificate, and issuer references.
func applyTLSOverlay(mdb *mariadbv1alpha1.MariaDB, desired *mariadbv1alpha1.TLS) {
	if mdb.Spec.TLS == nil {
		mdb.Spec.TLS = desired
		return
	}
	mdb.Spec.TLS.Enabled = desired.Enabled
	mdb.Spec.TLS.Required = desired.Required
	mdb.Spec.TLS.GaleraSSTEnabled = desired.GaleraSSTEnabled
}

func tlsCABundleSecretName(instanceName string) string {
	return instanceName + tlsCABundleSuffix
}
