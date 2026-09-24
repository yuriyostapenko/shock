// Package chart renders charts/shock with helm and checks what the compiler
// cannot: the template contract, forced fields, validation. Needs helm.
package chart

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	objs, _, err := helmTemplate(t, "--set", "orchestrator.maxActiveSessions=0")
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(objs, "SHOCK_MAX_ACTIVE_SESSIONS"); got != "0" {
		t.Errorf("SHOCK_MAX_ACTIVE_SESSIONS = %q, want 0", got)
	}
	if expr := ruleField(objs, "ClaudeOrchestratorSpawnHookFailing", "expr"); !strings.Contains(expr, `result!="ok"`) {
		t.Errorf("without a cap every non-ok hook result is a failure, got %s", expr)
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

// --- Secret injection (spec section 9) ---

// injectionValues is the smallest overlay that turns injection on: the default
// pki: managed, operator-supplied upstream roots, and two credentials, one
// path-scoped like ghcr.io's token endpoint and one not.
func injectionValues(t *testing.T) string {
	t.Helper()
	return overlay(t, `
secretInjection:
  enabled: true
  credentials:
    - name: ghcr
      host: ghcr.io
      secret: {namespace: shock-credentials, name: ghcr-pat}
      paths: ["/token(\\?.*)?"]
    - name: npm-pkg
      host: npm.pkg.github.com
      secret: {namespace: shock-credentials, name: npm-pat}
  clientConfigs:
    npmrc: |
      //npm.pkg.github.com/:_authToken=proxy-injected
`)
}

// overlay writes a values file Helm can merge over the others.
func overlay(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "overlay.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// runnerEgress returns the runner policy's egress rules split into the plain
// allow rules and the interception rules, keyed by host.
func runnerEgress(t *testing.T, objs []unstructured.Unstructured) (plain map[string]bool, intercept map[string]map[string]any) {
	t.Helper()
	cnp := find(objs, "CiliumNetworkPolicy", release+"-shock-runner")
	if cnp == nil {
		t.Fatal("runner CiliumNetworkPolicy not rendered")
	}
	plain, intercept = map[string]bool{}, map[string]map[string]any{}
	egress, _, _ := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	for _, e := range egress {
		rule, _ := e.(map[string]any)
		fqdns, _, _ := unstructured.NestedSlice(rule, "toFQDNs")
		if len(fqdns) == 0 {
			continue
		}
		var host string
		for _, v := range fqdns[0].(map[string]any) {
			host = v.(string)
		}
		ports, _, _ := unstructured.NestedSlice(rule, "toPorts")
		port, _ := ports[0].(map[string]any)
		if _, ok := port["terminatingTLS"]; ok {
			intercept[host] = port
		} else {
			plain[host] = true
		}
	}
	return plain, intercept
}

func TestSecretInjectionRendersInterceptionRules(t *testing.T) {
	objs, stderr, err := helmTemplate(t, "-f", injectionValues(t))
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	plain, intercept := runnerEgress(t, objs)

	// Both hosts are on Anthropic's Trusted list. A plain L4 allow surviving
	// beside the interception rule would admit the traffic unproxied.
	for _, host := range []string{"ghcr.io", "npm.pkg.github.com"} {
		if plain[host] {
			t.Errorf("%s still has a plain toFQDNs rule alongside its interception rule", host)
		}
		if intercept[host] == nil {
			t.Fatalf("%s has no interception rule", host)
		}
	}
	if !plain["api.anthropic.com"] {
		t.Error("api.anthropic.com must keep its plain rule")
	}
	if _, ok := intercept["api.anthropic.com"]; ok {
		t.Error("api.anthropic.com must never be intercepted")
	}

	ghcr := intercept["ghcr.io"]
	orig, _, _ := unstructured.NestedStringMap(ghcr, "originatingTLS", "secret")
	if orig["namespace"] != "runners" || orig["name"] != release+"-shock-upstream-ca" {
		t.Errorf("originatingTLS must default to the chart's pinned roots: %v", orig)
	}
	https, _, _ := unstructured.NestedSlice(ghcr, "rules", "http")
	if len(https) != 2 {
		t.Fatalf("a path-scoped credential renders the bound rule plus a catch-all, got %d: %v", len(https), https)
	}
	bound := https[0].(map[string]any)
	if bound["path"] != `/token(\?.*)?` {
		t.Errorf("bound path: %v", bound["path"])
	}
	hm, _, _ := unstructured.NestedSlice(bound, "headerMatches")
	m := hm[0].(map[string]any)
	if m["name"] != "Authorization" || m["mismatch"] != "REPLACE" {
		t.Errorf("headerMatch must REPLACE the bound header: %v", m)
	}
	sec, _, _ := unstructured.NestedStringMap(m, "secret")
	if sec["namespace"] != "shock-credentials" || sec["name"] != "ghcr-pat" {
		t.Errorf("headerMatch secret: %v", sec)
	}
	// The catch-all is what lets the OCI client present its derived Bearer JWT
	// on /v2/... unmodified instead of being dropped.
	if len(https[1].(map[string]any)) != 0 {
		t.Errorf("second http rule must be the empty catch-all, got %v", https[1])
	}

	// An unscoped credential injects on every request, so no catch-all.
	npm, _, _ := unstructured.NestedSlice(intercept["npm.pkg.github.com"], "rules", "http")
	if len(npm) != 1 {
		t.Fatalf("unscoped credential must render exactly one http rule, got %v", npm)
	}
	if _, ok := npm[0].(map[string]any)["path"]; ok {
		t.Error("unscoped credential must not render a path")
	}

	// The orchestrator is not a session and is never intercepted; it keeps the
	// full allow list.
	orch := find(objs, "CiliumNetworkPolicy", release+"-shock-orchestrator")
	if orch == nil {
		t.Fatal("orchestrator policy missing")
	}
	raw := fmt.Sprint(orch.Object)
	if strings.Contains(raw, "terminatingTLS") || strings.Contains(raw, "headerMatches") {
		t.Error("the orchestrator policy must carry no interception rules")
	}
	if !strings.Contains(raw, "ghcr.io") {
		t.Error("the orchestrator keeps the full allow list, including injected hosts")
	}
}

// Cilium reads a matched rule with no client TLS context as permission to use a
// raw socket upstream, so an interception rule without originatingTLS would
// forward the injected credential in cleartext. Every rule must carry it,
// whether the roots come from the chart or from an override.
func TestSecretInjectionAlwaysCarriesUpstreamRoots(t *testing.T) {
	for _, tc := range []struct {
		name          string
		args          []string
		wantNamespace string
		wantName      string
		wantChartCA   bool
	}{
		{name: "chart roots by default", wantNamespace: "runners", wantName: release + "-shock-upstream-ca", wantChartCA: true},
		{name: "operator override", args: []string{
			"--set", "secretInjection.upstreamCA.existingSecret.namespace=shock-credentials",
			"--set", "secretInjection.upstreamCA.existingSecret.name=our-roots"},
			wantNamespace: "shock-credentials", wantName: "our-roots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs, stderr, err := helmTemplate(t, append([]string{"-f", injectionValues(t)}, tc.args...)...)
			if err != nil {
				t.Fatalf("%v %s", err, stderr)
			}
			_, intercept := runnerEgress(t, objs)
			if len(intercept) == 0 {
				t.Fatal("no interception rules rendered")
			}
			for host, port := range intercept {
				sec, found, _ := unstructured.NestedStringMap(port, "originatingTLS", "secret")
				if !found {
					t.Fatalf("interception rule for %s has no originatingTLS: Cilium would forward the credential upstream in cleartext", host)
				}
				if sec["namespace"] != tc.wantNamespace || sec["name"] != tc.wantName {
					t.Errorf("%s originatingTLS = %v", host, sec)
				}
			}
			ca := find(objs, "Secret", release+"-shock-upstream-ca")
			if tc.wantChartCA {
				if ca == nil {
					t.Fatal("the chart must render its pinned roots when no override is set")
				}
				body, _, _ := unstructured.NestedString(ca.Object, "stringData", "ca.crt")
				// Mozilla's list, not a handful of certificates from somewhere.
				if n := strings.Count(body, "BEGIN CERTIFICATE"); n < 100 {
					t.Errorf("the shipped roots carry %d certificates; run `make ca-bundle`", n)
				}
				// The digest in the header is what `make ca-bundle-check` and CI
				// verify, so it has to survive into the Secret.
				if !strings.Contains(body, "# sha256:") || !strings.Contains(body, "https://curl.se/ca/cacert.pem") {
					t.Error("the shipped roots must carry their source and digest header")
				}
			} else if ca != nil {
				t.Error("an override must not also render the chart's roots")
			}
		})
	}
}

// The file the chart ships must be Mozilla's list and nothing else. This is the
// check that would have caught the interception CAs an earlier generator, which
// read the build host's trust store, baked into the chart.
func TestShippedUpstreamRootsAreOnlyMozillas(t *testing.T) {
	path := filepath.Join(chartDir(t), "files", "upstream-ca-bundle.pem")
	body, err := os.ReadFile(path) //nolint:gosec // path built from the chart dir
	if err != nil {
		t.Fatalf("the chart must ship upstream roots: %v", err)
	}
	text := string(body)
	marker := "# ---8<--- upstream file follows\n"
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatal("no upstream marker in the shipped roots; regenerate with `make ca-bundle`")
	}
	var recorded string
	for _, line := range strings.Split(text[:i], "\n") {
		if rest, ok := strings.CutPrefix(line, "# sha256:"); ok {
			recorded = strings.TrimSpace(rest)
		}
	}
	if recorded == "" {
		t.Fatal("the shipped roots record no sha256")
	}
	sum := sha256.Sum256([]byte(text[i+len(marker):]))
	if got := hex.EncodeToString(sum[:]); got != recorded {
		t.Errorf("the shipped roots do not match their recorded digest:\n recorded %s\n actual   %s\nThe file was edited by hand; re-run `make ca-bundle`.", recorded, got)
	}
	// Names that would mean a TLS-inspecting proxy's CA got in.
	for _, bad := range []string{"interception", "Interception", "Inspection", "inspection", "egress-gateway", "Proxy CA", "proxy-ca"} {
		if strings.Contains(text, bad) {
			t.Errorf("the shipped roots mention %q; these are meant to be Mozilla's public list only", bad)
		}
	}
}

