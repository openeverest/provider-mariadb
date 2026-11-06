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
	"regexp"
	"slices"
	"strconv"
	"strings"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/definition"
	"github.com/openeverest/provider-mariadb/internal/common"
)

var (
	componentsPath = field.NewPath("spec", "components")
	enginePath     = componentsPath.Key(common.ComponentEngine)
	proxyPath      = componentsPath.Key(common.ComponentProxy)
	monitoringPath = componentsPath.Key(common.ComponentMonitoring)

	minStorageSize = resource.MustParse("1Gi")

	// supportedServiceTypes are the Service types the operator can expose.
	supportedServiceTypes = []corev1.ServiceType{
		corev1.ServiceTypeClusterIP,
		corev1.ServiceTypeLoadBalancer,
		corev1.ServiceTypeNodePort,
	}
)

// Validate checks the rules about the Instance alone. The runtime runs it once
// the Instance matches the topologies, components, versions and parameter
// schemas the Provider declares.
func (p *MariaDBProvider) Validate(in controller.ValidationInput) controller.ValidationResult {
	spec := &in.Provider.Spec
	instance := in.Instance

	errs := validateEngine(instance)
	errs = append(errs, validateProxy(spec, instance)...)
	errs = append(errs, validateMonitoring(spec, instance)...)
	errs = append(errs, validateTLS(instance)...)
	errs = append(errs, validatePITR(instance)...)
	errs = append(errs, validateDataSource(instance)...)
	if _, ok := instance.Spec.Components[common.ComponentEngine]; ok && provisioned(in.Old) {
		errs = append(errs, validateEngineChange(spec, instance, instanceEngineState(spec, in.Old))...)
	}

	return controller.ValidationResult{Errors: errs}
}

// ValidateReferences checks the VMServiceScrape CRD and the update against the deployed MariaDB.
func (p *MariaDBProvider) ValidateReferences(c *controller.Context) (field.ErrorList, error) {
	errs, err := validateVMServiceScrapeCRD(c)
	if err != nil {
		return nil, err
	}

	mdb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(mdb, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return errs, nil
		}
		return nil, fmt.Errorf("get MariaDB: %w", err)
	}
	spec, err := c.ProviderSpec()
	if err != nil {
		return nil, fmt.Errorf("get provider spec: %w", err)
	}

	return append(errs, validateEngineChange(spec, c.Instance(), mariaDBEngineState(mdb))...), nil
}

// validateEngine checks the engine's node count, storage size, Service and scheduling.
func validateEngine(in *corev1alpha1.Instance) field.ErrorList {
	engine, ok := in.Spec.Components[common.ComponentEngine]
	if !ok {
		return field.ErrorList{field.Required(enginePath, "")}
	}

	errs := validateEngineReplicas(in)
	if engine.Storage != nil && !engine.Storage.Size.IsZero() && engine.Storage.Size.Cmp(minStorageSize) < 0 {
		errs = append(errs, field.Invalid(enginePath.Child("storage", "size"), engine.Storage.Size.String(),
			"must be at least "+minStorageSize.String(),
		))
	}
	return append(errs, validateServiceAndScheduling(enginePath, engine)...)
}

// validateEngineReplicas checks the engine's node count against its topology.
func validateEngineReplicas(in *corev1alpha1.Instance) field.ErrorList {
	path := enginePath.Child("replicas")
	replicas := engineReplicas(in)

	switch normalizeTopology(in.GetTopologyType()) {
	case definition.TopologyTypeStandalone:
		if replicas != standaloneDefaultReplicas {
			return field.ErrorList{field.Invalid(path, replicas,
				"the standalone topology runs a single node; use the galera or replication topology for more",
			)}
		}
	case definition.TopologyTypeGalera:
		if replicas < 3 || replicas%2 == 0 {
			return field.ErrorList{field.Invalid(path, replicas,
				"the galera topology needs an odd number of nodes, at least 3, to avoid split-brain",
			)}
		}
	case definition.TopologyTypeReplication:
		if replicas < 2 {
			return field.ErrorList{field.Invalid(path, replicas,
				"the replication topology needs at least 2 nodes: a primary and a replica",
			)}
		}
	}
	return nil
}

