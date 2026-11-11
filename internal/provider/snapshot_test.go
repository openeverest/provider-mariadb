package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller/validationtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openeverest/provider-mariadb/internal/common"
)

const snapshotParams = `{"bootstrap":{"volumeSnapshot":{"name":"snap"}}}`

func withSnapshot(raw string) func(*corev1alpha1.Instance) {
	return func(in *corev1alpha1.Instance) {
		engine := in.Spec.Components[common.ComponentEngine]
		engine.Parameters = &runtime.RawExtension{Raw: []byte(raw)}
		in.Spec.Components[common.ComponentEngine] = engine
	}
}

func TestValidateVolumeSnapshotBootstrap(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)
	ready := func(in *corev1alpha1.Instance) { in.Status.Phase = corev1alpha1.InstancePhaseReady }

	tests := []struct {
		name string
		old  []func(*corev1alpha1.Instance)
		in   []func(*corev1alpha1.Instance)
		want []string
	}{
		{name: "from a snapshot", in: []func(*corev1alpha1.Instance){withSnapshot(snapshotParams)}},
		{
			name: "invalid name",
			in:   []func(*corev1alpha1.Instance){withSnapshot(`{"bootstrap":{"volumeSnapshot":{"name":"Not_A_Name"}}}`)},
			want: []string{"spec.components[engine].parameters.bootstrap.volumeSnapshot.name"},
		},
		{
			name: "kept",
			old:  []func(*corev1alpha1.Instance){withSnapshot(snapshotParams), ready},
			in:   []func(*corev1alpha1.Instance){withSnapshot(snapshotParams)},
		},
		{
			name: "changed",
			old:  []func(*corev1alpha1.Instance){withSnapshot(snapshotParams), ready},
			in:   []func(*corev1alpha1.Instance){withSnapshot(`{"bootstrap":{"volumeSnapshot":{"name":"other"}}}`)},
			want: []string{"spec.components[engine].parameters.bootstrap.volumeSnapshot"},
		},
		{
			name: "added after creation",
			old:  []func(*corev1alpha1.Instance){ready},
			in:   []func(*corev1alpha1.Instance){withSnapshot(snapshotParams)},
			want: []string{"spec.components[engine].parameters.bootstrap.volumeSnapshot"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var old *corev1alpha1.Instance
			if tt.old != nil {
				old = validationtest.Load(t, standaloneFixture)
				for _, f := range tt.old {
					f(old)
				}
			}
			in := validationtest.Load(t, standaloneFixture)
			for _, f := range tt.in {
				f(in)
			}
			var got []string
			for _, err := range v.Run(in, old).Errors {
				got = append(got, err.Field)
			}
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("with a data source", func(t *testing.T) {
		in := validationtest.Load(t, standaloneFixture)
		withSnapshot(snapshotParams)(in)
		in.Spec.DataSource = &backupv1alpha1.DataSource{Type: backupv1alpha1.DataSourceTypeBackup}
		errs := validateVolumeSnapshotBootstrap(in, nil)
		require.Len(t, errs, 1)
		assert.Contains(t, errs[0].Error(), "spec.dataSource")
	})
}

// snapshotContext is a Context for Instance "db", with a scheme that knows VolumeSnapshots.
func snapshotContext(t *testing.T, params string, objs ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	scheme := newBackupTestScheme(t)
	scheme.AddKnownTypeWithName(volumeSnapshotGVK, &unstructured.Unstructured{})
	instance := testInstance()
	instance.UID = "db-uid"
	instance.Spec.Components = map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: {}}
	if params != "" {
		withSnapshot(params)(instance)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{instance}, objs...)...).Build()
	return controller.NewContext(context.Background(), cl, instance, common.ProviderName), cl
}

func volumeSnapshot(name, pvc, authorizedFor string) *unstructured.Unstructured {
	snap := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"source": map[string]any{"persistentVolumeClaimName": pvc}},
	}}
	snap.SetGroupVersionKind(volumeSnapshotGVK)
	snap.SetNamespace("ns")
	snap.SetName(name)
	if authorizedFor != "" {
		snap.SetAnnotations(map[string]string{BootstrapInstanceAnnotation: authorizedFor})
	}
	return snap
}

func sourcePVC(name, instance string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: name, Labels: map[string]string{instanceLabelKey: instance},
	}}
}

func startedFrom(snapshot string) *mariadbv1alpha1.MariaDB {
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "db"}}
	if snapshot != "" {
		mdb.Spec.BootstrapFrom = &mariadbv1alpha1.BootstrapFrom{VolumeSnapshotRef: &mariadbv1alpha1.LocalObjectReference{Name: snapshot}}
	}
	return mdb
}

