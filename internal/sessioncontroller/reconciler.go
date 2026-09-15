// Package sessioncontroller implements `shock session-controller` (spec
// section 7): sleep, wake, spawn observation, zombie detection, GC and alarms.
package sessioncontroller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/yuriyostapenko/shock/internal/naming"
	"github.com/yuriyostapenko/shock/internal/patch"
)

// Event reasons emitted on Sandboxes.
const (
	EventSpawnObserved       = "SpawnObserved"
	EventSleep               = "Sleep"
	EventWake                = "Wake"
	EventWakeBlocked         = "WakeBlocked"
	EventGarbageCollected    = "GarbageCollected"
	EventPodStuckTerminating = "PodStuckTerminating"
	EventMultiplePods        = sandboxv1beta1.SandboxReasonMultiplePods
)

// Options tune the lifecycle predicates.
type Options struct {
	GCEnabled        bool
	GCMaxIdle        time.Duration
	ZombieEnabled    bool
	ZombieAlertAfter time.Duration
}

// Reconciler reconciles one release's Sandboxes.
type Reconciler struct {
	// Client reads Sandboxes and Pods from the cache; every write is pinned
	// to the read's UID and resourceVersion.
	Client client.Client
	// Secrets is an uncached reader for work-order Secrets.
	Secrets client.Reader
	// Recorder emits Kubernetes Events on Sandboxes (events.k8s.io/v1).
	Recorder EventRecorder
	Release  string
	Options  Options
	Now      func() time.Time

	mu             sync.Mutex
	multiplePods   map[types.UID]bool
	zombiesAlerted map[types.UID]bool
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile evaluates the predicates in spec order and returns after any write.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	sb := &sandboxv1beta1.Sandbox{}
	if err := r.Client.Get(ctx, req.NamespacedName, sb); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !sb.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if sb.Labels[naming.LabelInstance] != r.Release || sb.Labels[naming.LabelName] != naming.ComponentRunner {
		// The cache selector already excludes these.
		return ctrl.Result{}, nil
	}

	pods, err := r.ownedPods(ctx, sb)
	if err != nil {
		return ctrl.Result{}, err
	}

	if done, err := r.spawnObservation(ctx, logger, sb, pods); done || err != nil {
		return ctrl.Result{}, err
	}
	if done, err := r.sleep(ctx, logger, sb); done || err != nil {
		return ctrl.Result{}, err
	}
	if done, err := r.wake(ctx, logger, sb); done || err != nil {
		return ctrl.Result{}, err
	}
	result := ctrl.Result{}
	if requeue := r.zombie(sb, pods); requeue > 0 {
		result.RequeueAfter = requeue
	}
	if done, requeue, err := r.gc(ctx, logger, sb); done || err != nil {
		return ctrl.Result{}, err
	} else if requeue > 0 && (result.RequeueAfter == 0 || requeue < result.RequeueAfter) {
		result.RequeueAfter = requeue
	}
	r.alarm(sb)
	return result, nil
}

func (r *Reconciler) forget(key types.NamespacedName) {
	// Dropped when the UID stops appearing.
	_ = key
}

// ownedPods lists the release's runner Pods and keeps those owned by this
// Sandbox's UID.
func (r *Reconciler) ownedPods(ctx context.Context, sb *sandboxv1beta1.Sandbox) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.Client.List(ctx, list, client.InNamespace(sb.Namespace), client.MatchingLabels(naming.SelectorLabels(naming.ComponentRunner, r.Release))); err != nil {
		return nil, fmt.Errorf("listing owned pods: %w", err)
	}
	out := make([]corev1.Pod, 0, len(list.Items))
	for _, p := range list.Items {
		if ref := metav1.GetControllerOf(&p); ref != nil && ref.UID == sb.UID {
			out = append(out, p)
		}
	}
	return out, nil
}

// currentGenerationTrue: condition True and observedGeneration current.
func currentGenerationTrue(sb *sandboxv1beta1.Sandbox, condType sandboxv1beta1.ConditionType) bool {
	c := meta.FindStatusCondition(sb.Status.Conditions, string(condType))
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == sb.Generation
}

