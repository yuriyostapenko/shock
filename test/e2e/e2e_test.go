//go:build e2e

// Package e2e runs against a kind cluster with the pinned agent-sandbox
// release installed (make e2e-setup). It installs the chart with a fake
// busybox runner, invokes `shock hook spawn-runner` directly (the fake
// control-plane stub) impersonating the orchestrator ServiceAccount, and
// asserts the lifecycle from spec section 12, including the agent-sandbox
// conformance items that pin upstream behavior and reason strings.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yuriyostapenko/shock/internal/hook"
	"github.com/yuriyostapenko/shock/internal/naming"
	"github.com/yuriyostapenko/shock/internal/sessioncontroller"
)

const (
	release = "shock-e2e"
	// A pod's exit is observed by the sandbox controller and Sleep must follow within this budget.
	sleepBudget = 90 * time.Second
	wakeBudget  = 3 * time.Minute
)

var (
	ns         string
	c          client.Client
	shockBin   string
	templateFn string
	hookKube   string
	repoRoot   string
)

func TestMain(m *testing.M) {
	ns = os.Getenv("E2E_NAMESPACE")
	if ns == "" {
		ns = "shock-e2e"
	}
	shockBin = os.Getenv("SHOCK_BIN")
	if shockBin == "" {
		fmt.Fprintln(os.Stderr, "SHOCK_BIN is required (path to the shock binary)")
		os.Exit(2)
	}
	var err error
	repoRoot, err = filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		panic(err)
	}
	cfg := ctrl.GetConfigOrDie()
	c, err = client.New(cfg, client.Options{Scheme: sessioncontroller.Scheme()})
	if err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func helmInstall(t *testing.T, extra ...string) {
	t.Helper()
	args := append([]string{"upgrade", "--install", release, "charts/shock", "-n", ns, "--create-namespace",
		"-f", "test/e2e/values.yaml", "--wait", "--timeout", "3m"}, extra...)
	run(t, "helm", args...)
}

// hookKubeconfig writes a kubeconfig impersonating the orchestrator SA so the
// hook runs with exactly the chart's Role.
func hookKubeconfig(t *testing.T) string {
	t.Helper()
	if hookKube != "" {
		return hookKube
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	raw, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctxName := raw.CurrentContext
	if ctxName == "" {
		t.Fatal("no current kube context")
	}
	kctx := raw.Contexts[ctxName]
	user := raw.AuthInfos[kctx.AuthInfo]
	user.Impersonate = fmt.Sprintf("system:serviceaccount:%s:%s-orchestrator", ns, release)
	user.ImpersonateGroups = []string{"system:serviceaccounts", "system:serviceaccounts:" + ns}
	// Not t.TempDir(): the file must outlive the test that first needed it.
	dir, err := os.MkdirTemp("", "shock-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "hook.kubeconfig")
	if err := clientcmd.WriteToFile(*raw, p); err != nil {
		t.Fatal(err)
	}
	hookKube = p
	return p
}

// templatePath extracts the rendered sandbox template from the live ConfigMap,
// exactly what the orchestrator pod sees at /etc/shock.
func templatePath(t *testing.T) string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: release + "-sandbox-template"}, cm); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "shock-e2e-tmpl-")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "sandbox-template.yaml")
	if err := os.WriteFile(p, []byte(cm.Data["sandbox-template.yaml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type order struct {
	id, session string
	attempt     int
	sleep, exit int
}

func (o order) workOrder() string {
	return fmt.Sprintf("SLEEP=%d EXIT=%d ORDER=%s", o.sleep, o.exit, o.id)
}

// spawn invokes the hook binary like the orchestrator would and returns the exit code.
func spawn(t *testing.T, o order, tmpl string) (int, string) {
	t.Helper()
	wo := filepath.Join(t.TempDir(), "work-order")
	if err := os.WriteFile(wo, []byte(o.workOrder()), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shockBin, "hook", "spawn-runner")
	cmd.Env = append(os.Environ(),
		"KUBECONFIG="+hookKubeconfig(t),
		"SHOCK_RELEASE="+release,
		"SHOCK_NAMESPACE="+ns,
		"SHOCK_TEMPLATE_PATH="+tmpl,
		"SHOCK_RUNNER_BASE_DIR=/workspace",
		"SHOCK_RUNNER_TERMINATION_GRACE_PERIOD_SECONDS=20",
		"SHOCK_HOOK_TIMEOUT_SECONDS=30",
		hook.EnvWorkOrderFile+"="+wo,
		hook.EnvOrderID+"="+o.id,
		hook.EnvSessionID+"="+o.session,
		hook.EnvAttempt+"="+fmt.Sprint(o.attempt),
		hook.EnvAccountID+"=user_e2e",
		hook.EnvAccountEmail+"=nobody@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running hook: %v", err)
		}
	}
	if strings.Contains(string(out), "SLEEP=") || strings.Contains(string(out), "example.invalid") {
		t.Fatalf("hook output leaked the work order or the account email:\n%s", out)
	}
	return code, string(out)
}

