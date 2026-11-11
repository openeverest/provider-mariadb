package provider

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/openeverest/provider-mariadb/definition/components"
	"github.com/openeverest/provider-mariadb/internal/common"
)

const (
	// BootstrapInstanceAnnotation on a VolumeSnapshot names the one Instance
	// allowed to start from it. Everest authorizes reading an Instance's
	// spec.dataSource, not its engine parameters, and its users can't
	// annotate snapshots: only whoever can, such as the Branching plugin, can
	// authorize a snapshot.
	BootstrapInstanceAnnotation = "provider-mariadb.openeverest.io/bootstrap-instance"
	// rotateRootAnnotation marks a root password Secret copied from the
	// source of a snapshot, to be replaced once the instance runs.
	rotateRootAnnotation = "provider-mariadb.openeverest.io/rotate-root"
	// rootRotatedAtAnnotation records when that password was replaced, until
	// the instance runs with the new one.
	rootRotatedAtAnnotation = "provider-mariadb.openeverest.io/root-rotated-at"
	rootRotationMessage     = "replacing the root password copied from the snapshot's source"
	// instanceLabelKey is the label the operator puts on a MariaDB's PVCs.
	instanceLabelKey = "app.kubernetes.io/instance"
	// mariadbContainerName is the operator's MariaDB container.
	mariadbContainerName = "mariadb"
)

// VolumeSnapshots are read unstructured: the client reads them from the API
// server, with get only, instead of caching them all.
var volumeSnapshotGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}

var bootstrapSnapshotPath = enginePath.Child("parameters", "bootstrap", "volumeSnapshot")

// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get
// +kubebuilder:rbac:groups="",resources=pods,verbs=delete

// bootstrapSnapshot is the VolumeSnapshot the Instance asks to start from, or "".
func bootstrapSnapshot(in *corev1alpha1.Instance) (string, error) {
	engine, ok := in.Spec.Components[common.ComponentEngine]
	if !ok || engine.Parameters == nil || engine.Parameters.Raw == nil {
		return "", nil
	}
	var params components.MariadbParameters
	if err := json.Unmarshal(engine.Parameters.Raw, &params); err != nil {
		return "", fmt.Errorf("decode engine parameters: %w", err)
	}
	if params.Bootstrap == nil || params.Bootstrap.VolumeSnapshot == nil {
		return "", nil
	}
	return params.Bootstrap.VolumeSnapshot.Name, nil
}

// validateVolumeSnapshotBootstrap checks a bootstrap from a VolumeSnapshot
// against the rest of the Instance, and that it doesn't change once the
// Instance is provisioned: the snapshot is only read at creation.
func validateVolumeSnapshotBootstrap(in, old *corev1alpha1.Instance) field.ErrorList {
	name, err := bootstrapSnapshot(in)
	if err != nil {
		return nil // reported with the other engine parameters
	}
	var errs field.ErrorList
	if provisioned(old) {
		if oldName, err := bootstrapSnapshot(old); err == nil && oldName != name {
			errs = append(errs, field.Forbidden(bootstrapSnapshotPath, "is immutable"))
		}
	}
	if name == "" {
		return errs
	}
	if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
		errs = append(errs, field.Invalid(bootstrapSnapshotPath.Child("name"), name, strings.Join(msgs, "; ")))
	}
	if in.Spec.DataSource != nil {
		errs = append(errs, field.Forbidden(bootstrapSnapshotPath, "cannot be combined with spec.dataSource"))
	}
	return errs
}

// validateVolumeSnapshotReference checks, before the MariaDB exists, that the
// bootstrap VolumeSnapshot exists and is authorized for this Instance; once
// the MariaDB exists, that the snapshot is the one it started from.
func validateVolumeSnapshotReference(c *controller.Context, mdb *mariadbv1alpha1.MariaDB) (field.ErrorList, error) {
	name, err := bootstrapSnapshot(c.Instance())
	if err != nil {
		return nil, nil
	}
	if mdb != nil {
		var started string
		if b := mdb.Spec.BootstrapFrom; b != nil && b.VolumeSnapshotRef != nil {
			started = b.VolumeSnapshotRef.Name
		}
		if started != name {
			return field.ErrorList{field.Forbidden(bootstrapSnapshotPath, "is immutable")}, nil
		}
		return nil, nil
	}
	if name == "" {
		return nil, nil
	}
	if _, err := authorizedSnapshot(c, name); err != nil {
		var invalid *snapshotError
		if errors.As(err, &invalid) {
			return field.ErrorList{field.Invalid(bootstrapSnapshotPath.Child("name"), name, invalid.msg)}, nil
		}
		return nil, err
	}
	return nil, nil
}

// snapshotError is a snapshot that can't be used, as opposed to a failed read.
type snapshotError struct{ msg string }

func (e *snapshotError) Error() string { return e.msg }

