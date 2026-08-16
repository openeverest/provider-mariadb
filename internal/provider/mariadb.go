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
	"strconv"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition/components"
	"github.com/openeverest/provider-mariadb/internal/common"
)

const (
	// defaultPort is the default MariaDB port.
	defaultPort = 3306

	// rootPasswordSecretKey is the key in the root password Secret.
	rootPasswordSecretKey = "root-password"

	// userPasswordSecretKey is the key in the initial user's password Secret.
	userPasswordSecretKey = "password"

	// defaultInitialDatabase is the initial database created on the cluster.
	defaultInitialDatabase = "everest"

	// defaultInitialUser is the initial user created on the cluster.
	defaultInitialUser = "everest"
)

// userSecretName returns the name of the Secret holding the initial user's password
// for the given instance. Follows the OpenEverest convention used across providers and
// is the Secret surfaced through the connection details.
func userSecretName(instanceName string) string {
	return "everest-secrets-" + instanceName
}

// rootSecretName returns the name of the Secret holding the root password for the given
// instance. The root password lives in a dedicated Secret because the operator generates
// each password Secret only when the whole Secret is absent, so root and user passwords
// cannot share a single Secret.
func rootSecretName(instanceName string) string {
	return "everest-secrets-" + instanceName + "-root"
}

// SyncMariaDB creates or updates the MariaDB CR based on the Instance spec.
func SyncMariaDB(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Syncing MariaDB cluster", "cluster", c.Name())
	defer l.Info("MariaDB cluster synced", "cluster", c.Name())

	engine := c.Instance().Spec.Components[common.ComponentEngine]

	// Resolve image from version bundle or provider default.
	image := ""
	if engine.Image != "" {
		image = engine.Image
	} else {
		spec, err := c.ProviderSpec()
		if err != nil {
			return fmt.Errorf("get provider spec: %w", err)
		}
		if engine.Version != "" {
			image = controller.GetImageForVersion(spec, common.ComponentEngine, engine.Version)
		}
		if image == "" {
			image = controller.GetDefaultImageForComponent(spec, common.ComponentEngine)
		}
	}

	// Replicas
	replicas := int32(1)
	if engine.Replicas != nil {
		replicas = *engine.Replicas
	}

	// Storage
	storageSize := resource.MustParse("10Gi")
	var storageClassName string
	if engine.Storage != nil {
		if !engine.Storage.Size.IsZero() {
			storageSize = engine.Storage.Size
		}
		if engine.Storage.StorageClass != nil {
			storageClassName = *engine.Storage.StorageClass
		}
	}

	// Resources — map from corev1.ResourceRequirements to the operator's local type.
	var resourceReqs *mariadbv1alpha1.ResourceRequirements
	if engine.Resources != nil && (engine.Resources.Limits != nil || engine.Resources.Requests != nil) {
		resourceReqs = &mariadbv1alpha1.ResourceRequirements{
			Limits:   engine.Resources.Limits,
			Requests: engine.Resources.Requests,
		}
	}

	// Optional my.cnf from the engine component's `configuration` parameter.
	var myCnf *string
	var params components.MariadbParameters
	if c.TryDecodeComponentParameters(engine, &params) && params.Configuration != "" {
		myCnf = &params.Configuration
	}

	// Exposure: map the requested Service onto the general Service (<name>). The
	// primary Service (<name>-primary) only exists for replication/Galera; for a
	// standalone instance the general Service is the endpoint clients connect to.
	generalService := configureService(engine.Service)

	// Metrics: map the optional monitoring component onto spec.metrics. Nil when
	// monitoring is not enabled, so the operator deploys no exporter.
	metrics, err := buildMetrics(c)
	if err != nil {
		return fmt.Errorf("build metrics: %w", err)
	}

	// Affinity: map the engine component's Kubernetes affinity onto the
	// operator's trimmed AffinityConfig. Nil when unset, so the operator keeps
	// its own scheduling defaults.
	affinity := convertAffinity(engine.Affinity)

	// Read-modify-write: c.Apply performs a full Update, so we must start from
	// the operator's current object and overlay only the fields we manage.
	// Building a bare spec here would wipe every operator-defaulted field
	// (probes, security context, updateStrategy, TLS, ...) on each reconcile,
	// causing an endless rolling update of the StatefulSet.
	existing := &mariadbv1alpha1.MariaDB{}
	err = c.Get(existing, c.Name())
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get MariaDB: %w", err)
	}

	var mariadbCR *mariadbv1alpha1.MariaDB
	if apierrors.IsNotFound(err) {
		mariadbCR = buildInitialMariaDB(c, image, replicas, storageSize, storageClassName, resourceReqs, myCnf, generalService)
		mariadbCR.Spec.Metrics = metrics
	} else {
		// Overlay only the managed, mutable fields; preserve operator defaults.
		mariadbCR = existing
		mariadbCR.Spec.Image = image
		mariadbCR.Spec.Replicas = replicas
		mariadbCR.Spec.Storage.Size = &storageSize
		mariadbCR.Spec.MyCnf = myCnf
		mariadbCR.Spec.Service = generalService
		applyMetricsOverlay(mariadbCR, metrics)
		if resourceReqs != nil {
			mariadbCR.Spec.Resources = resourceReqs
		}
	}

	// Overlay affinity for both the create and update paths. AntiAffinityEnabled
	// is left unset (the user supplies explicit rules), so the operator's
	// affinity defaulting is a no-op and does not fight this value.
	mariadbCR.Spec.Affinity = affinity

	if err := c.Apply(mariadbCR); err != nil {
		return fmt.Errorf("apply MariaDB: %w", err)
	}

	return nil
}

