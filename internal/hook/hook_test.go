package hook

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yuriyostapenko/shock/internal/naming"
)

func getSandbox(t *testing.T, c client.Client, session string) *sandboxv1beta1.Sandbox {
	t.Helper()
	sb := &sandboxv1beta1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "runners", Name: naming.SandboxName(testRelease, session)}, sb); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return sb
}

func listSecrets(t *testing.T, c client.Client) []corev1.Secret {
	t.Helper()
	l := &corev1.SecretList{}
	if err := c.List(context.Background(), l, client.InNamespace("runners")); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func run(t *testing.T, c client.Client, cfg Config, req Request) error {
	t.Helper()
	h := &Hook{Client: c, Config: cfg}
	return h.Run(context.Background(), req)
}

func TestFreshSessionCreatesSuspendedSandboxAndOwnedSecret(t *testing.T) {
	c := newFakeClient(t)
	cfg := testConfig(writeTemplate(t, testTemplate))
	req := testRequest("order-1", 1)
	if err := run(t, c, cfg, req); err != nil {
		t.Fatalf("hook failed: %v", err)
	}
	sb := getSandbox(t, c, req.SessionID)
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Errorf("operatingMode = %s, want Suspended", sb.Spec.OperatingMode)
	}
	ann := sb.Annotations
	wantSecret := naming.WorkOrderSecretName(testRelease, req.SessionID, string(sb.UID), "order-1")
	for k, want := range map[string]string{
		naming.AnnotationPendingSpawn:     "order-1",
		naming.AnnotationLastOrderID:      "order-1",
		naming.AnnotationLastOrderAttempt: "1",
		naming.AnnotationPendingSecret:    wantSecret,
		naming.AnnotationLastOrderSecret:  wantSecret,
	} {
		if ann[k] != want {
			t.Errorf("annotation %s = %q, want %q", k, ann[k], want)
		}
	}
	if _, ok := ann[naming.AnnotationAppliedSpawn]; ok {
		t.Error("applied-spawn must be absent on creation")
	}
	if ann[naming.AnnotationPendingSpawnAt] == "" {
		t.Error("pending-spawn-at missing")
	}
	// Forced fields.
	ps := sb.Spec.PodTemplate.Spec
	if ps.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", ps.RestartPolicy)
	}
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be false")
	}
	if ps.TerminationGracePeriodSeconds == nil || *ps.TerminationGracePeriodSeconds != 120 {
		t.Error("terminationGracePeriodSeconds not forced")
	}
	runner := ps.Containers[0]
	var wsPath, woPath string
	for _, m := range runner.VolumeMounts {
		switch m.Name {
		case "workspace":
			wsPath = m.MountPath
		case "work-order":
			woPath = m.MountPath
		}
	}
	if wsPath != "/home/runner" || woPath != naming.WorkOrderMountPath {
		t.Errorf("mounts: workspace=%q work-order=%q", wsPath, woPath)
	}
	if !strings.Contains(strings.Join(runner.Args, " "), "--lock-to-account user_01ACC") {
		t.Errorf("lock-to-account missing: %v", runner.Args)
	}
	pl := sb.Spec.PodTemplate.ObjectMeta.Labels
	if pl[naming.LabelSessionID] != req.SessionID || pl[naming.LabelAccountID] != req.AccountID || pl["custom/label"] != "keep-me" {
		t.Errorf("pod labels: %v", pl)
	}
	if sb.Labels[naming.LabelSessionID] != req.SessionID {
		t.Errorf("sandbox labels: %v", sb.Labels)
	}
	// The placeholder Secret is never materialized; the real one is owned and immutable.
	secrets := listSecrets(t, c)
	if len(secrets) != 1 {
		t.Fatalf("want exactly 1 secret, got %d", len(secrets))
	}
	s := secrets[0]
	if s.Name != wantSecret || s.Immutable == nil || !*s.Immutable || string(s.Data["work-order"]) != "jwt-order-1" {
		t.Errorf("secret %s malformed", s.Name)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != sb.UID || s.OwnerReferences[0].Controller != nil {
		t.Errorf("secret owner refs: %+v", s.OwnerReferences)
	}
	if vol := sb.Spec.PodTemplate.Spec.Volumes; len(vol) != 1 || vol[0].Secret.SecretName != "placeholder" {
		t.Errorf("work-order volume must keep the placeholder until Wake: %+v", vol)
	}
}

