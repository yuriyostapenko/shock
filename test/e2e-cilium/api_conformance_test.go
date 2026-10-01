//go:build envtest

// Package ciliume2e validates secret injection (spec section 9) at two levels.
// This file is the API-conformance half: it applies the chart's rendered
// CiliumNetworkPolicy to a real kube-apiserver carrying Cilium's own CRD, so
// the structural schema, the enums and the field spellings are checked by the
// same validation a cluster would apply. It needs no Cilium, no CNI and no
// container runtime, so it runs wherever envtest runs.
//
// The behavioral half (does Envoy actually replace the header) is in
// injection_test.go behind the e2e_cilium tag and needs the lab cluster from
// hack/cilium-lab.sh.
package ciliume2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const release = "cilium-conformance"

// renderPolicies returns the chart's CiliumNetworkPolicies for the given
// values overlay.
func renderPolicies(t *testing.T, values string) []unstructured.Unstructured {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "values.yaml")
	if err := writeFile(f, values); err != nil {
		t.Fatal(err)
	}
	chart, err := filepath.Abs(filepath.Join("..", "..", "charts", "shock"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("helm", "template", release, chart, //nolint:gosec // literals from this file
		"-n", "runners", "--kube-version", "1.35.0",
		"-f", filepath.Join("..", "values", "minimal.yaml"), "-f", f)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	var objs []unstructured.Unstructured
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var u unstructured.Unstructured
		if err := dec.Decode(&u); err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			t.Fatalf("decode: %v", err)
		}
		if u.Object != nil && u.GetKind() == "CiliumNetworkPolicy" {
			objs = append(objs, u)
		}
	}
	if len(objs) == 0 {
		t.Fatal("no CiliumNetworkPolicy rendered")
	}
	return objs
}

// Two shapes worth putting through the real schema: pki: managed, which is the
// default, and pki: existing with the operator's own references.
const managedValues = `
secretInjection:
  enabled: true
  upstreamCA:
    existingSecret: {namespace: shock-credentials, name: upstream-roots}
  credentials:
    - name: ghcr
      host: ghcr.io
      secret: {namespace: shock-credentials, name: ghcr-pat}
      paths: ["/token(\\?.*)?"]
    - name: npm-pkg
      host: npm.pkg.github.com
      secret: {namespace: shock-credentials, name: npm-pat}
      header: Authorization
`

const existingValues = `
secretInjection:
  enabled: true
  pki: existing
  tls:
    certificateSecret: {namespace: shock-credentials, name: egress-tls}
  ca:
    existingConfigMap: my-egress-ca
  upstreamCA:
    existingSecret: {namespace: shock-credentials, name: upstream-roots}
  credentials:
    - name: ghcr
      host: ghcr.io
      secret: {namespace: shock-credentials, name: ghcr-pat}
      paths: ["/token(\\?.*)?"]
`

// TestRenderedPolicyIsAcceptedByCiliumsOwnCRD is the check the golden tests
// cannot make: that Cilium's published schema accepts every field the chart
// emits for injection, notably mismatch: REPLACE with a secret reference, and
// serverNames on the same port rule as terminatingTLS (spec section 13, item 8).
func TestRenderedPolicyIsAcceptedByCiliumsOwnCRD(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"."},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest (needs KUBEBUILDER_ASSETS): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName("runners")
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		values string
	}{
		{"injection off", ""},
		{"injection on", managedValues},
		{"injection on existing pki", existingValues},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, obj := range renderPolicies(t, tc.values) {
				o := obj.DeepCopy()
				o.SetName(o.GetName() + "-" + strings.ReplaceAll(tc.name, " ", "-"))
				// A real apiserver with Cilium's CRD: structural schema,
				// required fields and enum values all apply here.
				if err := c.Create(ctx, o); err != nil {
					t.Errorf("Cilium's CRD rejected the rendered policy %s: %v", obj.GetName(), err)
					continue
				}
				// Read it back: a field the schema prunes would vanish, which
				// is how a wrong spelling fails silently in a live cluster.
				got := &unstructured.Unstructured{}
				got.SetAPIVersion(o.GetAPIVersion())
				got.SetKind(o.GetKind())
				if err := c.Get(ctx, client.ObjectKeyFromObject(o), got); err != nil {
					t.Fatal(err)
				}
				assertNothingPruned(t, obj.Object, got.Object, tc.values != "" && strings.HasSuffix(obj.GetName(), "-runner"))
			}
		})
	}
}

// assertNothingPruned checks that every value the chart sent came back from
// the apiserver. A structural schema prunes what it does not know *silently*,
// so a one-directional deep subset comparison is the only thing that catches a
// field Cilium's CRD does not carry. Defaults the server adds are ignored.
func assertNothingPruned(t *testing.T, sent, stored map[string]any, wantInterception bool) {
	t.Helper()
	sentSpec, _, _ := unstructured.NestedMap(sent, "spec")
	storedSpec, _, _ := unstructured.NestedMap(stored, "spec")
	subset(t, "spec", sentSpec, storedSpec)

	if !wantInterception {
		return
	}
	// Beyond "nothing was pruned", assert the load-bearing values survived as
	// the values they must be for the rewrite to happen at all.
	egress, _, _ := unstructured.NestedSlice(storedSpec, "egress")
	var sawInterception, sawReplace bool
	for _, e := range egress {
		ports, _, _ := unstructured.NestedSlice(e.(map[string]any), "toPorts")
		if len(ports) == 0 {
			continue
		}
		p, _ := ports[0].(map[string]any)
		if _, ok := p["terminatingTLS"]; !ok {
			continue
		}
		sawInterception = true
		https, _, _ := unstructured.NestedSlice(p, "rules", "http")
		for _, h := range https {
			hm, found, _ := unstructured.NestedSlice(h.(map[string]any), "headerMatches")
			if !found {
				continue
			}
			m, _ := hm[0].(map[string]any)
			if m["mismatch"] != "REPLACE" {
				t.Errorf("mismatch did not survive as REPLACE: %v", m["mismatch"])
			}
			if _, ok := m["secret"]; !ok {
				t.Error("the secret reference did not survive; the header would be matched, not replaced")
			}
			sawReplace = true
		}
	}
	if !sawInterception {
		t.Error("expected an interception rule on the runner policy")
	}
	if !sawReplace {
		t.Error("expected a REPLACE headerMatch on the interception rule")
	}
}

// subset fails when anything in want is missing from or different in got.
func subset(t *testing.T, path string, want, got any) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: apiserver stored %T, chart sent a map", path, got)
			return
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				t.Errorf("%s.%s was pruned by the CRD schema: Cilium does not carry this field", path, k)
				continue
			}
			subset(t, path+"."+k, wv, gv)
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			t.Errorf("%s: apiserver stored %T, chart sent a list", path, got)
			return
		}
		if len(w) != len(g) {
			t.Errorf("%s: chart sent %d entries, apiserver stored %d", path, len(w), len(g))
			return
		}
		for i := range w {
			subset(t, fmt.Sprintf("%s[%d]", path, i), w[i], g[i])
		}
	default:
		if fmt.Sprint(want) != fmt.Sprint(got) {
			t.Errorf("%s: chart sent %v, apiserver stored %v", path, want, got)
		}
	}
}
