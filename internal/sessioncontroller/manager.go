package sessioncontroller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/yuriyostapenko/shock/internal/buildinfo"
	"github.com/yuriyostapenko/shock/internal/naming"
)

// ManagerOptions configure Run.
type ManagerOptions struct {
	Namespace string
	Release   string
	// BindAddress serves /metrics, /healthz and /readyz on one port.
	BindAddress      string
	ResyncPeriod     time.Duration
	CRDCheckInterval time.Duration
	Lifecycle        Options
}

// Scheme returns the runtime scheme with core and agent-sandbox types.
func Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(sandboxv1beta1.AddToScheme(s))
	return s
}

// crdProbe checks via discovery whether the Sandbox CRD is served.
type crdProbe struct {
	dc     discovery.DiscoveryInterface
	served atomic.Bool
}

func (p *crdProbe) check() (bool, error) {
	res, err := p.dc.ServerResourcesForGroupVersion(sandboxv1beta1.GroupVersion.String())
	if err != nil {
		p.served.Store(false)
		if discovery.IsGroupDiscoveryFailedError(err) || isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, r := range res.APIResources {
		if r.Name == "sandboxes" {
			p.served.Store(true)
			CRDServed.Set(1)
			return true, nil
		}
	}
	p.served.Store(false)
	CRDServed.Set(0)
	return false, nil
}

func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

// Run starts the controller. Without the Sandbox CRD it serves /healthz 200
// and /readyz 503 and waits; once served, readiness follows cache sync.
func Run(ctx context.Context, cfg *rest.Config, opts ManagerOptions) error {
	logger := log.FromContext(ctx).WithName("session-controller")
	if opts.CRDCheckInterval == 0 {
		opts.CRDCheckInterval = 15 * time.Second
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	probe := &crdProbe{dc: dc}
	metrics.Registry.MustRegister(CRDServed, BuildInfo)
	bi := buildinfo.Get()
	BuildInfo.WithLabelValues(bi.Version, bi.Revision, bi.GoVersion).Set(1)

	if err := waitForCRD(ctx, logger, probe, opts); err != nil {
		return err
	}

	sel := labels.SelectorFromSet(naming.SelectorLabels(naming.ComponentRunner, opts.Release))
	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Scheme:                 Scheme(),
		Metrics:                metricsserver.Options{BindAddress: opts.BindAddress},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Cache: cache.Options{
			SyncPeriod:        &opts.ResyncPeriod,
			DefaultNamespaces: map[string]cache.Config{opts.Namespace: {}},
			ByObject: map[client.Object]cache.ByObject{
				&sandboxv1beta1.Sandbox{}: {Label: sel},
				&corev1.Pod{}:             {Label: sel},
			},
		},
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}
	rec := &Reconciler{
		Client:   mgr.GetClient(),
		Secrets:  mgr.GetAPIReader(),
		Recorder: mgr.GetEventRecorder("shock-session-controller"),
		Release:  opts.Release,
		Options:  opts.Lifecycle,
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("shock-session").
		For(&sandboxv1beta1.Sandbox{}).
		Owns(&corev1.Pod{}).
		Complete(rec); err != nil {
		return err
	}
	metrics.Registry.MustRegister(&StateCollector{Reader: mgr.GetClient(), Namespace: opts.Namespace})

	var synced atomic.Bool
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return errors.New("cache never synced")
		}
		synced.Store(true)
		t := time.NewTicker(opts.CRDCheckInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				if ok, err := probe.check(); err != nil {
					logger.Error(err, "CRD re-check failed")
				} else if !ok {
					logger.Info("Sandbox CRD is no longer served; reporting not ready")
				}
			}
		}
	})); err != nil {
		return err
	}
	if err := mgr.AddMetricsServerExtraHandler("/healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})); err != nil {
		return err
	}
	if err := mgr.AddMetricsServerExtraHandler("/readyz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch {
		case !probe.served.Load():
			http.Error(w, "sandboxes.agents.x-k8s.io/v1beta1 is not served", http.StatusServiceUnavailable)
		case !synced.Load():
			http.Error(w, "informer cache not synced", http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}
	})); err != nil {
		return err
	}
	logger.Info("starting manager", "namespace", opts.Namespace, "release", opts.Release, "gc", opts.Lifecycle.GCEnabled,
		"maxIdleAge", opts.Lifecycle.GCMaxIdleAge, "maxIdleSessions", opts.Lifecycle.GCMaxIdleSessions)
	return mgr.Start(ctx)
}

// waitForCRD blocks until the CRD is served, answering probes meanwhile.
func waitForCRD(ctx context.Context, logger interface {
	Info(string, ...any)
	Error(error, string, ...any)
}, probe *crdProbe, opts ManagerOptions) error {
	ok, err := probe.check()
	if err != nil {
		logger.Error(err, "discovery failed; will retry")
	}
	if ok {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sandboxes.agents.x-k8s.io/v1beta1 is not served; install agent-sandbox", http.StatusServiceUnavailable)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", opts.BindAddress)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", opts.BindAddress, err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	t := time.NewTicker(opts.CRDCheckInterval)
	defer t.Stop()
	for {
		logger.Info("waiting for prerequisite: sandboxes.agents.x-k8s.io/v1beta1 is not served; install agent-sandbox (https://github.com/kubernetes-sigs/agent-sandbox)",
			"recheck", opts.CRDCheckInterval.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		ok, err := probe.check()
		if err != nil {
			logger.Error(err, "discovery failed; will retry")
			continue
		}
		if ok {
			logger.Info("Sandbox CRD is served; starting")
			return nil
		}
	}
}
