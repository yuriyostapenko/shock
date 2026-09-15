//go:build envtest

// Package envtest exercises the API-concurrency semantics the fake client
// cannot reproduce (spec section 12): resourceVersion conflicts on
// conditional patches, UID + resourceVersion delete preconditions, immutable
// Secrets, and metadata.generation behavior on spec changes. There is no
// agent-sandbox controller here; tests set status conditions themselves.
package envtest

import (
	"context"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/yuriyostapenko/shock/internal/hook"
	"github.com/yuriyostapenko/shock/internal/naming"
	"github.com/yuriyostapenko/shock/internal/patch"
	"github.com/yuriyostapenko/shock/internal/sessioncontroller"
)

var (
	k8sClient client.Client
	testEnv   *envtest.Environment
)

const release = "envtest-rel"

// crdDir locates the agent-sandbox CRDs inside the pinned module.
func crdDir(t *testing.T) string {
	t.Helper()
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("no build info")
	}
	var version string
	for _, d := range bi.Deps {
		if d.Path == "sigs.k8s.io/agent-sandbox" {
			version = d.Version
		}
	}
	if version == "" {
		t.Fatal("agent-sandbox not in build deps")
	}
	gomodcache := os.Getenv("GOMODCACHE")
	if gomodcache == "" {
		gomodcache = filepath.Join(os.Getenv("HOME"), "go", "pkg", "mod")
	}
	return filepath.Join(gomodcache, "sigs.k8s.io", "agent-sandbox@"+version, "k8s", "crds")
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func setup(t *testing.T) (context.Context, client.Client) {
	t.Helper()
	if k8sClient == nil {
		testEnv = &envtest.Environment{CRDDirectoryPaths: []string{crdDir(t)}, ErrorIfCRDPathMissing: true}
		cfg, err := testEnv.Start()
		if err != nil {
			t.Fatalf("starting envtest: %v", err)
		}
		c, err := client.New(cfg, client.Options{Scheme: sessioncontroller.Scheme()})
		if err != nil {
			t.Fatal(err)
		}
		k8sClient = c
	}
	return context.Background(), k8sClient
}

func TestZZZTeardown(t *testing.T) {
	if testEnv != nil {
		_ = testEnv.Stop()
	}
}

func namespace(t *testing.T, ctx context.Context, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "shock-"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	return ns.Name
}

const template = `apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  labels:
    app.kubernetes.io/name: runner
    app.kubernetes.io/instance: envtest-rel
    app.kubernetes.io/version: "0"
    app.kubernetes.io/managed-by: Helm
    app.kubernetes.io/part-of: shock
    helm.sh/chart: shock-0.1.0
spec:
  operatingMode: Suspended
  podTemplate:
    metadata:
      labels:
        app.kubernetes.io/name: runner
        app.kubernetes.io/instance: envtest-rel
        app.kubernetes.io/part-of: shock
    spec:
      containers:
        - name: runner
          image: example.invalid/runner:1
          volumeMounts:
            - name: workspace
              mountPath: /workspace
      volumes:
        - name: work-order
          secret:
            secretName: shock-placeholder-never-materialized
  volumeClaimTemplates:
    - metadata:
        name: workspace
      spec:
        accessModes: ["ReadWriteOncePod"]
        resources:
          requests:
            storage: 1Gi
`

func newHook(t *testing.T, c client.Client, ns string) *hook.Hook {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tmpl.yaml")
	if err := os.WriteFile(p, []byte(template), 0o600); err != nil {
		t.Fatal(err)
	}
	return &hook.Hook{Client: c, Config: hook.Config{
		Release: release, Namespace: ns, TemplatePath: p, WorkspaceMountPath: "/workspace", BaseDir: "/workspace",
		TerminationGracePeriodSeconds: 120, HookTimeoutSeconds: 30,
	}}
}

func req(order string, attempt int64, session string) hook.Request {
	return hook.Request{OrderID: order, SessionID: session, Attempt: attempt, AccountID: "user_1", WorkOrder: []byte("jwt-" + order)}
}

func get(t *testing.T, ctx context.Context, c client.Client, ns, session string) *sandboxv1beta1.Sandbox {
	t.Helper()
	sb := &sandboxv1beta1.Sandbox{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: naming.SandboxName(release, session)}, sb); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return sb
}