func TestRedeliveryIsIdempotent(t *testing.T) {
	c := newFakeClient(t)
	cfg := testConfig(writeTemplate(t, testTemplate))
	req := testRequest("order-1", 1)
	if err := run(t, c, cfg, req); err != nil {
		t.Fatal(err)
	}
	before := getSandbox(t, c, req.SessionID)
	if err := run(t, c, cfg, req); err != nil {
		t.Fatalf("redelivery failed: %v", err)
	}
	after := getSandbox(t, c, req.SessionID)
	if before.ResourceVersion != after.ResourceVersion {
		t.Error("redelivery must not write when preparation is complete")
	}
	if n := len(listSecrets(t, c)); n != 1 {
		t.Errorf("redelivery created a secret: %d", n)
	}
}

func TestRedeliveryRepairsCrashedPreparation(t *testing.T) {
	c := newFakeClient(t)
	cfg := testConfig(writeTemplate(t, testTemplate))
	req := testRequest("order-1", 1)
	// Simulate a crash right after Sandbox creation: pending intent without a Secret or pointers.
	tmpl, err := LoadTemplate(cfg.TemplatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyContract(tmpl, Identity{Release: testRelease, Namespace: "runners", SessionID: req.SessionID, AccountID: req.AccountID}, Contract{WorkspaceMountPath: "/home/runner", BaseDir: "/home/runner/workspace", TerminationGracePeriodSeconds: 120}); err != nil {
		t.Fatal(err)
	}
	tmpl.Annotations = map[string]string{
		naming.AnnotationPendingSpawn:     "order-1",
		naming.AnnotationPendingSpawnAt:   "2026-01-01T00:00:00Z",
		naming.AnnotationLastOrderID:      "order-1",
		naming.AnnotationLastOrderAttempt: "1",
	}
	if err := c.Create(context.Background(), tmpl); err != nil {
		t.Fatal(err)
	}
	if err := run(t, c, cfg, req); err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	sb := getSandbox(t, c, req.SessionID)
	want := naming.WorkOrderSecretName(testRelease, req.SessionID, string(sb.UID), "order-1")
	if sb.Annotations[naming.AnnotationPendingSecret] != want || sb.Annotations[naming.AnnotationLastOrderSecret] != want {
		t.Errorf("pointers not repaired: %v", sb.Annotations)
	}
	if n := len(listSecrets(t, c)); n != 1 {
		t.Errorf("want 1 secret, got %d", n)
	}
}

func TestRedeliveryDoesNotRepairAcknowledgedOrder(t *testing.T) {
	c := newFakeClient(t)
	cfg := testConfig(writeTemplate(t, testTemplate))
	req := testRequest("order-1", 1)
	if err := run(t, c, cfg, req); err != nil {
		t.Fatal(err)
	}
	// The controller acknowledged order-1: pending pointers gone, applied set.
	sb := getSandbox(t, c, req.SessionID)
	sb.Annotations[naming.AnnotationAppliedSpawn] = "order-1"
	delete(sb.Annotations, naming.AnnotationPendingSpawn)
	delete(sb.Annotations, naming.AnnotationPendingSecret)
	sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	if err := c.Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	before := getSandbox(t, c, req.SessionID)
	if err := run(t, c, cfg, req); err != nil {
		t.Fatal(err)
	}
	after := getSandbox(t, c, req.SessionID)
	if after.ResourceVersion != before.ResourceVersion {
		t.Error("redelivery of an acknowledged order must not re-arm anything")
	}
	if _, ok := after.Annotations[naming.AnnotationPendingSpawn]; ok {
		t.Error("pending-spawn re-armed")
	}
}

func TestHigherAttemptBouncesAndKeepsAppliedOrder(t *testing.T) {
	c := newFakeClient(t)
	cfg := testConfig(writeTemplate(t, testTemplate))
	if err := run(t, c, cfg, testRequest("order-1", 1)); err != nil {
		t.Fatal(err)
	}
	// Pretend Wake happened and order-1 was acknowledged.
	sb := getSandbox(t, c, "session_01TEST")
	firstSecret := sb.Annotations[naming.AnnotationPendingSecret]
	sb.Annotations[naming.AnnotationAppliedSpawn] = "order-1"
	delete(sb.Annotations, naming.AnnotationPendingSpawn)
	delete(sb.Annotations, naming.AnnotationPendingSecret)
	sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	sb.Spec.PodTemplate.ObjectMeta.Annotations = map[string]string{naming.AnnotationOrderID: "order-1"}
	sb.Spec.PodTemplate.Spec.Volumes[0].Secret.SecretName = firstSecret
	if err := c.Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}

	if err := run(t, c, cfg, testRequest("order-2", 2)); err != nil {
		t.Fatalf("newer order failed: %v", err)
	}
	sb = getSandbox(t, c, "session_01TEST")
	secondSecret := naming.WorkOrderSecretName(testRelease, "session_01TEST", string(sb.UID), "order-2")
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Error("newer order must request suspension")
	}
	if sb.Annotations[naming.AnnotationPendingSpawn] != "order-2" || sb.Annotations[naming.AnnotationPendingSecret] != secondSecret ||
		sb.Annotations[naming.AnnotationLastOrderID] != "order-2" || sb.Annotations[naming.AnnotationLastOrderAttempt] != "2" {
		t.Errorf("pointers: %v", sb.Annotations)
	}
	if sb.Annotations[naming.AnnotationAppliedSpawn] != "order-1" {
		t.Error("applied-spawn must be untouched by the hook")
	}
	if sb.Spec.PodTemplate.ObjectMeta.Annotations[naming.AnnotationOrderID] != "order-1" || sb.Spec.PodTemplate.Spec.Volumes[0].Secret.SecretName != firstSecret {
		t.Error("the Pod template's current order and Secret must be untouched until Wake")
	}
	secrets := listSecrets(t, c)
	if len(secrets) != 2 {
		t.Fatalf("want 2 secrets, got %d", len(secrets))
	}
	for _, s := range secrets {
		if s.Name == firstSecret && string(s.Data["work-order"]) != "jwt-order-1" {
			t.Error("old secret was modified")
		}
	}

	// Older redelivery cannot roll state back.
	if err := run(t, c, cfg, testRequest("order-1", 1)); err != nil {
		t.Fatalf("older redelivery must exit 0: %v", err)
	}
	sb2 := getSandbox(t, c, "session_01TEST")
	if sb2.Annotations[naming.AnnotationPendingSpawn] != "order-2" || sb2.ResourceVersion != sb.ResourceVersion {
		t.Error("older order rolled state back")
	}
	// A stale lower attempt with an unknown id is superseded silently.
	if err := run(t, c, cfg, testRequest("order-0", 0)); err != nil {
		t.Fatalf("superseded must exit 0: %v", err)
	}
	if n := len(listSecrets(t, c)); n != 2 {
		t.Errorf("superseded order wrote a secret: %d", n)
	}
}

