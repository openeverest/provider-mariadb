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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition"
)

const (
	// standaloneDefaultReplicas is the default node count for the standalone topology.
	standaloneDefaultReplicas int32 = 1
	// galeraDefaultReplicas is the default node count for the Galera topology.
	// Galera needs an odd number of nodes (>= 3) to maintain quorum.
	galeraDefaultReplicas int32 = 3

	// primaryServiceSuffix is appended to the instance name to form the operator's
	// primary Service (<name>-primary), which routes writes to the current primary.
	primaryServiceSuffix = "-primary"
)

// isGaleraTopology reports whether the Instance selects the Galera HA topology.
// A missing topology defaults to standalone.
func isGaleraTopology(c *controller.Context) bool {
	return c.Instance().GetTopologyType() == string(definition.TopologyTypeGalera)
}

// defaultReplicasForTopology returns the default replica count applied when the
// user leaves engine.replicas unset.
func defaultReplicasForTopology(galera bool) int32 {
	if galera {
		return galeraDefaultReplicas
	}
	return standaloneDefaultReplicas
}

// applyGaleraOverlay enables Galera on the MariaDB spec while preserving the
// operator-defaulted Galera sub-fields (SST, recovery, agent/init images, ...)
// across reconciles. Topology is immutable, so this only ever turns Galera on;
// standalone instances are left untouched.
func applyGaleraOverlay(mariadb *mariadbv1alpha1.MariaDB, galera bool) {
	if !galera {
		return
	}
	if mariadb.Spec.Galera == nil {
		mariadb.Spec.Galera = &mariadbv1alpha1.Galera{Enabled: true}
		return
	}
	mariadb.Spec.Galera.Enabled = true
}

// defaultGaleraAffinity returns a soft (preferred) pod anti-affinity that spreads
// MariaDB pods across nodes for HA without blocking scheduling on clusters with
// fewer nodes than replicas. It is applied only when the user provides no
// explicit affinity, and matches the operator's own pod instance label.
func defaultGaleraAffinity(instanceName string) *mariadbv1alpha1.AffinityConfig {
	return &mariadbv1alpha1.AffinityConfig{
		Affinity: mariadbv1alpha1.Affinity{
			PodAntiAffinity: &mariadbv1alpha1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []mariadbv1alpha1.WeightedPodAffinityTerm{
					{
						Weight: 1,
						PodAffinityTerm: mariadbv1alpha1.PodAffinityTerm{
							TopologyKey: "kubernetes.io/hostname",
							LabelSelector: &mariadbv1alpha1.LabelSelector{
								MatchExpressions: []mariadbv1alpha1.LabelSelectorRequirement{
									{
										Key:      "app.kubernetes.io/instance",
										Operator: metav1.LabelSelectorOpIn,
										Values:   []string{instanceName},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
