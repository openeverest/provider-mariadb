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
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	goversion "github.com/hashicorp/go-version"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

// operatorImageRepo identifies the mariadb-operator image, independent of the
// registry it is pulled from and the Helm release name its Deployment carries.
// The operator's webhook and cert-controller run the same image, with their
// own image values, under a subcommand.
const operatorImageRepo = "mariadb-operator/mariadb-operator"

const inClusterNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// dataPlaneTokenPrefix starts the approval token of a data-plane update. The
// token carries the operator version, so an approval authorizes one update
// only: the provider's release version is not bumped per release.
const dataPlaneTokenPrefix = "update-data-plane-to-"

// currentNamespace returns the namespace the provider runs in: the chart's
// release namespace, where the operator subchart is deployed too. Overridable
// in tests.
var currentNamespace = func() (string, error) {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns, nil
	}
	data, err := os.ReadFile(inClusterNamespaceFile)
	if err != nil {
		return "", fmt.Errorf("determine the provider namespace: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// operatorVersion returns the version of the running mariadb-operator, from
// the image tag of its Deployment in the provider namespace. Several operator
// Deployments with conflicting versions are an error rather than a coin flip.
func operatorVersion(c *controller.Context) (string, error) {
	namespace, err := currentNamespace()
	if err != nil {
		return "", err
	}
	deployments := &appsv1.DeploymentList{}
	if err := c.Client().List(c.Context(), deployments, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list deployments in %q: %w", namespace, err)
	}
	version := ""
	for _, d := range deployments.Items {
		for _, container := range d.Spec.Template.Spec.Containers {
			repo, tag := splitImage(container.Image)
			if tag == "" || !strings.HasSuffix(repo, operatorImageRepo) || isOperatorSubcommand(container.Args) {
				continue
			}
			if version != "" && version != tag {
				return "", fmt.Errorf("mariadb-operator deployments with conflicting versions (%s, %s) in namespace %q", version, tag, namespace)
			}
			version = tag
		}
	}
	if version == "" {
		return "", errors.New("mariadb-operator deployment not found in namespace " + namespace)
	}
	return version, nil
}

// isOperatorSubcommand reports whether a container runs one of the operator
// image's subcommands, such as webhook or cert-controller, rather than the
// operator that reconciles MariaDB.
func isOperatorSubcommand(args []string) bool {
	return len(args) > 0 && !strings.HasPrefix(args[0], "-")
}

// splitImage splits an image reference into its repository and version tag.
// A registry port sits before the first slash, so only a colon after the last
// slash starts the tag. Digest-only references have no tag.
func splitImage(image string) (string, string) {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i:], "/") {
		return image[:i], strings.TrimPrefix(image[i+1:], "v")
	}
	return image, ""
}

// dataPlaneVersion returns the version of the init and agent containers the
// operator runs next to MariaDB in Galera and replication clusters, as
// persisted on the MariaDB CR. It is empty for standalone clusters and before
// the operator has defaulted the images.
func dataPlaneVersion(mdb *mariadbv1alpha1.MariaDB) string {
	var image string
	switch {
	case mdb.Spec.Galera != nil && mdb.Spec.Galera.Enabled:
		image = mdb.Spec.Galera.Agent.Image
	case mdb.Spec.Replication != nil && mdb.Spec.Replication.Enabled:
		image = mdb.Spec.Replication.Agent.Image
	}
	_, tag := splitImage(image)
	return tag
}

// approveDataPlaneUpdate decides updateStrategy.autoUpdateDataPlane. The
// operator rolls every Pod when it bumps the data-plane images to its own
// version, so a data plane behind the running operator is only updated once
// the owner approves; until then the cluster keeps running the previous data
// plane, which the operator supports within its skew window (see
// minDataPlaneVersion). New clusters get the operator's data plane on
// creation and need no update.
func approveDataPlaneUpdate(c *controller.Context, existing *mariadbv1alpha1.MariaDB) bool {
	if existing == nil {
		return false
	}
	current := dataPlaneVersion(existing)
	if current == "" {
		return false
	}
	target, err := operatorVersion(c)
	if err != nil {
		// Only this one switch depends on it: hold the update and keep
		// managing the rest of the spec.
		log.FromContext(c.Context()).Error(err, "Operator version discovery failed; holding the data-plane update", "dataPlane", current)
		return false
	}
	if !versionLessThan(current, target) {
		return false
	}
	return c.RequestMaintenance(dataPlaneTokenPrefix+target,
		fmt.Sprintf("Update the database management sidecars from %s to %s (causes a brief rolling restart)", current, target),
		controller.MaintenanceRollingRestart)
}

// versionLessThan reports a < b. Versions that do not parse are never less,
// so a malformed tag holds the update instead of churning.
func versionLessThan(a, b string) bool {
	va, errA := goversion.NewVersion(a)
	vb, errB := goversion.NewVersion(b)
	if errA != nil || errB != nil {
		return false
	}
	return va.LessThan(vb)
}

// HoldDataPlaneUpdates turns off updateStrategy.autoUpdateDataPlane on the
// MariaDB CRs of this provider's Instances. The chart's pre-upgrade hook runs it
// before Helm upgrades the operator: the new operator reconciles every
// MariaDB as soon as it starts, before the provider can hold an update, and
// provider releases before maintenance gating left the switch on. The next
// Sync takes the field back.
func HoldDataPlaneUpdates(ctx context.Context, cl client.Client) error {
	instances := &corev1alpha1.InstanceList{}
	if err := cl.List(ctx, instances); err != nil {
		return fmt.Errorf("list Instances: %w", err)
	}
	for i := range instances.Items {
		in := &instances.Items[i]
		if in.Spec.ProviderRef.Name != common.ProviderName {
			continue
		}
		mdb := &mariadbv1alpha1.MariaDB{}
		err := cl.Get(ctx, client.ObjectKey{Namespace: in.Namespace, Name: in.Name}, mdb)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("get MariaDB %s/%s: %w", in.Namespace, in.Name, err)
		}
		if !ptr.Deref(mdb.Spec.UpdateStrategy.AutoUpdateDataPlane, false) {
			continue
		}
		patch := client.RawPatch(client.Merge.Type(), []byte(`{"spec":{"updateStrategy":{"autoUpdateDataPlane":false}}}`))
		if err := cl.Patch(ctx, mdb, patch); err != nil {
			return fmt.Errorf("hold the data-plane update of MariaDB %s/%s: %w", in.Namespace, in.Name, err)
		}
	}
	return nil
}