func mustSpawn(t *testing.T, o order) {
	t.Helper()
	code, out := spawn(t, o, templateFn)
	if code != 0 {
		t.Fatalf("hook exit %d for %s:\n%s", code, o.id, out)
	}
}

func sandbox(t *testing.T, session string) *sandboxv1beta1.Sandbox {
	t.Helper()
	sb := &sandboxv1beta1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: naming.SandboxName(session)}, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	return sb
}

func ownedPods(t *testing.T, sb *sandboxv1beta1.Sandbox) []corev1.Pod {
	t.Helper()
	list := &corev1.PodList{}
	if err := c.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	var out []corev1.Pod
	for _, p := range list.Items {
		if ref := metav1.GetControllerOf(&p); ref != nil && ref.UID == sb.UID {
			out = append(out, p)
		}
	}
	return out
}

// updateSpec re-reads and retries on conflict: the upstream controller writes
// status concurrently, so a plain Update on a stale read races with it.
func updateSpec(t *testing.T, key types.NamespacedName, mutate func(*sandboxv1beta1.Sandbox)) {
	t.Helper()
	for i := 0; i < 20; i++ {
		sb := &sandboxv1beta1.Sandbox{}
		if err := c.Get(context.Background(), key, sb); err != nil {
			t.Fatal(err)
		}
		mutate(sb)
		err := c.Update(context.Background(), sb)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("updateSpec: persistent conflicts")
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ok, detail := cond()
		if ok {
			return
		}
		last = detail
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out waiting for %s (last: %s)", what, last)
}

func condIs(sb *sandboxv1beta1.Sandbox, typ sandboxv1beta1.ConditionType, status metav1.ConditionStatus, reason string) bool {
	cnd := meta.FindStatusCondition(sb.Status.Conditions, string(typ))
	return cnd != nil && cnd.Status == status && cnd.Reason == reason && cnd.ObservedGeneration == sb.Generation
}

func describe(sb *sandboxv1beta1.Sandbox) string {
	if sb == nil {
		return "<absent>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "mode=%s gen=%d ann=%v conds=", sb.Spec.OperatingMode, sb.Generation, filterAnn(sb.Annotations))
	for _, cnd := range sb.Status.Conditions {
		fmt.Fprintf(&b, "%s=%s/%s@%d ", cnd.Type, cnd.Status, cnd.Reason, cnd.ObservedGeneration)
	}
	return b.String()
}

func filterAnn(a map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		if strings.HasPrefix(k, naming.AnnotationPrefix) {
			out[strings.TrimPrefix(k, naming.AnnotationPrefix)] = v
		}
	}
	return out
}

func waitAsleep(t *testing.T, session string, budget time.Duration) *sandboxv1beta1.Sandbox {
	t.Helper()
	var sb *sandboxv1beta1.Sandbox
	waitFor(t, session+" asleep", budget, func() (bool, string) {
		sb = sandbox(t, session)
		if sb == nil {
			return false, "absent"
		}
		ok := sb.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended &&
			condIs(sb, sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated) &&
			sb.Annotations[naming.AnnotationPendingSpawn] == "" && len(ownedPods(t, sb)) == 0
		return ok, describe(sb)
	})
	return sb
}

// waitObserved waits until pending-spawn is cleared for the given order and returns the owned Pod.
func waitObserved(t *testing.T, session, orderID string, budget time.Duration) corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	waitFor(t, session+" spawn observed for "+orderID, budget, func() (bool, string) {
		sb := sandbox(t, session)
		if sb == nil {
			return false, "absent"
		}
		if sb.Annotations[naming.AnnotationPendingSpawn] != "" || sb.Annotations[naming.AnnotationAppliedSpawn] != orderID {
			return false, describe(sb)
		}
		pods := ownedPods(t, sb)
		for _, p := range pods {
			if p.Annotations[naming.AnnotationOrderID] == orderID {
				pod = p
				return true, ""
			}
		}
		return false, fmt.Sprintf("no pod for order (%d pods)", len(pods))
	})
	return pod
}