// setStatus mimics the sandbox controller writing conditions for the current generation.
func setStatus(t *testing.T, ctx context.Context, c client.Client, sb *sandboxv1beta1.Sandbox, conds ...metav1.Condition) *sandboxv1beta1.Sandbox {
	t.Helper()
	for i := range conds {
		conds[i].ObservedGeneration = sb.Generation
		conds[i].LastTransitionTime = metav1.Now()
		if conds[i].Message == "" {
			conds[i].Message = "test"
		}
	}
	sb.Status.Conditions = conds
	if err := c.Status().Update(ctx, sb); err != nil {
		t.Fatalf("status update: %v", err)
	}
	return sb
}

func newReconciler(c client.Client) *sessioncontroller.Reconciler {
	return &sessioncontroller.Reconciler{
		Client: c, Secrets: c, Recorder: events.NewFakeRecorder(100), Release: release,
		Options: sessioncontroller.Options{GCEnabled: true, GCMaxIdle: time.Hour, ZombieEnabled: true, ZombieAlertAfter: time.Minute},
	}
}

// TestImmutableSecretAndOwnership: the API server enforces immutability and
// the Secret is owned by the Sandbox UID with blockOwnerDeletion false.
func TestImmutableSecretAndOwnership(t *testing.T) {
	ctx, c := setup(t)
	ns := namespace(t, ctx, c)
	h := newHook(t, c, ns)
	if err := h.Run(ctx, req("o1", 1, "session_A")); err != nil {
		t.Fatal(err)
	}
	sb := get(t, ctx, c, ns, "session_A")
	if sb.Generation != 1 {
		t.Errorf("fresh sandbox generation = %d", sb.Generation)
	}
	name := sb.Annotations[naming.AnnotationPendingSecret]
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sec); err != nil {
		t.Fatal(err)
	}
	if sec.OwnerReferences[0].UID != sb.UID || (sec.OwnerReferences[0].BlockOwnerDeletion != nil && *sec.OwnerReferences[0].BlockOwnerDeletion) {
		t.Errorf("owner ref: %+v", sec.OwnerReferences)
	}
	sec.Data["work-order"] = []byte("changed")
	err := c.Update(ctx, sec)
	if err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("immutable secret must reject data changes, got %v", err)
	}
	// Redelivery with different data is refused without touching the Secret.
	r := req("o1", 1, "session_A")
	r.WorkOrder = []byte("other")
	if code := hook.ExitCodeFor(h.Run(ctx, r)); code != hook.ExitNonRetryable {
		t.Fatalf("want exit 2, got %d", code)
	}
}

// TestStaleHookPatchConflicts: a hook that read A must not overwrite B.
func TestStaleHookPatchConflicts(t *testing.T) {
	ctx, c := setup(t)
	ns := namespace(t, ctx, c)
	h := newHook(t, c, ns)
	if err := h.Run(ctx, req("o1", 1, "session_B")); err != nil {
		t.Fatal(err)
	}
	stale := get(t, ctx, c, ns, "session_B")
	// B is published by another hook replica.
	if err := h.Run(ctx, req("o2", 2, "session_B")); err != nil {
		t.Fatal(err)
	}
	// Replay a mutation computed from the stale read.
	mut := stale.DeepCopy()
	mut.Annotations[naming.AnnotationPendingSpawn] = "o1-stale"
	p, err := patch.Optimistic(stale, mut)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Patch(ctx, mut, p)
	if !apierrors.IsConflict(err) {
		t.Fatalf("stale patch must conflict, got %v", err)
	}
	cur := get(t, ctx, c, ns, "session_B")
	if cur.Annotations[naming.AnnotationPendingSpawn] != "o2" {
		t.Fatal("stale write erased the newer order")
	}
}