// pki: managed issues the chain with cert-manager and keeps every private key
// out of the Pod.
func TestSecretInjectionManagedPKI(t *testing.T) {
	objs, stderr, err := helmTemplate(t, "-f", injectionValues(t))
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	selfSigned := find(objs, "Issuer", release+"-shock-egress-selfsigned")
	caCert := find(objs, "Certificate", release+"-shock-egress-ca")
	caIssuer := find(objs, "Issuer", release+"-shock-egress-ca")
	leaf := find(objs, "Certificate", release+"-shock-egress-tls")
	for name, o := range map[string]*unstructured.Unstructured{
		"self-signed Issuer": selfSigned, "CA Certificate": caCert,
		"CA Issuer": caIssuer, "leaf Certificate": leaf,
	} {
		if o == nil {
			t.Fatalf("pki: managed must render the %s", name)
		}
		if o.GetNamespace() != "runners" {
			t.Errorf("%s must be namespaced in the release namespace, got %q", name, o.GetNamespace())
		}
	}
	if isCA, _, _ := unstructured.NestedBool(caCert.Object, "spec", "isCA"); !isCA {
		t.Error("the CA Certificate must set isCA")
	}
	// The leaf's SANs are the injected hosts, so adding a credential reissues it.
	names, _, _ := unstructured.NestedStringSlice(leaf.Object, "spec", "dnsNames")
	if fmt.Sprint(names) != "[ghcr.io npm.pkg.github.com]" {
		t.Errorf("leaf dnsNames must be exactly the injected hosts, got %v", names)
	}

	// Cilium presents the leaf; the runner trusts it via the same Secret's ca.crt.
	_, intercept := runnerEgress(t, objs)
	term, _, _ := unstructured.NestedStringMap(intercept["ghcr.io"], "terminatingTLS", "secret")
	if term["namespace"] != "runners" || term["name"] != release+"-shock-egress-tls" {
		t.Errorf("terminatingTLS must name the issued leaf Secret, got %v", term)
	}

	sb := sandboxTemplate(t, objs)
	var caVol *corev1.Volume
	for i, v := range sb.Spec.PodTemplate.Spec.Volumes {
		if v.Name == "egress-ca" {
			caVol = &sb.Spec.PodTemplate.Spec.Volumes[i]
		}
	}
	if caVol == nil || caVol.Secret == nil {
		t.Fatalf("pki: managed must mount the issued Secret's CA, got %+v", caVol)
	}
	if caVol.Secret.SecretName != release+"-shock-egress-tls" {
		t.Errorf("CA volume secret = %q", caVol.Secret.SecretName)
	}
	// items is what keeps the leaf private key out of the Pod.
	if len(caVol.Secret.Items) != 1 || caVol.Secret.Items[0].Key != "ca.crt" {
		t.Errorf("a Secret-backed CA volume must select ca.crt alone, got %v", caVol.Secret.Items)
	}
	// The CA private key lives in its own Secret, which no Pod may reference.
	if strings.Contains(sandboxTemplateRaw(t, objs), release+"-shock-egress-ca") {
		t.Error("the CA Certificate's Secret holds the CA private key and must be referenced by no Pod")
	}
}