// buildInitialMariaDB constructs the MariaDB CR for first creation. Subsequent
// reconciles read-modify-write the operator's object instead of rebuilding it.
func buildInitialMariaDB(
	c *controller.Context,
	image string,
	replicas int32,
	storageSize resource.Quantity,
	storageClassName string,
	resourceReqs *mariadbv1alpha1.ResourceRequirements,
	myCnf *string,
	generalService *mariadbv1alpha1.ServiceTemplate,
) *mariadbv1alpha1.MariaDB {
	storage := mariadbv1alpha1.Storage{
		Size:      &storageSize,
		Ephemeral: ptr.To(false),
	}
	if storageClassName != "" {
		storage.StorageClassName = storageClassName
	}

	rootRef := mariadbv1alpha1.GeneratedSecretKeyRef{
		SecretKeySelector: mariadbv1alpha1.SecretKeySelector{
			LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: rootSecretName(c.Name())},
			Key:                  rootPasswordSecretKey,
		},
		Generate: true,
	}

	initialUser := defaultInitialUser
	initialDB := defaultInitialDatabase
	passwordRef := &mariadbv1alpha1.GeneratedSecretKeyRef{
		SecretKeySelector: mariadbv1alpha1.SecretKeySelector{
			LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: userSecretName(c.Name())},
			Key:                  userPasswordSecretKey,
		},
		Generate: true,
	}

	return &mariadbv1alpha1.MariaDB{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec: mariadbv1alpha1.MariaDBSpec{
			Image:                    image,
			ImagePullPolicy:          corev1.PullIfNotPresent,
			Replicas:                 replicas,
			Storage:                  storage,
			RootEmptyPassword:        ptr.To(false),
			MyCnf:                    myCnf,
			RootPasswordSecretKeyRef: rootRef,
			Username:                 &initialUser,
			Database:                 &initialDB,
			PasswordSecretKeyRef:     passwordRef,
			Service:                  generalService,
			// The operator enables mutual TLS by default, which would require
			// clients to present a certificate. The connection details we emit
			// only carry username/password, so disable TLS to keep them usable.
			TLS: &mariadbv1alpha1.TLS{Enabled: false},
			ContainerTemplate: mariadbv1alpha1.ContainerTemplate{
				Resources: resourceReqs,
			},
		},
	}
}

