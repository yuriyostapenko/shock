// Command shock is the single SHOCK binary with two subcommands:
//
//	shock hook spawn-runner   the orchestrator's spawn-runner hook
//	shock session-controller  the lifecycle controller
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/yuriyostapenko/shock/internal/hook"
	"github.com/yuriyostapenko/shock/internal/sessioncontroller"
)

var version = "dev"

func main() {
	// Invoked through the /hooks/spawn-runner symlink, the binary is the hook.
	if filepath.Base(os.Args[0]) == "spawn-runner" {
		os.Exit(runHook())
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "hook":
		if len(os.Args) < 3 || os.Args[2] != "spawn-runner" {
			usage()
			os.Exit(2)
		}
		os.Exit(runHook())
	case "session-controller":
		if err := runSessionController(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "session-controller:", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: shock hook spawn-runner | shock session-controller [flags] | shock version")
}

// runHook returns the process exit code per the spawn-runner contract. The
// actionable error goes to stderr (the orchestrator surfaces its tail as the
// failure reason); nothing sensitive is ever printed.
func runHook() int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := hook.ConfigFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spawn-runner: configuration error:", err)
		return hook.ExitNonRetryable
	}
	req, err := hook.RequestFromEnv(os.Getenv, hook.OSReadFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spawn-runner: invalid request:", err)
		return hook.ExitNonRetryable
	}
	budget := time.Duration(cfg.HookTimeoutSeconds) * time.Second * 8 / 10
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "spawn-runner: kubeconfig:", err)
		return hook.ExitRetryable
	}
	restCfg.Timeout = 10 * time.Second
	c, err := client.New(restCfg, client.Options{Scheme: sessioncontroller.Scheme()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "spawn-runner: client:", err)
		return hook.ExitRetryable
	}
	h := &hook.Hook{Client: c, Config: cfg, Log: logger}
	if err := h.Run(ctx, req); err != nil {
		code := hook.ExitCodeFor(err)
		fmt.Fprintf(os.Stderr, "spawn-runner: exit %d: %v\n", code, err)
		return code
	}
	return hook.ExitSubmitted
}

func runSessionController(args []string) error {
	fs := flag.NewFlagSet("session-controller", flag.ContinueOnError)
	bind := fs.String("bind-address", envOr("SHOCK_BIND_ADDRESS", ":8080"), "address serving /metrics, /healthz and /readyz")
	namespace := fs.String("namespace", os.Getenv("SHOCK_NAMESPACE"), "namespace to watch")
	release := fs.String("release", os.Getenv("SHOCK_RELEASE"), "Helm release name (app.kubernetes.io/instance)")
	resync := fs.Duration("resync", envDuration("SHOCK_RESYNC", 300*time.Second), "informer resync backstop")
	gcEnabled := fs.Bool("gc-enabled", envBool("SHOCK_GC_ENABLED", true), "delete Sandboxes idle past --gc-max-idle")
	gcMaxIdle := fs.Duration("gc-max-idle", envDuration("SHOCK_GC_MAX_IDLE", 336*time.Hour), "idle threshold for GC")
	zombieEnabled := fs.Bool("zombie-enabled", envBool("SHOCK_ZOMBIE_ENABLED", true), "emit Events for Pods stuck Terminating")
	zombieAfter := fs.Duration("zombie-alert-after", envDuration("SHOCK_ZOMBIE_ALERT_AFTER", 5*time.Minute), "Terminating age that counts as stranded")
	crdInterval := fs.Duration("crd-check-interval", envDuration("SHOCK_CRD_CHECK_INTERVAL", 15*time.Second), "prerequisite re-check interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *namespace == "" || *release == "" {
		return errors.New("--namespace/SHOCK_NAMESPACE and --release/SHOCK_RELEASE are required")
	}
	zl := zap.New(zap.UseDevMode(false))
	ctrl.SetLogger(zl)
	ctx := ctrl.LoggerInto(signalContext(), zl)
	zl.Info("shock session-controller", "version", version)
	return sessioncontroller.Run(ctx, ctrl.GetConfigOrDie(), sessioncontroller.ManagerOptions{
		Namespace:        *namespace,
		Release:          *release,
		BindAddress:      *bind,
		ResyncPeriod:     *resync,
		CRDCheckInterval: *crdInterval,
		Lifecycle: sessioncontroller.Options{
			GCEnabled:        *gcEnabled,
			GCMaxIdle:        *gcMaxIdle,
			ZombieEnabled:    *zombieEnabled,
			ZombieAlertAfter: *zombieAfter,
		},
	})
}

func signalContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
		<-ch
		os.Exit(1)
	}()
	return ctx
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
