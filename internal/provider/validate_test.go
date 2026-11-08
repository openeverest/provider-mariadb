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
	"cmp"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller/validationtest"

	"github.com/openeverest/provider-mariadb/internal/common"
)

const (
	providerSpecPath   = "../../charts/provider-mariadb/generated/provider-spec.yaml"
	standaloneFixture  = "testdata/validate/standalone.yaml"
	galeraFixture      = "testdata/validate/galera.yaml"
	replicationFixture = "testdata/validate/replication.yaml"
)

// component applies mutate to the named component of the Instance.
func component(name string, mutate func(*corev1alpha1.ComponentSpec)) func(*corev1alpha1.Instance) {
	return func(in *corev1alpha1.Instance) {
		c := in.Spec.Components[name]
		mutate(&c)
		in.Spec.Components[name] = c
	}
}

// parameters sets the named component's parameters to raw.
func parameters(name, raw string) func(*corev1alpha1.Instance) {
	return component(name, func(c *corev1alpha1.ComponentSpec) {
		c.Parameters = &runtime.RawExtension{Raw: []byte(raw)}
	})
}

// replicas sets the engine's node count.
func replicas(n *int32) func(*corev1alpha1.Instance) {
	return component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) { c.Replicas = n })
}

// engineImage drops the topology type, so the engine's image is accepted, and sets the image.
func engineImage(image string) func(*corev1alpha1.Instance) {
	return func(in *corev1alpha1.Instance) {
		in.Spec.Topology = nil
		component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) { c.Image = image })(in)
	}
}

// engineStorage sets the engine's storage.
func engineStorage(size string, class *string) func(*corev1alpha1.Instance) {
	return component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) {
		c.Storage = &corev1alpha1.Storage{Size: resource.MustParse(size), StorageClass: class}
	})
}

// backupStorages enables backups to storages.
func backupStorages(storages ...corev1alpha1.InstanceBackupStorage) func(*corev1alpha1.Instance) {
	return func(in *corev1alpha1.Instance) {
		in.Spec.Backup = &corev1alpha1.InstanceBackupSpec{
			Enabled:  true,
			ClassRef: commonv1alpha1.ObjectRef{Name: "mariadb"},
			Storages: storages,
		}
	}
}

