// Package hook implements `shock hook spawn-runner` (spec section 6): it
// creates or patches the session's Sandbox, publishes the work-order Secret
// and stamps pending-spawn. It never waits on Pods; the controller sequences.
package hook

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yuriyostapenko/shock/internal/naming"
	"github.com/yuriyostapenko/shock/internal/patch"
)

// Exit codes per the documented spawn-runner contract.
const (
	ExitSubmitted    = 0 // submitted, redelivered or superseded
	ExitRetryable    = 1 // transient: the session backs off and is re-offered
	ExitNonRetryable = 2 // circuit-broken until an Owner retries
)

// DefaultMaxAttempts bounds the re-read/recompute loop on API conflicts.
const DefaultMaxAttempts = 12

// ExitError carries the exit code the process must return.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

func retryable(format string, args ...any) error {
	return &ExitError{Code: ExitRetryable, Err: fmt.Errorf(format, args...)}
}

func nonRetryable(format string, args ...any) error {
	return &ExitError{Code: ExitNonRetryable, Err: fmt.Errorf(format, args...)}
}

// ExitCodeFor maps any error to the process exit code.
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitSubmitted
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return ExitRetryable
}

// classifyAPIError maps a Kubernetes API error to the exit-code contract.
func classifyAPIError(op string, err error) error {
	switch {
	case meta.IsNoMatchError(err):
		return retryable("%s: the Sandbox CRD (%s) is not served; install agent-sandbox", op, sandboxv1beta1.GroupVersion.String())
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return nonRetryable("%s: RBAC denied: %v", op, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return nonRetryable("%s: rejected by the API server: %v", op, err)
	default:
		return retryable("%s: %v", op, err)
	}
}

// Hook runs one spawn request.
type Hook struct {
	Client      client.Client
	Config      Config
	Log         *slog.Logger
	Now         func() time.Time
	MaxAttempts int
}

// Run executes one request; ExitCodeFor maps the error to the exit code.
// Errors and logs never carry the work order or the account email.
func (h *Hook) Run(ctx context.Context, req Request) error {
	if h.Now == nil {
		h.Now = time.Now
	}
	if h.MaxAttempts == 0 {
		h.MaxAttempts = DefaultMaxAttempts
	}
	if h.Log == nil {
		h.Log = slog.Default()
	}
	if req.SessionID == "" {
		// Standby (pre-warm) order, sent only with --min-idle > 0. Unsupported:
		// a standby runner has no per-session disk.
		return nonRetryable("order %s has no session id: pre-warming is not supported; run the orchestrator without --min-idle", req.OrderID)
	}
	return h.runSession(ctx, req)
}

func (h *Hook) template(id Identity) (*sandboxv1beta1.Sandbox, error) {
	tmpl, err := LoadTemplate(h.Config.TemplatePath)
	if err != nil {
		return nil, nonRetryable("%v", err)
	}
	if err := ApplyContract(tmpl, id, Contract{
		WorkspaceMountPath:            h.Config.WorkspaceMountPath,
		BaseDir:                       h.Config.BaseDir,
		TerminationGracePeriodSeconds: h.Config.TerminationGracePeriodSeconds,
	}); err != nil {
		return nil, nonRetryable("%v", err)
	}
	return tmpl, nil
}

func (h *Hook) runSession(ctx context.Context, req Request) error {
	id := Identity{Release: h.Config.Release, Namespace: h.Config.Namespace, SessionID: req.SessionID, AccountID: req.AccountID}
	tmpl, err := h.template(id)
	if err != nil {
		return err
	}
	log := h.Log.With("sandbox", tmpl.Name, "order", req.OrderID, "attempt", req.Attempt)
	key := types.NamespacedName{Namespace: tmpl.Namespace, Name: tmpl.Name}

	for attempt := 0; attempt < h.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(50*(1<<min(attempt, 4)))*time.Millisecond); err != nil {
				return retryable("interrupted while retrying: %v", err)
			}
		}
		sb := &sandboxv1beta1.Sandbox{}
		err := h.Client.Get(ctx, key, sb)
		switch {
		case apierrors.IsNotFound(err):
			created, cerr := h.createSandbox(ctx, tmpl, req)
			if apierrors.IsAlreadyExists(cerr) {
				log.Info("sandbox appeared concurrently; taking the existing-object path")
				continue
			}
			if cerr != nil {
				return classifyAPIError("creating Sandbox", cerr)
			}
			log.Info("created suspended sandbox with pending order")
			sb = created
		case err != nil:
			return classifyAPIError("reading Sandbox", err)
		}

		done, err := h.reconcileExisting(ctx, log, sb, tmpl, req)
		if err != nil {
			if patch.IsStale(err) {
				log.Info("sandbox changed underneath us; re-reading", "reason", err.Error())
				continue
			}
			return err
		}
		if done {
			return nil
		}
	}
	return retryable("gave up after %d conflicting attempts on Sandbox %s", h.MaxAttempts, tmpl.Name)
}