// podWorkOrderSecret returns the secretName of the Pod's work-order volume.
func podWorkOrderSecret(pod *corev1.Pod) string {
	for _, v := range pod.Spec.Volumes {
		if v.Name == naming.WorkOrderVolumeName && v.Secret != nil {
			return v.Secret.SecretName
		}
	}
	return ""
}

// spawnObservation clears pending intent once the applied order's Pod exists.
func (r *Reconciler) spawnObservation(ctx context.Context, logger logr, sb *sandboxv1beta1.Sandbox, pods []corev1.Pod) (bool, error) {
	pending := sb.Annotations[naming.AnnotationPendingSpawn]
	if pending == "" || sb.Annotations[naming.AnnotationAppliedSpawn] != pending {
		return false, nil
	}
	pendingSecret := sb.Annotations[naming.AnnotationPendingSecret]
	var match *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Annotations[naming.AnnotationOrderID] == pending && pendingSecret != "" && podWorkOrderSecret(p) == pendingSecret {
			match = p
			break
		}
	}
	if match == nil {
		return false, nil
	}
	mut := sb.DeepCopy()
	delete(mut.Annotations, naming.AnnotationPendingSpawn)
	delete(mut.Annotations, naming.AnnotationPendingSpawnAt)
	delete(mut.Annotations, naming.AnnotationPendingSecret)
	if err := r.patch(ctx, sb, mut); err != nil {
		return false, err
	}
	logger.Info("spawn observed", "order", pending, "pod", match.Name, "podUID", match.UID, "phase", match.Status.Phase)
	r.event(sb, corev1.EventTypeNormal, EventSpawnObserved, "Owned Pod %s observed for order %s", match.Name, pending)
	return true, nil
}

// sleep suspends a Sandbox whose runner has exited for the current generation.
func (r *Reconciler) sleep(ctx context.Context, logger logr, sb *sandboxv1beta1.Sandbox) (bool, error) {
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		return false, nil
	}
	if sb.Annotations[naming.AnnotationPendingSpawn] != "" {
		return false, nil
	}
	if !currentGenerationTrue(sb, sandboxv1beta1.SandboxConditionFinished) {
		return false, nil
	}
	finished := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
	mut := sb.DeepCopy()
	mut.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	if mut.Annotations == nil {
		mut.Annotations = map[string]string{}
	}
	mut.Annotations[naming.AnnotationLastSuspendedAt] = r.now().UTC().Format(time.RFC3339)
	if err := r.patch(ctx, sb, mut); err != nil {
		return false, err
	}
	logger.Info("sleep: runner finished, requested suspension", "reason", finished.Reason)
	r.event(sb, corev1.EventTypeNormal, EventSleep, "Runner finished (%s); suspending", finished.Reason)
	return true, nil
}