// TestValidate runs the checks the runtime runs before every Sync: the
// schemas, supportedFields and catalog the provider publishes, then Validate.
func TestValidate(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)
	noTopology := func(in *corev1alpha1.Instance) { in.Spec.Topology = nil }
	proxy := func(raw string) func(*corev1alpha1.Instance) {
		return func(in *corev1alpha1.Instance) {
			in.Spec.Components[common.ComponentProxy] = corev1alpha1.ComponentSpec{
				Parameters: &runtime.RawExtension{Raw: []byte(raw)},
			}
		}
	}
	podAffinity := &corev1.Affinity{PodAffinity: &corev1.PodAffinity{}}

	tests := []struct {
		name    string
		fixture string
		mutate  func(*corev1alpha1.Instance)
		want    []string
	}{
		{name: "valid standalone", fixture: standaloneFixture},
		{name: "valid galera with MaxScale", fixture: galeraFixture},
		{name: "valid replication with metrics", fixture: replicationFixture},
		{name: "no topology type", fixture: standaloneFixture, mutate: noTopology},
		{
			name:    "unknown topology",
			fixture: standaloneFixture,
			mutate:  func(in *corev1alpha1.Instance) { in.Spec.Topology.Type = "sharded" },
			want:    []string{"spec.topology.type"},
		},
		{
			name:    "missing engine",
			fixture: standaloneFixture,
			mutate:  func(in *corev1alpha1.Instance) { delete(in.Spec.Components, common.ComponentEngine) },
			want:    []string{"spec.components[engine]"},
		},
		{
			name:    "missing engine without a topology type",
			fixture: standaloneFixture,
			mutate: func(in *corev1alpha1.Instance) {
				noTopology(in)
				delete(in.Spec.Components, common.ComponentEngine)
			},
			want: []string{"spec.components[engine]"},
		},
		{name: "standalone replicas unset", fixture: standaloneFixture, mutate: replicas(nil)},
		{
			name:    "two standalone nodes",
			fixture: standaloneFixture,
			mutate:  replicas(ptr.To(int32(2))),
			want:    []string{"spec.components[engine].replicas"},
		},
		{
			name:    "three standalone nodes",
			fixture: standaloneFixture,
			mutate:  replicas(ptr.To(int32(3))),
			want:    []string{"spec.components[engine].replicas"},
		},
		{
			name:    "three nodes without a topology type",
			fixture: standaloneFixture,
			mutate:  func(in *corev1alpha1.Instance) { noTopology(in); replicas(ptr.To(int32(3)))(in) },
			want:    []string{"spec.components[engine].replicas"},
		},
		{
			name:    "zero standalone nodes",
			fixture: standaloneFixture,
			mutate:  replicas(ptr.To(int32(0))),
			want:    []string{"spec.components[engine].replicas"},
		},
		{name: "galera replicas unset", fixture: galeraFixture, mutate: replicas(nil)},
		{name: "five galera nodes", fixture: galeraFixture, mutate: replicas(ptr.To(int32(5)))},
		{
			name:    "one galera node",
			fixture: galeraFixture,
			mutate:  replicas(ptr.To(int32(1))),
			want:    []string{"spec.components[engine].replicas"},
		},
		{
			name:    "even number of galera nodes",
			fixture: galeraFixture,
			mutate:  replicas(ptr.To(int32(4))),
			want:    []string{"spec.components[engine].replicas"},
		},
		{name: "replication replicas unset", fixture: replicationFixture, mutate: replicas(nil)},
		{name: "two replication nodes", fixture: replicationFixture, mutate: replicas(ptr.To(int32(2)))},
		{name: "four replication nodes", fixture: replicationFixture, mutate: replicas(ptr.To(int32(4)))},
		{
			name:    "one replication node",
			fixture: replicationFixture,
			mutate:  replicas(ptr.To(int32(1))),
			want:    []string{"spec.components[engine].replicas"},
		},
		{
			name:    "engine storage too small",
			fixture: standaloneFixture,
			mutate:  engineStorage("512Mi", nil),
			want:    []string{"spec.components[engine].storage.size"},
		},
		{
			name:    "unsupported engine service type",
			fixture: standaloneFixture,
			mutate: component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) {
				c.Service = &corev1alpha1.Service{ServiceType: corev1.ServiceTypeExternalName}
			}),
			want: []string{"spec.components[engine].service.serviceType"},
		},
		{
			name:    "engine node affinity",
			fixture: galeraFixture,
			mutate: component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) {
				c.SchedulingPolicy = &commonv1alpha1.SchedulingPolicy{
					Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}},
				}
			}),
		},
		{
			name:    "engine pod anti-affinity",
			fixture: galeraFixture,
			mutate: component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) {
				c.SchedulingPolicy = &commonv1alpha1.SchedulingPolicy{
					Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}},
				}
			}),
		},
		{
			name:    "engine pod affinity",
			fixture: galeraFixture,
			mutate: component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) {
				c.SchedulingPolicy = &commonv1alpha1.SchedulingPolicy{Affinity: podAffinity}
			}),
			want: []string{"spec.components[engine].schedulingPolicy.affinity.podAffinity"},
		},
		{
			name:    "proxy pod affinity",
			fixture: galeraFixture,
			mutate: component(common.ComponentProxy, func(c *corev1alpha1.ComponentSpec) {
				c.SchedulingPolicy = &commonv1alpha1.SchedulingPolicy{Affinity: podAffinity}
			}),
			want: []string{"spec.components[proxy].schedulingPolicy.affinity.podAffinity"},
		},
		{
			name:    "proxy disabled without a topology type",
			fixture: standaloneFixture,
			mutate:  func(in *corev1alpha1.Instance) { noTopology(in); proxy(`{"enabled":false}`)(in) },
		},
		{
			name:    "proxy enabled without a topology type",
			fixture: standaloneFixture,
			mutate:  func(in *corev1alpha1.Instance) { noTopology(in); proxy(`{"enabled":true}`)(in) },
			want:    []string{"spec.components[proxy].parameters.enabled"},
		},
		{
			name:    "proxy on the standalone topology",
			fixture: standaloneFixture,
			mutate:  proxy(`{"enabled":true}`),
			want:    []string{"spec.components[proxy]"},
		},
		{name: "proxy enabled on replication", fixture: replicationFixture, mutate: proxy(`{"enabled":true}`)},
		{name: "proxy enabled from the UI", fixture: galeraFixture, mutate: parameters(common.ComponentProxy, `{"enabled":"true"}`)},
		{
			name:    "zero proxy replicas",
			fixture: galeraFixture,
			mutate:  component(common.ComponentProxy, func(c *corev1alpha1.ComponentSpec) { c.Replicas = ptr.To(int32(0)) }),
			want:    []string{"spec.components[proxy].replicas"},
		},
		{
			name:    "malformed proxy parameters",
			fixture: galeraFixture,
			mutate:  parameters(common.ComponentProxy, `{"enabled":"maybe"}`),
			want:    []string{"spec.components[proxy].parameters.enabled"},
		},
		{
			name:    "unsupported proxy service type",
			fixture: galeraFixture,
			mutate: component(common.ComponentProxy, func(c *corev1alpha1.ComponentSpec) {
				c.Service = &corev1alpha1.Service{ServiceType: corev1.ServiceTypeExternalName}
			}),
			want: []string{"spec.components[proxy].service.serviceType"},
		},
		{
			name:    "VictoriaMetrics with metrics",
			fixture: replicationFixture,
			mutate:  parameters(common.ComponentMonitoring, `{"enabled":true,"victoriaMetrics":{"enabled":true}}`),
		},
		{
			name:    "metrics toggled from the UI",
			fixture: replicationFixture,
			mutate:  parameters(common.ComponentMonitoring, `{"enabled":"true","victoriaMetrics":{"enabled":"false"}}`),
		},
		{
			name:    "VictoriaMetrics without metrics",
			fixture: replicationFixture,
			mutate:  parameters(common.ComponentMonitoring, `{"victoriaMetrics":{"enabled":true}}`),
			want:    []string{"spec.components[monitoring].parameters.victoriaMetrics.enabled"},
		},
		{
			name:    "monitoring without parameters",
			fixture: replicationFixture,
			mutate:  component(common.ComponentMonitoring, func(c *corev1alpha1.ComponentSpec) { c.Parameters = nil }),
			want:    []string{"spec.components[monitoring].parameters"},
		},
		{
			name:    "TLS required",
			fixture: standaloneFixture,
			mutate:  parameters(common.ComponentEngine, `{"tls":{"required":true}}`),
		},
		{
			name:    "TLS required while disabled",
			fixture: standaloneFixture,
			mutate:  parameters(common.ComponentEngine, `{"tls":{"enabled":false,"required":true}}`),
			want:    []string{"spec.components[engine].parameters.tls.required"},
		},
		{
			name:    "TLS disabled on galera",
			fixture: galeraFixture,
			mutate:  parameters(common.ComponentEngine, `{"tls":{"enabled":false}}`),
		},
		{
			name:    "TLS settings from the UI",
			fixture: galeraFixture,
			mutate:  parameters(common.ComponentEngine, `{"tls":{"enabled":"true","required":"false","galeraSSTEnabled":"true"}}`),
		},
		{
			name:    "SST encryption ignored on standalone",
			fixture: standaloneFixture,
			mutate:  parameters(common.ComponentEngine, `{"tls":{"galeraSSTEnabled":true}}`),
		},
		{
			name:    "SST encryption ignored on replication",
			fixture: replicationFixture,
			mutate:  parameters(common.ComponentEngine, `{"tls":{"galeraSSTEnabled":true}}`),
		},
		{
			name:    "SST encryption while TLS is disabled",
			fixture: galeraFixture,
			mutate:  parameters(common.ComponentEngine, `{"tls":{"enabled":false,"galeraSSTEnabled":true}}`),
			want:    []string{"spec.components[engine].parameters.tls.galeraSSTEnabled"},
		},
		{
			name:    "PITR on replication with a physical schedule",
			fixture: replicationFixture,
			mutate:  backupStorages(pitrStorage("s3", true, physicalSchedule("daily"))),
		},
		{
			name:    "PITR on standalone",
			fixture: standaloneFixture,
			mutate:  backupStorages(pitrStorage("s3", true, physicalSchedule("daily"))),
			want:    []string{"spec.backup.storages[0].pitr.enabled"},
		},
		{
			name:    "PITR without a physical schedule",
			fixture: replicationFixture,
			mutate:  backupStorages(pitrStorage("s3", true, logicalSchedule("daily"))),
			want:    []string{"spec.backup.storages[0].schedules"},
		},
		{
			name:    "PITR on two storages",
			fixture: replicationFixture,
			mutate: backupStorages(
				pitrStorage("s1", true, physicalSchedule("daily")),
				pitrStorage("s2", true, physicalSchedule("hourly")),
			),
			want: []string{"spec.backup.storages"},
		},
		{
			name:    "PITR disabled on standalone",
			fixture: standaloneFixture,
			mutate:  backupStorages(pitrStorage("s3", false)),
		},
		{
			name:    "point-in-time recovery into standalone",
			fixture: standaloneFixture,
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.DataSource = instanceSeededFromPITR(backupv1alpha1.RecoveryTargetLatest, nil).Spec.DataSource
			},
			want: []string{"spec.dataSource.pointInTime"},
		},
		{
			name:    "point-in-time recovery into replication",
			fixture: replicationFixture,
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.DataSource = instanceSeededFromPITR(backupv1alpha1.RecoveryTargetLatest, nil).Spec.DataSource
			},
		},
		{
			name:    "standalone already recovered from a point in time",
			fixture: standaloneFixture,
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.DataSource = instanceSeededFromPITR(backupv1alpha1.RecoveryTargetLatest, nil).Spec.DataSource
				in.Status.Phase = corev1alpha1.InstancePhaseReady
			},
		},
		{
			name:    "standalone restored from a backup",
			fixture: standaloneFixture,
			mutate:  func(in *corev1alpha1.Instance) { in.Spec.DataSource = instanceSeededFrom("backup-1").Spec.DataSource },
		},
		{
			name:    "old engine at the minimum size",
			fixture: standaloneFixture,
			mutate: func(in *corev1alpha1.Instance) {
				engineImage("mariadb:10.11")(in)
				engineStorage("1Gi", nil)(in)
			},
		},
		{name: "four replication nodes without a storage", fixture: replicationFixture, mutate: replicas(ptr.To(int32(4)))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := validationtest.Load(t, tt.fixture)
			if tt.mutate != nil {
				tt.mutate(in)
			}

			res := v.Run(in, nil)

			var got []string
			for _, err := range res.Errors {
				got = append(got, err.Field)
			}
			assert.Equal(t, tt.want, got, "errors: %v", res.Errors)
		})
	}
}

