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
	"testing"

	"github.com/openeverest/openeverest/v2/provider-runtime/conformance"
)

// The probes set one field at a time, and proxy fields only apply while
// spec.components.proxy.parameters.enabled is true.
var proxyGatedPaths = map[string]string{
	"spec.components.proxy.replicas":            "applied only while the proxy is enabled",
	"spec.components.proxy.service.serviceType": "applied only while the proxy is enabled",
}

func TestUISchemaIsReconciled(t *testing.T) {
	conformance.UISchemaIsReconciled(t, conformance.Config{
		Provider:     New(),
		Unverifiable: proxyGatedPaths,
	})
}

func TestSupportedFieldsAreReconciled(t *testing.T) {
	conformance.SupportedFieldsAreReconciled(t, conformance.Config{
		Provider:     New(),
		Unverifiable: proxyGatedPaths,
	})
}