// readMarks mounts the Sandbox's workspace PVC in a short-lived reader pod
// after the runner is gone and returns the fake runner's log.
func readMarks(t *testing.T, sandboxName string) string {
	t.Helper()
	name := "reader-" + strings.TrimPrefix(sandboxName, "cs-")
	uid := int64(65534)
	nonRoot := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid, RunAsNonRoot: &nonRoot,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: "r", Image: "docker.io/library/busybox:1.38",
				Command:      []string{"sh", "-c", "cat /workspace/marks/log; echo; cat /workspace/marks/last-order"},
				VolumeMounts: []corev1.VolumeMount{{Name: "w", MountPath: "/workspace"}}}},
			Volumes: []corev1.Volume{{Name: "w", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: naming.PVCName(sandboxName)}}}},
		},
	}
	_ = c.Delete(context.Background(), pod)
	waitFor(t, "old reader gone", time.Minute, func() (bool, string) {
		err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{})
		return apierrors.IsNotFound(err), "still present"
	})
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Delete(context.Background(), pod) }()
	waitFor(t, "reader pod done", 2*time.Minute, func() (bool, string) {
		p := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), p); err != nil {
			return false, err.Error()
		}
		return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed, string(p.Status.Phase)
	})
	return run(t, "kubectl", "-n", ns, "logs", name)
}

func secretsOwnedBy(t *testing.T, uid types.UID) []corev1.Secret {
	t.Helper()
	list := &corev1.SecretList{}
	if err := c.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	var out []corev1.Secret
	for _, s := range list.Items {
		for _, ref := range s.OwnerReferences {
			if ref.UID == uid {
				out = append(out, s)
			}
		}
	}
	return out
}

// TestA_Install installs the chart and verifies the controller becomes ready.
func TestA_Install(t *testing.T) {
	if err := exec.Command("kubectl", "get", "ns", ns).Run(); err != nil {
		run(t, "kubectl", "create", "namespace", ns)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "e2e-environment"}, &corev1.Secret{}); apierrors.IsNotFound(err) {
		run(t, "kubectl", "-n", ns, "create", "secret", "generic", "e2e-environment", "--from-literal=environment-secret=not-a-real-secret")
	}
	helmInstall(t)
	run(t, "kubectl", "-n", ns, "rollout", "status", "deploy/"+release+"-session-controller", "--timeout=120s")
	templateFn = templatePath(t)
	// The template must strict-decode: the same check the hook does.
	if _, err := hook.LoadTemplate(templateFn); err != nil {
		t.Fatal(err)
	}
}

