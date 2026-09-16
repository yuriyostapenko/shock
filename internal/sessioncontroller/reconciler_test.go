package sessioncontroller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yuriyostapenko/shock/internal/naming"
)

const (
	release   = "rel"
	namespace = "runners"
	session   = "session_01TEST"
	sbUID     = types.UID("11111111-1111-1111-1111-111111111111")
)

var testNow = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t        *testing.T
	client   client.Client
	rec      *Reconciler
	recorder *events.FakeRecorder
}

func newFixture(t *testing.T, objs ...client.Object) *fixture {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(objs...).Build()
	recorder := events.NewFakeRecorder(50)
	rec := &Reconciler{
		Client: c, Secrets: c, Recorder: recorder, Release: release,
		Options: Options{GCEnabled: true, GCMaxIdleAge: 336 * time.Hour, ZombieEnabled: true, ZombieAlertAfter: 5 * time.Minute},
		Now:     func() time.Time { return testNow },
	}
	return &fixture{t: t, client: c, rec: rec, recorder: recorder}
}

func (f *fixture) reconcile() ctrl.Result {
	f.t.Helper()
	res, err := f.rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: naming.SandboxName(release, session)}})
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (f *fixture) reconcileErr() error {
	f.t.Helper()
	_, err := f.rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: naming.SandboxName(release, session)}})
	return err
}

func (f *fixture) sandbox() *sandboxv1beta1.Sandbox {
	f.t.Helper()
	sb := &sandboxv1beta1.Sandbox{}
	err := f.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: naming.SandboxName(release, session)}, sb)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		f.t.Fatal(err)
	}
	return sb
}

func (f *fixture) events() []string {
	var out []string
	for {
		select {
		case e := <-f.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func cond(t sandboxv1beta1.ConditionType, status metav1.ConditionStatus, reason string, gen int64) metav1.Condition {
	return metav1.Condition{Type: string(t), Status: status, Reason: reason, ObservedGeneration: gen}
}

type sbOpt func(*sandboxv1beta1.Sandbox)

func baseSandbox(mode sandboxv1beta1.SandboxOperatingMode, gen int64, opts ...sbOpt) *sandboxv1beta1.Sandbox {
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: naming.SandboxName(release, session), Namespace: namespace, UID: sbUID, Generation: gen, ResourceVersion: "1",
			Labels: map[string]string{
				naming.LabelName: naming.ComponentRunner, naming.LabelInstance: release, naming.LabelPartOf: naming.PartOf,
				naming.LabelSessionID: session,
			},
			Annotations: map[string]string{},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: naming.RunnerContainerName, Image: "x"}},
						Volumes: []corev1.Volume{{Name: naming.WorkOrderVolumeName, VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: naming.PlaceholderSecretName}}}},
					},
				},
			},
			OperatingMode: mode,
		},
	}
	for _, o := range opts {
		o(sb)
	}
	return sb
}

func withAnn(k, v string) sbOpt {
	return func(sb *sandboxv1beta1.Sandbox) { sb.Annotations[k] = v }
}

func withConds(cs ...metav1.Condition) sbOpt {
	return func(sb *sandboxv1beta1.Sandbox) { sb.Status.Conditions = append(sb.Status.Conditions, cs...) }
}

func ownedPod(name, order, secret string, phase corev1.PodPhase) *corev1.Pod {
	ctrlTrue := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, UID: types.UID("pod-" + name),
			Annotations: map[string]string{naming.AnnotationOrderID: order},
			Labels:      map[string]string{naming.LabelName: naming.ComponentRunner, naming.LabelInstance: release},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: "Sandbox", Name: naming.SandboxName(release, session), UID: sbUID, Controller: &ctrlTrue,
			}},
		},
		Spec:   corev1.PodSpec{Volumes: []corev1.Volume{{Name: naming.WorkOrderVolumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret}}}}},
		Status: corev1.PodStatus{Phase: phase, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func secretFor(order, attempt string) *corev1.Secret {
	imm := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: naming.WorkOrderSecretName(release, session, string(sbUID), order), Namespace: namespace,
			Annotations:     map[string]string{naming.AnnotationOrderID: order, naming.AnnotationOrderAttempt: attempt, naming.AnnotationSessionID: session},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: "Sandbox", Name: "x", UID: sbUID}},
		},
		Immutable: &imm,
		Data:      map[string][]byte{naming.WorkOrderSecretKey: []byte("jwt")},
	}
}