// createSandbox creates the Sandbox Suspended with the pending order in the
// create body; applied-spawn stays absent.
func (h *Hook) createSandbox(ctx context.Context, tmpl *sandboxv1beta1.Sandbox, req Request) (*sandboxv1beta1.Sandbox, error) {
	sb := tmpl.DeepCopy()
	if sb.Annotations == nil {
		sb.Annotations = map[string]string{}
	}
	sb.Annotations[naming.AnnotationPendingSpawn] = req.OrderID
	sb.Annotations[naming.AnnotationPendingSpawnAt] = h.Now().UTC().Format(time.RFC3339)
	sb.Annotations[naming.AnnotationLastOrderID] = req.OrderID
	sb.Annotations[naming.AnnotationLastOrderAttempt] = strconv.FormatInt(req.Attempt, 10)
	delete(sb.Annotations, naming.AnnotationAppliedSpawn)
	delete(sb.Annotations, naming.AnnotationPendingSecret)
	delete(sb.Annotations, naming.AnnotationLastOrderSecret)
	delete(sb.Annotations, naming.AnnotationLastSuspendedAt)
	sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	if err := h.Client.Create(ctx, sb); err != nil {
		return nil, err
	}
	return sb, nil
}

type decision int

const (
	decisionRedelivery decision = iota
	decisionSuperseded
	decisionNewer
)

// evaluate classifies the request against the Sandbox's accepted order.
func evaluate(sb *sandboxv1beta1.Sandbox, req Request, release string) (decision, error) {
	if !sb.DeletionTimestamp.IsZero() {
		return 0, retryable("Sandbox %s is being deleted; refusing to accept work onto it", sb.Name)
	}
	if got := sb.Labels[naming.LabelPartOf]; got != naming.PartOf {
		return 0, nonRetryable("Sandbox %s exists but is not managed by SHOCK (%s=%q)", sb.Name, naming.LabelPartOf, got)
	}
	if got := sb.Labels[naming.LabelInstance]; got != release {
		return 0, nonRetryable("Sandbox %s belongs to release %q, not %q; a session belongs to exactly one release", sb.Name, got, release)
	}
	if got, want := sb.Labels[naming.LabelSessionID], naming.LabelValue(req.SessionID); got != want {
		return 0, nonRetryable("Sandbox %s is labeled for a different session", sb.Name)
	}
	lastID := sb.Annotations[naming.AnnotationLastOrderID]
	lastAttempt, err := parseAttempt(sb.Annotations[naming.AnnotationLastOrderAttempt])
	if err != nil {
		return 0, nonRetryable("Sandbox %s has an unreadable %s annotation: %v", sb.Name, naming.AnnotationLastOrderAttempt, err)
	}
	switch {
	case req.OrderID == lastID:
		return decisionRedelivery, nil
	case req.Attempt < lastAttempt:
		return decisionSuperseded, nil
	case req.Attempt == lastAttempt:
		return 0, nonRetryable("protocol error: order %s and accepted order %s share attempt %d for session on Sandbox %s", req.OrderID, lastID, req.Attempt, sb.Name)
	default:
		return decisionNewer, nil
	}
}

