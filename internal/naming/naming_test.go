package naming

import (
	"regexp"
	"strings"
	"testing"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestSandboxName(t *testing.T) {
	cases := []struct {
		in       string
		wantHash bool
	}{
		{"abc123", false},
		{"session_01hxyz", true},        // underscore altered
		{"Session-ABC", true},           // case altered
		{strings.Repeat("a", 60), true}, // truncated
		{"", true},
		{"--weird__id--", true},
	}
	for _, c := range cases {
		got := SandboxName(c.in)
		if !strings.HasPrefix(got, "cs-") {
			t.Errorf("SandboxName(%q) = %q, missing prefix", c.in, got)
		}
		if !dnsLabel.MatchString(got) {
			t.Errorf("SandboxName(%q) = %q is not a DNS label", c.in, got)
		}
		if len(got) > 3+maxSanitizedLen+1+hashSuffixLength {
			t.Errorf("SandboxName(%q) = %q too long (%d)", c.in, got, len(got))
		}
		hasHash := regexp.MustCompile(`-[0-9a-f]{8}$`).MatchString(got)
		if hasHash != c.wantHash {
			t.Errorf("SandboxName(%q) = %q, hash suffix = %v, want %v", c.in, got, hasHash, c.wantHash)
		}
		if got != SandboxName(c.in) {
			t.Errorf("SandboxName(%q) not deterministic", c.in)
		}
	}
	if SandboxName("session_a") == SandboxName("session-a") {
		t.Error("distinct raw ids must not collide after sanitizing")
	}
}

func TestWorkOrderSecretName(t *testing.T) {
	a := WorkOrderSecretName("rel", "sess", "uid-1", "order-1")
	b := WorkOrderSecretName("rel", "sess", "uid-2", "order-1")
	if a == b {
		t.Fatal("secret name must depend on the Sandbox UID")
	}
	if !regexp.MustCompile(`^wo-[0-9a-f]{64}$`).MatchString(a) {
		t.Fatalf("unexpected secret name %q", a)
	}
	// Length-prefixed encoding: shifting a boundary must change the digest.
	if WorkOrderSecretName("ab", "c", "u", "o") == WorkOrderSecretName("a", "bc", "u", "o") {
		t.Fatal("encoding is ambiguous")
	}
}

func TestLabelValue(t *testing.T) {
	if LabelValue("session_01abc") != "session_01abc" {
		t.Error("valid label value must be preserved")
	}
	if LabelValue("") != "" {
		t.Error("empty stays empty")
	}
	long := strings.Repeat("x", 80)
	got := LabelValue(long)
	if len(got) > 63 || !isValidLabelValue(got) {
		t.Errorf("LabelValue(long) = %q invalid", got)
	}
	if LabelValue("bad/value") == "bad/value" {
		t.Error("invalid chars must be sanitized")
	}
}
