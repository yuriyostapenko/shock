//go:build e2e_cilium

// Behavioral half of the secret-injection acceptance criteria (spec section
// 9/12). Needs the lab cluster from `make e2e-cilium-setup`: Cilium with its
// L7 proxy and policy-secret sync. An in-cluster TLS echo service plays the
// injected host, so no real credential and no internet access is involved.
//
// What it proves, which the API-conformance test cannot: Cilium's Envoy
// actually replaces the bound header, only on the bound path, only for the
// bound host, and the value never appears inside the client Pod.
package ciliume2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	labNamespace = "shock-injection-lab"
	// The echo service doubles as the injected host: an exact in-cluster FQDN
	// resolvable through Cilium's DNS proxy, which is what toFQDNs needs.
	echoHost = "echo.shock-injection-lab.svc.cluster.local"
	// The lab's stand-in credential, generated for this test and never a real
	// token. It must never appear inside the client Pod.
	secretValue = "Bearer lab-secret-value-8f3a1c" //nolint:gosec // test fixture, not a credential
	placeholder = "proxy-injected"
)

func kubectl(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"--context", kubeContext()}, args...)
	cmd := exec.Command("kubectl", full...) //nolint:gosec // test helper
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustKubectl(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	out, err := kubectl(t, stdin, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func kubeContext() string {
	if c := os.Getenv("KUBECONTEXT"); c != "" {
		return c
	}
	return "kind-shock-cilium"
}

// selfSignedFor returns a CA certificate and a server certificate for host,
// signed by it. The CA private key is discarded with the test: nothing in the
// cluster ever needs it (spec section 9).
func selfSignedFor(t *testing.T, host string) (caPEM, certPEM, keyPEM string) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "shock injection lab CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	srvKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host, "echo", "echo.shock-injection-lab"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, ca, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(kind string, der []byte) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
	}
	return enc("CERTIFICATE", caDER), enc("CERTIFICATE", srvDER),
		enc("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(srvKey))
}

// The echo server reflects the Authorization header it received, so the test
// can tell what left the client Pod from what arrived at the host.
const echoManifest = `
apiVersion: v1
kind: ConfigMap
metadata: {name: echo-src, namespace: %[1]s}
data:
  echo.py: |
    import http.server, ssl
    class H(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            body = ("PATH=%%s\nAUTH=%%s\n" %% (self.path, self.headers.get("Authorization", "<none>"))).encode()
            self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers()
            self.wfile.write(body)
        def log_message(self, *a): pass
    s = http.server.HTTPServer(("0.0.0.0", 8443), H)
    c = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); c.load_cert_chain("/tls/tls.crt", "/tls/tls.key")
    s.socket = c.wrap_socket(s.socket, server_side=True); s.serve_forever()
---
apiVersion: v1
kind: Service
metadata: {name: echo, namespace: %[1]s}
spec:
  selector: {app: echo}
  ports: [{port: 443, targetPort: 8443}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: echo, namespace: %[1]s}
spec:
  replicas: 1
  selector: {matchLabels: {app: echo}}
  template:
    metadata: {labels: {app: echo}}
    spec:
      containers:
        - name: echo
          image: docker.io/library/python:3.13-alpine
          command: ["python3", "/src/echo.py"]
          ports: [{containerPort: 8443}]
          volumeMounts:
            - {name: src, mountPath: /src}
            - {name: tls, mountPath: /tls}
      volumes:
        - {name: src, configMap: {name: echo-src}}
        - {name: tls, secret: {secretName: echo-tls}}
`

// The client carries the runner selector labels, so the release's runner
// policy selects it exactly as it selects a real runner Pod.
const clientManifest = `
apiVersion: v1
kind: Pod
metadata:
  name: fake-runner
  namespace: %[1]s
  labels:
    app.kubernetes.io/name: runner
    app.kubernetes.io/instance: %[2]s
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
    - name: client
      image: docker.io/library/alpine:3.22
      command: ["sh", "-c", "apk add --no-cache curl >/dev/null 2>&1; sleep 3600"]
      volumeMounts:
        - {name: egress-ca, mountPath: /etc/shock/egress-ca, readOnly: true}
  volumes:
    - name: egress-ca
      configMap:
        name: %[2]s-shock-egress-ca
        items: [{key: ca.crt, path: ca.crt}]
`

func waitFor(t *testing.T, what string, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSecretInjectionReplacesHeaderOutsideThePod(t *testing.T) {
	if _, err := kubectl(t, "", "get", "nodes"); err != nil {
		t.Skip("no Cilium lab cluster; run `make e2e-cilium-setup`")
	}
	const release = "inject"
	credNS := labNamespace + "-credentials"

	caPEM, certPEM, keyPEM := selfSignedFor(t, echoHost)

	for _, ns := range []string{labNamespace, credNS} {
		_, _ = kubectl(t, "", "create", "namespace", ns)
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, "", "delete", "namespace", labNamespace, credNS, "--wait=false")
	})

	// The echo server's own certificate, and the one Cilium presents.
	writeTempTLS := func(name, ns, cert, key string) {
		dir := t.TempDir()
		c, k := dir+"/tls.crt", dir+"/tls.key"
		if err := writeFile(c, cert); err != nil {
			t.Fatal(err)
		}
		if err := writeFile(k, key); err != nil {
			t.Fatal(err)
		}
		_, _ = kubectl(t, "", "-n", ns, "delete", "secret", name, "--ignore-not-found")
		mustKubectl(t, "", "-n", ns, "create", "secret", "tls", name, "--cert="+c, "--key="+k)
	}
	writeTempTLS("echo-tls", labNamespace, certPEM, keyPEM)
	// Cilium presents this to the client for the injected host. Same CA, so
	// the client trusts it; in production it is the operator's own CA.
	writeTempTLS("egress-tls", credNS, certPEM, keyPEM)

	// The credential itself. It lives only here and in Cilium's Envoy.
	mustKubectl(t, "", "-n", credNS, "create", "secret", "generic", "echo-credential",
		"--from-literal=value="+secretValue)
	// originatingTLS: Cilium verifies the echo server against the lab CA.
	dir := t.TempDir()
	caFile := dir + "/ca.crt"
	if err := writeFile(caFile, caPEM); err != nil {
		t.Fatal(err)
	}
	mustKubectl(t, "", "-n", credNS, "create", "secret", "generic", "lab-upstream-ca",
		"--from-file=ca.crt="+caFile)

	mustKubectl(t, fmt.Sprintf(echoManifest, labNamespace), "apply", "-f", "-")
	mustKubectl(t, "", "-n", labNamespace, "rollout", "status", "deploy/echo", "--timeout=180s")

	// Install the chart with injection bound to the echo host. Only the
	// policy, the CA ConfigMap and the instructions matter here.
	chartValues := fmt.Sprintf(`
environment:
  existingSecret: lab-environment
orchestrator: {replicas: 0, pdb: {enabled: false}}
monitoring: {enabled: false}
network:
  mode: cilium
  anthropicTrustedDomains: false
  allowedFQDNs: [api.anthropic.com]
secretInjection:
  enabled: true
  # The lab signs its own certificate, so pki: existing. pki: managed is covered
  # by the chart golden tests and would need cert-manager on the lab cluster.
  pki: existing
  ca:
    bundle: |
%s
  tls:
    certificateSecret: {namespace: %s, name: egress-tls}
  upstreamCA:
    existingSecret: {namespace: %s, name: lab-upstream-ca}
  credentials:
    - name: echo
      host: %s
      secret: {namespace: %s, name: echo-credential}
      paths: ["/bound(/.*)?"]
`, indent(caPEM, "      "), credNS, credNS, echoHost, credNS)

	valuesFile := dir + "/values.yaml"
	if err := writeFile(valuesFile, chartValues); err != nil {
		t.Fatal(err)
	}
	_, _ = kubectl(t, "", "-n", labNamespace, "create", "secret", "generic", "lab-environment",
		"--from-literal=environment-secret=lab")
	out, err := exec.Command("helm", "upgrade", "--install", release, "../../charts/shock", //nolint:gosec // literals
		"--kube-context", kubeContext(), "-n", labNamespace, "-f", valuesFile, "--wait", "--timeout", "5m").CombinedOutput()
	if err != nil {
		t.Fatalf("helm install: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("helm", "uninstall", release, "--kube-context", kubeContext(), "-n", labNamespace).Run() //nolint:gosec // literals
	})

	mustKubectl(t, fmt.Sprintf(clientManifest, labNamespace, release), "apply", "-f", "-")
	mustKubectl(t, "", "-n", labNamespace, "wait", "--for=condition=Ready", "pod/fake-runner", "--timeout=180s")
	waitFor(t, "curl in the client pod", 3*time.Minute, func() bool {
		_, err := kubectl(t, "", "-n", labNamespace, "exec", "fake-runner", "--", "sh", "-c", "command -v curl")
		return err == nil
	})

	get := func(path string) string {
		t.Helper()
		var last string
		waitFor(t, "a response from "+path, 3*time.Minute, func() bool {
			out, err := kubectl(t, "", "-n", labNamespace, "exec", "fake-runner", "--", "sh", "-c",
				fmt.Sprintf("curl -sS --cacert /etc/shock/egress-ca/ca.crt -H 'Authorization: %s' https://%s%s", placeholder, echoHost, path))
			last = out
			return err == nil && strings.Contains(out, "AUTH=")
		})
		return last
	}

	// The bound path: Envoy replaced what the Pod sent.
	if bound := get("/bound/token"); !strings.Contains(bound, "AUTH="+secretValue) {
		t.Errorf("bound path did not receive the injected credential:\n%s", bound)
	} else if strings.Contains(bound, placeholder) {
		t.Errorf("the placeholder reached the host on the bound path:\n%s", bound)
	}

	// An unbound path on the same host passes through untouched, which is what
	// lets a derived registry token work (spec section 9).
	if un := get("/unbound"); !strings.Contains(un, "AUTH="+placeholder) {
		t.Errorf("unbound path must reach the host unmodified:\n%s", un)
	}

	// The credential is nowhere the session can read it.
	dump, _ := kubectl(t, "", "-n", labNamespace, "exec", "fake-runner", "--", "sh", "-c",
		"env; echo ---; find /etc/shock /var/run -type f 2>/dev/null | head -50; echo ---; cat /etc/shock/egress-ca/ca.crt")
	if strings.Contains(dump, secretValue) || strings.Contains(dump, "lab-secret-value") {
		t.Error("the credential is visible inside the Pod; injection must keep it outside the container")
	}
	spec, _ := kubectl(t, "", "-n", labNamespace, "get", "pod", "fake-runner", "-o", "yaml")
	if strings.Contains(spec, "echo-credential") {
		t.Error("the Pod spec references the credential Secret")
	}
}

func indent(s, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