// --- Spawn observation ---

func TestSpawnObservationClearsPendingOnOwnedPodAnyPhase(t *testing.T) {
	secret := naming.WorkOrderSecretName(release, session, string(sbUID), "o2")
	for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning, corev1.PodFailed, corev1.PodSucceeded} {
		f := newFixture(t,
			baseSandbox(sandboxv1beta1.SandboxOperatingModeRunning, 2,
				withAnn(naming.AnnotationPendingSpawn, "o2"), withAnn(naming.AnnotationPendingSpawnAt, "2026-01-01T00:00:00Z"),
				withAnn(naming.AnnotationPendingSecret, secret), withAnn(naming.AnnotationAppliedSpawn, "o2"),
				withAnn(naming.AnnotationLastOrderID, "o2")),
			ownedPod(naming.SandboxName(release, session), "o2", secret, phase),
		)
		f.reconcile()
		sb := f.sandbox()
		for _, k := range []string{naming.AnnotationPendingSpawn, naming.AnnotationPendingSpawnAt, naming.AnnotationPendingSecret} {
			if _, ok := sb.Annotations[k]; ok {
				t.Errorf("phase %s: %s must be cleared", phase, k)
			}
		}
		if sb.Annotations[naming.AnnotationAppliedSpawn] != "o2" || sb.Annotations[naming.AnnotationLastOrderID] != "o2" {
			t.Errorf("phase %s: retained annotations damaged: %v", phase, sb.Annotations)
		}
	}
}

// Mandatory case (c): an old, possibly Ready pod must not acknowledge a new order.
func TestOldPodDoesNotAcknowledgeNewOrder(t *testing.T) {
	oldSecret := naming.WorkOrderSecretName(release, session, string(sbUID), "o1")
	newSecret := naming.WorkOrderSecretName(release, session, string(sbUID), "o2")
	// applied already names o2 (Wake happened) but the live pod is still o1.
	f := newFixture(t,
		baseSandbox(sandboxv1beta1.SandboxOperatingModeRunning, 3,
			withAnn(naming.AnnotationPendingSpawn, "o2"), withAnn(naming.AnnotationPendingSecret, newSecret),
			withAnn(naming.AnnotationAppliedSpawn, "o2"), withAnn(naming.AnnotationLastOrderID, "o2")),
		ownedPod(naming.SandboxName(release, session), "o1", oldSecret, corev1.PodRunning),
	)
	f.reconcile()
	if f.sandbox().Annotations[naming.AnnotationPendingSpawn] != "o2" {
		t.Fatal("pending-spawn cleared by a pod carrying the previous order")
	}
	// Same, with applied still naming the previous order (Wake not yet issued).
	f = newFixture(t,
		baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 3,
			withAnn(naming.AnnotationPendingSpawn, "o2"), withAnn(naming.AnnotationPendingSecret, newSecret),
			withAnn(naming.AnnotationAppliedSpawn, "o1"), withAnn(naming.AnnotationLastOrderID, "o2"),
			withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonSuspendedPodTerminating, 3))),
		ownedPod(naming.SandboxName(release, session), "o1", oldSecret, corev1.PodRunning),
	)
	f.reconcile()
	if f.sandbox().Annotations[naming.AnnotationPendingSpawn] != "o2" {
		t.Fatal("pending-spawn cleared while applied-spawn names the prior order")
	}
}