// wake installs the pending order once suspension is confirmed and the Secret validates.
func (r *Reconciler) wake(ctx context.Context, logger logr, sb *sandboxv1beta1.Sandbox) (bool, error) {
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		return false, nil
	}
	pending := sb.Annotations[naming.AnnotationPendingSpawn]
	if pending == "" {
		return false, nil
	}
	if !currentGenerationTrue(sb, sandboxv1beta1.SandboxConditionSuspended) {
		return false, nil
	}
	secretName := sb.Annotations[naming.AnnotationPendingSecret]
	if secretName == "" {
		r.event(sb, corev1.EventTypeWarning, EventWakeBlocked, "Pending order %s has no pending-secret; waiting for redelivery to repair preparation", pending)
		return false, fmt.Errorf("wake blocked: pending order %s has no %s", pending, naming.AnnotationPendingSecret)
	}
	if last := sb.Annotations[naming.AnnotationLastOrderID]; last != pending {
		return false, fmt.Errorf("wake blocked: pending order %s is not the last accepted order %s", pending, last)
	}
	secret := &corev1.Secret{}
	if err := r.Secrets.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: secretName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			r.event(sb, corev1.EventTypeWarning, EventWakeBlocked, "Work-order Secret %s for pending order %s is missing; waiting for redelivery", secretName, pending)
		}
		return false, fmt.Errorf("wake blocked: reading work-order Secret %s: %w", secretName, err)
	}
	if err := validateWorkOrderSecret(secret, sb, pending, r.Release); err != nil {
		r.event(sb, corev1.EventTypeWarning, EventWakeBlocked, "Work-order Secret %s rejected: %v", secretName, err)
		return false, fmt.Errorf("wake blocked: %w", err)
	}

	mut := sb.DeepCopy()
	mut.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	mut.Annotations[naming.AnnotationAppliedSpawn] = pending
	delete(mut.Annotations, naming.AnnotationLastSuspendedAt)
	pt := &mut.Spec.PodTemplate
	if pt.ObjectMeta.Annotations == nil {
		pt.ObjectMeta.Annotations = map[string]string{}
	}
	pt.ObjectMeta.Annotations[naming.AnnotationOrderID] = pending
	installed := false
	for i := range pt.Spec.Volumes {
		if pt.Spec.Volumes[i].Name == naming.WorkOrderVolumeName {
			if pt.Spec.Volumes[i].Secret == nil {
				pt.Spec.Volumes[i].VolumeSource = corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{}}
			}
			pt.Spec.Volumes[i].Secret.SecretName = secretName
			installed = true
		}
	}
	if !installed {
		return false, fmt.Errorf("wake blocked: podTemplate has no volume named %q", naming.WorkOrderVolumeName)
	}
	if err := r.patch(ctx, sb, mut); err != nil {
		return false, err
	}
	logger.Info("wake: installed order and requested Running", "order", pending, "secret", secretName)
	r.event(sb, corev1.EventTypeNormal, EventWake, "Installed order %s; resuming", pending)
	return true, nil
}

// validateWorkOrderSecret checks the Secret against the accepted order; messages carry no data.
func validateWorkOrderSecret(secret *corev1.Secret, sb *sandboxv1beta1.Sandbox, order, release string) error {
	if secret.Immutable == nil || !*secret.Immutable {
		return errors.New("secret is not immutable")
	}
	if len(secret.Data[naming.WorkOrderSecretKey]) == 0 {
		return fmt.Errorf("secret has no %q key", naming.WorkOrderSecretKey)
	}
	if got := secret.Annotations[naming.AnnotationOrderID]; got != order {
		return fmt.Errorf("secret is for order %q, expected %q", got, order)
	}
	if got, want := secret.Annotations[naming.AnnotationOrderAttempt], sb.Annotations[naming.AnnotationLastOrderAttempt]; got != want {
		return fmt.Errorf("secret attempt %q does not match accepted attempt %q", got, want)
	}
	session := secret.Annotations[naming.AnnotationSessionID]
	if naming.LabelValue(session) != sb.Labels[naming.LabelSessionID] {
		return errors.New("secret is for a different session")
	}
	if want := naming.WorkOrderSecretName(release, session, string(sb.UID), order); secret.Name != want {
		return errors.New("secret name does not derive from this release, session, Sandbox UID and order")
	}
	for _, ref := range secret.OwnerReferences {
		if ref.UID == sb.UID {
			return nil
		}
	}
	return errors.New("secret is not owned by this Sandbox")
}

// zombie emits an Event per Pod Terminating past the threshold; it never
// deletes. Requeues while a Pod is terminating below it.
func (r *Reconciler) zombie(sb *sandboxv1beta1.Sandbox, pods []corev1.Pod) time.Duration {
	if !r.Options.ZombieEnabled {
		return 0
	}
	var requeue time.Duration
	now := r.now()
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp.IsZero() {
			continue
		}
		age := now.Sub(p.DeletionTimestamp.Time)
		if age < r.Options.ZombieAlertAfter {
			if d := r.Options.ZombieAlertAfter - age; requeue == 0 || d < requeue {
				requeue = d
			}
			continue
		}
		r.mu.Lock()
		if r.zombiesAlerted == nil {
			r.zombiesAlerted = map[types.UID]bool{}
		}
		already := r.zombiesAlerted[p.UID]
		r.zombiesAlerted[p.UID] = true
		r.mu.Unlock()
		if !already {
			r.event(sb, corev1.EventTypeWarning, EventPodStuckTerminating,
				"Pod %s has been Terminating for %s (node %q); session stranded, cluster-admin action required. SHOCK never force-deletes Pods.",
				p.Name, age.Truncate(time.Second), p.Spec.NodeName)
		}
	}
	return requeue
}

