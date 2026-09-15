package hook

import (
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/yaml"

	"github.com/yuriyostapenko/shock/internal/naming"
)

// LoadTemplate strict-decodes the Helm-rendered Sandbox template. An unknown
// field is an error, not a silent drop (spec section 6, template contract).
func LoadTemplate(path string) (*sandboxv1beta1.Sandbox, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading sandbox template: %w", err)
	}
	return ParseTemplate(raw)
}

// ParseTemplate strict-decodes template bytes into a typed Sandbox.
func ParseTemplate(raw []byte) (*sandboxv1beta1.Sandbox, error) {
	sb := &sandboxv1beta1.Sandbox{}
	if err := yaml.UnmarshalStrict(raw, sb); err != nil {
		return nil, fmt.Errorf("sandbox template is not a valid v1beta1 Sandbox: %w", err)
	}
	if sb.APIVersion != sandboxv1beta1.GroupVersion.String() || sb.Kind != "Sandbox" {
		return nil, fmt.Errorf("sandbox template must be apiVersion %s kind Sandbox, got %s %s", sandboxv1beta1.GroupVersion.String(), sb.APIVersion, sb.Kind)
	}
	return sb, nil
}

// Identity is the per-session data the hook stamps onto the template.
type Identity struct {
	Release   string
	Namespace string
	SessionID string
	AccountID string
}

// Contract holds the values the hook forces onto every podTemplate.
type Contract struct {
	// WorkspaceMountPath is the PVC mount path forced onto the runner's
	// workspace volumeMount; BaseDir (the runner's --base-dir) must be equal
	// to it or below it so the canonical clone lives on the disk.
	WorkspaceMountPath            string
	BaseDir                       string
	TerminationGracePeriodSeconds int64
}

// Validate checks the contract's own consistency.
func (c Contract) Validate() error {
	if c.WorkspaceMountPath == "" || !strings.HasPrefix(c.WorkspaceMountPath, "/") {
		return fmt.Errorf("workspace mount path %q must be absolute", c.WorkspaceMountPath)
	}
	mount := strings.TrimRight(c.WorkspaceMountPath, "/")
	if c.BaseDir != mount && !strings.HasPrefix(c.BaseDir, mount+"/") {
		return fmt.Errorf("runner base dir %q must be the workspace mount path %q or below it, or resumed sessions clone afresh", c.BaseDir, c.WorkspaceMountPath)
	}
	return nil
}

// ValidateAnchors checks that the template still has the anchors the hook
// needs. The error names the missing anchor.
func ValidateAnchors(sb *sandboxv1beta1.Sandbox) error {
	if findContainer(&sb.Spec.PodTemplate.Spec, naming.RunnerContainerName) == nil {
		return fmt.Errorf("sandbox template: required anchor missing: container named %q", naming.RunnerContainerName)
	}
	if findVolume(&sb.Spec.PodTemplate.Spec, naming.WorkOrderVolumeName) == nil {
		return fmt.Errorf("sandbox template: required anchor missing: volume named %q", naming.WorkOrderVolumeName)
	}
	if len(sb.Spec.VolumeClaimTemplates) == 0 || !hasClaimTemplate(sb, naming.WorkspaceClaimName) {
		return fmt.Errorf("sandbox template: required anchor missing: volumeClaimTemplate named %q", naming.WorkspaceClaimName)
	}
	for _, l := range []string{naming.LabelName, naming.LabelInstance, naming.LabelPartOf} {
		if sb.Labels[l] == "" {
			return fmt.Errorf("sandbox template: metadata.labels[%q] must be rendered by the chart", l)
		}
	}
	if sb.Labels[naming.LabelName] != naming.ComponentRunner {
		return fmt.Errorf("sandbox template: metadata.labels[%q] must be %q", naming.LabelName, naming.ComponentRunner)
	}
	return nil
}

