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
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition"
)

// replicationDefaultReplicas is the default node count for the replication topology:
// one primary and two replicas. The minimum is 2 (one primary and one replica).
const replicationDefaultReplicas int32 = 3

// isReplicationTopology reports whether the Instance selects the async replication topology.
func isReplicationTopology(c *controller.Context) bool {
	return c.Instance().GetTopologyType() == string(definition.TopologyTypeReplication)
}

// isHATopology reports whether the topology runs multiple coordinated nodes
// (Galera or replication). These share primary-Service routing, client host
// resolution and the default pod anti-affinity.
func isHATopology(c *controller.Context) bool {
	return isGaleraTopology(c) || isReplicationTopology(c)
}

// applyReplicationOverlay enables replication on the MariaDB spec while preserving
// the operator-defaulted replication sub-fields (primary index, replica settings,
// agent/init images, ...) across reconciles. Topology is immutable, so this only
// ever turns replication on; other topologies are left untouched.
func applyReplicationOverlay(mariadb *mariadbv1alpha1.MariaDB, replication bool) {
	if !replication {
		return
	}
	if mariadb.Spec.Replication == nil {
		mariadb.Spec.Replication = &mariadbv1alpha1.Replication{Enabled: true}
		return
	}
	mariadb.Spec.Replication.Enabled = true
}