func TestUnownedPodDoesNotAcknowledge(t *testing.T) {
	secret := naming.WorkOrderSecretName(release, session, string(sbUID), "o2")
	pod := ownedPod(naming.SandboxName(release, session), "o2", secret, corev1.PodRunning)
	pod.OwnerReferences[0].UID = "someone-else"
	f := newFixture(t,
		baseSandbox(sandboxv1beta1.SandboxOperatingModeRunning, 2,
			withAnn(naming.AnnotationPendingSpawn, "o2"), withAnn(naming.AnnotationPendingSecret, secret),
			withAnn(naming.AnnotationAppliedSpawn, "o2"), withAnn(naming.AnnotationLastOrderID, "o2")),
		pod)
	f.reconcile()
	if f.sandbox().Annotations[naming.AnnotationPendingSpawn] != "o2" {
		t.Fatal("a pod not owned by this Sandbox UID acknowledged the order")
	}
}

// --- Sleep ---

func TestSleepOnCurrentGenerationFinished(t *testing.T) {
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeRunning, 2,
		withConds(cond(sandboxv1beta1.SandboxConditionFinished, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonPodSucceeded, 2))))
	f.reconcile()
	sb := f.sandbox()
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatal("sleep did not fire")
	}
	if sb.Annotations[naming.AnnotationLastSuspendedAt] != testNow.Format(time.RFC3339) {
		t.Errorf("last-suspended-at = %q", sb.Annotations[naming.AnnotationLastSuspendedAt])
	}
}

func TestSleepBlockedByPendingSpawnOrStaleFinished(t *testing.T) {
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeRunning, 2,
		withAnn(naming.AnnotationPendingSpawn, "o2"), withAnn(naming.AnnotationAppliedSpawn, "o1"),
		withConds(cond(sandboxv1beta1.SandboxConditionFinished, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonPodSucceeded, 2))))
	f.reconcile()
	if f.sandbox().Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatal("sleep fired despite pending-spawn (a stale Sleep cannot suspend a newly accepted order)")
	}
	f = newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeRunning, 3,
		withConds(cond(sandboxv1beta1.SandboxConditionFinished, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonPodFailed, 2))))
	f.reconcile()
	if f.sandbox().Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatal("sleep fired on a Finished condition from an older generation")
	}
}

// --- Wake ---

func TestWakeInstallsOrderAfterConfirmedSuspension(t *testing.T) {
	secret := secretFor("o1", "1")
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 1,
		withAnn(naming.AnnotationPendingSpawn, "o1"), withAnn(naming.AnnotationPendingSecret, secret.Name),
		withAnn(naming.AnnotationLastOrderID, "o1"), withAnn(naming.AnnotationLastOrderAttempt, "1"),
		withAnn(naming.AnnotationLastSuspendedAt, "2026-01-01T00:00:00Z"),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 1))),
		secret)
	f.reconcile()
	sb := f.sandbox()
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatal("wake did not fire")
	}
	if sb.Annotations[naming.AnnotationAppliedSpawn] != "o1" || sb.Annotations[naming.AnnotationPendingSpawn] != "o1" {
		t.Errorf("wake must set applied-spawn and keep pending until observation: %v", sb.Annotations)
	}
	if _, ok := sb.Annotations[naming.AnnotationLastSuspendedAt]; ok {
		t.Error("last-suspended-at must be cleared")
	}
	if sb.Spec.PodTemplate.ObjectMeta.Annotations[naming.AnnotationOrderID] != "o1" || sb.Spec.PodTemplate.Spec.Volumes[0].Secret.SecretName != secret.Name {
		t.Error("pod template order/secret not installed")
	}
}

// Mandatory case (b): a terminating pod must not wake.
func TestTerminatingPodDoesNotWake(t *testing.T) {
	secret := secretFor("o2", "2")
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 3,
		withAnn(naming.AnnotationPendingSpawn, "o2"), withAnn(naming.AnnotationPendingSecret, secret.Name),
		withAnn(naming.AnnotationLastOrderID, "o2"), withAnn(naming.AnnotationLastOrderAttempt, "2"),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonSuspendedPodTerminating, 3))),
		secret)
	f.reconcile()
	if f.sandbox().Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatal("wake fired while the previous pod was still terminating")
	}
}