func parseAttempt(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// reconcileExisting handles an existing Sandbox; a stale error means re-read.
func (h *Hook) reconcileExisting(ctx context.Context, log *slog.Logger, sb, tmpl *sandboxv1beta1.Sandbox, req Request) (bool, error) {
	d, err := evaluate(sb, req, h.Config.Release)
	if err != nil {
		return false, err
	}
	switch d {
	case decisionSuperseded:
		log.Info("order superseded by a higher accepted attempt; nothing written",
			"acceptedAttempt", sb.Annotations[naming.AnnotationLastOrderAttempt])
		return true, nil
	case decisionRedelivery:
		return h.repairRedelivery(ctx, log, sb, tmpl, req)
	case decisionNewer:
		return h.publishNewer(ctx, log, sb, tmpl, req)
	}
	return false, nonRetryable("unreachable decision %d", d)
}

// repairRedelivery completes the accepted order's Secret and pointers
// without re-arming pending-spawn.
func (h *Hook) repairRedelivery(ctx context.Context, log *slog.Logger, sb, tmpl *sandboxv1beta1.Sandbox, req Request) (bool, error) {
	secretName := naming.WorkOrderSecretName(h.Config.Release, req.SessionID, string(sb.UID), req.OrderID)
	if err := h.ensureSecret(ctx, sb, tmpl, req, secretName); err != nil {
		return false, err
	}
	mut := sb.DeepCopy()
	changed := false
	if mut.Annotations[naming.AnnotationPendingSpawn] == req.OrderID {
		switch cur := mut.Annotations[naming.AnnotationPendingSecret]; cur {
		case "":
			mut.Annotations[naming.AnnotationPendingSecret] = secretName
			changed = true
		case secretName:
		default:
			return false, nonRetryable("Sandbox %s points pending order %s at Secret %s, expected %s", sb.Name, req.OrderID, cur, secretName)
		}
	}
	if mut.Annotations[naming.AnnotationLastOrderID] == req.OrderID && mut.Annotations[naming.AnnotationLastOrderSecret] == "" {
		mut.Annotations[naming.AnnotationLastOrderSecret] = secretName
		changed = true
	}
	if !changed {
		log.Info("redelivery of the accepted order; preparation already complete")
		return true, nil
	}
	if err := h.patch(ctx, sb, mut); err != nil {
		return false, err
	}
	log.Info("redelivery repaired pointer annotations", "secret", secretName)
	return true, nil
}

// publishNewer accepts a higher attempt: create its Secret, then in one
// conditional patch move the pointers, request suspension and refresh the
// chart template. Wake installs the order once suspension is confirmed.
func (h *Hook) publishNewer(ctx context.Context, log *slog.Logger, sb, tmpl *sandboxv1beta1.Sandbox, req Request) (bool, error) {
	secretName := naming.WorkOrderSecretName(h.Config.Release, req.SessionID, string(sb.UID), req.OrderID)
	if err := h.ensureSecret(ctx, sb, tmpl, req, secretName); err != nil {
		return false, err
	}
	mut := sb.DeepCopy()
	if mut.Annotations == nil {
		mut.Annotations = map[string]string{}
	}
	mut.Annotations[naming.AnnotationPendingSpawn] = req.OrderID
	mut.Annotations[naming.AnnotationPendingSpawnAt] = h.Now().UTC().Format(time.RFC3339)
	mut.Annotations[naming.AnnotationPendingSecret] = secretName
	mut.Annotations[naming.AnnotationLastOrderID] = req.OrderID
	mut.Annotations[naming.AnnotationLastOrderAttempt] = strconv.FormatInt(req.Attempt, 10)
	mut.Annotations[naming.AnnotationLastOrderSecret] = secretName
	mut.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended

	// Refresh the pod template so image and flag changes reach the next spawn.
	newPT := tmpl.Spec.PodTemplate.DeepCopy()
	if cur := sb.Spec.PodTemplate.ObjectMeta.Annotations[naming.AnnotationOrderID]; cur != "" {
		if newPT.ObjectMeta.Annotations == nil {
			newPT.ObjectMeta.Annotations = map[string]string{}
		}
		newPT.ObjectMeta.Annotations[naming.AnnotationOrderID] = cur
	}
	if curVol := findVolume(&sb.Spec.PodTemplate.Spec, naming.WorkOrderVolumeName); curVol != nil && curVol.Secret != nil {
		if v := findVolume(&newPT.Spec, naming.WorkOrderVolumeName); v != nil {
			v.Secret.SecretName = curVol.Secret.SecretName
		}
	}
	// Keep the CR's own labels; add the session set.
	for k, v := range sb.Spec.PodTemplate.ObjectMeta.Labels {
		if _, ok := newPT.ObjectMeta.Labels[k]; !ok {
			newPT.ObjectMeta.Labels[k] = v
		}
	}
	mut.Spec.PodTemplate = *newPT

	if err := h.patch(ctx, sb, mut); err != nil {
		return false, err
	}
	log.Info("accepted newer order; requested suspension", "secret", secretName,
		"previousOrder", sb.Annotations[naming.AnnotationLastOrderID])
	return true, nil
}

// patch applies a UID + resourceVersion pinned merge patch.
func (h *Hook) patch(ctx context.Context, orig, mut *sandboxv1beta1.Sandbox) error {
	p, err := patch.Optimistic(orig, mut)
	if err != nil {
		return nonRetryable("building patch: %v", err)
	}
	if err := h.Client.Patch(ctx, mut, p); err != nil {
		if patch.IsStale(err) {
			return err
		}
		return classifyAPIError("patching Sandbox", err)
	}
	return nil
}

// ensureSecret creates the immutable, owned Secret; on AlreadyExists the
// existing one must match exactly or the order is non-retryable.
func (h *Hook) ensureSecret(ctx context.Context, sb, tmpl *sandboxv1beta1.Sandbox, req Request, name string) error {
	desired := workOrderSecret(sb, tmpl.Labels, req, name)
	err := h.Client.Create(ctx, desired)
	if err == nil {
		h.Log.Info("created work-order secret", "secret", name, "order", req.OrderID)
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return classifyAPIError("creating work-order Secret", err)
	}
	existing := &corev1.Secret{}
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: name}, existing); err != nil {
		return classifyAPIError("reading existing work-order Secret", err)
	}
	return verifySecret(existing, desired)
}