// gc deletes a Sandbox idle past maxIdle with UID and resourceVersion
// preconditions; PVC and Secrets cascade.
func (r *Reconciler) gc(ctx context.Context, logger logr, sb *sandboxv1beta1.Sandbox) (bool, time.Duration, error) {
	if !r.Options.GCEnabled {
		return false, 0, nil
	}
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		return false, 0, nil
	}
	if sb.Annotations[naming.AnnotationPendingSpawn] != "" {
		return false, 0, nil
	}
	if !currentGenerationTrue(sb, sandboxv1beta1.SandboxConditionSuspended) {
		return false, 0, nil
	}
	raw := sb.Annotations[naming.AnnotationLastSuspendedAt]
	if raw == "" {
		return false, 0, nil
	}
	suspendedAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false, 0, fmt.Errorf("unreadable %s: %w", naming.AnnotationLastSuspendedAt, err)
	}
	idle := r.now().Sub(suspendedAt)
	if idle < r.Options.GCMaxIdle {
		return false, r.Options.GCMaxIdle - idle, nil
	}
	uid := sb.UID
	rv := sb.ResourceVersion
	err = r.Client.Delete(ctx, sb, client.Preconditions{UID: &uid, ResourceVersion: &rv})
	if apierrors.IsNotFound(err) {
		return true, 0, nil
	}
	if err != nil {
		// Conflict: re-evaluate on the next pass.
		return false, 0, fmt.Errorf("gc delete: %w", err)
	}
	logger.Info("gc: deleted idle sandbox", "idle", idle.Truncate(time.Second))
	r.event(sb, corev1.EventTypeNormal, EventGarbageCollected, "Idle for %s (> %s); deleted with PVC and work-order Secrets", idle.Truncate(time.Second), r.Options.GCMaxIdle)
	return true, 0, nil
}

// alarm emits one Event per Ready=False/MultiplePods transition.
func (r *Reconciler) alarm(sb *sandboxv1beta1.Sandbox) {
	ready := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	multiple := ready != nil && ready.Status == metav1.ConditionFalse && ready.Reason == sandboxv1beta1.SandboxReasonMultiplePods
	r.mu.Lock()
	if r.multiplePods == nil {
		r.multiplePods = map[types.UID]bool{}
	}
	was := r.multiplePods[sb.UID]
	r.multiplePods[sb.UID] = multiple
	r.mu.Unlock()
	if multiple && !was {
		r.event(sb, corev1.EventTypeWarning, EventMultiplePods, "Sandbox reports Ready=False/MultiplePods: %s. The sandbox controller refuses to act; do not force-delete Pods.", ready.Message)
	}
}

func (r *Reconciler) patch(ctx context.Context, orig, mut *sandboxv1beta1.Sandbox) error {
	p, err := patch.Optimistic(orig, mut)
	if err != nil {
		return err
	}
	if err := r.Client.Patch(ctx, mut, p); err != nil {
		return fmt.Errorf("conditional patch of Sandbox %s: %w", orig.Name, err)
	}
	return nil
}

// logr is the small logging surface used here.
type logr interface {
	Info(msg string, keysAndValues ...any)
}

// EventRecorder is the subset of events.EventRecorder used here.
type EventRecorder interface {
	Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...any)
}

// event emits an Event on the Sandbox with the lifecycle action as its action.
func (r *Reconciler) event(sb *sandboxv1beta1.Sandbox, eventtype, reason, note string, args ...any) {
	r.Recorder.Eventf(sb, nil, eventtype, reason, reason, note, args...)
}
