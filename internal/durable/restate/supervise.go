package restate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	restateServerBinary = "restate-server"

	defaultReadyTimeout  = 60 * time.Second
	defaultShutdownGrace = 10 * time.Second
	defaultStableReset   = 5 * time.Minute

	SupervisionStarting = "starting"
	SupervisionRunning  = "running"
	SupervisionBackoff  = "backoff"
	SupervisionStopped  = "stopped"
)

type SuperviseConfig struct {
	BinaryPath string
	DataDir    string
	IngressURL string
	AdminURL   string
	HandlerURL string

	ReadyTimeout   time.Duration
	ShutdownGrace  time.Duration
	RestartInitial time.Duration
	RestartMax     time.Duration
	StableReset    time.Duration
	Logger         *slog.Logger
}

type Snapshot struct {
	State      string
	PID        int
	Restarts   int
	Registered bool
	LastError  string
}

type Supervisor struct {
	config *SuperviseConfig
	logger *slog.Logger

	started    atomic.Bool
	mu         sync.Mutex
	state      string
	pid        int
	restarts   int
	registered bool
	lastError  string
}

func NewSupervisor(cfg *SuperviseConfig) (*Supervisor, error) {
	if cfg == nil {
		return nil, &Error{Code: ErrorCodeUnreachable, Detail: "supervisor requires a configuration"}
	}
	if cfg.IngressURL == "" {
		return nil, &Error{Code: ErrorCodeUnreachable, Detail: "supervisor requires an ingress URL"}
	}
	if cfg.AdminURL == "" {
		return nil, &Error{Code: ErrorCodeUnreachable, Detail: "supervisor requires an admin URL"}
	}
	if cfg.HandlerURL == "" {
		return nil, &Error{Code: ErrorCodeRegistrationFailed, Detail: "supervisor requires a handler URL to register"}
	}
	if cfg.ReadyTimeout < 0 || cfg.ShutdownGrace < 0 || cfg.RestartInitial < 0 || cfg.RestartMax < 0 || cfg.StableReset < 0 {
		return nil, &Error{Code: ErrorCodeUnreachable, Detail: "supervisor timeouts must not be negative"}
	}
	if cfg.RestartMax != 0 && cfg.RestartInitial != 0 && cfg.RestartMax < cfg.RestartInitial {
		return nil, &Error{Code: ErrorCodeUnreachable, Detail: "supervisor maximum backoff must cover the initial backoff"}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{config: cfg, logger: logger, state: SupervisionStopped}, nil
}

func (s *Supervisor) Name() string { return "durable-supervisor" }

func (s *Supervisor) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{State: s.state, PID: s.pid, Restarts: s.restarts, Registered: s.registered, LastError: s.lastError}
}

func (s *Supervisor) setState(state string, pid int, registered bool, lastError string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	s.pid = pid
	s.registered = registered
	if lastError != "" {
		s.lastError = lastError
	}
}

func (s *Supervisor) Start(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return &Error{Code: ErrorCodeUnreachable, Detail: "supervisor is already running"}
	}
	binary, err := resolveServerBinary(s.config.BinaryPath)
	if err != nil {
		return err
	}
	dataDir, err := resolveServerDataDir(s.config.DataDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("prepare durable data directory: %w", err)
	}
	initial, ceiling := s.backoffBounds()
	stable := s.config.StableReset
	if stable == 0 {
		stable = defaultStableReset
	}
	restarts := 0
	for {
		select {
		case <-ctx.Done():
			s.setState(SupervisionStopped, 0, false, "")
			return nil
		default:
		}
		generationStart := time.Now()
		runErr := s.runGeneration(ctx, binary, dataDir)
		if runErr == nil {
			s.setState(SupervisionStopped, 0, false, "")
			return nil
		}
		if time.Since(generationStart) >= stable {
			restarts = 0
		}
		restarts++
		s.mu.Lock()
		s.restarts = restarts
		s.mu.Unlock()
		s.setState(SupervisionBackoff, 0, false, runErr.Error())
		s.logger.WarnContext(ctx, "durable runtime exited, restarting with backoff", "restarts", restarts, "error", runErr.Error())
		delay := cappedBackoff(initial, ceiling, restarts)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			s.setState(SupervisionStopped, 0, false, "")
			return nil
		}
	}
}