// authorizedSnapshot returns the VolumeSnapshot if it exists and is
// annotated for this Instance.
func authorizedSnapshot(c *controller.Context, name string) (*unstructured.Unstructured, error) {
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(volumeSnapshotGVK)
	if err := c.Get(snap, name); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &snapshotError{fmt.Sprintf("VolumeSnapshot %q not found", name)}
		}
		return nil, fmt.Errorf("get VolumeSnapshot %q: %w", name, err)
	}
	if got := snap.GetAnnotations()[BootstrapInstanceAnnotation]; got != c.Name() {
		return nil, &snapshotError{fmt.Sprintf("VolumeSnapshot %q isn't authorized to start instance %q: it needs the annotation %s=%s",
			name, c.Name(), BootstrapInstanceAnnotation, c.Name())}
	}
	return snap, nil
}

// resolveVolumeSnapshotBootstrap builds bootstrapFrom for an Instance that
// starts from a VolumeSnapshot, and gives it the root password of the
// instance the snapshot was taken of: the operator connects as root, which
// the snapshot's mysql.global_priv defines. That password is replaced once
// the instance runs (rotateCopiedRootPassword). The user password is the new
// instance's own: the operator sets it on the restored user.
func resolveVolumeSnapshotBootstrap(c *controller.Context, name string) (*mariadbv1alpha1.BootstrapFrom, error) {
	snap, err := authorizedSnapshot(c, name)
	if err != nil {
		return nil, err
	}
	pvcName, _, _ := unstructured.NestedString(snap.Object, "spec", "source", "persistentVolumeClaimName")
	// Unstructured, so the client reads it from the API server rather than
	// starting a cache of every PVC, which needs list and watch.
	pvc := &unstructured.Unstructured{}
	pvc.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim"))
	if err := c.Get(pvc, pvcName); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &snapshotError{fmt.Sprintf("VolumeSnapshot %q: its volume %q is gone, so its instance's root password can't be found", name, pvcName)}
		}
		return nil, fmt.Errorf("get PVC %q: %w", pvcName, err)
	}
	source := pvc.GetLabels()[instanceLabelKey]
	if source == "" {
		return nil, &snapshotError{fmt.Sprintf("VolumeSnapshot %q isn't of a MariaDB volume: PVC %q has no %s label", name, pvcName, instanceLabelKey)}
	}
	if err := copyRootSecret(c, source); err != nil {
		return nil, err
	}
	return &mariadbv1alpha1.BootstrapFrom{
		VolumeSnapshotRef: &mariadbv1alpha1.LocalObjectReference{Name: name},
	}, nil
}

// copyRootSecret creates the Instance's root password Secret as a copy of
// the source instance's, marked for rotation. An existing Secret is kept.
func copyRootSecret(c *controller.Context, source string) error {
	dstName := rootSecretName(c.Name())
	dst := &corev1.Secret{}
	if err := c.Get(dst, dstName); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get Secret %q: %w", dstName, err)
	}
	src := &corev1.Secret{}
	if err := c.Get(src, rootSecretName(source)); err != nil {
		if apierrors.IsNotFound(err) {
			return &snapshotError{fmt.Sprintf("root password Secret %q of instance %q, the snapshot's source, not found", rootSecretName(source), source)}
		}
		return fmt.Errorf("get Secret %q: %w", rootSecretName(source), err)
	}
	secret := &corev1.Secret{
		ObjectMeta: c.ObjectMeta(dstName),
		Type:       src.Type,
		Data:       map[string][]byte{rootPasswordSecretKey: src.Data[rootPasswordSecretKey]},
	}
	secret.Annotations = map[string]string{rotateRootAnnotation: "true"}
	if err := controllerutil.SetControllerReference(c.Instance(), secret, c.Client().Scheme()); err != nil {
		return fmt.Errorf("set owner reference on Secret %q: %w", dstName, err)
	}
	if err := c.Client().Create(c.Context(), secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create Secret %q: %w", dstName, err)
	}
	return nil
}

