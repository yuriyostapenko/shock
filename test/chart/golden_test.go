// Package chart renders charts/shock with helm and checks the pieces the Go
// compiler cannot see: the sandbox-template ConfigMap must strict-decode into
// the typed v1beta1 Sandbox and satisfy the template contract, forced fields
// must survive a hostile runner.podTemplate, and chart-level validation must
// fail loudly. Requires `helm` on PATH.
package chart

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	"github.com/yuriyostapenko/shock/internal/hook"
	"github.com/yuriyostapenko/shock/internal/naming"
)

const release = "golden"

func chartDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "charts", "shock"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func helmTemplate(t *testing.T, extra ...string) ([]unstructured.Unstructured, string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	args := append([]string{"template", release, chartDir(t), "-n", "runners", "--kube-version", "1.35.0",
		"-f", filepath.Join("..", "values", "minimal.yaml")}, extra...)
	cmd := exec.Command("helm", args...) //nolint:gosec // test helper; arguments are literals from this file
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, stderr.String(), err
	}
	var objs []unstructured.Unstructured
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var u unstructured.Unstructured
		if err := dec.Decode(&u); err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			t.Fatalf("decoding rendered manifests: %v", err)
		}
		if u.Object != nil && u.GetKind() != "" {
			objs = append(objs, u)
		}
	}
	return objs, stderr.String(), nil
}

func find(objs []unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for i := range objs {
		if objs[i].GetKind() == kind && objs[i].GetName() == name {
			return &objs[i]
		}
	}
	return nil
}

func sandboxTemplateRaw(t *testing.T, objs []unstructured.Unstructured) string {
	t.Helper()
	cm := find(objs, "ConfigMap", release+"-shock-sandbox-template")
	if cm == nil {
		t.Fatal("sandbox-template ConfigMap not rendered")
	}
	data, _, _ := unstructured.NestedString(cm.Object, "data", "sandbox-template.yaml")
	return data
}

func sandboxTemplate(t *testing.T, objs []unstructured.Unstructured) *sandboxv1beta1.Sandbox {
	t.Helper()
	cm := find(objs, "ConfigMap", release+"-shock-sandbox-template")
	if cm == nil {
		t.Fatal("sandbox-template ConfigMap not rendered")
	}
	data, _, _ := unstructured.NestedString(cm.Object, "data", "sandbox-template.yaml")
	sb, err := hook.ParseTemplate([]byte(data))
	if err != nil {
		t.Fatalf("rendered template does not strict-decode into v1beta1 Sandbox: %v\n%s", err, data)
	}
	return sb
}