func TestSecretInjectionExistingPKI(t *testing.T) {
	objs, stderr, err := helmTemplate(t, "-f", injectionValues(t),
		"--set", "secretInjection.pki=existing",
		"--set", "secretInjection.tls.certificateSecret.namespace=shock-credentials",
		"--set", "secretInjection.tls.certificateSecret.name=egress-tls",
		"--set", "secretInjection.ca.existingConfigMap=my-egress-ca")
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	for _, kind := range []string{"Issuer", "Certificate"} {
		for _, o := range objs {
			if o.GetKind() == kind {
				t.Errorf("pki: existing must render no cert-manager %s (%s)", kind, o.GetName())
			}
		}
	}
	_, intercept := runnerEgress(t, objs)
	term, _, _ := unstructured.NestedStringMap(intercept["ghcr.io"], "terminatingTLS", "secret")
	if term["namespace"] != "shock-credentials" || term["name"] != "egress-tls" {
		t.Errorf("terminatingTLS must name the operator's Secret, got %v", term)
	}
	sb := sandboxTemplate(t, objs)
	for _, v := range sb.Spec.PodTemplate.Spec.Volumes {
		if v.Name == "egress-ca" {
			if v.ConfigMap == nil || v.ConfigMap.Name != "my-egress-ca" {
				t.Errorf("pki: existing must mount the operator's ConfigMap, got %+v", v)
			}
			if v.Secret != nil {
				t.Error("the CA volume must not be a Secret in this mode")
			}
		}
	}
}

