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
	"strings"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition"
	"github.com/openeverest/provider-mariadb/definition/components"
	"github.com/openeverest/provider-mariadb/internal/common"
)

// ValidateMariaDB validates the Instance spec for the MariaDB provider.
func ValidateMariaDB(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Validating MariaDB instance", "name", c.Name())

	if err := validateComponents(c); err != nil {
		l.Error(err, "Component validation failed", "name", c.Name())
		return fmt.Errorf("component validation: %w", err)
	}

	if err := validateMonitoring(c); err != nil {
		l.Error(err, "Monitoring validation failed", "name", c.Name())
		return fmt.Errorf("monitoring validation: %w", err)
	}

	if err := validateTopology(c); err != nil {
		l.Error(err, "Topology validation failed", "name", c.Name())
		return fmt.Errorf("topology validation: %w", err)
	}

	if err := validateTLS(c); err != nil {
		l.Error(err, "TLS validation failed", "name", c.Name())
		return fmt.Errorf("TLS validation: %w", err)
	}

	return nil
}

// validateMonitoring ensures the monitoring component parameters are decodable
// and, when metrics are enabled, that an exporter image can be resolved.
func validateMonitoring(c *controller.Context) error {
	params, err := monitoringParams(c)
	if err != nil {
		return err
	}
	if params == nil || !params.Enabled {
		return nil
	}

	image, err := resolveExporterImage(c)
	if err != nil {
		return err
	}
	if image == "" {
		return fmt.Errorf("unable to resolve a mysqld-exporter image for the monitoring component")
	}
	return nil
}

// validateComponents verifies that required components are present and their values are sane.
func validateComponents(c *controller.Context) error {
	engine, ok := c.Instance().Spec.Components[common.ComponentEngine]
	if !ok {
		return fmt.Errorf("component %q is required", common.ComponentEngine)
	}

	if engine.Replicas != nil && *engine.Replicas < 1 {
		return fmt.Errorf("engine replicas must be at least 1, got %d", *engine.Replicas)
	}

	if engine.Storage != nil && !engine.Storage.Size.IsZero() {
		if engine.Storage.Size.Cmp(mustParseQuantity("1Gi")) < 0 {
			return fmt.Errorf("engine storage size must be at least 1Gi")
		}
	}

	if err := validateService(engine.Service); err != nil {
		return err
	}

	if err := validateAffinity(engine.Affinity); err != nil {
		return err
	}

	if err := validateNodeAffinity(c, engine); err != nil {
		return err
	}

	return nil
}

// validateNodeAffinity checks the engine's node-targeting parameter: it must parse and
// must not be combined with a raw affinity.nodeAffinity (the two would conflict).
func validateNodeAffinity(c *controller.Context, engine corev1alpha1.ComponentSpec) error {
	var params components.MariadbParameters
	if !c.TryDecodeComponentParameters(engine, &params) || strings.TrimSpace(params.NodeAffinity) == "" {
		return nil
	}
	if engine.Affinity != nil && engine.Affinity.NodeAffinity != nil {
		return fmt.Errorf(
			"spec.components.%s: set node targeting via either affinity.nodeAffinity or the nodeAffinity parameter, not both",
			common.ComponentEngine,
		)
	}
	if _, err := parseNodeAffinityRules(params.NodeAffinity); err != nil {
		return fmt.Errorf("spec.components.%s.parameters.nodeAffinity: %w", common.ComponentEngine, err)
	}
	return nil
}

// validateService ensures the requested Service type is one the operator supports.
func validateService(svc *corev1alpha1.Service) error {
	if svc == nil || svc.ServiceType == "" {
		return nil
	}

	switch svc.ServiceType {
	case corev1.ServiceTypeClusterIP,
		corev1.ServiceTypeLoadBalancer,
		corev1.ServiceTypeNodePort:
		return nil
	default:
		return fmt.Errorf(
			"spec.components.%s.service.serviceType must be one of ClusterIP, LoadBalancer or NodePort",
			common.ComponentEngine,
		)
	}
}

// validateTopology checks the selected topology, its Galera-specific replica
// rules, and that the topology is not being changed on an existing instance.
func validateTopology(c *controller.Context) error {
	topology := c.Instance().GetTopologyType()
	switch topology {
	case "", string(definition.TopologyTypeStandalone), string(definition.TopologyTypeGalera):
	default:
		return fmt.Errorf(
			"unsupported spec.topology.type %q; supported values are %q and %q",
			topology, definition.TopologyTypeStandalone, definition.TopologyTypeGalera,
		)
	}

	galera := topology == string(definition.TopologyTypeGalera)
	if galera {
		if err := validateGaleraReplicas(c); err != nil {
			return err
		}
	} else {
		if err := validateStandaloneReplicas(c); err != nil {
			return err
		}
	}
	return validateTopologyImmutable(c, galera)
}

// validateStandaloneReplicas enforces that the standalone topology runs a single
// node; MariaDB has no clustering without Galera, so more than one replica is invalid.
func validateStandaloneReplicas(c *controller.Context) error {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	if engine.Replicas != nil && *engine.Replicas != standaloneDefaultReplicas {
		return fmt.Errorf(
			"standalone topology supports a single node only, got %d; use the galera topology for multiple nodes",
			*engine.Replicas,
		)
	}
	return nil
}

// validateGaleraReplicas enforces Galera's quorum requirement: an odd number of
// engine nodes, at least 3.
func validateGaleraReplicas(c *controller.Context) error {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	replicas := galeraDefaultReplicas
	if engine.Replicas != nil {
		replicas = *engine.Replicas
	}
	if replicas < 3 {
		return fmt.Errorf("galera topology requires at least 3 engine replicas, got %d", replicas)
	}
	if replicas%2 == 0 {
		return fmt.Errorf(
			"galera topology requires an odd number of engine replicas to avoid split-brain, got %d",
			replicas,
		)
	}
	return nil
}

// validateTopologyImmutable rejects switching an existing instance between the
// standalone and Galera topologies. The provider has no access to the previous
// Instance spec at admission time, so it compares against the Galera state of
// the already-provisioned MariaDB CR.
func validateTopologyImmutable(c *controller.Context, galera bool) error {
	existing := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(existing, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get MariaDB: %w", err)
	}
	if existing.IsGaleraEnabled() != galera {
		return fmt.Errorf("spec.topology is immutable and cannot be changed on an existing instance")
	}
	return nil
}

// mustParseQuantity is a helper that panics on invalid quantity strings (compile-time constants only).
func mustParseQuantity(s string) resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic(fmt.Sprintf("invalid quantity %q: %v", s, err))
	}
	return q
}
