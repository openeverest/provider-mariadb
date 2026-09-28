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

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"
)

func TestApplyUpdateStrategyOverlay(t *testing.T) {
	t.Run("fresh object enables both flags", func(t *testing.T) {
		mdb := &mariadbv1alpha1.MariaDB{}
		applyUpdateStrategyOverlay(mdb)
		assert.Equal(t, ptr.To(true), mdb.Spec.UpdateStrategy.AutoUpdateDataPlane)
		assert.Equal(t, ptr.To(true), mdb.Spec.UpdateStrategy.MariaDBAutoUpgradeEnabled)
	})

	t.Run("existing operator defaults are overridden and other fields preserved", func(t *testing.T) {
		mdb := &mariadbv1alpha1.MariaDB{
			Spec: mariadbv1alpha1.MariaDBSpec{
				UpdateStrategy: mariadbv1alpha1.UpdateStrategy{
					Type:                      mariadbv1alpha1.ReplicasFirstPrimaryLastUpdateType,
					AutoUpdateDataPlane:       ptr.To(false),
					MariaDBAutoUpgradeEnabled: ptr.To(false),
				},
			},
		}
		applyUpdateStrategyOverlay(mdb)
		assert.Equal(t, mariadbv1alpha1.ReplicasFirstPrimaryLastUpdateType, mdb.Spec.UpdateStrategy.Type)
		assert.Equal(t, ptr.To(true), mdb.Spec.UpdateStrategy.AutoUpdateDataPlane)
		assert.Equal(t, ptr.To(true), mdb.Spec.UpdateStrategy.MariaDBAutoUpgradeEnabled)
	})
}