func TestSameAttemptDifferentOrderIsProtocolError(t *testing.T) {
	c := newFakeClient(t)
	cfg := testConfig(writeTemplate(t, testTemplate))
	if err := run(t, c, cfg, testRequest("order-1", 1)); err != nil {
		t.Fatal(err)
	}
	err := run(t, c, cfg, testRequest("order-x", 1))
	if ExitCodeFor(err) != ExitNonRetryable {
		t.Fatalf("want exit 2, got %d (%v)", ExitCodeFor(err), err)
	}
	if n := len(listSecrets(t, c)); n != 1 {
		t.Errorf("protocol error must not create a secret: %d", n)
	}
}

func TestDeletingSandboxIsRetryable(t *testing.T) {
	now := metav1.Now()
	sb := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: naming.SandboxName(testRelease, "session_01TEST"), Namespace: "runners",
		DeletionTimestamp: &now, Finalizers: []string{"test/keep"},
		Labels: map[string]string{naming.LabelPartOf: naming.PartOf, naming.LabelInstance: testRelease, naming.LabelSessionID: "session_01TEST"},
	}}
	c := newFakeClient(t, sb)
	cfg := testConfig(writeTemplate(t, testTemplate))
	err := run(t, c, cfg, testRequest("order-1", 1))
	if ExitCodeFor(err) != ExitRetryable {
		t.Fatalf("want exit 1, got %d (%v)", ExitCodeFor(err), err)
	}
	if n := len(listSecrets(t, c)); n != 0 {
		t.Errorf("no secret may be created for a deleting sandbox: %d", n)
	}
}