// ApplyContract stamps identity and forces the load-bearing fields onto the
// template (spec section 6, "Forced fields"). It is idempotent.
func ApplyContract(sb *sandboxv1beta1.Sandbox, id Identity, c Contract) error {
	if err := ValidateAnchors(sb); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if sb.Labels[naming.LabelInstance] != id.Release {
		return fmt.Errorf("sandbox template: metadata.labels[%q]=%q does not match %s=%q", naming.LabelInstance, sb.Labels[naming.LabelInstance], EnvShockRelease, id.Release)
	}
	sb.Name = naming.SandboxName(id.SessionID)
	sb.Namespace = id.Namespace
	sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	// Never set: the sandbox controller short-circuits reconciliation on expiry.
	sb.Spec.ShutdownTime = nil
	sb.Spec.ShutdownPolicy = nil

	sessionLabels := map[string]string{
		naming.LabelSessionID: naming.LabelValue(id.SessionID),
		naming.LabelAccountID: naming.LabelValue(id.AccountID),
	}
	if sb.Labels == nil {
		sb.Labels = map[string]string{}
	}
	for k, v := range sessionLabels {
		sb.Labels[k] = v
	}

	pt := &sb.Spec.PodTemplate
	if pt.ObjectMeta.Labels == nil {
		pt.ObjectMeta.Labels = map[string]string{}
	}
	// The common set plus the session set, merged last so a user overlay can
	// add labels but never displace the selector set.
	for k, v := range sb.Labels {
		pt.ObjectMeta.Labels[k] = v
	}
	spec := &pt.Spec
	spec.RestartPolicy = corev1.RestartPolicyNever
	f := false
	spec.AutomountServiceAccountToken = &f
	grace := c.TerminationGracePeriodSeconds
	spec.TerminationGracePeriodSeconds = &grace

	runner := findContainer(spec, naming.RunnerContainerName)
	forceMount(runner, naming.WorkspaceClaimName, c.WorkspaceMountPath, false)
	forceMount(runner, naming.WorkOrderVolumeName, naming.WorkOrderMountPath, true)
	if id.AccountID != "" {
		runner.Args = withLockToAccount(runner.Args, id.AccountID)
	}

	wo := findVolume(spec, naming.WorkOrderVolumeName)
	if wo.Secret == nil {
		wo.VolumeSource = corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{}}
	}
	if wo.Secret.SecretName == "" {
		wo.Secret.SecretName = naming.PlaceholderSecretName
	}
	// The workspace volume itself is derived by the sandbox controller from
	// volumeClaimTemplates; a user-supplied one would shadow the PVC.
	spec.Volumes = removeVolume(spec.Volumes, naming.WorkspaceClaimName)
	return nil
}

// withLockToAccount appends --lock-to-account once, replacing any existing value.
func withLockToAccount(args []string, account string) []string {
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		if args[i] == "--lock-to-account" {
			i++ // skip its value
			continue
		}
		out = append(out, args[i])
	}
	return append(out, "--lock-to-account", account)
}

func forceMount(c *corev1.Container, name, path string, readOnly bool) {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == name {
			c.VolumeMounts[i].MountPath = path
			c.VolumeMounts[i].ReadOnly = readOnly
			c.VolumeMounts[i].SubPath = ""
			return
		}
	}
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: name, MountPath: path, ReadOnly: readOnly})
}

func findContainer(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

func findVolume(spec *corev1.PodSpec, name string) *corev1.Volume {
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == name {
			return &spec.Volumes[i]
		}
	}
	return nil
}

func removeVolume(vols []corev1.Volume, name string) []corev1.Volume {
	out := vols[:0]
	for _, v := range vols {
		if v.Name != name {
			out = append(out, v)
		}
	}
	return out
}

func hasClaimTemplate(sb *sandboxv1beta1.Sandbox, name string) bool {
	for _, t := range sb.Spec.VolumeClaimTemplates {
		if t.Name == name {
			return true
		}
	}
	return false
}