func TestValidateVolumeSnapshotReference(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		objs    []client.Object
		mdb     *mariadbv1alpha1.MariaDB
		wantErr string
	}{
		{name: "no snapshot", params: ""},
		{name: "authorized", params: snapshotParams, objs: []client.Object{volumeSnapshot("snap", "storage-src-1", "db")}},
		{name: "missing", params: snapshotParams, wantErr: `VolumeSnapshot "snap" not found`},
		{name: "not annotated", params: snapshotParams, objs: []client.Object{volumeSnapshot("snap", "storage-src-1", "")}, wantErr: "isn't authorized"},
		{name: "authorized for another instance", params: snapshotParams, objs: []client.Object{volumeSnapshot("snap", "storage-src-1", "other")}, wantErr: "isn't authorized"},
		// Only read at creation: once the MariaDB exists, the snapshot may be gone.
		{name: "started from it", params: snapshotParams, mdb: startedFrom("snap")},
		{name: "started from another", params: snapshotParams, mdb: startedFrom("other"), wantErr: "immutable"},
		{name: "added to an existing instance", params: snapshotParams, mdb: startedFrom(""), wantErr: "immutable"},
		{name: "existing instance without one", mdb: startedFrom("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := snapshotContext(t, tt.params, tt.objs...)
			errs, err := validateVolumeSnapshotReference(c, tt.mdb)
			require.NoError(t, err)
			if tt.wantErr == "" {
				assert.Empty(t, errs)
				return
			}
			require.Len(t, errs, 1)
			assert.Contains(t, errs[0].Error(), tt.wantErr)
		})
	}
}

func TestResolveVolumeSnapshotBootstrap(t *testing.T) {
	srcRoot := credentialSecret(rootSecretName("src"), rootPasswordSecretKey, "src-root")
	srcUser := credentialSecret(userSecretName("src"), userPasswordSecretKey, "src-user")

	t.Run("starts from the snapshot with the source's root password, to rotate", func(t *testing.T) {
		c, _ := snapshotContext(t, snapshotParams, volumeSnapshot("snap", "storage-src-1", "db"), sourcePVC("storage-src-1", "src"), srcRoot, srcUser)
		got, err := resolveVolumeSnapshotBootstrap(c, "snap")
		require.NoError(t, err)
		require.NotNil(t, got.VolumeSnapshotRef)
		assert.Equal(t, "snap", got.VolumeSnapshotRef.Name)

		root := &corev1.Secret{}
		require.NoError(t, c.Get(root, rootSecretName("db")))
		assert.Equal(t, []byte("src-root"), root.Data[rootPasswordSecretKey])
		assert.Equal(t, "true", root.Annotations[rotateRootAnnotation])
		require.Len(t, root.OwnerReferences, 1)
		assert.Equal(t, "db", root.OwnerReferences[0].Name)
		// The user password is the new instance's own.
		assert.Error(t, c.Get(&corev1.Secret{}, userSecretName("db")))
	})

	t.Run("refuses a snapshot not authorized for the instance", func(t *testing.T) {
		c, _ := snapshotContext(t, snapshotParams, volumeSnapshot("snap", "storage-src-1", "other"), sourcePVC("storage-src-1", "src"), srcRoot)
		_, err := resolveVolumeSnapshotBootstrap(c, "snap")
		require.ErrorContains(t, err, "isn't authorized")
		assert.Error(t, c.Get(&corev1.Secret{}, rootSecretName("db")))
	})

	t.Run("needs the snapshot's volume to find its instance", func(t *testing.T) {
		c, _ := snapshotContext(t, snapshotParams, volumeSnapshot("snap", "storage-src-1", "db"), srcRoot)
		_, err := resolveVolumeSnapshotBootstrap(c, "snap")
		require.ErrorContains(t, err, "is gone")
	})

	t.Run("needs the source's root password", func(t *testing.T) {
		c, _ := snapshotContext(t, snapshotParams, volumeSnapshot("snap", "storage-src-1", "db"), sourcePVC("storage-src-1", "src"))
		_, err := resolveVolumeSnapshotBootstrap(c, "snap")
		require.ErrorContains(t, err, "not found")
	})
}