// validateServiceAndScheduling checks a component's Service type and affinity against the operator's.
func validateServiceAndScheduling(path *field.Path, component corev1alpha1.ComponentSpec) field.ErrorList {
	var errs field.ErrorList
	if svc := component.Service; svc != nil && svc.ServiceType != "" && !slices.Contains(supportedServiceTypes, svc.ServiceType) {
		errs = append(errs, field.NotSupported(path.Child("service", "serviceType"), svc.ServiceType, supportedServiceTypes))
	}
	if policy := component.SchedulingPolicy; policy != nil && policy.Affinity != nil && policy.Affinity.PodAffinity != nil {
		errs = append(errs, field.Forbidden(path.Child("schedulingPolicy", "affinity", "podAffinity"),
			"the MariaDB operator supports nodeAffinity and podAntiAffinity only",
		))
	}
	return errs
}

// validateProxy checks MaxScale when the proxy enables it.
func validateProxy(spec *corev1alpha1.ProviderSpec, in *corev1alpha1.Instance) field.ErrorList {
	enabled, err := isProxyEnabled(in)
	if err != nil {
		return field.ErrorList{field.Invalid(proxyPath.Child("parameters"), field.OmitValueType{}, err.Error())}
	}
	if !enabled {
		return nil
	}

	var errs field.ErrorList
	switch normalizeTopology(in.GetTopologyType()) {
	case definition.TopologyTypeGalera, definition.TopologyTypeReplication:
	default:
		errs = append(errs, field.Forbidden(proxyPath.Child("parameters", "enabled"),
			"MaxScale is only supported on the galera and replication topologies",
		))
	}
	proxy := in.Spec.Components[common.ComponentProxy]
	if proxy.Replicas != nil && *proxy.Replicas < 1 {
		errs = append(errs, field.Invalid(proxyPath.Child("replicas"), *proxy.Replicas, "must be at least 1"))
	}
	errs = append(errs, validateServiceAndScheduling(proxyPath, proxy)...)
	return append(errs, validateImage(spec, in, proxyPath, common.ComponentProxy)...)
}

// validateMonitoring checks the metrics exporter parameters and image.
func validateMonitoring(spec *corev1alpha1.ProviderSpec, in *corev1alpha1.Instance) field.ErrorList {
	monitoring, ok := in.Spec.Components[common.ComponentMonitoring]
	if !ok {
		return nil
	}
	if monitoring.Parameters == nil || monitoring.Parameters.Raw == nil {
		return field.ErrorList{field.Required(monitoringPath.Child("parameters"), "")}
	}
	params, err := monitoringParams(in)
	if err != nil {
		return field.ErrorList{field.Invalid(monitoringPath.Child("parameters"), field.OmitValueType{}, err.Error())}
	}
	if !params.Enabled {
		if params.VictoriaMetrics.Enabled {
			return field.ErrorList{field.Invalid(monitoringPath.Child("parameters", "victoriaMetrics", "enabled"), true,
				"requires spec.components[monitoring].parameters.enabled",
			)}
		}
		return nil
	}
	return validateImage(spec, in, monitoringPath, common.ComponentMonitoring)
}

// validateVMServiceScrapeCRD checks the cluster serves the CRD VictoriaMetrics scraping needs.
func validateVMServiceScrapeCRD(c *controller.Context) (field.ErrorList, error) {
	params, err := monitoringParams(c.Instance())
	if err != nil {
		return nil, err
	}
	if !isVictoriaMetricsEnabled(params) {
		return nil, nil
	}
	installed, err := vmServiceScrapeCRDInstalled(c)
	if err != nil || installed {
		return nil, err
	}
	return field.ErrorList{field.Invalid(monitoringPath.Child("parameters", "victoriaMetrics", "enabled"), true,
		"the VMServiceScrape CRD (operator.victoriametrics.com) is not installed",
	)}, nil
}

// validateImage checks that the component's image resolves.
func validateImage(spec *corev1alpha1.ProviderSpec, in *corev1alpha1.Instance, path *field.Path, name string) field.ErrorList {
	if componentImage(spec, in, name) != "" {
		return nil
	}
	return field.ErrorList{field.Required(path.Child("image"),
		fmt.Sprintf("the provider's catalog has no default %s image", controller.GetComponentType(spec, name)),
	)}
}