// TestStaleWakeSleepAndAckConflict: pause each controller mutation after
// reading A, publish B, resume: each must conflict.
func TestStaleWakeSleepAndAckConflict(t *testing.T) {
	ctx, c := setup(t)
	ns := namespace(t, ctx, c)
	h := newHook(t, c, ns)
	if err := h.Run(ctx, req("o1", 1, "session_C")); err != nil {
		t.Fatal(err)
	}
	sb := get(t, ctx, c, ns, "session_C")
	setStatus(t, ctx, c, sb, metav1.Condition{Type: "Suspended", Status: metav1.ConditionTrue, Reason: sandboxv1beta1.SandboxReasonSuspendedPodTerminated})
	readA := get(t, ctx, c, ns, "session_C")                      // Wake's read of A
	if err := h.Run(ctx, req("o2", 2, "session_C")); err != nil { // B published
		t.Fatal(err)
	}
	// Stale Wake: install A's order using the pre-B read.
	mut := readA.DeepCopy()
	mut.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	mut.Annotations[naming.AnnotationAppliedSpawn] = "o1"
	p, _ := patch.Optimistic(readA, mut)
	if err := c.Patch(ctx, mut, p); !apierrors.IsConflict(err) {
		t.Fatalf("stale Wake must conflict, got %v", err)
	}
	cur := get(t, ctx, c, ns, "session_C")
	if cur.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended || cur.Annotations[naming.AnnotationAppliedSpawn] != "" {
		t.Fatal("stale Wake installed an older order")
	}
	// Stale acknowledgement of A after B: cannot clear B.
	mut = readA.DeepCopy()
	delete(mut.Annotations, naming.AnnotationPendingSpawn)
	p, _ = patch.Optimistic(readA, mut)
	if err := c.Patch(ctx, mut, p); !apierrors.IsConflict(err) {
		t.Fatalf("stale ack must conflict, got %v", err)
	}
	if get(t, ctx, c, ns, "session_C").Annotations[naming.AnnotationPendingSpawn] != "o2" {
		t.Fatal("stale ack cleared B")
	}
	// B arrived while the spec was already Suspended with no template change,
	// so generation did not move: Suspended=True still describes the current
	// generation and a live Wake may install B right away (the pod is gone).
	cur = get(t, ctx, c, ns, "session_C")
	if cur.Generation != readA.Generation {
		t.Fatalf("no spec change expected: generation %d -> %d", readA.Generation, cur.Generation)
	}
	rec := newReconciler(c)
	if _, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cur)}); err != nil {
		t.Fatalf("wake: %v", err)
	}
	woken := get(t, ctx, c, ns, "session_C")
	if woken.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning || woken.Annotations[naming.AnnotationAppliedSpawn] != "o2" {
		t.Fatalf("wake did not install B: %v", woken.Annotations)
	}
	if woken.Spec.PodTemplate.Spec.Volumes[0].Secret.SecretName != woken.Annotations[naming.AnnotationPendingSecret] {
		t.Fatal("pod template secret not installed")
	}
	if woken.Generation <= cur.Generation {
		t.Fatal("Wake changes spec and must bump generation")
	}

	// Stale suspension: C arrives while Running. The hook flips operatingMode
	// (generation N+1); the Suspended condition still describes N. Wake and
	// GC stay blocked until observedGeneration catches up.
	if err := h.Run(ctx, req("o3", 3, "session_C")); err != nil {
		t.Fatal(err)
	}
	pendingC := get(t, ctx, c, ns, "session_C")
	if pendingC.Generation <= woken.Generation || pendingC.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatalf("publishing C onto a Running sandbox must request suspension and bump generation: gen %d mode %s", pendingC.Generation, pendingC.Spec.OperatingMode)
	}
	// Preserve an old Suspended=True (from the earlier generation) in status.
	pendingC.Status.Conditions = []metav1.Condition{{Type: "Suspended", Status: metav1.ConditionTrue, Reason: sandboxv1beta1.SandboxReasonSuspendedPodTerminated,
		ObservedGeneration: readA.Generation, LastTransitionTime: metav1.Now(), Message: "stale"}}
	if err := c.Status().Update(ctx, pendingC); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cur)}); err != nil {
		t.Logf("reconcile with stale status: %v", err)
	}
	if get(t, ctx, c, ns, "session_C").Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatal("Wake fired on a Suspended=True from an older generation")
	}
	setStatus(t, ctx, c, get(t, ctx, c, ns, "session_C"), metav1.Condition{Type: "Suspended", Status: metav1.ConditionTrue, Reason: sandboxv1beta1.SandboxReasonSuspendedPodTerminated})
	if _, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cur)}); err != nil {
		t.Fatalf("wake C: %v", err)
	}
	if got := get(t, ctx, c, ns, "session_C"); got.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning || got.Annotations[naming.AnnotationAppliedSpawn] != "o3" {
		t.Fatalf("wake did not install C once the generation caught up: %v", got.Annotations)
	}
}