// The credential must not reach the Pod in any form: the hook renders the
// Sandbox from this template, so a Secret name here would become a mount.
func TestSecretInjectionKeepsCredentialsOutOfThePod(t *testing.T) {
	objs, stderr, err := helmTemplate(t, "-f", injectionValues(t))
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	raw := sandboxTemplateRaw(t, objs)
	for _, forbidden := range []string{"ghcr-pat", "npm-pat", "shock-credentials"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("rendered Sandbox template names %q; credential and upstream-root Secrets belong to the policy only", forbidden)
		}
	}
	sb := sandboxTemplate(t, objs)
	runner := sb.Spec.PodTemplate.Spec.Containers[0]

	var caMount, registriesMount bool
	for _, m := range runner.VolumeMounts {
		switch m.MountPath {
		case "/etc/shock/egress-ca":
			caMount = m.ReadOnly
		case "/etc/shock/registries":
			registriesMount = m.ReadOnly
		}
	}
	if !caMount {
		t.Error("the egress CA must be mounted read-only at /etc/shock/egress-ca")
	}
	if !registriesMount {
		t.Error("client configs must be mounted read-only at /etc/shock/registries")
	}
	var env string
	for _, e := range runner.Env {
		if e.Name == "SHOCK_EGRESS_CA_FILE" {
			env = e.Value
		}
	}
	if env != "/etc/shock/egress-ca/ca.crt" {
		t.Errorf("SHOCK_EGRESS_CA_FILE = %q, the entrypoint keys the trust wiring on it", env)
	}
	// Every injection volume is either a ConfigMap or a Secret restricted to
	// ca.crt. Anything else would project key material into the session.
	for _, v := range sb.Spec.PodTemplate.Spec.Volumes {
		if v.Name != "egress-ca" && v.Name != "registries" {
			continue
		}
		switch {
		case v.ConfigMap != nil:
		case v.Secret != nil:
			if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "ca.crt" {
				t.Errorf("volume %s is a Secret and must select ca.crt alone, got %v", v.Name, v.Secret.Items)
			}
		default:
			t.Errorf("volume %s must be a ConfigMap or a restricted Secret, got %+v", v.Name, v)
		}
	}

	// The agent is told the hosts and the placeholder, never a Secret.
	cm := find(objs, "ConfigMap", release+"-shock-runner-instructions")
	if cm == nil {
		t.Fatal("instructions ConfigMap missing")
	}
	instr, _, _ := unstructured.NestedString(cm.Object, "data", "CLAUDE.md")
	for _, want := range []string{"proxy-injected", "ghcr.io", "npm.pkg.github.com", `/token(\?.*)?`} {
		if !strings.Contains(instr, want) {
			t.Errorf("instructions must name %q", want)
		}
	}
	for _, forbidden := range []string{"ghcr-pat", "npm-pat", "shock-credentials"} {
		if strings.Contains(instr, forbidden) {
			t.Errorf("instructions name %q; hosts and the placeholder only", forbidden)
		}
	}
}

func TestSecretInjectionValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "non-cilium mode", args: []string{"--set", "network.mode=kubernetes"},
			want: "requires network.mode cilium"},
		{name: "anthropic api", args: []string{"--set", "secretInjection.credentials[0].host=api.anthropic.com"},
			want: "api.anthropic.com can never be intercepted"},
		{name: "duplicate host", args: []string{"--set", "secretInjection.credentials[1].host=ghcr.io"},
			want: "already injected"},
		{name: "duplicate name", args: []string{"--set", "secretInjection.credentials[1].name=ghcr"},
			want: "duplicate name"},
		{name: "secret name in a client config", args: []string{"--set", `secretInjection.clientConfigs.npmrc=token=ghcr-pat`},
			want: "client configs carry the placeholder only"},
		// A stale field from the other mode must not look effective.
		{name: "managed with a certificate reference", args: []string{"--set", "secretInjection.tls.certificateSecret.name=stale"},
			want: "cert-manager issues the certificate"},
		{name: "managed with a ca reference", args: []string{"--set", "secretInjection.ca.existingConfigMap=stale"},
			want: "cert-manager issues the certificate"},
		{name: "existing without a certificate", args: []string{"--set", "secretInjection.pki=existing", "--set", "secretInjection.ca.existingConfigMap=x"},
			want: "certificateSecret.name is required"},
		{name: "existing without a ca", args: []string{"--set", "secretInjection.pki=existing", "--set", "secretInjection.tls.certificateSecret.name=x"},
			want: "must trust the interception CA"},
		{name: "existing with two cas", args: []string{"--set", "secretInjection.pki=existing", "--set", "secretInjection.tls.certificateSecret.name=x",
			"--set", "secretInjection.ca.existingConfigMap=a", "--set", "secretInjection.ca.bundle=b"}, want: "only one of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := helmTemplate(t, append([]string{"-f", injectionValues(t)}, tc.args...)...)
			if err == nil {
				t.Fatalf("expected a render failure, got none")
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr must name the problem %q:\n%s", tc.want, stderr)
			}
		})
	}
}

// A wildcard allow-list entry covering an injected host would admit it on L4
// without the proxy, so the render refuses rather than silently leaking it.
func TestSecretInjectionRefusesHostUnderWildcard(t *testing.T) {
	base := injectionValues(t)
	_, stderr, err := helmTemplate(t, "-f", base,
		"--set", "network.extraAllowedFQDNs={*.pkg.github.com}")
	if err == nil {
		t.Fatal("a wildcard covering an injected host must fail the render")
	}
	if !strings.Contains(stderr, "excludeFQDNs") {
		t.Fatalf("the failure must name the remedy:\n%s", stderr)
	}
	// Excluding it makes the same values render.
	if _, stderr, err := helmTemplate(t, "-f", base,
		"--set", "network.extraAllowedFQDNs={*.pkg.github.com}",
		"--set", "network.excludeFQDNs={*.pkg.github.com}"); err != nil {
		t.Fatalf("excluding the wildcard must make it render: %v %s", err, stderr)
	}
}

// Injection off is the default and must change nothing.
func TestSecretInjectionOffByDefault(t *testing.T) {
	objs, _, err := helmTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		switch o.GetKind() {
		case "Issuer", "Certificate":
			t.Errorf("%s %s must not render with injection off", o.GetKind(), o.GetName())
		}
	}
	for _, n := range []string{release + "-shock-registries", release + "-shock-egress-ca-bundle"} {
		if find(objs, "ConfigMap", n) != nil {
			t.Errorf("%s must not render with injection off", n)
		}
	}
	if find(objs, "Secret", release+"-shock-upstream-ca") != nil {
		t.Error("the upstream roots Secret must not render with injection off")
	}
	_, intercept := runnerEgress(t, objs)
	if len(intercept) != 0 {
		t.Errorf("no interception rules by default, got %v", intercept)
	}
	sb := sandboxTemplate(t, objs)
	for _, e := range sb.Spec.PodTemplate.Spec.Containers[0].Env {
		if e.Name == "SHOCK_EGRESS_CA_FILE" {
			t.Error("SHOCK_EGRESS_CA_FILE must not be set with injection off")
		}
	}
	for _, v := range sb.Spec.PodTemplate.Spec.Volumes {
		if v.Name == "egress-ca" || v.Name == "registries" {
			t.Errorf("volume %s must not render with injection off", v.Name)
		}
	}
}
