// Package runner covers images/runner/entrypoint.sh, the runner half of secret
// injection (spec section 9): without the CA the chart mounts, every request to
// an injected host fails on certificate verification, so the trust wiring is
// load-bearing. Needs only /bin/sh and openssl-free Go crypto.
package runner

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runEntrypoint runs the real entrypoint with a stub `claude` on PATH that
// dumps its environment, and returns that environment.
func runEntrypoint(t *testing.T, home string, env ...string) map[string]string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "images", "runner", "entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	dump := filepath.Join(bin, "env.txt")
	stub := "#!/bin/sh\nenv > " + dump + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(stub), 0o700); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script, "self-hosted-runner", "--capacity", "1") //nolint:gosec // path built above
	cmd.Env = append([]string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"HOME=" + home,
	}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(dump) //nolint:gosec // path built above
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	return got
}

// The variables the chart's injection support depends on, and which of them
// take a full bundle rather than an extra certificate.
var bundleVars = []string{"SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "PIP_CERT", "GIT_SSL_CAINFO"}

func TestEntrypointLeavesTrustAloneWithoutInjection(t *testing.T) {
	home := t.TempDir()
	got := runEntrypoint(t, home)
	for _, v := range append(bundleVars, "NODE_EXTRA_CA_CERTS") {
		if _, ok := got[v]; ok {
			t.Errorf("%s must not be set when the chart mounts no CA: %q", v, got[v])
		}
	}
	// The pre-existing behavior must survive.
	if got["CLAUDE_CODE_TMPDIR"] != filepath.Join(home, ".cache", "claude-tmp") {
		t.Errorf("CLAUDE_CODE_TMPDIR = %q", got["CLAUDE_CODE_TMPDIR"])
	}
}

func TestEntrypointExportsTrustForInjectedHosts(t *testing.T) {
	home := t.TempDir()
	caDir := t.TempDir()
	caFile := filepath.Join(caDir, "ca.crt")
	caPEM, _ := testCA(t, "unused.example")
	if err := os.WriteFile(caFile, []byte(caPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runEntrypoint(t, home, "SHOCK_EGRESS_CA_FILE="+caFile)

	bundle := filepath.Join(home, ".cache", "shock", "ca-bundle.crt")
	for _, v := range bundleVars {
		if got[v] != bundle {
			t.Errorf("%s = %q, want the combined bundle %q: these replace the trust store, so a bare CA would drop every public root", v, got[v], bundle)
		}
	}
	// NODE_EXTRA_CA_CERTS extends the store instead of replacing it, so it
	// takes the CA alone.
	if got["NODE_EXTRA_CA_CERTS"] != caFile {
		t.Errorf("NODE_EXTRA_CA_CERTS = %q, want the CA itself %q", got["NODE_EXTRA_CA_CERTS"], caFile)
	}

	body, err := os.ReadFile(bundle) //nolint:gosec // path built above
	if err != nil {
		t.Fatalf("the entrypoint must write the bundle: %v", err)
	}
	if !strings.Contains(string(body), caPEM) {
		t.Error("the bundle does not contain the mounted CA")
	}
	if system, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt"); err == nil {
		got, want := strings.Count(string(body), "BEGIN CERTIFICATE"), strings.Count(string(system), "BEGIN CERTIFICATE")+1
		if got != want {
			t.Errorf("bundle has %d certificates, want the system store plus the CA (%d)", got, want)
		}
	}
}

// The point of the wiring: a client using what the entrypoint exported trusts
// a certificate signed by the mounted CA, and one using the system store alone
// does not.
func TestEntrypointBundleIsWhatMakesAnInterceptedHostVerify(t *testing.T) {
	home := t.TempDir()
	caDir := t.TempDir()
	caPEM, srvCert := testCA(t, "localhost")
	caFile := filepath.Join(caDir, "ca.crt")
	if err := os.WriteFile(caFile, []byte(caPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runEntrypoint(t, home, "SHOCK_EGRESS_CA_FILE="+caFile)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{srvCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	bundle, err := os.ReadFile(got["SSL_CERT_FILE"]) //nolint:gosec // path from the entrypoint under test
	if err != nil {
		t.Fatal(err)
	}
	if !pool.AppendCertsFromPEM(bundle) {
		t.Fatal("the entrypoint's bundle parsed as no certificates")
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("a client using the entrypoint's bundle must verify the intercepted host: %v", err)
	}
	_ = resp.Body.Close()

	// Without it, the same request fails: the CA is what does the work.
	strict := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
	}
	if _, err := strict.Get(srv.URL); err == nil {
		t.Error("the system store alone must reject the intercepted host, or this test proves nothing")
	}
}

// testCA returns a CA certificate in PEM and a server certificate for host
// signed by it. Shape of a real interception setup; the CA key is discarded.
func testCA(t *testing.T, host string) (caPEM string, srv tls.Certificate) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "shock entrypoint test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
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
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		IPAddresses:  nil,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if host == "localhost" {
		tmpl.IPAddresses = append(tmpl.IPAddresses, parseIP("127.0.0.1"), parseIP("::1"))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	srvPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	srv, err = tls.X509KeyPair([]byte(srvPEM), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return caPEM, srv
}

func parseIP(s string) net.IP { return net.ParseIP(s) }