// TestB_Conformance pins the agent-sandbox behaviors SHOCK depends on, with
// a plain Sandbox driven directly (no hook, no session controller involved).
func TestB_Conformance(t *testing.T) {
	tmpl, err := hook.LoadTemplate(templateFn)
	if err != nil {
		t.Fatal(err)
	}
	if err := hook.ApplyContract(tmpl, hook.Identity{Release: "conformance", Namespace: ns, SessionID: "conf", AccountID: ""}, hook.Contract{BaseDir: "/workspace", TerminationGracePeriodSeconds: 20}); err != nil {
		// The template carries the e2e release label; conformance uses its own identity.
		if !strings.Contains(err.Error(), "does not match") {
			t.Fatal(err)
		}
	}
	tmpl.Labels[naming.LabelInstance] = "conformance"
	tmpl.Spec.PodTemplate.ObjectMeta.Labels[naming.LabelInstance] = "conformance"
	if err := hook.ApplyContract(tmpl, hook.Identity{Release: "conformance", Namespace: ns, SessionID: "conf", AccountID: ""}, hook.Contract{BaseDir: "/workspace", TerminationGracePeriodSeconds: 20}); err != nil {
		t.Fatal(err)
	}
	// A synthetic work-order Secret, since there is no hook here.
	imm := true
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "conformance-wo", Namespace: ns}, Immutable: &imm,
		Data: map[string][]byte{"work-order": []byte("SLEEP=5 EXIT=0 ORDER=conf-1")}}
	_ = c.Delete(context.Background(), sec)
	waitFor(t, "old conformance secret gone", time.Minute, func() (bool, string) {
		return apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(sec), &corev1.Secret{})), ""
	})
	if err := c.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), sec) })
	tmpl.Spec.PodTemplate.Spec.Volumes[0].Secret.SecretName = sec.Name
	tmpl.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	tmpl.Name = "cs-conformance"
	_ = c.Delete(context.Background(), tmpl)
	waitFor(t, "old conformance sandbox gone", 2*time.Minute, func() (bool, string) {
		return apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(tmpl), &sandboxv1beta1.Sandbox{})), ""
	})
	if err := c.Create(context.Background(), tmpl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), tmpl) })
	key := client.ObjectKeyFromObject(tmpl)
	get := func() *sandboxv1beta1.Sandbox {
		sb := &sandboxv1beta1.Sandbox{}
		if err := c.Get(context.Background(), key, sb); err != nil {
			t.Fatal(err)
		}
		return sb
	}

	// (6) a Running Sandbox carries Suspended with status False / NotSuspended.
	waitFor(t, "Suspended=False/NotSuspended while Running", wakeBudget, func() (bool, string) {
		sb := get()
		return condIs(sb, sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonNotSuspended), describe(sb)
	})
	// (1) a pod reaching Succeeded under Running is left alone; Finished=True/PodSucceeded appears.
	var firstPodUID types.UID
	waitFor(t, "Finished=True/PodSucceeded", wakeBudget, func() (bool, string) {
		sb := get()
		pods := ownedPods(t, sb)
		if len(pods) == 1 {
			firstPodUID = pods[0].UID
		}
		return condIs(sb, sandboxv1beta1.SandboxConditionFinished, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonPodSucceeded) &&
			condIs(sb, sandboxv1beta1.SandboxConditionReady, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonPodSucceeded), describe(sb)
	})
	time.Sleep(10 * time.Second)
	sb := get()
	pods := ownedPods(t, sb)
	if len(pods) != 1 || pods[0].UID != firstPodUID || pods[0].Status.Phase != corev1.PodSucceeded {
		t.Fatalf("terminal pod must be left alone under Running: %d pods", len(pods))
	}
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatal("nothing else may suspend the sandbox in this test")
	}
	// (7) conditions report the reconciled generation.
	for _, cnd := range sb.Status.Conditions {
		if cnd.ObservedGeneration != sb.Generation {
			t.Errorf("condition %s observedGeneration %d != generation %d", cnd.Type, cnd.ObservedGeneration, sb.Generation)
		}
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: naming.PVCName(tmpl.Name)}, pvc); err != nil {
		t.Fatalf("PVC: %v", err)
	}
	pvcUID := pvc.UID
	if ref := metav1.GetControllerOf(pvc); ref == nil || ref.UID != sb.UID {
		t.Error("PVC must be controller-owned by the Sandbox")
	}
	if pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("PVC access mode %v", pvc.Spec.AccessModes)
	}

	// (2) patching Suspended deletes the terminal pod, Suspended=True/PodTerminated, PVC intact; (4) Finished disappears.
	gen := sb.Generation
	updateSpec(t, key, func(sb *sandboxv1beta1.Sandbox) { sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended })
	waitFor(t, "Suspended=True/PodTerminated", wakeBudget, func() (bool, string) {
		sb := get()
		return condIs(sb, sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated) &&
			len(ownedPods(t, sb)) == 0, describe(sb)
	})
	sb = get()
	if sb.Generation <= gen {
		t.Error("spec change must bump generation")
	}
	if meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished)) != nil {
		t.Error("(4) Finished must disappear after suspension")
	}
	if !condIs(sb, sandboxv1beta1.SandboxConditionReady, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonSuspended) {
		t.Errorf("(5) Ready=False reason while suspended must be %s: %s", sandboxv1beta1.SandboxReasonSuspended, describe(sb))
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), pvc); err != nil || pvc.UID != pvcUID {
		t.Fatalf("PVC must survive suspension: %v", err)
	}

	// (3) patching back to Running recreates the pod and remounts the same PVC; the marker file persists.
	updateSpec(t, key, func(sb *sandboxv1beta1.Sandbox) { sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning })
	waitFor(t, "second pod finished", wakeBudget, func() (bool, string) {
		sb := get()
		pods := ownedPods(t, sb)
		return len(pods) == 1 && pods[0].UID != firstPodUID && pods[0].Status.Phase == corev1.PodSucceeded, describe(sb)
	})
	sb = get()
	if p := ownedPods(t, sb)[0]; p.Spec.Volumes[len(p.Spec.Volumes)-1].PersistentVolumeClaim == nil {
		// The PVC volume is derived from volumeClaimTemplates.
		found := false
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == pvc.Name {
				found = true
			}
		}
		if !found {
			t.Error("recreated pod does not mount the session PVC")
		}
	}
	updateSpec(t, key, func(sb *sandboxv1beta1.Sandbox) { sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended })
	waitFor(t, "suspended again", wakeBudget, func() (bool, string) {
		sb := get()
		return condIs(sb, sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated) && len(ownedPods(t, sb)) == 0, describe(sb)
	})
	marks := readMarks(t, tmpl.Name)
	if strings.Count(marks, "ORDER=conf-1") != 2 {
		t.Fatalf("expected two runs recorded on the same disk, got:\n%s", marks)
	}

	// (5) PodTerminating while a pod ignores SIGTERM, and MultiplePods when a second owned pod exists.
	updateSpec(t, key, func(sb *sandboxv1beta1.Sandbox) {
		sb.Spec.PodTemplate.Spec.Containers[0].Args = []string{"trap '' TERM; i=0; while [ $i -lt 600 ]; do sleep 1; i=$((i+1)); done"}
		sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	})
	waitFor(t, "stubborn pod running", wakeBudget, func() (bool, string) {
		sb := get()
		pods := ownedPods(t, sb)
		return len(pods) == 1 && pods[0].Status.Phase == corev1.PodRunning, describe(sb)
	})
	sb = get()
	// Second owned pod: same controller ownerRef plus the upstream tracking label read from status.selector.
	kv := strings.SplitN(sb.Status.LabelSelector, "=", 2)
	if len(kv) != 2 {
		t.Fatalf("status.selector not populated: %q", sb.Status.LabelSelector)
	}
	ctrlTrue := true
	extra := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cs-conformance-extra", Namespace: ns, Labels: map[string]string{kv[0]: kv[1]},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: sandboxv1beta1.GroupVersion.String(), Kind: "Sandbox", Name: sb.Name, UID: sb.UID, Controller: &ctrlTrue}}},
		Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "x", Image: "docker.io/library/busybox:1.38", Command: []string{"sleep", "300"}}}}}
	if err := c.Create(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Ready=False/MultiplePods", wakeBudget, func() (bool, string) {
		sb := get()
		return condIs(sb, sandboxv1beta1.SandboxConditionReady, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonMultiplePods), describe(sb)
	})
	if err := c.Delete(context.Background(), extra, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "extra pod gone", time.Minute, func() (bool, string) {
		return apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(extra), &corev1.Pod{})), ""
	})
	updateSpec(t, key, func(sb *sandboxv1beta1.Sandbox) { sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended })
	waitFor(t, "Suspended=False/PodTerminating", time.Minute, func() (bool, string) {
		sb := get()
		return condIs(sb, sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonSuspendedPodTerminating), describe(sb)
	})
	waitFor(t, "Suspended=True after grace", wakeBudget, func() (bool, string) {
		sb := get()
		return condIs(sb, sandboxv1beta1.SandboxConditionSuspended, metav1.ConditionTrue, sandboxv1beta1.SandboxReasonSuspendedPodTerminated), describe(sb)
	})
}