func TestRenderedTemplateSatisfiesContract(t *testing.T) {
	objs, _, err := helmTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	sb := sandboxTemplate(t, objs)
	if err := hook.ValidateAnchors(sb); err != nil {
		t.Fatal(err)
	}
	if err := hook.ApplyContract(sb, hook.Identity{Release: release, Namespace: "runners", SessionID: "session_x", AccountID: "user_x"},
		hook.Contract{BaseDir: "/workspace", TerminationGracePeriodSeconds: 120}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{naming.LabelName, naming.LabelInstance, naming.LabelVersion, naming.LabelManagedBy, naming.LabelPartOf, naming.LabelChart} {
		if sb.Labels[want] == "" {
			t.Errorf("Sandbox label %s missing", want)
		}
		if sb.Spec.PodTemplate.ObjectMeta.Labels[want] == "" {
			t.Errorf("podTemplate label %s missing", want)
		}
	}
	if sb.Labels[naming.LabelInstance] != release || sb.Labels[naming.LabelName] != naming.ComponentRunner {
		t.Errorf("selector labels wrong: %v", sb.Labels)
	}
	if _, ok := sb.Labels["app.kubernetes.io/component"]; ok {
		t.Error("no component label by design")
	}
	args := strings.Join(sb.Spec.PodTemplate.Spec.Containers[0].Args, " ")
	for _, want := range []string{"--capacity 1", "--base-dir /workspace", "--environment-secret-file /var/run/claude/work-order/work-order",
		"--release-idle-session-min 30", "--kill-session-after-min 480", "--exit-if-unused-min 10", "--push-outcome-on-release", "--health-port 8080", "--lock-to-account user_x"} {
		if !strings.Contains(args, want) {
			t.Errorf("runner args missing %q: %s", want, args)
		}
	}
	if sb.Spec.ShutdownTime != nil || sb.Spec.ShutdownPolicy != nil {
		t.Error("shutdownTime/shutdownPolicy must never be set")
	}
	if len(sb.Spec.VolumeClaimTemplates) != 1 || sb.Spec.VolumeClaimTemplates[0].Name != "workspace" ||
		sb.Spec.VolumeClaimTemplates[0].Spec.AccessModes[0] != corev1.ReadWriteOncePod {
		t.Errorf("volumeClaimTemplates: %+v", sb.Spec.VolumeClaimTemplates)
	}
}

func TestHostilePodTemplateCannotBreakForcedFields(t *testing.T) {
	overlay := filepath.Join(t.TempDir(), "hostile.yaml")
	if err := os.WriteFile(overlay, []byte(`
runner:
  podTemplate:
    metadata:
      labels:
        app.kubernetes.io/name: hijacked
        extra: user-label
    spec:
      restartPolicy: Always
      automountServiceAccountToken: true
      terminationGracePeriodSeconds: 5
      containers:
        - name: runner
          image: example.invalid/other:1
          volumeMounts:
            - name: workspace
              mountPath: /elsewhere
      volumes:
        - name: workspace
          emptyDir: {}
        - name: work-order
          secret:
            secretName: attacker
`), 0o600); err != nil {
		t.Fatal(err)
	}
	objs, _, err := helmTemplate(t, "-f", overlay)
	if err != nil {
		t.Fatal(err)
	}
	sb := sandboxTemplate(t, objs)
	if err := hook.ApplyContract(sb, hook.Identity{Release: release, Namespace: "runners", SessionID: "session_x", AccountID: "user_x"},
		hook.Contract{BaseDir: "/workspace", TerminationGracePeriodSeconds: 120}); err != nil {
		t.Fatal(err)
	}
	ps := sb.Spec.PodTemplate.Spec
	if ps.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("restartPolicy not forced")
	}
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken not forced")
	}
	if *ps.TerminationGracePeriodSeconds != 120 {
		t.Error("terminationGracePeriodSeconds not forced")
	}
	labels := sb.Spec.PodTemplate.ObjectMeta.Labels
	if labels[naming.LabelName] != naming.ComponentRunner || labels["extra"] != "user-label" {
		t.Errorf("labels must merge with the selector set winning: %v", labels)
	}
	var ws string
	for _, m := range ps.Containers[0].VolumeMounts {
		if m.Name == "workspace" {
			ws = m.MountPath
		}
	}
	if ws != "/workspace" {
		t.Errorf("workspace mountPath = %q", ws)
	}
	for _, v := range ps.Volumes {
		if v.Name == "workspace" {
			t.Error("a user-supplied workspace volume must be dropped so the PVC is mounted")
		}
	}
	if ps.Containers[0].Image != "example.invalid/other:1" {
		t.Error("users keep everything else, including the image")
	}
}