func (s *Supervisor) backoffBounds() (initial, ceiling time.Duration) {
	initial, ceiling = s.config.RestartInitial, s.config.RestartMax
	if initial == 0 {
		initial = time.Second
	}
	if ceiling == 0 {
		ceiling = 30 * time.Second
	}
	return initial, ceiling
}

func cappedBackoff(initial, ceiling time.Duration, restarts int) time.Duration {
	delay := initial
	for i := 1; i < restarts && delay < ceiling; i++ {
		delay *= 2
		if delay >= ceiling || delay <= 0 {
			return ceiling
		}
	}
	if delay > ceiling {
		return ceiling
	}
	return delay
}

func (s *Supervisor) runGeneration(ctx context.Context, binary, dataDir string) error {
	s.setState(SupervisionStarting, 0, false, "")
	childConfig, err := writeChildConfig(dataDir, s.config.IngressURL, s.config.AdminURL)
	if err != nil {
		return err
	}
	childCtx, stopChild := context.WithCancel(context.WithoutCancel(ctx))
	defer stopChild()
	cmd := exec.CommandContext(childCtx, binary, "--base-dir", dataDir, "-c", childConfig)
	cmd.Stdout = &logLineWriter{logger: s.logger}
	cmd.Stderr = &logLineWriter{logger: s.logger}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start durable runtime: %w", err)
	}
	s.setState(SupervisionStarting, cmd.Process.Pid, false, "")
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	procExited, readyErr := s.waitReady(ctx, exited)
	if readyErr != nil {
		if !procExited {
			_ = cmd.Process.Kill()
			<-exited
		}
		return readyErr
	}
	if regExited, regErr := s.registerWithRetry(ctx, exited); regErr != nil {
		if !regExited {
			_ = cmd.Process.Kill()
			<-exited
		}
		return regErr
	}
	s.setState(SupervisionRunning, cmd.Process.Pid, true, "")
	s.logger.InfoContext(ctx, "durable runtime is running", "pid", cmd.Process.Pid)

	select {
	case <-ctx.Done():
		s.drainChild(cmd, stopChild, exited)
		return nil
	case err := <-exited:
		if err != nil {
			return fmt.Errorf("durable runtime exited: %w", err)
		}
		return errors.New("durable runtime exited unexpectedly")
	}
}

func (s *Supervisor) waitReady(ctx context.Context, exited <-chan error) (procExited bool, err error) {
	timeout := s.config.ReadyTimeout
	if timeout == 0 {
		timeout = defaultReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		if s.probeIngress(ctx) {
			return false, nil
		}
		select {
		case err := <-exited:
			if err != nil {
				return true, fmt.Errorf("durable runtime exited during startup: %w", err)
			}
			return true, errors.New("durable runtime exited during startup")
		case <-ctx.Done():
			return false, nil
		default:
		}
		if time.Now().After(deadline) {
			return false, &Error{Code: ErrorCodeUnreachable, Detail: "durable runtime did not become ready in time"}
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return false, nil
		}
	}
}

func (s *Supervisor) probeIngress(ctx context.Context) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.config.IngressURL+"/restate/health", http.NoBody)
	if err != nil {
		return false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode == http.StatusOK
}

func (s *Supervisor) registerWithRetry(ctx context.Context, exited <-chan error) (procExited bool, err error) {
	timeout := s.config.ReadyTimeout
	if timeout == 0 {
		timeout = defaultReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return false, nil
		default:
		}
		if err := RegisterDeployment(ctx, s.config.AdminURL, s.config.HandlerURL); err == nil {
			return false, nil
		}
		select {
		case <-exited:
			return true, errors.New("durable runtime exited before deployment registration")
		default:
		}
		if time.Now().After(deadline) {
			return false, &Error{Code: ErrorCodeRegistrationFailed, Detail: "durable handler deployment was not accepted in time"}
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return false, nil
		}
	}
}

