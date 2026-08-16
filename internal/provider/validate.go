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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

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

// mustParseQuantity is a helper that panics on invalid quantity strings (compile-time constants only).
func mustParseQuantity(s string) resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic(fmt.Sprintf("invalid quantity %q: %v", s, err))
	}
	return q
}