func TestRemovedAnchorFailsLoudly(t *testing.T) {
	overlay := filepath.Join(t.TempDir(), "anchor.yaml")
	if err := os.WriteFile(overlay, []byte("runner:\n  podTemplate:\n    spec:\n      containers:\n        - name: claude\n          image: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	objs, _, err := helmTemplate(t, "-f", overlay)
	if err != nil {
		t.Fatal(err)
	}
	sb := sandboxTemplate(t, objs)
	err = hook.ValidateAnchors(sb)
	if err == nil || !strings.Contains(err.Error(), `container named "runner"`) {
		t.Fatalf("want anchor error, got %v", err)
	}
}

func TestChartValidation(t *testing.T) {
	_, stderr, err := helmTemplate(t, "--set", "orchestrator.hookTimeout=200")
	if err == nil || !strings.Contains(stderr, "hookTimeout") {
		t.Fatalf("hookTimeout + 5 >= expectedSpawnSeconds must fail: %v %s", err, stderr)
	}
	_, stderr, err = helmTemplate(t, "--set", "environment.existingSecret=")
	if err == nil || !strings.Contains(stderr, "environment.existingSecret") {
		t.Fatalf("missing environment secret must fail: %v %s", err, stderr)
	}
	_, stderr, err = helmTemplate(t, "--set", "network.mode=bogus")
	if err == nil {
		t.Fatalf("bogus network mode must fail: %s", stderr)
	}
	_, stderr, err = helmTemplate(t, "--set", "runner.storage.accessMode=ReadWriteMany")
	if err == nil {
		t.Fatalf("RWX must be rejected by the schema: %s", stderr)
	}
}

func TestEveryObjectCarriesCommonLabelsAndSelectors(t *testing.T) {
	for _, mode := range []string{"cilium", "kubernetes"} {
		objs, _, err := helmTemplate(t, "--set", "network.mode="+mode)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			l := o.GetLabels()
			for _, want := range []string{naming.LabelName, naming.LabelInstance, naming.LabelVersion, naming.LabelManagedBy, naming.LabelPartOf, naming.LabelChart} {
				if l[want] == "" {
					t.Errorf("%s %s (%s) missing label %s", o.GetKind(), o.GetName(), mode, want)
				}
			}
			if l[naming.LabelInstance] != release {
				t.Errorf("%s %s: instance label %q", o.GetKind(), o.GetName(), l[naming.LabelInstance])
			}
		}
		// Selectors carry instance and never version/chart.
		for _, o := range objs {
			var sel map[string]any
			switch o.GetKind() {
			case "Deployment":
				sel, _, _ = unstructured.NestedMap(o.Object, "spec", "selector", "matchLabels")
			case "PodDisruptionBudget":
				sel, _, _ = unstructured.NestedMap(o.Object, "spec", "selector", "matchLabels")
			case "NetworkPolicy":
				sel, _, _ = unstructured.NestedMap(o.Object, "spec", "podSelector", "matchLabels")
			case "CiliumNetworkPolicy":
				sel, _, _ = unstructured.NestedMap(o.Object, "spec", "endpointSelector", "matchLabels")
			default:
				continue
			}
			if sel[naming.LabelInstance] != release || sel[naming.LabelName] == nil {
				t.Errorf("%s %s selector lacks instance/name: %v", o.GetKind(), o.GetName(), sel)
			}
			for _, banned := range []string{naming.LabelVersion, naming.LabelChart, naming.LabelPartOf} {
				if _, ok := sel[banned]; ok {
					t.Errorf("%s %s selector must not include %s", o.GetKind(), o.GetName(), banned)
				}
			}
		}
	}
	// RBAC: the session controller never gets pod or secret delete; the hook never gets list/watch.
	objs, _, err := helmTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.GetKind() != "Role" {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(o.Object, "rules")
		for _, r := range rules {
			rm := r.(map[string]any)
			res, _, _ := unstructured.NestedStringSlice(rm, "resources")
			verbs, _, _ := unstructured.NestedStringSlice(rm, "verbs")
			joined := strings.Join(verbs, ",")
			for _, rs := range res {
				if (rs == "pods" || rs == "secrets") && strings.Contains(joined, "delete") {
					t.Errorf("%s grants delete on %s", o.GetName(), rs)
				}
				if strings.HasSuffix(o.GetName(), "-orchestrator") && (strings.Contains(joined, "list") || strings.Contains(joined, "watch")) {
					t.Errorf("hook role %s must not list/watch %s", o.GetName(), rs)
				}
			}
		}
	}
	// Nothing mounts at /hooks on the orchestrator.
	dep := find(objs, "Deployment", release+"-shock-orchestrator")
	containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	mounts, _, _ := unstructured.NestedSlice(containers[0].(map[string]any), "volumeMounts")
	for _, m := range mounts {
		if mp, _ := m.(map[string]any)["mountPath"].(string); strings.HasPrefix(mp, "/hooks") {
			t.Error("nothing may mount at /hooks; it would shadow the baked-in shim")
		}
	}
}

func TestOrchestratorImageFollowsChartVersion(t *testing.T) {
	// Defaults: repository from values, tag from appVersion, no digest.
	objs, _, err := helmTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	image := func(objs []unstructured.Unstructured, name string) string {
		dep := find(objs, "Deployment", release+"-shock-"+name)
		if dep == nil {
			t.Fatalf("deployment %s not rendered", name)
		}
		containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		img, _ := containers[0].(map[string]any)["image"].(string)
		return img
	}
	if got := image(objs, "orchestrator"); got != "ghcr.io/yuriyostapenko/shock:0.0.0-dev" {
		t.Errorf("default orchestrator image = %q", got)
	}
	if got := image(objs, "session-controller"); got != "ghcr.io/yuriyostapenko/shock:0.0.0-dev" {
		t.Errorf("default session-controller image = %q", got)
	}
	// A released chart: version and appVersion injected, digest pinned.
	digest := "sha256:" + strings.Repeat("ab", 32)
	objs, _, err = helmTemplate(t, "--set", "orchestrator.image.digest="+digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := image(objs, "orchestrator"); got != "ghcr.io/yuriyostapenko/shock:0.0.0-dev@"+digest {
		t.Errorf("pinned orchestrator image = %q", got)
	}
	// An explicit tag still wins over appVersion.
	objs, _, err = helmTemplate(t, "--set-string", "orchestrator.image.tag=custom")
	if err != nil {
		t.Fatal(err)
	}
	if got := image(objs, "orchestrator"); got != "ghcr.io/yuriyostapenko/shock:custom" {
		t.Errorf("explicit tag image = %q", got)
	}
	if got := find(objs, "Deployment", release+"-shock-orchestrator").GetLabels()[naming.LabelVersion]; got != "custom" {
		t.Errorf("version label = %q", got)
	}
	// The runner image defaults to the released runner image at appVersion and
	// fails loudly when the repository is emptied.
	objs, _, err = helmTemplate(t, "--set", "runner.image.digest="+digest)
	if err != nil {
		t.Fatal(err)
	}
	sb := sandboxTemplate(t, objs)
	if got := sb.Spec.PodTemplate.Spec.Containers[0].Image; got != "ghcr.io/yuriyostapenko/shock-runner:0.0.0-dev@"+digest {
		t.Errorf("default runner image = %q", got)
	}
	_, stderr, err := helmTemplate(t, "--set", "runner.image.repository=")
	if err == nil || !strings.Contains(stderr, "runner.image.repository") {
		t.Fatalf("an empty runner repository must fail: %v %s", err, stderr)
	}
	// hostUsers is rendered only when set.
	if strings.Contains(sandboxTemplateRaw(t, objs), "hostUsers") {
		t.Error("hostUsers must be absent when unset")
	}
	objs, _, err = helmTemplate(t, "--set", "runner.hostUsers=false")
	if err != nil {
		t.Fatal(err)
	}
	if sb := sandboxTemplate(t, objs); sb.Spec.PodTemplate.Spec.HostUsers == nil || *sb.Spec.PodTemplate.Spec.HostUsers {
		t.Error("runner.hostUsers=false must render hostUsers: false")
	}
	// A malformed digest is rejected by the schema.
	if _, _, err := helmTemplate(t, "--set", "orchestrator.image.digest=abc"); err == nil {
		t.Fatal("malformed digest must fail the schema")
	}
}