// TestC_FreshSessionSleepResumeCrash runs the core lifecycle through the hook and controller.
func TestC_FreshSessionSleepResumeCrash(t *testing.T) {
	session := "session_e2e_core"
	mustSpawn(t, order{id: "core-1", session: session, attempt: 1, sleep: 8, exit: 0})
	sb := sandbox(t, session)
	if sb == nil {
		t.Fatal("hook did not create the sandbox")
	}
	// The hook creates it Suspended; the session controller may already have
	// issued Wake by the time we look, which is fine as long as it did so for
	// this order and the pending intent is still held until observation.
	if applied := sb.Annotations[naming.AnnotationAppliedSpawn]; applied != "" && applied != "core-1" {
		t.Fatalf("unexpected applied-spawn: %s", describe(sb))
	}
	if sb.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeRunning && sb.Annotations[naming.AnnotationAppliedSpawn] != "core-1" {
		t.Fatalf("Running without applied-spawn: %s", describe(sb))
	}
	if sb.Annotations[naming.AnnotationPendingSpawn] != "core-1" || sb.Annotations[naming.AnnotationLastOrderAttempt] != "1" || sb.Annotations[naming.AnnotationPendingSecret] == "" {
		t.Fatalf("pending order/attempt/secret not published: %v", filterAnn(sb.Annotations))
	}
	secrets := secretsOwnedBy(t, sb.UID)
	if len(secrets) != 1 || secrets[0].Immutable == nil || !*secrets[0].Immutable || secrets[0].Name != sb.Annotations[naming.AnnotationPendingSecret] {
		t.Fatalf("expected one immutable owned secret, got %d", len(secrets))
	}
	firstSecret := secrets[0].Name
	pod := waitObserved(t, session, "core-1", wakeBudget)
	if got := pod.Spec.Volumes; len(got) == 0 {
		t.Fatal("pod has no volumes")
	}
	// Forced fields on the real pod.
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("forced fields missing on pod: restart=%s automount=%v", pod.Spec.RestartPolicy, pod.Spec.AutomountServiceAccountToken)
	}
	for _, v := range pod.Spec.Volumes {
		if v.Projected != nil && strings.Contains(v.Name, "kube-api-access") {
			t.Error("runner pod received a service-account token")
		}
	}
	if pod.Labels[naming.LabelName] != naming.ComponentRunner || pod.Labels[naming.LabelInstance] != release || pod.Labels[naming.LabelSessionID] == "" {
		t.Errorf("pod labels: %v", pod.Labels)
	}
	// On exit 0 the Sandbox is Suspended within the budget; PVC persists; Secret untouched.
	sb = waitAsleep(t, session, sleepBudget+30*time.Second)
	if sb.Annotations[naming.AnnotationLastSuspendedAt] == "" {
		t.Fatal("last-suspended-at not set by Sleep")
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: naming.PVCName(sb.Name)}, pvc); err != nil {
		t.Fatalf("PVC must persist: %v", err)
	}
	if pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("RWOP/RWO: PVC reports %v", pvc.Spec.AccessModes)
	}
	if s := secretsOwnedBy(t, sb.UID); len(s) != 1 || s[0].Name != firstSecret {
		t.Fatal("suspend must not touch the Secret")
	}

	// Redelivery of the acknowledged order creates no workload and no Secret.
	rv := sb.ResourceVersion
	mustSpawn(t, order{id: "core-1", session: session, attempt: 1, sleep: 8, exit: 0})
	time.Sleep(5 * time.Second)
	sb2 := sandbox(t, session)
	if sb2.ResourceVersion != rv || len(ownedPods(t, sb2)) != 0 || len(secretsOwnedBy(t, sb2.UID)) != 1 {
		t.Fatalf("redelivery re-armed the session: %s", describe(sb2))
	}

	// Resume: a new order reuses Sandbox and PVC; a new Pod UID appears; both marks present.
	mustSpawn(t, order{id: "core-2", session: session, attempt: 2, sleep: 5, exit: 1})
	pod2 := waitObserved(t, session, "core-2", wakeBudget)
	if pod2.UID == pod.UID {
		t.Fatal("resume must produce a new pod")
	}
	sb = sandbox(t, session)
	if len(secretsOwnedBy(t, sb.UID)) != 2 {
		t.Fatal("expected a second immutable Secret for the second order")
	}
	if sb.Spec.PodTemplate.Spec.Volumes[0].Secret.SecretName != sb.Annotations[naming.AnnotationLastOrderSecret] || sb.Spec.PodTemplate.Spec.Volumes[0].Secret.SecretName == firstSecret {
		t.Fatal("new pod must read its new immutable Secret")
	}
	// Crash: exit 1 leaves Finished=True/PodFailed, then Sleep follows.
	waitFor(t, "Finished=True/PodFailed", sleepBudget, func() (bool, string) {
		sb := sandbox(t, session)
		f := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished))
		return (f != nil && f.Reason == sandboxv1beta1.SandboxReasonPodFailed) || sb.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended, describe(sb)
	})
	waitAsleep(t, session, sleepBudget)
	marks := readMarks(t, naming.SandboxName(session))
	if !strings.Contains(marks, "ORDER=core-1") || !strings.Contains(marks, "ORDER=core-2") {
		t.Fatalf("workspace must carry both runs:\n%s", marks)
	}
	// Next spawn after a crash wakes cleanly.
	mustSpawn(t, order{id: "core-3", session: session, attempt: 3, sleep: 3, exit: 0})
	waitObserved(t, session, "core-3", wakeBudget)
	waitAsleep(t, session, sleepBudget)
	// helm upgrade with everything asleep is a no-op for sleeping sessions.
	before := sandbox(t, session).ResourceVersion
	helmInstall(t)
	time.Sleep(5 * time.Second)
	if sandbox(t, session).ResourceVersion != before {
		t.Fatal("helm upgrade touched a sleeping session")
	}
}

