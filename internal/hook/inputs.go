package hook

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Environment set by the orchestrator on every spawn-runner invocation
// (code.claude.com/docs/en/self-hosted-environments-configuration). ATTEMPT
// is zero-based; a standby order has an empty session id.
const (
	EnvWorkOrderFile = "CLAUDE_RUNNER_WORK_ORDER_FILE"
	EnvOrderID       = "CLAUDE_RUNNER_ORDER_ID"
	EnvSessionID     = "CLAUDE_RUNNER_SESSION_ID"
	EnvSessionUUID   = "CLAUDE_RUNNER_SESSION_UUID"
	EnvAttempt       = "CLAUDE_RUNNER_ATTEMPT"
	EnvAccountID     = "CLAUDE_RUNNER_ACCOUNT_ID"
	EnvAccountEmail  = "CLAUDE_RUNNER_ACCOUNT_EMAIL" // PII: read for nothing, never logged
	EnvPrimaryRepo   = "CLAUDE_RUNNER_PRIMARY_REPO_URL"
	EnvPoolID        = "CLAUDE_RUNNER_POOL_ID"
)

// Environment variable names the chart sets on the orchestrator Deployment.
const (
	EnvShockRelease        = "SHOCK_RELEASE"
	EnvShockNamespace      = "SHOCK_NAMESPACE"
	EnvShockTemplatePath   = "SHOCK_TEMPLATE_PATH"
	EnvShockBaseDir        = "SHOCK_RUNNER_BASE_DIR"
	EnvShockMountPath      = "SHOCK_RUNNER_WORKSPACE_MOUNT_PATH"
	EnvShockGracePeriod    = "SHOCK_RUNNER_TERMINATION_GRACE_PERIOD_SECONDS"
	EnvShockHookTimeout    = "SHOCK_HOOK_TIMEOUT_SECONDS"
	EnvShockMaxActive      = "SHOCK_MAX_ACTIVE_SESSIONS"
	DefaultTemplatePath    = "/etc/shock/sandbox-template.yaml"
	DefaultMountPath       = "/home/runner"
	DefaultBaseDir         = "/home/runner/workspace"
	DefaultGracePeriodSecs = 120
	DefaultHookTimeoutSecs = 30
)

// Request is the parsed spawn request.
type Request struct {
	OrderID     string
	SessionID   string
	SessionUUID string
	Attempt     int64
	AccountID   string
	// WorkOrder is the JWT copied out of the temp file. Never log it.
	WorkOrder []byte
}

// Config is the chart-provided configuration.
type Config struct {
	Release      string
	Namespace    string
	TemplatePath string
	// WorkspaceMountPath is where the per-session PVC is mounted; BaseDir must
	// lie under it.
	WorkspaceMountPath            string
	BaseDir                       string
	TerminationGracePeriodSeconds int64
	HookTimeoutSeconds            int
	// MaxActiveSessions caps Running-or-pending Sandboxes; 0 = unlimited.
	MaxActiveSessions int
}

var orderIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)

// ConfigFromEnv reads the chart-provided configuration.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{
		Release:                       getenv(EnvShockRelease),
		Namespace:                     getenv(EnvShockNamespace),
		TemplatePath:                  getenv(EnvShockTemplatePath),
		WorkspaceMountPath:            getenv(EnvShockMountPath),
		BaseDir:                       getenv(EnvShockBaseDir),
		TerminationGracePeriodSeconds: DefaultGracePeriodSecs,
		HookTimeoutSeconds:            DefaultHookTimeoutSecs,
	}
	if c.TemplatePath == "" {
		c.TemplatePath = DefaultTemplatePath
	}
	if c.WorkspaceMountPath == "" {
		c.WorkspaceMountPath = DefaultMountPath
	}
	if c.BaseDir == "" {
		c.BaseDir = DefaultBaseDir
	}
	if c.Release == "" {
		return c, errors.New(EnvShockRelease + " is required")
	}
	if c.Namespace == "" {
		return c, errors.New(EnvShockNamespace + " is required")
	}
	if v := getenv(EnvShockGracePeriod); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return c, fmt.Errorf("%s: invalid value", EnvShockGracePeriod)
		}
		c.TerminationGracePeriodSeconds = n
	}
	if v := getenv(EnvShockHookTimeout); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("%s: invalid value", EnvShockHookTimeout)
		}
		c.HookTimeoutSeconds = n
	}
	if v := getenv(EnvShockMaxActive); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("%s: invalid value", EnvShockMaxActive)
		}
		c.MaxActiveSessions = n
	}
	return c, nil
}

// RequestFromEnv parses the spawn request and reads the work-order file,
// which the orchestrator deletes when the hook exits.
func RequestFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (Request, error) {
	r := Request{
		OrderID:     getenv(EnvOrderID),
		SessionID:   getenv(EnvSessionID),
		SessionUUID: getenv(EnvSessionUUID),
		AccountID:   getenv(EnvAccountID),
	}
	if r.OrderID == "" {
		return r, errors.New(EnvOrderID + " is required")
	}
	if !orderIDPattern.MatchString(r.OrderID) {
		return r, errors.New(EnvOrderID + " is not usable in Kubernetes resource names or annotations")
	}
	attempt := getenv(EnvAttempt)
	if attempt == "" {
		return r, errors.New(EnvAttempt + " is required")
	}
	n, err := strconv.ParseInt(attempt, 10, 64)
	if err != nil || n < 0 {
		return r, errors.New(EnvAttempt + " must be a non-negative integer")
	}
	r.Attempt = n
	if strings.ContainsAny(r.SessionID, " \t\n") {
		return r, errors.New(EnvSessionID + " contains whitespace")
	}
	path := getenv(EnvWorkOrderFile)
	if path == "" {
		return r, errors.New(EnvWorkOrderFile + " is required")
	}
	data, err := readFile(path)
	if err != nil {
		return r, fmt.Errorf("reading work order file: %w", err)
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		return r, errors.New("work order file is empty")
	}
	r.WorkOrder = data
	return r, nil
}

// OSReadFile is the production file reader.
func OSReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