func TestStaleSuspendedTrueDoesNotWake(t *testing.T) {
	secret := secretFor("o2", "2")
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 5,
		withAnn(naming.AnnotationPendingSpawn, "o2"), withAnn(naming.AnnotationPendingSecret, secret.Name),
		withAnn(naming.AnnotationLastOrderID, "o2"), withAnn(naming.AnnotationLastOrderAttempt, "2"),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 3))),
		secret)
	f.reconcile()
	if f.sandbox().Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatal("wake fired on Suspended=True from generation 3 while spec is at generation 5")
	}
}

func TestWakeBlockedWithoutPreparation(t *testing.T) {
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 1,
		withAnn(naming.AnnotationPendingSpawn, "o1"), withAnn(naming.AnnotationLastOrderID, "o1"),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 1))))
	if err := f.reconcileErr(); err == nil || !strings.Contains(err.Error(), "pending-secret") {
		t.Fatalf("missing preparation must block with an error for backoff: %v", err)
	}
	// Pointer present but the Secret is missing.
	f = newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 1,
		withAnn(naming.AnnotationPendingSpawn, "o1"), withAnn(naming.AnnotationPendingSecret, "wo-missing"),
		withAnn(naming.AnnotationLastOrderID, "o1"), withAnn(naming.AnnotationLastOrderAttempt, "1"),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 1))))
	if err := f.reconcileErr(); err == nil {
		t.Fatal("missing Secret must block Wake")
	}
	if f.sandbox().Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatal("wake fired without a Secret")
	}
}

func TestWakeRejectsSecretForDifferentOrderOrUID(t *testing.T) {
	bad := secretFor("o1", "1")
	bad.Annotations[naming.AnnotationOrderID] = "o0"
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 1,
		withAnn(naming.AnnotationPendingSpawn, "o1"), withAnn(naming.AnnotationPendingSecret, bad.Name),
		withAnn(naming.AnnotationLastOrderID, "o1"), withAnn(naming.AnnotationLastOrderAttempt, "1"),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 1))),
		bad)
	if err := f.reconcileErr(); err == nil {
		t.Fatal("secret for another order must be rejected")
	}
	notOwned := secretFor("o1", "1")
	notOwned.OwnerReferences[0].UID = "other"
	f = newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 1,
		withAnn(naming.AnnotationPendingSpawn, "o1"), withAnn(naming.AnnotationPendingSecret, notOwned.Name),
		withAnn(naming.AnnotationLastOrderID, "o1"), withAnn(naming.AnnotationLastOrderAttempt, "1"),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 1))),
		notOwned)
	if err := f.reconcileErr(); err == nil {
		t.Fatal("secret owned by another UID must be rejected")
	}
}

// --- GC ---

func TestGCDeletesIdleSuspendedSandbox(t *testing.T) {
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 4,
		withAnn(naming.AnnotationLastSuspendedAt, testNow.Add(-400*time.Hour).Format(time.RFC3339)),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 4))))
	f.reconcile()
	if f.sandbox() != nil {
		t.Fatal("idle sandbox not garbage collected")
	}
}

// Mandatory case (a): a woken sandbox still carries Suspended (False) and an old last-suspended-at.
func TestWokenSandboxIsNotGCd(t *testing.T) {
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeRunning, 5,
		withAnn(naming.AnnotationLastSuspendedAt, testNow.Add(-400*time.Hour).Format(time.RFC3339)),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonNotSuspended, 5))))
	f.reconcile()
	if f.sandbox() == nil {
		t.Fatal("GC deleted a running session: live-session data loss")
	}
	// Even with spec Suspended, a False condition (pod still terminating) must not GC.
	f = newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 5,
		withAnn(naming.AnnotationLastSuspendedAt, testNow.Add(-400*time.Hour).Format(time.RFC3339)),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonSuspendedPodTerminating, 5))))
	f.reconcile()
	if f.sandbox() == nil {
		t.Fatal("GC deleted a suspending session")
	}
}