// TestD_SpawnAgainstRunningSession: a new order against a live pod bounces
// through Suspended without ever having two pods.
func TestD_SpawnAgainstRunningSession(t *testing.T) {
	session := "session_e2e_bounce"
	mustSpawn(t, order{id: "b-1", session: session, attempt: 1, sleep: 120, exit: 0})
	pod1 := waitObserved(t, session, "b-1", wakeBudget)
	waitFor(t, "pod running", time.Minute, func() (bool, string) {
		p := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(&pod1), p); err != nil {
			return false, err.Error()
		}
		return p.Status.Phase == corev1.PodRunning, string(p.Status.Phase)
	})
	mustSpawn(t, order{id: "b-2", session: session, attempt: 2, sleep: 3, exit: 0})
	sb := sandbox(t, session)
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended || sb.Annotations[naming.AnnotationPendingSpawn] != "b-2" || sb.Annotations[naming.AnnotationAppliedSpawn] != "b-1" {
		t.Fatalf("hook must request suspension and keep applied-spawn: %s", describe(sb))
	}
	var newUID types.UID
	waitFor(t, "bounce to a new pod", wakeBudget, func() (bool, string) {
		sb := sandbox(t, session)
		pods := ownedPods(t, sb)
		if len(pods) > 1 {
			t.Fatalf("two pods on one PVC: %d", len(pods))
		}
		if condIs(sb, sandboxv1beta1.SandboxConditionReady, metav1.ConditionFalse, sandboxv1beta1.SandboxReasonMultiplePods) {
			t.Fatal("MultiplePods appeared")
		}
		if sb.Annotations[naming.AnnotationPendingSpawn] == "" {
			if len(pods) != 1 || pods[0].UID == pod1.UID || pods[0].Annotations[naming.AnnotationOrderID] != "b-2" {
				t.Fatalf("pending cleared without a new pod for b-2: %d pods", len(pods))
			}
			newUID = pods[0].UID
			return true, ""
		}
		return false, describe(sb)
	})
	if newUID == "" {
		t.Fatal("no new pod")
	}
	waitAsleep(t, session, sleepBudget)
}