func workOrderSecret(sb *sandboxv1beta1.Sandbox, labels map[string]string, req Request, name string) *corev1.Secret {
	immutable := true
	block := false
	lbls := make(map[string]string, len(labels)+2)
	for k, v := range labels {
		lbls[k] = v
	}
	lbls[naming.LabelSessionID] = naming.LabelValue(req.SessionID)
	lbls[naming.LabelAccountID] = naming.LabelValue(req.AccountID)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sb.Namespace,
			Labels:    lbls,
			Annotations: map[string]string{
				naming.AnnotationOrderID:      req.OrderID,
				naming.AnnotationOrderAttempt: strconv.FormatInt(req.Attempt, 10),
				naming.AnnotationSessionID:    req.SessionID,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         sandboxv1beta1.GroupVersion.String(),
				Kind:               "Sandbox",
				Name:               sb.Name,
				UID:                sb.UID,
				BlockOwnerDeletion: &block,
			}},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{naming.WorkOrderSecretKey: req.WorkOrder},
	}
}

// verifySecret compares an existing Secret to the desired one; messages carry no data.
func verifySecret(existing, desired *corev1.Secret) error {
	if existing.Immutable == nil || !*existing.Immutable {
		return nonRetryable("work-order Secret %s exists but is not immutable", existing.Name)
	}
	if subtle.ConstantTimeCompare(existing.Data[naming.WorkOrderSecretKey], desired.Data[naming.WorkOrderSecretKey]) != 1 {
		return nonRetryable("work-order Secret %s exists with different data", existing.Name)
	}
	for _, k := range []string{naming.AnnotationOrderID, naming.AnnotationSessionID, naming.AnnotationOrderAttempt} {
		if existing.Annotations[k] != desired.Annotations[k] {
			return nonRetryable("work-order Secret %s exists with a different %s", existing.Name, k)
		}
	}
	want := desired.OwnerReferences[0].UID
	for _, ref := range existing.OwnerReferences {
		if ref.UID == want {
			return nil
		}
	}
	return nonRetryable("work-order Secret %s exists but is not owned by Sandbox UID %s", existing.Name, want)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