// configureService maps the SDK Service exposure request onto the operator's
// ServiceTemplate. Returns nil when no exposure is requested (operator defaults
// to a ClusterIP Service).
func configureService(svc *corev1alpha1.Service) *mariadbv1alpha1.ServiceTemplate {
	if svc == nil {
		return nil
	}

	serviceType := svc.ServiceType
	if serviceType == "" {
		serviceType = corev1.ServiceTypeClusterIP
	}

	tmpl := &mariadbv1alpha1.ServiceTemplate{
		Type: serviceType,
	}
	if len(svc.Annotations) > 0 {
		tmpl.Metadata = &mariadbv1alpha1.Metadata{Annotations: svc.Annotations}
	}
	if serviceType == corev1.ServiceTypeLoadBalancer &&
		svc.LoadBalancerService != nil &&
		svc.LoadBalancerService.SourceRanges != nil {
		tmpl.LoadBalancerSourceRanges = svc.LoadBalancerService.SourceRanges.NormalizedSourceRanges()
	}
	return tmpl
}

// StatusMariaDB reads the MariaDB CR status and translates it to an Instance status.
func StatusMariaDB(c *controller.Context) (controller.Status, error) {
	mariadbCR := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(mariadbCR, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.Provisioning("Waiting for MariaDB cluster to be created"), nil
		}
		return controller.Status{}, fmt.Errorf("get MariaDB: %w", err)
	}

	// Check the Ready condition.
	if mariadbCR.IsReady() {
		details, err := buildConnectionDetails(c)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return controller.Provisioning("Waiting for MariaDB credentials secret"), nil
			}
			return controller.Status{}, err
		}
		return controller.ReadyWithConnectionDetails(details), nil
	}

	// Extract a useful message from the Ready condition if available.
	if cond, ok := getReadyCondition(mariadbCR); ok && cond.Message != "" {
		return controller.Provisioning(cond.Message), nil
	}

	return controller.Provisioning("Waiting for MariaDB cluster to be ready"), nil
}

// buildConnectionDetails reads the generated user credentials secret and combines
// it with the primary Service host to produce a full set of connection details.
func buildConnectionDetails(c *controller.Context) (controller.ConnectionDetails, error) {
	secret := &corev1.Secret{}
	if err := c.Get(secret, userSecretName(c.Name())); err != nil {
		return controller.ConnectionDetails{}, fmt.Errorf("get credentials secret: %w", err)
	}

	host := resolveHost(c)
	port := strconv.Itoa(defaultPort)
	username := defaultInitialUser
	password := string(secret.Data[userPasswordSecretKey])

	return controller.ConnectionDetails{
		Type:     "mysql",
		Provider: common.ProviderName,
		Host:     host,
		Port:     port,
		Username: username,
		Password: password,
		URI: fmt.Sprintf(
			"mysql://%s:%s@%s:%s/%s",
			username, password, host, port, defaultInitialDatabase,
		),
	}, nil
}

// resolveHost returns the externally reachable host for the general Service
// (<name>): a LoadBalancer ingress address when available, otherwise the
// internal cluster FQDN.
func resolveHost(c *controller.Context) string {
	internal := fmt.Sprintf("%s.%s.svc", c.Name(), c.Namespace())

	svc := &corev1.Service{}
	if err := c.Get(svc, c.Name()); err != nil {
		return internal
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return internal
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP
		}
		if ing.Hostname != "" {
			return ing.Hostname
		}
	}
	return internal
}

// CleanupMariaDB deletes the MariaDB CR when the Instance is being deleted.
// The MariaDB CR is owned by the Instance (via c.ObjectMeta), so cascaded GC
// handles child resources automatically. This explicit delete ensures the operator
// performs any finalizer-driven cleanup (e.g., releasing PVCs via operator policy).
func CleanupMariaDB(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Cleaning up MariaDB cluster", "cluster", c.Name())

	mariadbCR := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.Name(),
			Namespace: c.Namespace(),
		},
	}
	if err := c.Delete(mariadbCR); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete MariaDB: %w", err)
	}

	return nil
}

// getReadyCondition returns the Ready condition from the MariaDB status, if present.
func getReadyCondition(m *mariadbv1alpha1.MariaDB) (metav1.Condition, bool) {
	for _, cond := range m.Status.Conditions {
		if cond.Type == mariadbv1alpha1.ConditionTypeReady {
			return cond, true
		}
	}
	return metav1.Condition{}, false
}