// TestE_ConcurrentSessionsAndAnchors: two sessions for one account are
// independent; a template without the runner anchor is exit 2 before any write.
func TestE_ConcurrentSessionsAndAnchors(t *testing.T) {
	mustSpawn(t, order{id: "c-a", session: "session_e2e_acc_a", attempt: 1, sleep: 2, exit: 0})
	mustSpawn(t, order{id: "c-b", session: "session_e2e_acc_b", attempt: 1, sleep: 2, exit: 0})
	a, b := sandbox(t, "session_e2e_acc_a"), sandbox(t, "session_e2e_acc_b")
	if a == nil || b == nil || a.UID == b.UID {
		t.Fatal("expected two independent sandboxes")
	}
	waitObserved(t, "session_e2e_acc_a", "c-a", wakeBudget)
	waitObserved(t, "session_e2e_acc_b", "c-b", wakeBudget)
	for _, s := range []string{"session_e2e_acc_a", "session_e2e_acc_b"} {
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: naming.PVCName(naming.SandboxName(s))}, &corev1.PersistentVolumeClaim{}); err != nil {
			t.Fatalf("PVC for %s: %v", s, err)
		}
	}
	waitAsleep(t, "session_e2e_acc_a", sleepBudget)
	waitAsleep(t, "session_e2e_acc_b", sleepBudget)

	raw, err := os.ReadFile(templateFn)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(t.TempDir(), "broken.yaml")
	// Rename the container (an indented list-entry line), not the name label.
	renamed := strings.Replace(string(raw), "\n        name: runner\n", "\n        name: claude\n", 1)
	if renamed == string(raw) {
		t.Fatal("test fixture: container entry not found in the rendered template")
	}
	if err := os.WriteFile(broken, []byte(renamed), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := spawn(t, order{id: "x-1", session: "session_e2e_broken", attempt: 1, sleep: 1, exit: 0}, broken)
	if code != 2 || !strings.Contains(out, `container named "runner"`) {
		t.Fatalf("anchor removal must exit 2 naming the anchor, got %d:\n%s", code, out)
	}
	if sandbox(t, "session_e2e_broken") != nil {
		t.Fatal("broken template created a sandbox")
	}
}