// validateTLS rejects TLS enforcement or Galera SST encryption while TLS is disabled.
func validateTLS(in *corev1alpha1.Instance) field.ErrorList {
	settings, err := resolveTLSSettings(in)
	if err != nil {
		return field.ErrorList{field.Invalid(enginePath.Child("parameters"), field.OmitValueType{}, err.Error())}
	}
	if settings.Enabled {
		return nil
	}

	path := enginePath.Child("parameters", "tls")
	var errs field.ErrorList
	if settings.Required {
		errs = append(errs, field.Invalid(path.Child("required"), true, "must be false while TLS is disabled"))
	}
	if settings.GaleraSSTEnabled {
		errs = append(errs, field.Invalid(path.Child("galeraSSTEnabled"), true, "must be false while TLS is disabled"))
	}
	return errs
}

// validatePITR checks that binary logs of a replication primary go to one storage with a physical base backup.
func validatePITR(in *corev1alpha1.Instance) field.ErrorList {
	backup := in.Spec.Backup
	if backup == nil || !backup.Enabled {
		return nil
	}

	path := field.NewPath("spec", "backup", "storages")
	replication := normalizeTopology(in.GetTopologyType()) == definition.TopologyTypeReplication
	var errs field.ErrorList
	var enabled []string
	for i := range backup.Storages {
		storage := &backup.Storages[i]
		if storage.PITR == nil || !storage.PITR.Enabled {
			continue
		}
		enabled = append(enabled, storage.StorageRef.Name)
		if !replication {
			errs = append(errs, field.Forbidden(path.Index(i).Child("pitr", "enabled"),
				"point-in-time recovery is only supported on the replication topology",
			))
		}
		if _, err := firstPhysicalSchedule(storage); err != nil {
			errs = append(errs, field.Invalid(path.Index(i).Child("schedules"), field.OmitValueType{}, err.Error()))
		}
	}
	if len(enabled) > 1 {
		errs = append(errs, field.Invalid(path, field.OmitValueType{}, fmt.Sprintf(
			"point-in-time recovery can be enabled on at most one storage, got %d: %s",
			len(enabled), strings.Join(enabled, ", "),
		)))
	}
	return errs
}

// validateDataSource rejects a point-in-time recovery the operator would stop at the base backup.
func validateDataSource(in *corev1alpha1.Instance) field.ErrorList {
	ds := in.Spec.DataSource
	if ds == nil || ds.Type != backupv1alpha1.DataSourceTypePointInTime || provisioned(in) ||
		normalizeTopology(in.GetTopologyType()) == definition.TopologyTypeReplication {
		return nil
	}
	return field.ErrorList{field.Forbidden(field.NewPath("spec", "dataSource", "pointInTime"),
		"point-in-time recovery is only supported into the replication topology",
	)}
}

// provisioned reports whether the Instance got past validation, so its engine may exist.
func provisioned(in *corev1alpha1.Instance) bool {
	return in != nil && in.Status.Phase != "" && in.Status.Phase != corev1alpha1.InstancePhasePending
}

// engineState is what an engine runs, to check an update against.
type engineState struct {
	topology     definition.TopologyType
	image        string
	size         *resource.Quantity
	storageClass string
	replicas     int32
}

// instanceEngineState is the engine an Instance deploys.
func instanceEngineState(spec *corev1alpha1.ProviderSpec, in *corev1alpha1.Instance) engineState {
	engine := in.Spec.Components[common.ComponentEngine]
	size := engineStorageSize(engine)
	state := engineState{
		topology: normalizeTopology(in.GetTopologyType()),
		image:    componentImage(spec, in, common.ComponentEngine),
		size:     &size,
		replicas: engineReplicas(in),
	}
	if engine.Storage != nil && engine.Storage.StorageClass != nil {
		state.storageClass = *engine.Storage.StorageClass
	}
	return state
}

// mariaDBEngineState is the engine a MariaDB runs.
func mariaDBEngineState(mdb *mariadbv1alpha1.MariaDB) engineState {
	return engineState{
		topology:     existingTopology(mdb),
		image:        mdb.Spec.Image,
		size:         mdb.Spec.Storage.Size,
		storageClass: mdb.Spec.Storage.StorageClassName,
		replicas:     mdb.Spec.Replicas,
	}
}