func (s *Supervisor) drainChild(cmd *exec.Cmd, stopChild context.CancelFunc, exited <-chan error) {
	grace := s.config.ShutdownGrace
	if grace == 0 {
		grace = defaultShutdownGrace
	}
	_ = cmd.Process.Signal(terminatingSignal())
	select {
	case <-exited:
	case <-time.After(grace):
		stopChild()
		<-exited
	}
}

func resolveServerBinary(configured string) (string, error) {
	if configured == "" {
		path, err := exec.LookPath(restateServerBinary)
		if err != nil {
			return "", &Error{Code: ErrorCodeBinaryMissing, Detail: "restate-server was not found on PATH; install it or set the binary path"}
		}
		return path, nil
	}
	if !filepath.IsAbs(configured) && filepath.Dir(configured) == "." {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", &Error{Code: ErrorCodeBinaryMissing, Detail: "configured durable runtime binary was not found on PATH"}
		}
		return path, nil
	}
	info, err := os.Stat(configured)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", &Error{Code: ErrorCodeBinaryMissing, Detail: "configured durable runtime binary is not executable"}
	}
	return configured, nil
}

func writeChildConfig(dataDir, ingressURL, adminURL string) (string, error) {
	ingressAddr, err := serverBindAddress(ingressURL)
	if err != nil {
		return "", err
	}
	adminAddr, err := serverBindAddress(adminURL)
	if err != nil {
		return "", err
	}
	content := "[ingress]\nbind-address = \"" + ingressAddr + "\"\n\n[admin]\nbind-address = \"" + adminAddr + "\"\n"
	path := filepath.Join(dataDir, "restate.toml")
	tmp, err := os.CreateTemp(dataDir, "restate-*.toml")
	if err != nil {
		return "", fmt.Errorf("write durable runtime config: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("write durable runtime config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("write durable runtime config: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("write durable runtime config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("write durable runtime config: %w", err)
	}
	return path, nil
}

func serverBindAddress(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", &Error{Code: ErrorCodeUnreachable, Detail: "durable endpoint URL is not parseable"}
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if host == "" || port == "" {
		return "", &Error{Code: ErrorCodeUnreachable, Detail: "durable endpoint URL must carry host and port"}
	}
	return net.JoinHostPort(host, port), nil
}

func resolveServerDataDir(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "aura", "restate"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", &Error{Code: ErrorCodeUnreachable, Detail: "durable data directory is not configured and neither XDG_STATE_HOME nor HOME resolves"}
	}
	return filepath.Join(home, ".local", "state", "aura", "restate"), nil
}

func RegisterDeployment(ctx context.Context, adminURL, handlerURL string) error {
	for _, force := range []bool{false, true} {
		body, err := json.Marshal(map[string]any{"uri": handlerURL, "force": force})
		if err != nil {
			return fmt.Errorf("encode deployment request: %w", err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, adminURL+"/deployments", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build deployment request: %w", err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return &Error{Code: ErrorCodeRegistrationFailed, Detail: "durable admin endpoint is unreachable"}
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		if response.StatusCode < 300 {
			return nil
		}
		if response.StatusCode != http.StatusConflict || force {
			return &Error{Code: ErrorCodeRegistrationFailed, Detail: fmt.Sprintf("durable handler deployment was rejected (%s)", response.Status)}
		}
	}
	return nil
}

func CheckExternal(ctx context.Context, ingressURL, adminURL string) error {
	if ingressURL == "" || adminURL == "" {
		return &Error{Code: ErrorCodeUnreachable, Detail: "external durable runtime requires endpoint and admin endpoint"}
	}
	if !endpointHealthy(ctx, ingressURL+"/restate/health") {
		return &Error{Code: ErrorCodeUnreachable, Detail: "durable ingress endpoint is unreachable"}
	}
	if !endpointHealthy(ctx, adminURL+"/version") {
		return &Error{Code: ErrorCodeUnreachable, Detail: "durable admin endpoint is unreachable"}
	}
	return nil
}

func endpointHealthy(ctx context.Context, rawURL string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode == http.StatusOK
}

type logLineWriter struct {
	logger *slog.Logger
}

func (w *logLineWriter) Write(data []byte) (int, error) {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		w.logger.DebugContext(context.Background(), "durable runtime output", "line", string(line))
	}
	return len(data), nil
}