// TestF_GC: backdated idle sessions are collected with PVC and all Secrets;
// a woken session survives a sweep.
func TestF_GC(t *testing.T) {
	sb := sandbox(t, "session_e2e_acc_a")
	if sb == nil {
		t.Skip("depends on TestE")
	}
	uid := sb.UID
	pvcName := naming.PVCName(sb.Name)
	sb.Annotations[naming.AnnotationLastSuspendedAt] = time.Now().Add(-400 * time.Hour).UTC().Format(time.RFC3339)
	if err := c.Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sandbox garbage collected", 2*time.Minute, func() (bool, string) {
		return sandbox(t, "session_e2e_acc_a") == nil, describe(sandbox(t, "session_e2e_acc_a"))
	})
	waitFor(t, "PVC and secrets cascade", 2*time.Minute, func() (bool, string) {
		err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvcName}, &corev1.PersistentVolumeClaim{})
		return apierrors.IsNotFound(err) && len(secretsOwnedBy(t, uid)) == 0, fmt.Sprintf("pvc err=%v secrets=%d", err, len(secretsOwnedBy(t, uid)))
	})
	// Zero orphaned work-order Secrets: every wo- Secret has a live Sandbox owner.
	list := &corev1.SecretList{}
	if err := c.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	live := &sandboxv1beta1.SandboxList{}
	if err := c.List(context.Background(), live, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	uids := map[types.UID]bool{}
	for _, s := range live.Items {
		uids[s.UID] = true
	}
	for _, s := range list.Items {
		if !strings.HasPrefix(s.Name, "wo-") {
			continue
		}
		owned := false
		for _, ref := range s.OwnerReferences {
			if uids[ref.UID] {
				owned = true
			}
		}
		if !owned {
			t.Errorf("orphaned work-order secret %s", s.Name)
		}
	}

	// GC does not touch a woken session.
	sb = sandbox(t, "session_e2e_acc_b")
	sb.Annotations[naming.AnnotationLastSuspendedAt] = time.Now().Add(-400 * time.Hour).UTC().Format(time.RFC3339)
	if err := c.Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	mustSpawn(t, order{id: "c-b2", session: "session_e2e_acc_b", attempt: 2, sleep: 40, exit: 0})
	waitObserved(t, "session_e2e_acc_b", "c-b2", wakeBudget)
	// Force a sweep by touching the object.
	sb = sandbox(t, "session_e2e_acc_b")
	sb.Annotations["e2e/touch"] = time.Now().Format(time.RFC3339Nano)
	if err := c.Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Second)
	if sandbox(t, "session_e2e_acc_b") == nil {
		t.Fatal("GC deleted a woken session")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: naming.PVCName(naming.SandboxName("session_e2e_acc_b"))}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatal("PVC of a woken session lost")
	}
	waitAsleep(t, "session_e2e_acc_b", 2*time.Minute)
}

// TestG_HostilePodTemplate upgrades with an overlay that tries to break the
// lifecycle; the created pod must carry every forced field and still sleep.
func TestG_HostilePodTemplate(t *testing.T) {
	overlay := filepath.Join(t.TempDir(), "hostile.yaml")
	if err := os.WriteFile(overlay, []byte(`
runner:
  podTemplate:
    metadata:
      labels:
        app.kubernetes.io/name: hijacked
    spec:
      restartPolicy: Always
      automountServiceAccountToken: true
      terminationGracePeriodSeconds: 1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	helmInstall(t, "-f", overlay)
	t.Cleanup(func() { helmInstall(t) })
	hostileTmpl := templatePath(t)
	session := "session_e2e_hostile"
	code, out := spawn(t, order{id: "h-1", session: session, attempt: 1, sleep: 3, exit: 0}, hostileTmpl)
	if code != 0 {
		t.Fatalf("hook exit %d:\n%s", code, out)
	}
	pod := waitObserved(t, session, "h-1", wakeBudget)
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", pod.Spec.RestartPolicy)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken not forced")
	}
	if *pod.Spec.TerminationGracePeriodSeconds != 20 {
		t.Errorf("terminationGracePeriodSeconds = %d", *pod.Spec.TerminationGracePeriodSeconds)
	}
	if pod.Labels[naming.LabelName] != naming.ComponentRunner {
		t.Errorf("selector label hijacked: %v", pod.Labels)
	}
	waitAsleep(t, session, sleepBudget)
}