func TestIdentityMismatchIsNonRetryable(t *testing.T) {
	sb := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: naming.SandboxName(testRelease, "session_01TEST"), Namespace: "runners",
		Labels: map[string]string{naming.LabelPartOf: naming.PartOf, naming.LabelInstance: "other-release", naming.LabelSessionID: "session_01TEST"},
	}}
	c := newFakeClient(t, sb)
	err := run(t, c, testConfig(writeTemplate(t, testTemplate)), testRequest("order-1", 1))
	if ExitCodeFor(err) != ExitNonRetryable {
		t.Fatalf("want exit 2, got %d (%v)", ExitCodeFor(err), err)
	}
}

func TestMissingAnchorFailsBeforeAnyWrite(t *testing.T) {
	c := newFakeClient(t)
	broken := strings.Replace(testTemplate, "- name: runner", "- name: claude", 1)
	err := run(t, c, testConfig(writeTemplate(t, broken)), testRequest("order-1", 1))
	if ExitCodeFor(err) != ExitNonRetryable || !strings.Contains(err.Error(), `container named "runner"`) {
		t.Fatalf("want exit 2 naming the anchor, got %d: %v", ExitCodeFor(err), err)
	}
	l := &sandboxv1beta1.SandboxList{}
	if err := c.List(context.Background(), l); err != nil || len(l.Items) != 0 {
		t.Errorf("nothing may be created: %v %d", err, len(l.Items))
	}
	if n := len(listSecrets(t, c)); n != 0 {
		t.Errorf("no secret may be created: %d", n)
	}
}

func TestUnknownTemplateFieldIsRejected(t *testing.T) {
	c := newFakeClient(t)
	bad := strings.Replace(testTemplate, "  operatingMode: Suspended\n", "  operatingMode: Suspended\n  autoSuspension: {}\n", 1)
	err := run(t, c, testConfig(writeTemplate(t, bad)), testRequest("order-1", 1))
	if ExitCodeFor(err) != ExitNonRetryable {
		t.Fatalf("unknown field must be exit 2: %v", err)
	}
}

func TestExistingSecretWithDifferentDataIsNonRetryable(t *testing.T) {
	c := newFakeClient(t)
	cfg := testConfig(writeTemplate(t, testTemplate))
	if err := run(t, c, cfg, testRequest("order-1", 1)); err != nil {
		t.Fatal(err)
	}
	req := testRequest("order-1", 1)
	req.WorkOrder = []byte("tampered")
	err := run(t, c, cfg, req)
	if ExitCodeFor(err) != ExitNonRetryable {
		t.Fatalf("want exit 2, got %d (%v)", ExitCodeFor(err), err)
	}
	if strings.Contains(err.Error(), "tampered") || strings.Contains(err.Error(), "jwt-") {
		t.Error("error must not include secret data")
	}
}

func TestConflictIsRereadAndRecomputed(t *testing.T) {
	base := newFakeClient(t)
	cfg := testConfig(writeTemplate(t, testTemplate))
	if err := run(t, base, cfg, testRequest("order-1", 1)); err != nil {
		t.Fatal(err)
	}
	conflicts := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			if conflicts < 2 {
				conflicts++
				return apierrors.NewConflict(schema.GroupResource{Group: "agents.x-k8s.io", Resource: "sandboxes"}, obj.GetName(), errors.New("injected"))
			}
			return cl.Patch(ctx, obj, p, opts...)
		},
	})
	if err := run(t, c, cfg, testRequest("order-2", 2)); err != nil {
		t.Fatalf("hook must recover from conflicts: %v", err)
	}
	if conflicts != 2 {
		t.Errorf("expected 2 injected conflicts, got %d", conflicts)
	}
	sb := getSandbox(t, base, "session_01TEST")
	if sb.Annotations[naming.AnnotationPendingSpawn] != "order-2" {
		t.Error("newer order not published after conflict")
	}
	if n := len(listSecrets(t, base)); n != 2 {
		t.Errorf("secret must be created exactly once per order: %d", n)
	}
}

