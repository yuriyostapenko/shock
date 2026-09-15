package naming

import (
	"regexp"
	"strings"
	"testing"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestSandboxName(t *testing.T) {
	cases := []struct {
		release, in string
		wantHash    bool
	}{
		{"shock", "abc123", false},
		{"shock", "session_01hxyz", true},        // underscore altered
		{"shock", "Session-ABC", true},           // case altered
		{"shock", strings.Repeat("a", 60), true}, // truncated
		{"shock", "", true},
		{"shock", "--weird__id--", true},
		{strings.Repeat("r", 40), strings.Repeat("s", 60), true}, // both parts cut
		{"Rel_Name", "abc", false},                               // release sanitized, id untouched
		{"", "abc", false},                                       // no release part
	}
	for _, c := range cases {
		got := SandboxName(c.release, c.in)
		rel, _ := sanitizeRFC1123(c.release)
		if len(rel) > maxReleaseLen {
			rel = rel[:maxReleaseLen]
		}
		wantPrefix := "cs-"
		if rel != "" {
			wantPrefix = rel + "-cs-"
		}
		if !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("SandboxName(%q, %q) = %q, want prefix %q", c.release, c.in, got, wantPrefix)
		}
		if !dnsLabel.MatchString(got) {
			t.Errorf("SandboxName(%q, %q) = %q is not a DNS label", c.release, c.in, got)
		}
		if len(got) > maxNameLen {
			t.Errorf("SandboxName(%q, %q) = %q too long (%d)", c.release, c.in, got, len(got))
		}
		hasHash := regexp.MustCompile(`-[0-9a-f]{8}$`).MatchString(got)
		if hasHash != c.wantHash {
			t.Errorf("SandboxName(%q, %q) = %q, hash suffix = %v, want %v", c.release, c.in, got, hasHash, c.wantHash)
		}
		if got != SandboxName(c.release, c.in) {
			t.Errorf("SandboxName(%q, %q) not deterministic", c.release, c.in)
		}
	}
	if SandboxName("shock", "session_a") == SandboxName("shock", "session-a") {
		t.Error("distinct raw ids must not collide after sanitizing")
	}
	if SandboxName("a", "session-1") == SandboxName("b", "session-1") {
		t.Error("the same session in two releases must get distinct names")
	}
}

func TestWorkOrderSecretName(t *testing.T) {
	if got := WorkOrderSecretName("Rel_1", "sess", "uid-1", "order-1"); !strings.HasPrefix(got, "rel-1-wo-") || len(got) != len("rel-1-wo-")+64 {
		t.Errorf("secret name must be <release>-wo-<sha256>, got %q", got)
	}
	a := WorkOrderSecretName("rel", "sess", "uid-1", "order-1")
	b := WorkOrderSecretName("rel", "sess", "uid-2", "order-1")
	if a == b {
		t.Fatal("secret name must depend on the Sandbox UID")
	}
	if !regexp.MustCompile(`^rel-wo-[0-9a-f]{64}$`).MatchString(a) {
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
