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
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/openeverest/openeverest/v2/provider-runtime/conformance"
)

// The probes set one field at a time, and some fields only apply while another
// one is enabled: proxy fields need spec.components.proxy.parameters.enabled.
var gatedPaths = map[string]string{
	"spec.components.proxy.replicas":            "applied only while the proxy is enabled",
	"spec.components.proxy.service.serviceType": "applied only while the proxy is enabled",
}

func TestUISchemaIsReconciled(t *testing.T) {
	conformance.UISchemaIsReconciled(t, conformance.Config{
		Provider:         New(),
		ProviderSpecPath: typedFlexBoolSpec(t),
		Unverifiable:     gatedPaths,
	})
}

// typedFlexBoolSpec writes the generated Provider CR with its FlexBool
// parameters typed as booleans, since the probes only generate typed values.
func typedFlexBoolSpec(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(providerSpecPath)
	require.NoError(t, err)
	data, err := yaml.ToJSON(raw)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))

	flexBool := []any{true, false, "true", "false"}
	var typeFlexBools func(node any)
	typeFlexBools = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			if _, typed := n["type"]; !typed && reflect.DeepEqual(n["enum"], flexBool) {
				n["type"] = "boolean"
				delete(n, "enum")
			}
			for _, child := range n {
				typeFlexBools(child)
			}
		case []any:
			for _, child := range n {
				typeFlexBools(child)
			}
		}
	}
	typeFlexBools(doc)

	data, err = json.Marshal(doc)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "provider-spec.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func TestSupportedFieldsAreReconciled(t *testing.T) {
	conformance.SupportedFieldsAreReconciled(t, conformance.Config{
		Provider:     New(),
		Unverifiable: gatedPaths,
	})
}