func TestGCBlockedByPendingSpawnStaleGenerationAndAge(t *testing.T) {
	old := testNow.Add(-400 * time.Hour).Format(time.RFC3339)
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 4,
		withAnn(naming.AnnotationLastSuspendedAt, old), withAnn(naming.AnnotationPendingSpawn, "o9"),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 4))))
	_ = f.reconcileErr() // wake is blocked (no secret) and errors; the sandbox must survive either way
	if f.sandbox() == nil {
		t.Fatal("GC deleted a sandbox with a pending order")
	}
	f = newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 6,
		withAnn(naming.AnnotationLastSuspendedAt, old),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 4))))
	f.reconcile()
	if f.sandbox() == nil {
		t.Fatal("GC acted on a stale Suspended=True")
	}
	f = newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 4,
		withAnn(naming.AnnotationLastSuspendedAt, testNow.Add(-1*time.Hour).Format(time.RFC3339)),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 4))))
	res := f.reconcile()
	if f.sandbox() == nil {
		t.Fatal("GC deleted a recently suspended sandbox")
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 336*time.Hour {
		t.Errorf("expected a requeue until maxIdleAge, got %v", res.RequeueAfter)
	}
	f.rec.Options.GCEnabled = false
	f = newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 4,
		withAnn(naming.AnnotationLastSuspendedAt, old),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 4))))
	f.rec.Options.GCEnabled = false
	f.reconcile()
	if f.sandbox() == nil {
		t.Fatal("GC ran while disabled")
	}
}

// --- Zombie and alarm ---

func TestZombieEmitsOneEventAndNeverDeletes(t *testing.T) {
	pod := ownedPod(naming.SandboxName(release, session), "o1", "s", corev1.PodRunning)
	del := metav1.NewTime(testNow.Add(-10 * time.Minute))
	pod.DeletionTimestamp = &del
	pod.Finalizers = []string{"test/keep"}
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 2,
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonSuspendedPodTerminating, 2))), pod)
	f.reconcile()
	f.reconcile()
	ev := f.events()
	n := 0
	for _, e := range ev {
		if strings.Contains(e, EventPodStuckTerminating) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one stranded-pod event, got %d: %v", n, ev)
	}
	got := &corev1.Pod{}
	if err := f.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: pod.Name}, got); err != nil {
		t.Fatalf("the controller must never delete a pod: %v", err)
	}
	// Below threshold: requeue instead of alerting.
	recent := metav1.NewTime(testNow.Add(-1 * time.Minute))
	pod2 := ownedPod(naming.SandboxName(release, session), "o1", "s", corev1.PodRunning)
	pod2.DeletionTimestamp = &recent
	pod2.Finalizers = []string{"test/keep"}
	f = newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 2,
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonSuspendedPodTerminating, 2))), pod2)
	res := f.reconcile()
	if res.RequeueAfter != 4*time.Minute {
		t.Errorf("requeue = %v, want 4m", res.RequeueAfter)
	}
	if len(f.events()) != 0 {
		t.Error("no event below threshold")
	}
}

func TestMultiplePodsAlarmOncePerTransition(t *testing.T) {
	f := newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeRunning, 2,
		withConds(cond(sandboxv1beta1.SandboxConditionReady, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonMultiplePods, 2))))
	f.reconcile()
	f.reconcile()
	ev := f.events()
	if len(ev) != 1 || !strings.Contains(ev[0], "MultiplePods") {
		t.Fatalf("want one MultiplePods event, got %v", ev)
	}
	// Ready is monitoring-only: a Ready=True pod must not drive any transition.
	f = newFixture(t, baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 2,
		withAnn(naming.AnnotationPendingSpawn, "o1"), withAnn(naming.AnnotationPendingSecret, "x"), withAnn(naming.AnnotationLastOrderID, "o1"),
		withConds(cond(sandboxv1beta1.SandboxConditionReady, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonDependenciesReady, 2),
			cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonSuspendedPodTerminating, 2))))
	f.reconcile()
	if f.sandbox().Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatal("Ready=True drove a wake")
	}
}

func TestDeletingSandboxIsIgnored(t *testing.T) {
	now := metav1.Now()
	sb := baseSandbox(sandboxv1beta1.SandboxOperatingModeRunning, 2,
		withConds(cond(sandboxv1beta1.SandboxConditionFinished, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonPodSucceeded, 2)))
	sb.DeletionTimestamp = &now
	sb.Finalizers = []string{"test/keep"}
	f := newFixture(t, sb)
	f.reconcile()
	if f.sandbox().Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatal("acted on a deleting sandbox")
	}
}