func TestRotateCopiedRootPassword(t *testing.T) {
	copied := func() *corev1.Secret {
		s := credentialSecret(rootSecretName("db"), rootPasswordSecretKey, "src-root")
		s.Annotations = map[string]string{rotateRootAnnotation: "true"}
		return s
	}
	mariaDB := func(ready bool, appliedPassword string, ha bool) *mariadbv1alpha1.MariaDB {
		mdb := startedFrom("snap")
		status := metav1.ConditionFalse
		if ready {
			status = metav1.ConditionTrue
		}
		mdb.Status.Conditions = []metav1.Condition{{Type: mariadbv1alpha1.ConditionTypeReady, Status: status, Reason: "Test"}}
		if appliedPassword != "" {
			sum := sha256.Sum256([]byte(appliedPassword))
			mdb.Status.RootPasswordHash = ptr.To(hex.EncodeToString(sum[:]))
		}
		if ha {
			mdb.Spec.Replication = &mariadbv1alpha1.Replication{Enabled: true}
		}
		return mdb
	}
	pod := func(started time.Time) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "db-0", Labels: mariadbPodLabels("db")},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  mariadbContainerName,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}},
			}}},
		}
	}
	root := func(t *testing.T, c *controller.Context) *corev1.Secret {
		s := &corev1.Secret{}
		require.NoError(t, c.Get(s, rootSecretName("db")))
		return s
	}
	pending := func(t *testing.T, c *controller.Context) bool {
		mdb := &mariadbv1alpha1.MariaDB{}
		require.NoError(t, c.Get(mdb, "db"))
		p, err := rootRotationPending(c, mdb)
		require.NoError(t, err)
		return p
	}
	rotated := func(t *testing.T, c *controller.Context, cl client.Client, at time.Time) string {
		s := root(t, c)
		s.Annotations[rootRotatedAtAnnotation] = at.UTC().Format(time.RFC3339)
		require.NoError(t, cl.Update(context.Background(), s))
		return string(s.Data[rootPasswordSecretKey])
	}

	t.Run("waits until the MariaDB is ready", func(t *testing.T) {
		c, _ := snapshotContext(t, snapshotParams, copied(), mariaDB(false, "", false))
		require.NoError(t, rotateCopiedRootPassword(c))
		assert.Equal(t, []byte("src-root"), root(t, c).Data[rootPasswordSecretKey])
		assert.True(t, pending(t, c))
	})

	t.Run("replaces it once ready", func(t *testing.T) {
		c, _ := snapshotContext(t, snapshotParams, copied(), mariaDB(true, "src-root", false))
		require.NoError(t, rotateCopiedRootPassword(c))
		got := root(t, c)
		assert.NotEqual(t, []byte("src-root"), got.Data[rootPasswordSecretKey])
		assert.Len(t, got.Data[rootPasswordSecretKey], 32)
		assert.NotContains(t, got.Annotations, rotateRootAnnotation)
		assert.Contains(t, got.Annotations, rootRotatedAtAnnotation)
		// The operator hasn't applied it yet.
		assert.True(t, pending(t, c))
	})

	t.Run("standalone: done once applied and the pod restarted with it", func(t *testing.T) {
		at := time.Now().Add(-time.Minute)
		c, cl := snapshotContext(t, snapshotParams, copied(), pod(at.Add(-time.Minute)))
		password := rotated(t, c, cl, at)
		require.NoError(t, cl.Create(context.Background(), mariaDB(true, password, false)))
		require.NoError(t, cl.Update(context.Background(), func() *corev1.Secret {
			s := root(t, c)
			delete(s.Annotations, rotateRootAnnotation)
			return s
		}()))
		// Applied, but the pod still runs with the old password: restart it.
		assert.True(t, pending(t, c))
		require.NoError(t, rotateCopiedRootPassword(c))
		assert.Contains(t, root(t, c).Annotations, rootRotatedAtAnnotation)
		assert.Error(t, c.Get(&corev1.Pod{}, "db-0"), "the pod wasn't restarted")

		// The new pod isn't running yet: it isn't restarted again.
		starting := pod(at)
		starting.Status = corev1.PodStatus{}
		require.NoError(t, cl.Create(context.Background(), starting))
		require.NoError(t, rotateCopiedRootPassword(c))
		require.NoError(t, c.Get(&corev1.Pod{}, "db-0"), "restarted a pod that wasn't running yet")
		assert.True(t, pending(t, c))

		require.NoError(t, cl.Delete(context.Background(), starting))
		require.NoError(t, cl.Create(context.Background(), pod(at.Add(time.Second))))
		assert.False(t, pending(t, c))
		require.NoError(t, rotateCopiedRootPassword(c))
		assert.NotContains(t, root(t, c).Annotations, rootRotatedAtAnnotation)
	})

	t.Run("HA: done once applied", func(t *testing.T) {
		at := time.Now().Add(-time.Minute)
		c, cl := snapshotContext(t, snapshotParams, copied(), pod(at.Add(-time.Minute)))
		password := rotated(t, c, cl, at)
		s := root(t, c)
		delete(s.Annotations, rotateRootAnnotation)
		require.NoError(t, cl.Update(context.Background(), s))
		require.NoError(t, cl.Create(context.Background(), mariaDB(true, password, true)))
		assert.False(t, pending(t, c))
	})

	t.Run("leaves other root passwords alone", func(t *testing.T) {
		c, _ := snapshotContext(t, "", credentialSecret(rootSecretName("db"), rootPasswordSecretKey, "own"), mariaDB(true, "own", false))
		require.NoError(t, rotateCopiedRootPassword(c))
		assert.Equal(t, []byte("own"), root(t, c).Data[rootPasswordSecretKey])
		assert.False(t, pending(t, c))
	})
}