// TestValidateEngineChange runs the pipeline as admission does on an update,
// with the stored Instance as the previous one.
func TestValidateEngineChange(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)
	version := func(name string) func(*corev1alpha1.Instance) {
		return func(in *corev1alpha1.Instance) { in.Spec.Version = name }
	}

	tests := []struct {
		name    string
		fixture string
		before  func(*corev1alpha1.Instance)
		phase   corev1alpha1.InstancePhase
		update  string
		after   func(*corev1alpha1.Instance)
		want    []string
	}{
		{
			name:    "minor downgrade",
			fixture: standaloneFixture,
			before:  engineImage("mariadb:12.3"),
			after:   engineImage("mariadb:11.8"),
			want:    []string{"spec.components[engine].image"},
		},
		{
			name:    "upgrade",
			fixture: standaloneFixture,
			before:  engineImage("mariadb:11.8"),
			after:   engineImage("mariadb:12.3"),
		},
		{
			name:    "patch-level change",
			fixture: standaloneFixture,
			before:  engineImage("mariadb:11.4.5"),
			after:   engineImage("mariadb:11.4.4"),
		},
		{
			name:    "unparsable tag",
			fixture: standaloneFixture,
			before:  engineImage("mariadb:12.3"),
			after:   engineImage("mariadb:lts"),
		},
		{
			name:    "downgrade via spec.version",
			fixture: galeraFixture,
			before:  version("12.3"),
			after:   version("11.8"),
			want:    []string{"spec.version"},
		},
		{name: "upgrade via spec.version", fixture: galeraFixture, before: version("11.8"), after: version("12.3")},
		{
			name:    "status.version older than the default",
			fixture: galeraFixture,
			before:  func(in *corev1alpha1.Instance) { in.Status.Version = "11.4" },
		},
		{
			name:    "storage shrink",
			fixture: standaloneFixture,
			after:   engineStorage("5Gi", nil),
			want:    []string{"spec.components[engine].storage.size"},
		},
		{
			name:    "omitted size shrinking to the default",
			fixture: standaloneFixture,
			before:  engineStorage("20Gi", nil),
			after:   component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) { c.Storage = nil }),
			want:    []string{"spec.components[engine].storage.size"},
		},
		{name: "storage expansion", fixture: standaloneFixture, after: engineStorage("20Gi", nil)},
		{
			name:    "storage class change",
			fixture: standaloneFixture,
			before:  engineStorage("10Gi", ptr.To("standard")),
			after:   engineStorage("10Gi", ptr.To("fast")),
			want:    []string{"spec.components[engine].storage.storageClass"},
		},
		{
			name:    "same storage class",
			fixture: standaloneFixture,
			before:  engineStorage("10Gi", ptr.To("standard")),
			after:   engineStorage("10Gi", ptr.To("standard")),
		},
		{
			name:    "class on a default-class instance",
			fixture: standaloneFixture,
			after:   engineStorage("10Gi", ptr.To("fast")),
		},
		{name: "galera stays galera", fixture: galeraFixture},
		{
			name:    "standalone stays standalone",
			fixture: standaloneFixture,
			before:  func(in *corev1alpha1.Instance) { in.Spec.Topology = nil },
			after:   func(in *corev1alpha1.Instance) { in.Spec.Topology = nil },
		},
		{
			name:    "standalone to galera",
			fixture: standaloneFixture,
			update:  galeraFixture,
			want:    []string{"spec.topology.type"},
		},
		{
			name:    "galera to standalone",
			fixture: galeraFixture,
			update:  standaloneFixture,
			after:   func(in *corev1alpha1.Instance) { in.Spec.Topology = nil },
			want:    []string{"spec.topology.type"},
		},
		{name: "replication stays replication", fixture: replicationFixture},
		{
			name:    "standalone to replication",
			fixture: standaloneFixture,
			update:  replicationFixture,
			want:    []string{"spec.topology.type"},
		},
		{
			name:    "galera to replication",
			fixture: galeraFixture,
			update:  replicationFixture,
			want:    []string{"spec.topology.type"},
		},
		{
			name:    "replication to galera",
			fixture: replicationFixture,
			update:  galeraFixture,
			want:    []string{"spec.topology.type"},
		},
		{
			name:    "replication scale-out without a storage",
			fixture: replicationFixture,
			after:   replicas(ptr.To(int32(4))),
			want:    []string{"spec.components[engine].replicas"},
		},
		{
			name:    "replication scale-out with a storage",
			fixture: replicationFixture,
			after: func(in *corev1alpha1.Instance) {
				replicas(ptr.To(int32(4)))(in)
				backupStorages(pitrStorage("s3", false))(in)
			},
		},
		{name: "replication scale-in", fixture: replicationFixture, after: replicas(ptr.To(int32(2)))},
		{
			name:    "topology changed before the cluster is created",
			fixture: standaloneFixture,
			phase:   corev1alpha1.InstancePhasePending,
			update:  galeraFixture,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			old := validationtest.Load(t, tt.fixture)
			if tt.before != nil {
				tt.before(old)
			}
			old.Status.Phase = cmp.Or(tt.phase, corev1alpha1.InstancePhaseReady)
			in := validationtest.Load(t, cmp.Or(tt.update, tt.fixture))
			if tt.after != nil {
				tt.after(in)
			}
			// A spec update keeps the stored status.
			in.Status = old.Status

			res := v.Run(in, old)

			var got []string
			for _, err := range res.Errors {
				got = append(got, err.Field)
			}
			assert.Equal(t, tt.want, got, "errors: %v", res.Errors)
		})
	}
}
