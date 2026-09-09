package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/durable/restate"
	"github.com/anggasct/aura/internal/runtime"
	runtimeengine "github.com/anggasct/aura/internal/runtime/engine"
	runtimesessions "github.com/anggasct/aura/internal/runtime/sessions"
	"github.com/anggasct/aura/internal/server"
	"github.com/anggasct/aura/internal/store"
)

const durableProbeTimeout = 5 * time.Second

func newDurableCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "durable",
		Short: "Inspect the durable workflow runtime",
	}
	cmd.AddCommand(newDurableStatusCmd(gf))
	return cmd
}

func newDurableStatusCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report durable runtime mode and reachability",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			return runDurableStatus(cmd, result.Config)
		},
	}
}

func runDurableStatus(cmd *cobra.Command, cfg *config.Config) error {
	out := cmd.OutOrStdout()
	durableCfg := cfg.Durable
	if durableCfg == nil || !durableCfg.Enabled {
		_, err := fmt.Fprintln(out, "durable: disabled")
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 3*durableProbeTimeout)
	defer cancel()
	ingressOK := restateEndpointHealthy(ctx, durableCfg.Endpoint+"/restate/health")
	adminOK := restateEndpointHealthy(ctx, durableCfg.AdminEndpoint+"/version")
	registration := "unknown"
	if adminOK {
		switch probeDeploymentRegistration(ctx, durableCfg.AdminEndpoint, durableCfg.HandlerAddr) {
		case deploymentRegistered:
			registration = "registered"
		case deploymentAbsent:
			registration = "absent"
		case deploymentUnknown:
			registration = "unknown"
		}
	}
	for _, line := range []string{
		"mode: " + durableCfg.Mode,
		"endpoint: " + durableCfg.Endpoint,
		"admin_endpoint: " + durableCfg.AdminEndpoint,
		"handler_addr: " + durableCfg.HandlerAddr,
		"ingress: " + reachableWord(ingressOK),
		"admin: " + reachableWord(adminOK),
		"deployment: " + registration,
	} {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	if !ingressOK || !adminOK {
		return &restate.Error{Code: restate.ErrorCodeUnreachable, Detail: "durable runtime is unreachable"}
	}
	return nil
}

func reachableWord(ok bool) string {
	if ok {
		return "reachable"
	}
	return "unreachable"
}

func restateEndpointHealthy(ctx context.Context, url string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
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

type deploymentPresence int

const (
	deploymentUnknown deploymentPresence = iota
	deploymentRegistered
	deploymentAbsent
)

func probeDeploymentRegistration(ctx context.Context, adminURL, handlerAddr string) deploymentPresence {
	probeCtx, cancel := context.WithTimeout(ctx, durableProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, adminURL+"/deployments", http.NoBody)
	if err != nil {
		return deploymentUnknown
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return deploymentUnknown
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return deploymentUnknown
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return deploymentUnknown
	}
	var listed struct {
		Deployments []struct {
			URI string `json:"uri"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &listed); err != nil {
		return deploymentUnknown
	}
	if len(listed.Deployments) == 0 {
		return deploymentAbsent
	}
	for _, deployment := range listed.Deployments {
		if deployment.URI != "" && strings.Contains(deployment.URI, handlerAddr) {
			return deploymentRegistered
		}
	}
	return deploymentAbsent
}

func durableRuntimeForConfig(cfg *config.Config, logger *slog.Logger) (durable.Runtime, error) {
	if cfg != nil && cfg.Durable != nil && cfg.Durable.Enabled {
		return restate.NewAdapter(restate.Config{IngressURL: cfg.Durable.Endpoint}, logger)
	}
	return durable.NewFake(), nil
}

func buildDurableListener(ctx context.Context, cfg *config.Config, db *sql.DB, logger *slog.Logger, engine *runtimeengine.Engine) (server.Listener, error) {
	durableCfg := cfg.Durable
	interpreter, err := buildWorkflowInterpreterWithDB(ctx, cfg, db, logger)
	if err != nil {
		return nil, err
	}
	endpoint, err := restate.NewEndpoint(restate.EndpointConfig{HandlerAddr: durableCfg.HandlerAddr}, logger)
	if err != nil {
		return nil, err
	}
	endpoint.RegisterSessionTurns(restate.SessionStores{
		Events: store.NewEventStore(db),
		Dedupe: store.NewDedupeStore(db),
	})
	endpoint.RegisterHandler("turn", func(ctx context.Context, inv durable.Invocation) error {
		if engine == nil {
			return errors.New("durable turn drive requires an engine")
		}
		var desc runtimesessions.Descriptor
		if err := json.Unmarshal(inv.Payload(), &desc); err != nil {
			return fmt.Errorf("decode durable turn payload: %w", err)
		}
		return engine.DriveTurn(ctx, inv, &desc)
	})
	endpoint.RegisterHandler("workflow", func(ctx context.Context, inv durable.Invocation) error {
		var tick struct {
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal(inv.Payload(), &tick); err != nil || tick.RunID == "" {
			return fmt.Errorf("decode durable run payload: %w", err)
		}
		_, err := interpreter.DriveSerial(ctx, inv, tick.RunID)
		return err
	})
	return &durableListener{config: durableCfg, endpoint: endpoint, logger: logger, engine: engine, events: store.NewEventStore(db)}, nil
}

type durableListener struct {
	config   *config.Durable
	endpoint *restate.Endpoint
	logger   *slog.Logger
	engine   *runtimeengine.Engine
	events   store.EventStore
}

func (l *durableListener) Name() string { return "durable" }

func (l *durableListener) Start(ctx context.Context) error {
	if l.config.Mode == config.DurableModeExternal {
		gateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := restate.CheckExternal(gateCtx, l.config.Endpoint, l.config.AdminEndpoint); err != nil {
			return err
		}
		if l.engine != nil {
			go l.recoverExternal(ctx)
		}
		return l.serveEndpointUntilDone(ctx)
	}
	supervisor, err := restate.NewSupervisor(&restate.SuperviseConfig{
		BinaryPath:     l.config.BinaryPath,
		DataDir:        l.config.DataDir,
		IngressURL:     l.config.Endpoint,
		AdminURL:       l.config.AdminEndpoint,
		HandlerURL:     "http://" + l.config.HandlerAddr,
		RestartInitial: time.Duration(l.config.RestartBackoffInitial),
		RestartMax:     time.Duration(l.config.RestartBackoffMax),
		Logger:         l.logger,
	})
	if err != nil {
		return err
	}
	endpointCtx, stopEndpoint := context.WithCancel(ctx)
	endpointDone := make(chan error, 1)
	go func() { endpointDone <- l.endpoint.Start(endpointCtx) }()
	if l.engine != nil {
		go l.recoverSupervised(ctx, supervisor)
	}
	supervisorErr := supervisor.Start(ctx)
	stopEndpoint()
	select {
	case err := <-endpointDone:
		if supervisorErr == nil {
			supervisorErr = err
		}
	case <-time.After(15 * time.Second):
		if supervisorErr == nil {
			supervisorErr = &restate.Error{Code: restate.ErrorCodeUnreachable, Detail: "durable handler endpoint did not stop"}
		}
	}
	return supervisorErr
}

func (l *durableListener) serveEndpointUntilDone(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- l.endpoint.Start(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := l.endpoint.Stop(shutdownCtx); err != nil {
			return err
		}
		select {
		case err := <-done:
			if err != nil && ctx.Err() != nil {
				return nil
			}
			return err
		case <-time.After(15 * time.Second):
			return &restate.Error{Code: restate.ErrorCodeUnreachable, Detail: "durable handler endpoint did not stop"}
		}
	}
}

const sessionRecoveryWindow = 24 * time.Hour

type durableSessionStore struct {
	runtime durable.Runtime
}

func callSessionObject[Result any](ctx context.Context, rt durable.Runtime, sessionID, handler string, payload any) (Result, error) {
	var zero Result
	raw, err := json.Marshal(payload)
	if err != nil {
		return zero, fmt.Errorf("encode session %s request: %w", handler, err)
	}
	out, err := rt.Call(ctx, durable.CallRequest{
		Service: restate.SessionServiceName,
		Key:     sessionID,
		Handler: handler,
		Payload: raw,
	})
	if err != nil {
		return zero, err
	}
	var result Result
	if err := json.Unmarshal(out, &result); err != nil {
		return zero, fmt.Errorf("decode session %s response: %w", handler, err)
	}
	return result, nil
}

func (s *durableSessionStore) Admit(ctx context.Context, req *runtimesessions.AdmitRequest) (runtimesessions.AdmitResult, error) {
	return callSessionObject[runtimesessions.AdmitResult](ctx, s.runtime, req.Turn.SessionID, restate.SessionAdmitHandler, req)
}

func (s *durableSessionStore) Release(ctx context.Context, sessionID, turnID string) (runtimesessions.ReleaseResult, error) {
	return callSessionObject[runtimesessions.ReleaseResult](ctx, s.runtime, sessionID, restate.SessionReleaseHandler, runtimesessions.ReleaseRequest{TurnID: turnID})
}

func (s *durableSessionStore) Recover(ctx context.Context, sessionID string, open, terminal []string) (runtimesessions.RecoverResult, error) {
	return callSessionObject[runtimesessions.RecoverResult](ctx, s.runtime, sessionID, restate.SessionRecoverHandler, runtimesessions.RecoverRequest{Open: open, Terminal: terminal})
}

func (s *durableSessionStore) Abort(ctx context.Context, sessionID string) (runtimesessions.AbortResult, error) {
	return callSessionObject[runtimesessions.AbortResult](ctx, s.runtime, sessionID, restate.SessionAbortHandler, json.RawMessage(`{}`))
}

func recoverDurableSessions(ctx context.Context, engine *runtimeengine.Engine, events store.EventStore, logger *slog.Logger) error {
	activity, err := events.ListTurnActivity(ctx, time.Now().Add(-sessionRecoveryWindow))
	if err != nil {
		return fmt.Errorf("scan recent turn activity: %w", err)
	}
	restored := 0
	for sessionID, turns := range activity {
		var open, terminal []string
		for _, turn := range turns {
			if hasTerminalKind(turn.Kinds) {
				terminal = append(terminal, turn.TurnID)
			} else {
				open = append(open, turn.TurnID)
			}
		}
		before := len(open)
		if err := engine.RecoverSession(ctx, sessionID, open, terminal); err != nil {
			return fmt.Errorf("recover session: %w", err)
		}
		restored += before
	}
	logger.InfoContext(ctx, "durable session recovery complete",
		"component", "runtime",
		"sessions", len(activity),
		"open_turns", restored,
	)
	return nil
}

func hasTerminalKind(kinds []string) bool {
	for _, kind := range kinds {
		switch kind {
		case runtime.EventKindTurnCompleted, runtime.EventKindTurnFailed, runtime.EventKindTurnCancelled:
			return true
		}
	}
	return false
}

const sessionRecoveryTimeout = 3 * time.Minute
const sessionRecoveryPoll = 500 * time.Millisecond

func (l *durableListener) recoverSupervised(ctx context.Context, supervisor *restate.Supervisor) {
	err := l.waitSupervisorReady(ctx, supervisor)
	if err == nil {
		err = l.recoverSessions(ctx)
	}
	l.finishSessionRecovery(ctx, err)
}

func (l *durableListener) waitSupervisorReady(ctx context.Context, supervisor *restate.Supervisor) error {
	deadline := time.Now().Add(sessionRecoveryTimeout)
	for {
		snapshot := supervisor.Snapshot()
		if snapshot.State == restate.SupervisionRunning && snapshot.Registered {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("durable runtime did not become ready: %s", snapshot.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sessionRecoveryPoll):
		}
	}
}

func (l *durableListener) recoverExternal(ctx context.Context) {
	deadline := time.Now().Add(sessionRecoveryTimeout)
	for {
		err := l.recoverSessions(ctx)
		if err == nil {
			l.finishSessionRecovery(ctx, nil)
			return
		}
		if time.Now().After(deadline) {
			l.finishSessionRecovery(ctx, err)
			return
		}
		select {
		case <-ctx.Done():
			l.finishSessionRecovery(ctx, ctx.Err())
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (l *durableListener) finishSessionRecovery(ctx context.Context, err error) {
	if err != nil {
		l.logger.ErrorContext(ctx, "durable session recovery failed; queued turns resume on the next boot",
			"component", "runtime", "error", err)
	}
	if l.engine != nil {
		l.engine.MarkRecovered()
	}
}

func (l *durableListener) recoverSessions(ctx context.Context) error {
	if l.engine == nil || l.events == nil {
		return nil
	}
	return recoverDurableSessions(ctx, l.engine, l.events, l.logger)
}