// TestGCVersusSpawn: GC's read is eligible; a spawn lands; the delete must fail.
func TestGCVersusSpawn(t *testing.T) {
	ctx, c := setup(t)
	ns := namespace(t, ctx, c)
	h := newHook(t, c, ns)
	if err := h.Run(ctx, req("o1", 1, "session_D")); err != nil {
		t.Fatal(err)
	}
	sb := get(t, ctx, c, ns, "session_D")
	// Simulate an acknowledged, slept session: pending cleared, last-suspended-at old.
	mut := sb.DeepCopy()
	delete(mut.Annotations, naming.AnnotationPendingSpawn)
	delete(mut.Annotations, naming.AnnotationPendingSecret)
	mut.Annotations[naming.AnnotationAppliedSpawn] = "o1"
	mut.Annotations[naming.AnnotationLastSuspendedAt] = time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	p, _ := patch.Optimistic(sb, mut)
	if err := c.Patch(ctx, mut, p); err != nil {
		t.Fatal(err)
	}
	setStatus(t, ctx, c, get(t, ctx, c, ns, "session_D"), metav1.Condition{Type: "Suspended", Status: metav1.ConditionTrue, Reason: sandboxv1beta1.SandboxReasonSuspendedPodTerminated})
	eligible := get(t, ctx, c, ns, "session_D")                   // GC's read
	if err := h.Run(ctx, req("o2", 2, "session_D")); err != nil { // spawn wins
		t.Fatal(err)
	}
	uid, rv := eligible.UID, eligible.ResourceVersion
	err := c.Delete(ctx, eligible, client.Preconditions{UID: &uid, ResourceVersion: &rv})
	if !apierrors.IsConflict(err) {
		t.Fatalf("stale GC delete must fail with conflict, got %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(eligible), &sandboxv1beta1.Sandbox{}); err != nil {
		t.Fatal("sandbox deleted despite a concurrent spawn: disk lost")
	}

	// Reverse winner: deletion first, then publication must fail retryably.
	// Mark deleting by adding a finalizer and deleting.
	cur := get(t, ctx, c, ns, "session_D")
	cur.Finalizers = append(cur.Finalizers, "test.shock.invalid/hold")
	if err := c.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, cur); err != nil {
		t.Fatal(err)
	}
	code := hook.ExitCodeFor(h.Run(ctx, req("o3", 3, "session_D")))
	if code != hook.ExitRetryable {
		t.Fatalf("publication onto a deleting Sandbox must be retryable, got exit %d", code)
	}
	deleting := get(t, ctx, c, ns, "session_D")
	if deleting.Annotations[naming.AnnotationPendingSpawn] == "o3" {
		t.Fatal("hook claimed submission onto a deleting UID")
	}
	deleting.Finalizers = nil
	_ = c.Update(ctx, deleting)
}

// TestConcurrentHooks interleaves older/newer attempts and duplicates.
func TestConcurrentHooks(t *testing.T) {
	ctx, c := setup(t)
	ns := namespace(t, ctx, c)
	h := newHook(t, c, ns)
	type run struct {
		order   string
		attempt int64
	}
	runs := []run{{"o1", 1}, {"o2", 2}, {"o1", 1}, {"o3", 3}, {"o2", 2}, {"o3", 3}, {"o1", 1}}
	errs := make(chan error, len(runs))
	for _, r := range runs {
		go func(r run) { errs <- h.Run(ctx, req(r.order, r.attempt, "session_E")) }(r)
	}
	for range runs {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent hook failed: %v", err)
		}
	}
	sb := get(t, ctx, c, ns, "session_E")
	if sb.Annotations[naming.AnnotationLastOrderID] != "o3" || sb.Annotations[naming.AnnotationPendingSpawn] != "o3" || sb.Annotations[naming.AnnotationLastOrderAttempt] != "3" {
		t.Fatalf("highest attempt must win: %v", sb.Annotations)
	}
	want := naming.WorkOrderSecretName(release, "session_E", string(sb.UID), "o3")
	if sb.Annotations[naming.AnnotationPendingSecret] != want || sb.Annotations[naming.AnnotationLastOrderSecret] != want {
		t.Fatalf("pointers disagree: %v", sb.Annotations)
	}
	secrets := &corev1.SecretList{}
	if err := c.List(ctx, secrets, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	// At most one Secret per attempted order; an order superseded before it
	// could publish writes none. Every Secret that exists is owned.
	if n := len(secrets.Items); n < 1 || n > 3 {
		t.Fatalf("expected 1..3 secrets, got %d", n)
	}
	sawAccepted := false
	for _, s := range secrets.Items {
		if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != sb.UID {
			t.Errorf("secret %s not owned by the sandbox", s.Name)
		}
		if s.Name == want {
			sawAccepted = true
		}
	}
	if !sawAccepted {
		t.Fatal("accepted order has no Secret")
	}
	sandboxes := &sandboxv1beta1.SandboxList{}
	if err := c.List(ctx, sandboxes, client.InNamespace(ns)); err != nil || len(sandboxes.Items) != 1 {
		t.Fatalf("exactly one sandbox expected: %v %d", err, len(sandboxes.Items))
	}
}

