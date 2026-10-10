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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/csaupgrade"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

const (
	// applyFieldManager is the field manager controller.Context.Apply uses.
	applyFieldManager = "provider-" + common.ProviderName
	// legacyFieldManager owned the fields the provider wrote with Create/Update
	// before server-side apply; it is the default user-agent derived name.
	legacyFieldManager = "provider"
)

// toApplyObject converts a desired typed object into the unstructured form sent
// with server-side apply. Operator types serialize zero values of fields without
// omitempty ("" images, empty secret refs, empty structs); applied, those would
// take ownership of and clobber operator defaults, so they are pruned and only
// the fields the provider actually set remain. Explicit false and 0 are kept.
func toApplyObject(c *controller.Context, obj runtime.Object) (*unstructured.Unstructured, error) {
	gvk, err := apiutil.GVKForObject(obj, c.Client().Scheme())
	if err != nil {
		return nil, fmt.Errorf("resolve GVK: %w", err)
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("convert %s: %w", gvk.Kind, err)
	}
	delete(raw, "status")
	pruneUnset(raw)
	u := &unstructured.Unstructured{Object: raw}
	u.SetGroupVersionKind(gvk)
	return u, nil
}

// pruneUnset drops nulls, empty strings and the empty objects and lists they
// leave behind. List elements are pruned in place but never removed.
func pruneUnset(m map[string]any) {
	for k, v := range m {
		switch val := v.(type) {
		case nil:
			delete(m, k)
		case string:
			if val == "" {
				delete(m, k)
			}
		case map[string]any:
			pruneUnset(val)
			if len(val) == 0 {
				delete(m, k)
			}
		case []any:
			for _, item := range val {
				if child, ok := item.(map[string]any); ok {
					pruneUnset(child)
				}
			}
			if len(val) == 0 {
				delete(m, k)
			}
		}
	}
}

// migrateFieldOwnership hands the fields the provider wrote with Create/Update
// over to its server-side apply field manager, once per object. Without it, the
// legacy manager keeps owning them, so fields the provider stops applying (e.g.
// spec.metrics when monitoring is turned off) would never be removed.
func migrateFieldOwnership(c *controller.Context, obj client.Object) error {
	if !hasLegacyUpdateManager(obj.GetManagedFields()) {
		return nil
	}
	if err := csaupgrade.UpgradeManagedFields(obj, sets.New(legacyFieldManager), applyFieldManager); err != nil {
		return fmt.Errorf("upgrade managed fields of %s: %w", obj.GetName(), err)
	}
	if err := c.Client().Update(c.Context(), obj); err != nil {
		return fmt.Errorf("migrate field ownership of %s: %w", obj.GetName(), err)
	}
	return nil
}

func hasLegacyUpdateManager(entries []metav1.ManagedFieldsEntry) bool {
	for _, e := range entries {
		if e.Manager == legacyFieldManager && e.Operation == metav1.ManagedFieldsOperationUpdate && e.Subresource == "" {
			return true
		}
	}
	return false
}
