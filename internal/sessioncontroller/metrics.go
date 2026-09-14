package sessioncontroller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yuriyostapenko/shock/internal/naming"
)

var (
	descCondition = prometheus.NewDesc("shock_sandbox_condition_status",
		"Sandbox condition status: 1 True, 0 False, -1 Unknown. current is whether observedGeneration matches metadata.generation.",
		[]string{"sandbox", "type", "reason", "operating_mode", "current"}, nil)
	descPendingAge = prometheus.NewDesc("shock_sandbox_pending_spawn_age_seconds",
		"Seconds since pending-spawn was written for Sandboxes with an unobserved order.",
		[]string{"sandbox"}, nil)
	descTerminating = prometheus.NewDesc("shock_sandbox_pod_terminating_seconds",
		"Seconds an owned runner Pod has been Terminating.",
		[]string{"sandbox", "pod"}, nil)
	descSandboxes = prometheus.NewDesc("shock_sandboxes",
		"Number of Sandboxes in this release by operatingMode.",
		[]string{"operating_mode"}, nil)
	descInfo = prometheus.NewDesc("shock_sandbox_info",
		"One series per Sandbox with its operatingMode and whether a spawn is pending.",
		[]string{"sandbox", "operating_mode", "pending"}, nil)
)

// CRDServed is 1 while sandboxes.agents.x-k8s.io/v1beta1 is served.
var CRDServed = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "shock_crd_served",
	Help: "1 when the agent-sandbox v1beta1 Sandbox CRD is served by the API server.",
})

// StateCollector exports sandbox-state series straight from the informer
// cache at scrape time, so nothing goes stale when a Sandbox disappears.
type StateCollector struct {
	Reader    client.Reader
	Namespace string
	Now       func() time.Time
}

// Describe implements prometheus.Collector.
func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descCondition
	ch <- descPendingAge
	ch <- descTerminating
	ch <- descSandboxes
	ch <- descInfo
}

// Collect implements prometheus.Collector.
func (c *StateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	list := &sandboxv1beta1.SandboxList{}
	if err := c.Reader.List(ctx, list, client.InNamespace(c.Namespace)); err != nil {
		return
	}
	byMode := map[string]float64{}
	byUID := map[string]string{}
	for i := range list.Items {
		sb := &list.Items[i]
		mode := string(sb.Spec.OperatingMode)
		byMode[mode]++
		byUID[string(sb.UID)] = sb.Name
		pending := sb.Annotations[naming.AnnotationPendingSpawn] != ""
		ch <- prometheus.MustNewConstMetric(descInfo, prometheus.GaugeValue, 1, sb.Name, mode, boolString(pending))
		for _, cond := range sb.Status.Conditions {
			v := 0.0
			switch cond.Status {
			case metav1.ConditionTrue:
				v = 1
			case metav1.ConditionUnknown:
				v = -1
			}
			ch <- prometheus.MustNewConstMetric(descCondition, prometheus.GaugeValue, v,
				sb.Name, cond.Type, cond.Reason, mode, boolString(cond.ObservedGeneration == sb.Generation))
		}
		if at := sb.Annotations[naming.AnnotationPendingSpawnAt]; pending && at != "" {
			if t, err := time.Parse(time.RFC3339, at); err == nil {
				ch <- prometheus.MustNewConstMetric(descPendingAge, prometheus.GaugeValue, now.Sub(t).Seconds(), sb.Name)
			}
		}
	}
	for mode, n := range byMode {
		ch <- prometheus.MustNewConstMetric(descSandboxes, prometheus.GaugeValue, n, mode)
	}
	pods := &corev1.PodList{}
	if err := c.Reader.List(ctx, pods, client.InNamespace(c.Namespace)); err != nil {
		return
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp.IsZero() {
			continue
		}
		ref := metav1.GetControllerOf(p)
		if ref == nil {
			continue
		}
		if name, ok := byUID[string(ref.UID)]; ok {
			ch <- prometheus.MustNewConstMetric(descTerminating, prometheus.GaugeValue, now.Sub(p.DeletionTimestamp.Time).Seconds(), name, p.Name)
		}
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