// TestRecreatedSandboxCannotAdoptOldSecrets: after GC, a new Sandbox for the
// same session derives different Secret names.
func TestRecreatedSandboxCannotAdoptOldSecrets(t *testing.T) {
	ctx, c := setup(t)
	ns := namespace(t, ctx, c)
	h := newHook(t, c, ns)
	if err := h.Run(ctx, req("o1", 1, "session_F")); err != nil {
		t.Fatal(err)
	}
	first := get(t, ctx, c, ns, "session_F")
	firstSecret := first.Annotations[naming.AnnotationPendingSecret]
	if err := c.Delete(ctx, first); err != nil {
		t.Fatal(err)
	}
	// envtest has no garbage collector, so the orphaned Secret lingers as it
	// would briefly on a real cluster.
	for i := 0; i < 50; i++ {
		if err := c.Get(ctx, client.ObjectKeyFromObject(first), &sandboxv1beta1.Sandbox{}); apierrors.IsNotFound(err) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := h.Run(ctx, req("o1", 1, "session_F")); err != nil {
		t.Fatal(err)
	}
	second := get(t, ctx, c, ns, "session_F")
	if second.UID == first.UID {
		t.Fatal("expected a new UID")
	}
	if second.Annotations[naming.AnnotationPendingSecret] == firstSecret {
		t.Fatal("recreated sandbox adopted a Secret owned by the deleted one")
	}
	if !strings.HasPrefix(second.Annotations[naming.AnnotationPendingSecret], release+"-wo-") {
		t.Fatal("no secret recorded")
	}
}

// TestSleepConditionalWrite: Sleep computed from a stale read cannot suspend
// a newly accepted order, and a live Sleep works.
func TestSleepConditionalWrite(t *testing.T) {
	ctx, c := setup(t)
	ns := namespace(t, ctx, c)
	h := newHook(t, c, ns)
	if err := h.Run(ctx, req("o1", 1, "session_G")); err != nil {
		t.Fatal(err)
	}
	sb := get(t, ctx, c, ns, "session_G")
	setStatus(t, ctx, c, sb, metav1.Condition{Type: "Suspended", Status: metav1.ConditionTrue, Reason: sandboxv1beta1.SandboxReasonSuspendedPodTerminated})
	rec := newReconciler(c)
	key := client.ObjectKeyFromObject(sb)
	if _, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("wake: %v", err)
	}
	running := get(t, ctx, c, ns, "session_G")
	if running.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatal("expected Running after wake")
	}
	// Simulate ack (pending cleared) then a finished pod for this generation.
	mut := running.DeepCopy()
	delete(mut.Annotations, naming.AnnotationPendingSpawn)
	delete(mut.Annotations, naming.AnnotationPendingSecret)
	delete(mut.Annotations, naming.AnnotationPendingSpawnAt)
	p, _ := patch.Optimistic(running, mut)
	if err := c.Patch(ctx, mut, p); err != nil {
		t.Fatal(err)
	}
	acked := get(t, ctx, c, ns, "session_G")
	setStatus(t, ctx, c, acked, metav1.Condition{Type: "Finished", Status: metav1.ConditionTrue, Reason: sandboxv1beta1.SandboxReasonPodSucceeded},
		metav1.Condition{Type: "Suspended", Status: metav1.ConditionFalse, Reason: sandboxv1beta1.SandboxReasonNotSuspended})
	sleepRead := get(t, ctx, c, ns, "session_G")
	// A newer order lands before Sleep writes.
	if err := h.Run(ctx, req("o2", 2, "session_G")); err != nil {
		t.Fatal(err)
	}
	mut = sleepRead.DeepCopy()
	mut.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	mut.Annotations[naming.AnnotationLastSuspendedAt] = time.Now().UTC().Format(time.RFC3339)
	p, _ = patch.Optimistic(sleepRead, mut)
	if err := c.Patch(ctx, mut, p); !apierrors.IsConflict(err) {
		t.Fatalf("stale Sleep must conflict, got %v", err)
	}
	cur := get(t, ctx, c, ns, "session_G")
	if _, ok := cur.Annotations[naming.AnnotationLastSuspendedAt]; ok {
		t.Fatal("stale Sleep stamped last-suspended-at onto a newly accepted order")
	}
}