func TestStandbyOrderIsNonRetryable(t *testing.T) {
	c := newFakeClient(t)
	cfg := testConfig(writeTemplate(t, testTemplate))
	req := Request{OrderID: "pw-order", Attempt: 0, WorkOrder: []byte("jwt-pw")}
	err := run(t, c, cfg, req)
	if ExitCodeFor(err) != ExitNonRetryable {
		t.Fatalf("an order without a session id must exit non-retryable, got %v", err)
	}
	if n := len(listSecrets(t, c)); n != 0 {
		t.Errorf("no Secret may be written for a standby order, got %d", n)
	}
}

func TestRequestFromEnv(t *testing.T) {
	env := map[string]string{
		EnvOrderID: "ord_1", EnvSessionID: "session_1", EnvAttempt: "1", EnvWorkOrderFile: "/x", EnvAccountID: "user_1", EnvAccountEmail: "someone@example.invalid",
	}
	read := func(string) ([]byte, error) { return []byte("  jwt \n"), nil }
	r, err := RequestFromEnv(func(k string) string { return env[k] }, read)
	if err != nil || string(r.WorkOrder) != "jwt" || r.Attempt != 1 {
		t.Fatalf("parse: %v %+v", err, r)
	}
	env[EnvAttempt] = "0"
	if r, err := RequestFromEnv(func(k string) string { return env[k] }, read); err != nil || r.Attempt != 0 || r.SessionID == "" {
		t.Errorf("a session's first spawn request carries attempt 0: %v %+v", err, r)
	}
	env[EnvSessionID] = ""
	if r, err := RequestFromEnv(func(k string) string { return env[k] }, read); err != nil || r.SessionID != "" {
		t.Errorf("an empty session id must parse (standby order): %v", err)
	}
	env[EnvOrderID] = "bad/name"
	if _, err := RequestFromEnv(func(k string) string { return env[k] }, read); err == nil {
		t.Error("order id with a slash must be rejected")
	}
}

// capTemplate is a Sandbox of another session counting against the cap.
func otherSandbox(name string, mode sandboxv1beta1.SandboxOperatingMode, pending string) *sandboxv1beta1.Sandbox {
	sb := &sandboxv1beta1.Sandbox{}
	sb.Name = name
	sb.Namespace = "runners"
	sb.UID = types.UID("uid-" + name)
	sb.Labels = map[string]string{
		naming.LabelName:     naming.ComponentRunner,
		naming.LabelInstance: testRelease,
		naming.LabelPartOf:   naming.PartOf,
	}
	if pending != "" {
		sb.Annotations = map[string]string{naming.AnnotationPendingSpawn: pending}
	}
	sb.Spec.OperatingMode = mode
	return sb
}

func TestCapRejectsNewSessionRetryable(t *testing.T) {
	c := newFakeClient(t,
		otherSandbox("running", sandboxv1beta1.SandboxOperatingModeRunning, ""),
		otherSandbox("waiting", sandboxv1beta1.SandboxOperatingModeSuspended, "o-w"),
		otherSandbox("asleep", sandboxv1beta1.SandboxOperatingModeSuspended, ""),
	)
	cfg := testConfig(writeTemplate(t, testTemplate))
	cfg.MaxActiveSessions = 2
	err := run(t, c, cfg, testRequest("order-1", 1))
	if ExitCodeFor(err) != ExitRetryable || !strings.Contains(err.Error(), "at capacity: 2 of 2") {
		t.Fatalf("at the cap the hook must exit retryable naming the counts, got %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "runners", Name: naming.SandboxName(testRelease, "session_01TEST")}, &sandboxv1beta1.Sandbox{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a rejected order must create no Sandbox, got %v", err)
	}
	if n := len(listSecrets(t, c)); n != 0 {
		t.Errorf("a rejected order must create no Secret, got %d", n)
	}

	cfg.MaxActiveSessions = 3
	if err := run(t, c, cfg, testRequest("order-1", 1)); err != nil {
		t.Fatalf("below the cap the same order must be accepted: %v", err)
	}
}