// rotateCopiedRootPassword replaces a root password copied from a snapshot's
// source with a new one once the MariaDB is ready: the operator then changes
// it on root@localhost and root@%. Until then the new instance shares its
// source's root password, which its connection details expose.
//
// A standalone instance's probes authenticate with the root password the pod
// started with, so its pod restarts once (failing its liveness probe) after
// the change; HA instances' probes go through the agent, which the operator
// updates. The rotation ends when the operator has applied the password and,
// for standalone, the pods run with it: until then, rootRotationPending holds
// the Instance's phase.
func rotateCopiedRootPassword(c *controller.Context) error {
	secret := &corev1.Secret{}
	if err := c.Get(secret, rootSecretName(c.Name())); err != nil {
		return client404(err)
	}
	copied := secret.Annotations[rotateRootAnnotation] != ""
	if !copied && secret.Annotations[rootRotatedAtAnnotation] == "" {
		return nil
	}
	mdb := &mariadbv1alpha1.MariaDB{}
	if err := c.Get(mdb, c.Name()); err != nil {
		return client404(err)
	}
	if copied {
		if !mdb.IsReady() {
			return nil
		}
		password, err := randomPassword()
		if err != nil {
			return err
		}
		secret.Data[rootPasswordSecretKey] = []byte(password)
		delete(secret.Annotations, rotateRootAnnotation)
		secret.Annotations[rootRotatedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
		if err := c.Client().Update(c.Context(), secret); err != nil {
			return fmt.Errorf("rotate the root password copied from the snapshot's source: %w", err)
		}
		return nil
	}
	done, err := rootRotationDone(c, mdb, secret)
	if err != nil {
		return err
	}
	if !done {
		return restartForRotatedRoot(c, mdb, secret)
	}
	delete(secret.Annotations, rootRotatedAtAnnotation)
	if err := c.Client().Update(c.Context(), secret); err != nil {
		return fmt.Errorf("finish the root password rotation: %w", err)
	}
	return nil
}

// restartForRotatedRoot deletes the standalone pods still running with the
// old root password once the operator has applied the new one, rather than
// waiting for their liveness probe to fail.
func restartForRotatedRoot(c *controller.Context, mdb *mariadbv1alpha1.MariaDB, secret *corev1.Secret) error {
	if mdb.IsHAEnabled() || !rootPasswordApplied(mdb, secret) {
		return nil
	}
	rotatedAt, err := time.Parse(time.RFC3339, secret.Annotations[rootRotatedAtAnnotation])
	if err != nil {
		return nil
	}
	pods, err := mariadbPods(c)
	if err != nil {
		return err
	}
	for i := range pods {
		// A pod not running yet starts with the new password: leave it be.
		if runningSince(&pods[i], rotatedAt) == oldPassword && pods[i].DeletionTimestamp == nil {
			if err := c.Client().Delete(c.Context(), &pods[i]); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("restart pod %s with the new root password: %w", pods[i].Name, err)
			}
		}
	}
	return nil
}

func rootPasswordApplied(mdb *mariadbv1alpha1.MariaDB, secret *corev1.Secret) bool {
	sum := sha256.Sum256(secret.Data[rootPasswordSecretKey])
	return mdb.Status.RootPasswordHash != nil && *mdb.Status.RootPasswordHash == hex.EncodeToString(sum[:])
}

func mariadbPods(c *controller.Context) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := c.Client().List(c.Context(), pods, client.InNamespace(c.Namespace()), client.MatchingLabels(mariadbPodLabels(c.Name()))); err != nil {
		return nil, fmt.Errorf("list MariaDB pods: %w", err)
	}
	return pods.Items, nil
}

type podPassword int

const (
	notRunning podPassword = iota
	oldPassword
	newPassword
)

// runningSince tells whether the pod's MariaDB container runs with the root
// password set at t (started at or after it), an older one, or not at all.
func runningSince(pod *corev1.Pod, t time.Time) podPassword {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != mariadbContainerName || cs.State.Running == nil {
			continue
		}
		if cs.State.Running.StartedAt.Time.Before(t) {
			return oldPassword
		}
		return newPassword
	}
	return notRunning
}

// rootRotationPending tells whether the root password copied from a
// snapshot's source is still being replaced.
func rootRotationPending(c *controller.Context, mdb *mariadbv1alpha1.MariaDB) (bool, error) {
	secret := &corev1.Secret{}
	if err := c.Get(secret, rootSecretName(c.Name())); err != nil {
		return false, client404(err)
	}
	switch {
	case secret.Annotations[rotateRootAnnotation] != "":
		return true, nil
	case secret.Annotations[rootRotatedAtAnnotation] != "":
		done, err := rootRotationDone(c, mdb, secret)
		return !done, err
	}
	return false, nil
}

// rootRotationDone tells whether the operator has applied the rotated root
// password and, for standalone, the pods have restarted with it.
func rootRotationDone(c *controller.Context, mdb *mariadbv1alpha1.MariaDB, secret *corev1.Secret) (bool, error) {
	if !rootPasswordApplied(mdb, secret) || !mdb.IsReady() {
		return false, nil
	}
	if mdb.IsHAEnabled() {
		return true, nil
	}
	rotatedAt, err := time.Parse(time.RFC3339, secret.Annotations[rootRotatedAtAnnotation])
	if err != nil {
		return true, nil
	}
	pods, err := mariadbPods(c)
	if err != nil {
		return false, err
	}
	for i := range pods {
		if runningSince(&pods[i], rotatedAt) != newPassword {
			return false, nil
		}
	}
	return true, nil
}

func client404(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
