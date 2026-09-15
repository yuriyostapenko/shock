package hook

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/yuriyostapenko/shock/internal/naming"
)

// LabelPreWarm marks standby (unbound) runner Jobs and their Secrets.
const LabelPreWarm = naming.AnnotationPrefix + "prewarm"

// runPreWarm handles a pre-warming request (empty session id). With
// orchestrator.minIdle == 0 it is a no-op. Otherwise it creates one
// ephemeral, unbound runner Job: no PVC, no account lock. Standby runners
// claim arbitrary sessions and therefore never get per-session disks.
func (h *Hook) runPreWarm(ctx context.Context, req Request) error {
	if h.Config.MinIdle <= 0 {
		h.Log.Info("pre-warm request ignored: orchestrator.minIdle is 0", "order", req.OrderID)
		return nil
	}
	id := Identity{Release: h.Config.Release, Namespace: h.Config.Namespace, SessionID: "", AccountID: ""}
	tmpl, err := h.template(id)
	if err != nil {
		return err
	}
	jobName := naming.PrewarmJobName(h.Config.Release, req.OrderID)
	secretName := naming.WorkOrderSecretName(h.Config.Release, "prewarm", "", req.OrderID)

	pt := tmpl.Spec.PodTemplate
	spec := pt.Spec.DeepCopy()
	// No PVC: a standby runner is not bound to a session, so its disk is scratch.
	if findVolume(spec, naming.WorkspaceClaimName) != nil {
		spec.Volumes = removeVolume(spec.Volumes, naming.WorkspaceClaimName)
	}
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name:         naming.WorkspaceClaimName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	if v := findVolume(spec, naming.WorkOrderVolumeName); v != nil {
		v.Secret.SecretName = secretName
	}
	labels := map[string]string{}
	for k, v := range pt.ObjectMeta.Labels {
		labels[k] = v
	}
	delete(labels, naming.LabelSessionID)
	delete(labels, naming.LabelAccountID)
	labels[LabelPreWarm] = "true"
	annotations := map[string]string{}
	for k, v := range pt.ObjectMeta.Annotations {
		annotations[k] = v
	}
	annotations[naming.AnnotationOrderID] = req.OrderID

	var backoff int32
	ttl := int32(3600)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobName,
			Namespace:   h.Config.Namespace,
			Labels:      labels,
			Annotations: map[string]string{naming.AnnotationOrderID: req.OrderID},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec:       *spec,
			},
		},
	}
	if err := h.Client.Create(ctx, job); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return classifyAPIError("creating pre-warm Job", err)
		}
		if err := h.Client.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, job); err != nil {
			return classifyAPIError("reading existing pre-warm Job", err)
		}
		if job.Annotations[naming.AnnotationOrderID] != req.OrderID {
			return nonRetryable("pre-warm Job %s exists for a different order", jobName)
		}
		h.Log.Info("pre-warm redelivery: Job already exists", "job", jobName)
	}

	immutable := true
	block := false
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        secretName,
			Namespace:   h.Config.Namespace,
			Labels:      labels,
			Annotations: map[string]string{naming.AnnotationOrderID: req.OrderID, naming.AnnotationOrderAttempt: "0"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, BlockOwnerDeletion: &block,
			}},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{naming.WorkOrderSecretKey: req.WorkOrder},
	}
	if err := h.Client.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return classifyAPIError("creating pre-warm work-order Secret", err)
		}
		existing := &corev1.Secret{}
		if err := h.Client.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, existing); err != nil {
			return classifyAPIError("reading existing pre-warm Secret", err)
		}
		if err := verifySecret(existing, secret); err != nil {
			return fmt.Errorf("pre-warm: %w", err)
		}
	}
	h.Log.Info("submitted pre-warm runner", "job", jobName)
	return nil
}
