// Package chart renders charts/shock with helm and checks what the compiler
// cannot: the template contract, forced fields, validation. Needs helm.
package chart

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sigsyaml "sigs.k8s.io/yaml"

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
		hook.Contract{WorkspaceMountPath: "/home/runner", BaseDir: "/home/runner/workspace", TerminationGracePeriodSeconds: 120}); err != nil {
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
	for _, want := range []string{"--capacity 1", "--base-dir /home/runner/workspace", "--environment-secret-file /var/run/claude/work-order/work-order",
		"--release-idle-session-min 30", "--kill-session-after-min 480", "--exit-if-unused-min 10", "--push-outcome-on-release", "--use-anthropic-git-proxy", "--health-port 8080", "--lock-to-account user_x"} {
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
		hook.Contract{WorkspaceMountPath: "/home/runner", BaseDir: "/home/runner/workspace", TerminationGracePeriodSeconds: 120}); err != nil {
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
	if ws != "/home/runner" {
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
	_, stderr, err = helmTemplate(t, "--set", "runner.baseDir=/elsewhere")
	if err == nil || !strings.Contains(stderr, "runner.baseDir") {
		t.Fatalf("baseDir outside the mount path must fail: %v %s", err, stderr)
	}
}

func TestCiliumRunnerPolicyEnforcesSNI(t *testing.T) {
	objs, _, err := helmTemplate(t, "--set", "network.allowedFQDNs={api.anthropic.com,*.githubusercontent.com}")
	if err != nil {
		t.Fatal(err)
	}
	cnp := find(objs, "CiliumNetworkPolicy", release+"-shock-runner")
	if cnp == nil {
		t.Fatal("runner CiliumNetworkPolicy not rendered")
	}
	egress, _, _ := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	var exact, wildcard bool
	for _, e := range egress {
		rule, _ := e.(map[string]any)
		fqdns, _, _ := unstructured.NestedSlice(rule, "toFQDNs")
		if len(fqdns) == 0 {
			continue
		}
		ports, _, _ := unstructured.NestedSlice(rule, "toPorts")
		names, _, _ := unstructured.NestedStringSlice(ports[0].(map[string]any), "serverNames")
		sel := fqdns[0].(map[string]any)
		switch {
		case sel["matchName"] == "api.anthropic.com":
			exact = len(names) == 1 && names[0] == "api.anthropic.com"
		case sel["matchPattern"] == "**.githubusercontent.com":
			wildcard = len(names) == 1 && names[0] == "**.githubusercontent.com"
		}
	}
	if !exact {
		t.Error("exact allowedFQDNs entry must carry serverNames with that host")
	}
	if !wildcard {
		t.Error("a leading *. must render as Cilium's multilevel **. in toFQDNs and serverNames")
	}
	objs, _, err = helmTemplate(t, "--set", "network.enforceSNI=false")
	if err != nil {
		t.Fatal(err)
	}
	if raw := fmt.Sprint(find(objs, "CiliumNetworkPolicy", release+"-shock-runner").Object); strings.Contains(raw, "serverNames") {
		t.Error("enforceSNI=false must render no serverNames")
	}
}

func fqdnPatterns(t *testing.T, objs []unstructured.Unstructured) map[string]int {
	t.Helper()
	cnp := find(objs, "CiliumNetworkPolicy", release+"-shock-runner")
	if cnp == nil {
		t.Fatal("runner CiliumNetworkPolicy not rendered")
	}
	egress, _, _ := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	out := map[string]int{}
	for _, e := range egress {
		fqdns, _, _ := unstructured.NestedSlice(e.(map[string]any), "toFQDNs")
		for _, f := range fqdns {
			for _, v := range f.(map[string]any) {
				out[v.(string)]++
			}
		}
	}
	return out
}

func TestAnthropicTrustedDomainsMergeAndToggle(t *testing.T) {
	objs, _, err := helmTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	pats := fqdnPatterns(t, objs)
	for _, want := range []string{"ghcr.io", "**.gcr.io", "api.nuget.org"} {
		if pats[want] == 0 {
			t.Errorf("default render must include Trusted host %q", want)
		}
	}
	for name, n := range pats {
		if n > 1 {
			t.Errorf("host %q rendered %d times; lists must be deduplicated", name, n)
		}
	}
	if _, _, err := helmTemplate(t, "--set", "network.anthropicTrustedDomains=false"); err == nil {
		t.Error("Trusted list off without api.anthropic.com in allowedFQDNs must fail to render")
	}
	objs, _, err = helmTemplate(t, "--set", "network.anthropicTrustedDomains=false", "--set", "network.allowedFQDNs={api.anthropic.com,mise-versions.jdx.dev}")
	if err != nil {
		t.Fatal(err)
	}
	pats = fqdnPatterns(t, objs)
	if pats["ghcr.io"] != 0 || pats["**.gcr.io"] != 0 {
		t.Error("anthropicTrustedDomains=false must render none of the Trusted list")
	}
	if pats["api.anthropic.com"] != 1 || pats["mise-versions.jdx.dev"] != 1 {
		t.Error("allowedFQDNs entries must remain when the Trusted list is off")
	}
}

func TestAllowedFQDNsAppendAndExclude(t *testing.T) {
	objs, _, err := helmTemplate(t,
		"--set", "network.extraAllowedFQDNs={git.example.internal,mise.jdx.dev}",
		"--set", "network.excludeFQDNs={*.amazonaws.com,registry.k8s.io}")
	if err != nil {
		t.Fatal(err)
	}
	pats := fqdnPatterns(t, objs)
	if pats["git.example.internal"] != 1 {
		t.Error("extraAllowedFQDNs entry must be appended")
	}
	if pats["mise.jdx.dev"] != 1 {
		t.Error("an extra that duplicates a default must collapse into one rule")
	}
	if pats["**.amazonaws.com"] != 0 {
		t.Error("excludeFQDNs must drop a Trusted-list wildcard written as in the list")
	}
	if pats["registry.k8s.io"] != 0 {
		t.Error("excludeFQDNs must drop a default allowedFQDNs entry")
	}
	if pats["api.anthropic.com"] != 1 || pats["ghcr.io"] != 1 {
		t.Error("unrelated entries must survive an exclude")
	}
	if _, _, err := helmTemplate(t, "--set", "network.excludeFQDNs={api.anthropic.com}"); err == nil {
		t.Error("excluding api.anthropic.com must fail to render")
	}
	if _, _, err := helmTemplate(t, "--set", "network.anthropicTrustedDomains=false", "--set", "network.extraAllowedFQDNs={api.anthropic.com}"); err != nil {
		t.Errorf("api.anthropic.com supplied via extraAllowedFQDNs must satisfy the guard: %v", err)
	}
}

func TestDefaultAllowedFQDNsDisjointFromTrusted(t *testing.T) {
	dir := chartDir(t)
	raw, err := os.ReadFile(filepath.Join(dir, "files", "anthropic-trusted-domains.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var trusted []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			trusted = append(trusted, l)
		}
	}
	valuesRaw, err := os.ReadFile(filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Network struct {
			AllowedFQDNs []string `json:"allowedFQDNs"`
		} `json:"network"`
	}
	if err := sigsyaml.Unmarshal(valuesRaw, &values); err != nil {
		t.Fatal(err)
	}
	if len(values.Network.AllowedFQDNs) == 0 {
		t.Fatal("default allowedFQDNs is empty")
	}
	for _, entry := range values.Network.AllowedFQDNs {
		host := strings.Split(entry, ":")[0]
		for _, tr := range trusted {
			if host == tr || (strings.HasPrefix(tr, "*.") && strings.HasSuffix(host, tr[1:])) {
				t.Errorf("default allowedFQDNs entry %q is already covered by Trusted entry %q", host, tr)
			}
		}
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
	// RBAC: the session controller never gets pod or secret delete; the hook lists only sandboxes and never watches.
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
				if strings.HasSuffix(o.GetName(), "-orchestrator") && (strings.Contains(joined, "watch") || (rs != "sandboxes" && strings.Contains(joined, "list"))) {
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

func TestRunnerInstructionsMountedAsManagedClaudeMd(t *testing.T) {
	objs, _, err := helmTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	cm := find(objs, "ConfigMap", release+"-shock-runner-instructions")
	if cm == nil {
		t.Fatal("runner-instructions ConfigMap not rendered by default")
	}
	if text, _, _ := unstructured.NestedString(cm.Object, "data", "CLAUDE.md"); !strings.Contains(text, "persistent disk") {
		t.Errorf("default instructions missing: %q", text[:min(len(text), 80)])
	}
	sb := sandboxTemplate(t, objs)
	var mounted bool
	for _, m := range sb.Spec.PodTemplate.Spec.Containers[0].VolumeMounts {
		if m.Name == "instructions" && m.MountPath == "/etc/claude-code/CLAUDE.md" && m.SubPath == "CLAUDE.md" && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Error("instructions must be mounted read-only at /etc/claude-code/CLAUDE.md")
	}
	objs, _, err = helmTemplate(t, "--set", "runner.instructions=")
	if err != nil {
		t.Fatal(err)
	}
	if find(objs, "ConfigMap", release+"-shock-runner-instructions") != nil {
		t.Error("empty instructions must render no ConfigMap")
	}
	if strings.Contains(sandboxTemplateRaw(t, objs), "claude-code") {
		t.Error("empty instructions must mount nothing")
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

// TestActiveSessionCap: the cap reaches the hook as an env var and switches the
// hook-failure alert to non-retryable results only.
func TestActiveSessionCap(t *testing.T) {
	envValue := func(objs []unstructured.Unstructured, name string) string {
		dep := find(objs, "Deployment", release+"-shock-orchestrator")
		if dep == nil {
			t.Fatal("orchestrator Deployment not rendered")
		}
		containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		for _, c := range containers {
			env, _, _ := unstructured.NestedSlice(c.(map[string]any), "env")
			for _, e := range env {
				em := e.(map[string]any)
				if em["name"] == name {
					return fmt.Sprint(em["value"])
				}
			}
		}
		return ""
	}
	ruleField := func(objs []unstructured.Unstructured, alert, field string) string {
		for _, o := range objs {
			if o.GetKind() != "PrometheusRule" {
				continue
			}
			groups, _, _ := unstructured.NestedSlice(o.Object, "spec", "groups")
			for _, g := range groups {
				rules, _, _ := unstructured.NestedSlice(g.(map[string]any), "rules")
				for _, r := range rules {
					rm := r.(map[string]any)
					if rm["alert"] == alert {
						return fmt.Sprint(rm[field])
					}
				}
			}
		}
		t.Fatalf("alert %s not rendered", alert)
		return ""
	}
	objs, _, err := helmTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(objs, "SHOCK_MAX_ACTIVE_SESSIONS"); got != "2" {
		t.Errorf("default SHOCK_MAX_ACTIVE_SESSIONS = %q, want 2", got)
	}
	objs, _, err = helmTemplate(t, "--set", "orchestrator.maxActiveSessions=0")
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(objs, "SHOCK_MAX_ACTIVE_SESSIONS"); got != "0" {
		t.Errorf("SHOCK_MAX_ACTIVE_SESSIONS = %q, want 0", got)
	}
	if expr := ruleField(objs, "ClaudeOrchestratorSpawnHookFailing", "expr"); !strings.Contains(expr, `result!="ok"`) {
		t.Errorf("without a cap every non-ok hook result is a failure, got %s", expr)
	}
	if got := ruleField(objs, "ClaudeSessionsBackingOff", "for"); got != "15m" {
		t.Errorf("ClaudeSessionsBackingOff for = %q, want 15m", got)
	}

	objs, _, err = helmTemplate(t, "--set", "orchestrator.maxActiveSessions=2", "--set", "monitoring.prometheusRule.backingOffFor=30m")
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(objs, "SHOCK_MAX_ACTIVE_SESSIONS"); got != "2" {
		t.Errorf("SHOCK_MAX_ACTIVE_SESSIONS = %q, want 2", got)
	}
	if expr := ruleField(objs, "ClaudeOrchestratorSpawnHookFailing", "expr"); !strings.Contains(expr, `result="non_retryable"`) || strings.Contains(expr, `!="ok"`) {
		t.Errorf("with a cap only non_retryable results are failures, got %s", expr)
	}
	if got := ruleField(objs, "ClaudeSessionsBackingOff", "for"); got != "30m" {
		t.Errorf("ClaudeSessionsBackingOff for = %q, want 30m", got)
	}
	if _, _, err := helmTemplate(t, "--set", "orchestrator.maxActiveSessions=-1"); err == nil {
		t.Error("a negative cap must fail schema validation")
	}
}

// TestResourceDefaults: every Pod the chart produces carries requests and a
// memory limit; the runner follows Anthropic's per-session sizing.
func TestResourceDefaults(t *testing.T) {
	objs, _, err := helmTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	sb := sandboxTemplate(t, objs)
	res := sb.Spec.PodTemplate.Spec.Containers[0].Resources
	if res.Requests.Memory().String() != "4Gi" || res.Limits.Memory().String() != "4Gi" {
		t.Errorf("runner memory request/limit = %s/%s, want 4Gi/4Gi", res.Requests.Memory(), res.Limits.Memory())
	}
	if res.Requests.Cpu().String() != "2" || res.Limits.Cpu().String() != "4" {
		t.Errorf("runner cpu request/limit = %s/%s, want 2/4", res.Requests.Cpu(), res.Limits.Cpu())
	}
	for _, name := range []string{release + "-shock-orchestrator", release + "-shock-session-controller"} {
		dep := find(objs, "Deployment", name)
		if dep == nil {
			t.Fatalf("%s not rendered", name)
		}
		containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		r, _, _ := unstructured.NestedMap(containers[0].(map[string]any), "resources")
		req, _, _ := unstructured.NestedMap(r, "requests")
		lim, _, _ := unstructured.NestedMap(r, "limits")
		if req["cpu"] == nil || req["memory"] == nil || lim["memory"] == nil {
			t.Errorf("%s: want cpu+memory requests and a memory limit, got %v", name, r)
		}
		if lim["cpu"] != nil {
			t.Errorf("%s: control-plane components must not be CPU-throttled, got limit %v", name, lim["cpu"])
		}
	}
}