func TestCapIgnoresSleepingDeletingAndForeignSandboxes(t *testing.T) {
	deleting := otherSandbox("deleting", sandboxv1beta1.SandboxOperatingModeRunning, "")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"test/hold"}
	foreign := otherSandbox("foreign", sandboxv1beta1.SandboxOperatingModeRunning, "")
	foreign.Labels[naming.LabelInstance] = "other-release"
	c := newFakeClient(t,
		otherSandbox("asleep", sandboxv1beta1.SandboxOperatingModeSuspended, ""),
		deleting, foreign,
	)
	cfg := testConfig(writeTemplate(t, testTemplate))
	cfg.MaxActiveSessions = 1
	if err := run(t, c, cfg, testRequest("order-1", 1)); err != nil {
		t.Fatalf("sleeping, deleting and foreign Sandboxes must not count: %v", err)
	}
}

func TestCapExcludesOwnSandboxAndPassesRedelivery(t *testing.T) {
	c := newFakeClient(t, otherSandbox("running", sandboxv1beta1.SandboxOperatingModeRunning, ""))
	cfg := testConfig(writeTemplate(t, testTemplate))
	cfg.MaxActiveSessions = 2
	if err := run(t, c, cfg, testRequest("order-1", 1)); err != nil {
		t.Fatalf("first order: %v", err)
	}
	cfg.MaxActiveSessions = 1
	// Redelivery of the accepted order is already committed: exit 0 at the cap.
	if err := run(t, c, cfg, testRequest("order-1", 1)); err != nil {
		t.Fatalf("redelivery at the cap must exit 0: %v", err)
	}
	// A bounce onto the session's own slot: its Sandbox is Running.
	sb := getSandbox(t, c, "session_01TEST")
	sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	delete(sb.Annotations, naming.AnnotationPendingSpawn)
	if err := c.Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	cfg.MaxActiveSessions = 1
	if err := run(t, c, cfg, testRequest("order-2", 2)); err != nil {
		t.Fatalf("a newer order for a session holding a slot must be accepted: %v", err)
	}
	// Now the other Running Sandbox alone fills the cap and this session is asleep.
	sb = getSandbox(t, c, "session_01TEST")
	sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	delete(sb.Annotations, naming.AnnotationPendingSpawn)
	if err := c.Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	err := run(t, c, cfg, testRequest("order-3", 3))
	if ExitCodeFor(err) != ExitRetryable {
		t.Fatalf("a newer order for a sleeping session at the cap must exit retryable, got %v", err)
	}
	if got := getSandbox(t, c, "session_01TEST").Annotations[naming.AnnotationLastOrderID]; got != "order-2" {
		t.Errorf("a rejected order must leave intent untouched, last-order-id = %s", got)
	}
}

func TestConfigFromEnvMaxActive(t *testing.T) {
	base := map[string]string{EnvShockRelease: "r", EnvShockNamespace: "n"}
	get := func(extra map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := extra[k]; ok {
				return v
			}
			return base[k]
		}
	}
	if c, err := ConfigFromEnv(get(nil)); err != nil || c.MaxActiveSessions != 0 {
		t.Fatalf("default must be unlimited: %+v %v", c, err)
	}
	if c, err := ConfigFromEnv(get(map[string]string{EnvShockMaxActive: "3"})); err != nil || c.MaxActiveSessions != 3 {
		t.Fatalf("want 3: %+v %v", c, err)
	}
	if _, err := ConfigFromEnv(get(map[string]string{EnvShockMaxActive: "-1"})); err == nil {
		t.Fatal("negative cap must be rejected")
	}
}