// --- Idle-session cap ---

func asleepSandbox(name string, uid types.UID, suspendedAt time.Time) *sandboxv1beta1.Sandbox {
	return baseSandbox(sandboxv1beta1.SandboxOperatingModeSuspended, 4,
		func(sb *sandboxv1beta1.Sandbox) { sb.Name = name; sb.UID = uid },
		withAnn(naming.AnnotationLastSuspendedAt, suspendedAt.Format(time.RFC3339)),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated, 4)))
}

func (f *fixture) reconcileName(name string) {
	f.t.Helper()
	if _, err := f.rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}); err != nil {
		f.t.Fatalf("reconcile %s: %v", name, err)
	}
}

func (f *fixture) exists(name string) bool {
	f.t.Helper()
	err := f.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, &sandboxv1beta1.Sandbox{})
	if err != nil && client.IgnoreNotFound(err) != nil {
		f.t.Fatal(err)
	}
	return err == nil
}

func TestIdleSessionCapDeletesOldestOnly(t *testing.T) {
	f := newFixture(t,
		asleepSandbox("old", "u-old", testNow.Add(-10*time.Hour)),
		asleepSandbox("mid", "u-mid", testNow.Add(-5*time.Hour)),
		asleepSandbox("new", "u-new", testNow.Add(-1*time.Hour)),
	)
	f.rec.Options.GCMaxIdleSessions = 2
	for _, n := range []string{"new", "mid", "old"} {
		f.reconcileName(n)
	}
	if f.exists("old") {
		t.Error("the oldest idle sandbox beyond the cap must be deleted")
	}
	if !f.exists("mid") || !f.exists("new") {
		t.Error("idle sandboxes within the cap must survive")
	}
	// Ties on the timestamp break by name, so the choice is deterministic.
	f = newFixture(t,
		asleepSandbox("b", "u-b", testNow.Add(-time.Hour)),
		asleepSandbox("a", "u-a", testNow.Add(-time.Hour)),
	)
	f.rec.Options.GCMaxIdleSessions = 1
	f.reconcileName("a")
	f.reconcileName("b")
	if f.exists("a") || !f.exists("b") {
		t.Error("tie-break must delete the lexically smaller name")
	}
}

func TestIdleSessionCapCountsOnlyAsleepSandboxes(t *testing.T) {
	running := baseSandbox(sandboxv1beta1.SandboxOperatingModeRunning, 5,
		func(sb *sandboxv1beta1.Sandbox) { sb.Name = "running"; sb.UID = "u-run" },
		withAnn(naming.AnnotationLastSuspendedAt, testNow.Add(-20*time.Hour).Format(time.RFC3339)),
		withConds(cond(sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonNotSuspended, 5)))
	pending := asleepSandbox("pending", "u-pend", testNow.Add(-20*time.Hour))
	pending.Annotations[naming.AnnotationPendingSpawn] = "o1"
	stale := asleepSandbox("stale", "u-stale", testNow.Add(-20*time.Hour))
	stale.Generation = 6
	f := newFixture(t, running, pending, stale, asleepSandbox("idle", "u-idle", testNow.Add(-time.Hour)))
	f.rec.Options.GCMaxIdleSessions = 1
	f.reconcileName("idle") // only "idle" reconciles: pending's wake would error on its missing Secret
	if !f.exists("idle") {
		t.Fatal("the only asleep sandbox must survive: running, pending and stale ones do not count")
	}
	for _, n := range []string{"running", "pending", "stale"} {
		if !f.exists(n) {
			t.Errorf("%s must never be deleted by the cap", n)
		}
	}
}

func TestIdleSessionCapZeroIsUnlimited(t *testing.T) {
	f := newFixture(t,
		asleepSandbox("old", "u-old", testNow.Add(-10*time.Hour)),
		asleepSandbox("new", "u-new", testNow.Add(-1*time.Hour)),
	)
	f.rec.Options.GCMaxIdleSessions = 0
	f.reconcileName("old")
	if !f.exists("old") {
		t.Fatal("cap 0 must not delete anything")
	}
}
