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

// package provider implements the OpenEverest provider for MariaDB,
// backed by the mariadb-operator (k8s.mariadb.com API group).
package provider

import (
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

// Compile-time check that MariaDBProvider implements the required interface.
var _ controller.ProviderInterface = (*MariaDBProvider)(nil)

// MariaDBProvider implements controller.ProviderInterface for MariaDB via mariadb-operator.
type MariaDBProvider struct {
	controller.BaseProvider
}

// New creates a new MariaDBProvider instance.
func New() *MariaDBProvider {
	return &MariaDBProvider{
		BaseProvider: controller.BaseProvider{
			ProviderName: common.ProviderName,
			SchemeFuncs: []func(*runtime.Scheme) error{
				mariadbv1alpha1.AddToScheme,
			},
			WatchConfigs: []controller.WatchConfig{
				// Re-enqueue the Instance when its owned MariaDB CR changes
				// (e.g., operator updates status after reconciliation).
				controller.WatchOwned(&mariadbv1alpha1.MariaDB{}),
			},
		},
	}
}

// Validate checks if the Instance spec is valid before a create/update is accepted.
// Called by the validation webhook.
func (p *MariaDBProvider) Validate(c *controller.Context) error {
	return ValidateMariaDB(c)
}

// Sync ensures the MariaDB CR exists and reflects the current Instance spec.
func (p *MariaDBProvider) Sync(c *controller.Context) error {
	return SyncMariaDB(c)
}

// Status reads the MariaDB CR status and translates it to an Instance status.
func (p *MariaDBProvider) Status(c *controller.Context) (controller.Status, error) {
	return StatusMariaDB(c)
}

// Cleanup deletes the MariaDB CR when the Instance is deleted.
// Owner references handle cascaded cleanup of child resources.
func (p *MariaDBProvider) Cleanup(c *controller.Context) error {
	return CleanupMariaDB(c)
}
