package hook

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yuriyostapenko/shock/internal/sessioncontroller"
)

const testRelease = "shock-test"

// testTemplate mirrors what charts/shock renders into the ConfigMap.
const testTemplate = `apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  labels:
    app.kubernetes.io/name: runner
    app.kubernetes.io/instance: shock-test
    app.kubernetes.io/version: "1.0.0"
    app.kubernetes.io/managed-by: Helm
    app.kubernetes.io/part-of: shock
    helm.sh/chart: shock-0.1.0
spec:
  operatingMode: Suspended
  podTemplate:
    metadata:
      labels:
        app.kubernetes.io/name: runner
        app.kubernetes.io/instance: shock-test
        app.kubernetes.io/part-of: shock
        custom/label: keep-me
    spec:
      restartPolicy: Always
      automountServiceAccountToken: true
      containers:
        - name: runner
          image: example.invalid/runner:1
          args: ["self-hosted-runner", "--capacity", "1", "--base-dir", "/workspace"]
          volumeMounts:
            - name: workspace
              mountPath: /wrong
      volumes:
        - name: work-order
          secret:
            secretName: placeholder
  volumeClaimTemplates:
    - metadata:
        name: workspace
      spec:
        accessModes: ["ReadWriteOncePod"]
        resources:
          requests:
            storage: 1Gi
`

func writeTemplate(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sandbox-template.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// newFakeClient returns a fake client that assigns UIDs on Create like a
// real API server, so ownerReferences and optimistic patches work.
func newFakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	base := fake.NewClientBuilder().WithScheme(sessioncontroller.Scheme()).WithObjects(objs...).Build()
	return interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				obj.SetUID(uuid.NewUUID())
			}
			return c.Create(ctx, obj, opts...)
		},
	})
}

func testConfig(path string) Config {
	return Config{
		Release:                       testRelease,
		Namespace:                     "runners",
		TemplatePath:                  path,
		BaseDir:                       "/workspace",
		TerminationGracePeriodSeconds: 120,
		HookTimeoutSeconds:            30,
	}
}

func testRequest(order string, attempt int64) Request {
	return Request{
		OrderID:   order,
		SessionID: "session_01TEST",
		Attempt:   attempt,
		AccountID: "user_01ACC",
		WorkOrder: []byte("jwt-" + order),
	}
}
