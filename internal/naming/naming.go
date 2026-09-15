// Package naming holds the names, labels and annotations shared by the hook
// and the session controller (spec section 5).
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strings"
	"unicode"
)

// Label keys. app.kubernetes.io/* are the Kubernetes recommended labels; the
// shock.invalid/* keys are SHOCK's own.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelInstance  = "app.kubernetes.io/instance"
	LabelVersion   = "app.kubernetes.io/version"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelPartOf    = "app.kubernetes.io/part-of"
	LabelChart     = "helm.sh/chart"

	// LabelSessionID carries the raw Claude session id when it is a valid
	// label value, otherwise the sanitized form (see LabelValue).
	LabelSessionID = "shock.invalid/session-id"
	// LabelAccountID carries the stable Anthropic account id (never the email).
	LabelAccountID = "shock.invalid/account-id"

	// PartOf is the constant value of app.kubernetes.io/part-of.
	PartOf = "shock"

	// Component values for app.kubernetes.io/name.
	ComponentOrchestrator      = "orchestrator"
	ComponentSessionController = "session-controller"
	ComponentRunner            = "runner"
)

// Annotation keys on the Sandbox (spec section 5).
const (
	AnnotationPrefix = "shock.invalid/"

	// AnnotationPendingSpawn is the order id whose new runner Pod has not yet
	// been observed.
	AnnotationPendingSpawn = AnnotationPrefix + "pending-spawn"
	// AnnotationPendingSpawnAt is the RFC 3339 UTC time the pending order was written.
	AnnotationPendingSpawnAt = AnnotationPrefix + "pending-spawn-at"
	// AnnotationAppliedSpawn is the most recent order id for which
	// operatingMode: Running has been issued.
	AnnotationAppliedSpawn = AnnotationPrefix + "applied-spawn"
	// AnnotationPendingSecret is the immutable Secret name for pending-spawn.
	AnnotationPendingSecret = AnnotationPrefix + "pending-secret"
	// AnnotationLastOrderID is the highest accepted order id.
	AnnotationLastOrderID = AnnotationPrefix + "last-order-id"
	// AnnotationLastOrderAttempt is the highest accepted attempt counter.
	AnnotationLastOrderAttempt = AnnotationPrefix + "last-order-attempt"
	// AnnotationLastOrderSecret is the Secret for the highest accepted order.
	AnnotationLastOrderSecret = AnnotationPrefix + "last-order-secret"
	// AnnotationLastSuspendedAt is the RFC 3339 UTC time Sleep was issued.
	AnnotationLastSuspendedAt = AnnotationPrefix + "last-suspended-at"

	// AnnotationOrderID is set on the Sandbox podTemplate (and therefore on the
	// runner Pod): the order assigned to that Pod.
	AnnotationOrderID = AnnotationPrefix + "order-id"
	// AnnotationOrderAttempt is set on work-order Secrets.
	AnnotationOrderAttempt = AnnotationPrefix + "order-attempt"
	// AnnotationSessionID is set on work-order Secrets.
	AnnotationSessionID = AnnotationPrefix + "session-id"
)

// Template anchors and runtime paths (spec sections 5, 6, 8).
const (
	// RunnerContainerName is the anchor container the hook injects into.
	RunnerContainerName = "runner"
	// WorkOrderVolumeName is the anchor volume whose secretName Wake sets.
	WorkOrderVolumeName = "work-order"
	// WorkOrderMountPath is where the work-order Secret is mounted in the runner.
	WorkOrderMountPath = "/var/run/claude/work-order"
	// WorkOrderSecretKey is the key inside the work-order Secret.
	WorkOrderSecretKey = "work-order"
	// WorkspaceClaimName is the volumeClaimTemplate name; the sandbox
	// controller derives the PVC name as <WorkspaceClaimName>-<sandboxName>.
	WorkspaceClaimName = "workspace"

	// PlaceholderSecretName is the reserved work-order Secret reference in a
	// Suspended Sandbox before Wake installs a real order. It is never created.
	PlaceholderSecretName = "shock-placeholder-never-materialized"

	sandboxPrefix    = "cs" // Claude session: Sandbox names
	secretPrefix     = "wo" // work order: Secret names
	maxNameLen       = 63   // Sandbox and Pod names stay DNS labels
	maxReleaseLen    = 24   // release part of a scoped name, before "-cs-"
	hashSuffixLength = 8
)

// SelectorLabels returns the frozen selector subset (name + instance).
func SelectorLabels(component, release string) map[string]string {
	return map[string]string{
		LabelName:     component,
		LabelInstance: release,
	}
}

// SandboxName is "<release>-cs-<session-id>", RFC 1123 sanitized and at most
// 63 chars: the release part is cut to 24, the id part to what fits, with an
// 8-char fnv hash suffix whenever sanitizing or truncating changed the id.
func SandboxName(release, sessionID string) string {
	return scopedName(release, sandboxPrefix, sessionID)
}

func scopedName(release, kind, raw string) string {
	prefix := releasePrefix(release) + kind + "-"
	maxID := maxNameLen - len(prefix) - 1 - hashSuffixLength
	sanitized, altered := sanitizeRFC1123(raw)
	if len(sanitized) > maxID {
		sanitized = strings.TrimRight(sanitized[:maxID], "-")
		altered = true
	}
	if altered || sanitized == "" {
		return prefix + sanitized + "-" + fnvHash(raw)
	}
	return prefix + sanitized
}

// PVCName is the PVC name the sandbox controller derives for a Sandbox.
func PVCName(sandboxName string) string {
	return WorkspaceClaimName + "-" + sandboxName
}

// WorkOrderSecretName is "<release>-wo-<sha256>" over a length-prefixed
// encoding of (release, sessionID, sandboxUID, orderID). The UID keeps a
// recreated Sandbox from adopting a deleted one's Secret.
func WorkOrderSecretName(release, sessionID, sandboxUID, orderID string) string {
	h := sha256.New()
	for _, part := range []string{release, sessionID, sandboxUID, orderID} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return releasePrefix(release) + secretPrefix + "-" + hex.EncodeToString(h.Sum(nil))
}

// releasePrefix is "<sanitized release>-" cut to maxReleaseLen, or "".
func releasePrefix(release string) string {
	rel, _ := sanitizeRFC1123(release)
	if len(rel) > maxReleaseLen {
		rel = strings.TrimRight(rel[:maxReleaseLen], "-")
	}
	if rel == "" {
		return ""
	}
	return rel + "-"
}

// LabelValue returns v if it is a valid label value, else a sanitized form
// with an fnv hash suffix.
func LabelValue(v string) string {
	if v == "" {
		return ""
	}
	if isValidLabelValue(v) {
		return v
	}
	sanitized, _ := sanitizeRFC1123(v)
	if len(sanitized) > 63-hashSuffixLength-1 {
		sanitized = strings.TrimRight(sanitized[:63-hashSuffixLength-1], "-")
	}
	return sanitized + "-" + fnvHash(v)
}

func isValidLabelValue(v string) bool {
	if len(v) > 63 {
		return false
	}
	for i, r := range v {
		alnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if i == 0 || i == len(v)-1 {
			if !alnum {
				return false
			}
			continue
		}
		if !alnum && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// sanitizeRFC1123 lowercases, maps other characters to "-", collapses and
// trims dashes. altered reports whether the output differs.
func sanitizeRFC1123(s string) (string, bool) {
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		r = unicode.ToLower(r)
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			lastDash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	return out, out != s
}

func fnvHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}