// validateEngineChange rejects the engine changes the running cluster cannot apply.
func validateEngineChange(spec *corev1alpha1.ProviderSpec, in *corev1alpha1.Instance, current engineState) field.ErrorList {
	desired := instanceEngineState(spec, in)
	if desired.topology != current.topology {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "topology", "type"), in.GetTopologyType(),
			fmt.Sprintf("cannot change on an existing instance, which runs the %s topology", current.topology),
		)}
	}

	var errs field.ErrorList
	if from, ok := imageMinorVersion(current.image); ok {
		if to, ok := imageMinorVersion(desired.image); ok && (to[0] < from[0] || (to[0] == from[0] && to[1] < from[1])) {
			errs = append(errs, field.Forbidden(engineVersionPath(in), fmt.Sprintf(
				"downgrading MariaDB from %d.%d to %d.%d is not supported; restore a backup into a new Instance instead",
				from[0], from[1], to[0], to[1],
			)))
		}
	}
	if current.size != nil && desired.size.Cmp(*current.size) < 0 {
		errs = append(errs, field.Invalid(enginePath.Child("storage", "size"), desired.size.String(),
			"cannot be decreased from "+current.size.String(),
		))
	}
	if current.storageClass != "" && desired.storageClass != "" && desired.storageClass != current.storageClass {
		errs = append(errs, field.Invalid(enginePath.Child("storage", "storageClass"), desired.storageClass,
			fmt.Sprintf("cannot change on an existing instance, which uses %q", current.storageClass),
		))
	}
	if desired.topology == definition.TopologyTypeReplication && desired.replicas > current.replicas &&
		replicaTemplateStorage(in) == nil {
		errs = append(errs, field.Forbidden(enginePath.Child("replicas"),
			"adding replicas needs a backup storage in spec.backup: new replicas are seeded from a physical backup staged there",
		))
	}
	return errs
}

// engineVersionPath is the field that selects the engine's version.
func engineVersionPath(in *corev1alpha1.Instance) *field.Path {
	engine := in.Spec.Components[common.ComponentEngine]
	switch {
	case engine.Image != "":
		return enginePath.Child("image")
	case engine.Version != "":
		return enginePath.Child("version")
	default:
		return field.NewPath("spec", "version")
	}
}

// engineReplicas is the engine's node count, or its topology's default.
func engineReplicas(in *corev1alpha1.Instance) int32 {
	if replicas := in.Spec.Components[common.ComponentEngine].Replicas; replicas != nil {
		return *replicas
	}
	return topologyDefaultReplicas(in.GetTopologyType())
}

// componentImage is the image Sync deploys for the named component once the runtime resolves its version bundle.
func componentImage(spec *corev1alpha1.ProviderSpec, in *corev1alpha1.Instance, name string) string {
	component := in.Spec.Components[name]
	if component.Image != "" {
		return component.Image
	}
	version := component.Version
	if version == "" {
		if bundle, err := controller.ResolveVersionBundle(spec, controller.EffectiveVersionBundleName(spec, in)); err == nil {
			version = bundle.Components[name]
		}
	}
	if image := controller.GetImageForVersion(spec, name, version); image != "" {
		return image
	}
	return controller.GetDefaultImageForComponent(spec, name)
}

var minorVersionRegexp = regexp.MustCompile(`^(\d+)\.(\d+)`)

// imageMinorVersion extracts the major and minor version from an image tag,
// e.g. "mariadb:11.4.5-ubi9" -> [11, 4].
func imageMinorVersion(image string) ([2]int, bool) {
	image, _, _ = strings.Cut(image, "@")
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i:], "/") {
		return [2]int{}, false
	}
	m := minorVersionRegexp.FindStringSubmatch(image[i+1:])
	if m == nil {
		return [2]int{}, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return [2]int{major, minor}, true
}

// normalizeTopology maps an empty topology to the standalone default.
func normalizeTopology(topology string) definition.TopologyType {
	if topology == "" {
		return definition.TopologyTypeStandalone
	}
	return definition.TopologyType(topology)
}

// existingTopology derives the topology of a provisioned MariaDB CR from its HA state.
func existingTopology(mdb *mariadbv1alpha1.MariaDB) definition.TopologyType {
	switch {
	case mdb.IsGaleraEnabled():
		return definition.TopologyTypeGalera
	case mdb.IsReplicationEnabled():
		return definition.TopologyTypeReplication
	default:
		return definition.TopologyTypeStandalone
	}
}
