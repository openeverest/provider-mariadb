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

// rootPasswordSecretName returns the name of the Secret holding the root password
// for the given instance. Follows the OpenEverest convention used across providers.
func rootPasswordSecretName(instanceName string) string {
	return "everest-secrets-" + instanceName
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

	// Optional myCnf from component parameters.
	var myCnf *string
	var params components.MariadbParameters
	if c.TryDecodeComponentParameters(engine, &params) && params.MyCnf != "" {
		myCnf = &params.MyCnf
	}

	// Read-modify-write: c.Apply performs a full Update, so we must start from
	// the operator's current object and overlay only the fields we manage.
	// Building a bare spec here would wipe every operator-defaulted field
	// (probes, security context, updateStrategy, TLS, ...) on each reconcile,
	// causing an endless rolling update of the StatefulSet.
	existing := &mariadbv1alpha1.MariaDB{}
	err := c.Get(existing, c.Name())
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get MariaDB: %w", err)
	}

	var mariadbCR *mariadbv1alpha1.MariaDB
	if apierrors.IsNotFound(err) {
		mariadbCR = buildInitialMariaDB(c, image, replicas, storageSize, storageClassName, resourceReqs, myCnf)
	} else {
		// Overlay only the managed, mutable fields; preserve operator defaults.
		mariadbCR = existing
		mariadbCR.Spec.Image = image
		mariadbCR.Spec.Replicas = replicas
		mariadbCR.Spec.Storage.Size = &storageSize
		mariadbCR.Spec.MyCnf = myCnf
		if resourceReqs != nil {
			mariadbCR.Spec.ContainerTemplate.Resources = resourceReqs
		}
	}

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
) *mariadbv1alpha1.MariaDB {
	storage := mariadbv1alpha1.Storage{
		Size:      &storageSize,
		Ephemeral: ptr.To(false),
	}
	if storageClassName != "" {
		storage.StorageClassName = storageClassName
	}

	secretName := rootPasswordSecretName(c.Name())
	rootRef := mariadbv1alpha1.GeneratedSecretKeyRef{
		SecretKeySelector: mariadbv1alpha1.SecretKeySelector{
			LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: secretName},
			Key:                  rootPasswordSecretKey,
		},
		Generate: true,
	}

	initialUser := defaultInitialUser
	initialDB := defaultInitialDatabase
	passwordRef := &mariadbv1alpha1.GeneratedSecretKeyRef{
		SecretKeySelector: mariadbv1alpha1.SecretKeySelector{
			LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: secretName},
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
			ContainerTemplate: mariadbv1alpha1.ContainerTemplate{
				Resources: resourceReqs,
			},
		},
	}
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
		host := fmt.Sprintf("%s-primary.%s.svc", c.Name(), c.Namespace())
		details := controller.ConnectionDetails{
			Type:     "mysql",
			Host:     host,
			Port:     strconv.Itoa(defaultPort),
			Username: defaultInitialUser,
		}
		return controller.ReadyWithConnectionDetails(details), nil
	}

	// Extract a useful message from the Ready condition if available.
	if cond, ok := getReadyCondition(mariadbCR); ok && cond.Message != "" {
		return controller.Provisioning(cond.Message), nil
	}

	return controller.Provisioning("Waiting for MariaDB cluster to be ready"), nil
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

