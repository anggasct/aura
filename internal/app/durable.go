package app

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
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/child"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/restate"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
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
		switch probeDeploymentRegistration(ctx, durableCfg.AdminEndpoint, deploymentMatch(durableCfg)) {
		case deploymentRegistered:
			registration = "registered"
		case deploymentAbsent:
			registration = "absent"
		case deploymentUnknown:
			registration = "unknown"
		}
	}
	lines := []string{
		"mode: " + durableCfg.Mode,
		"endpoint: " + durableCfg.Endpoint,
		"admin_endpoint: " + durableCfg.AdminEndpoint,
		"handler_addr: " + durableCfg.HandlerAddr,
	}
	if durableCfg.HandlerURL != "" {
		lines = append(lines, "handler_url: "+durableCfg.HandlerURL)
	}
	lines = append(lines,
		"node_addr: "+durableCfg.NodeAddr,
		"ingress: "+reachableWord(ingressOK),
		"admin: "+reachableWord(adminOK),
		"deployment: "+registration,
	)
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	if !ingressOK || !adminOK {
		return &restate.Error{Code: restate.ErrorCodeUnreachable, Detail: "durable runtime is unreachable"}
	}
	return nil
}

func deploymentMatch(durableCfg *config.Durable) string {
	if durableCfg.HandlerURL != "" {
		return durableCfg.HandlerURL
	}
	return durableCfg.HandlerAddr
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

func durableRuntimeForConfig(cfg *config.Config, db *sql.DB, logger *slog.Logger) (durable.Runtime, error) {
	if cfg != nil && cfg.Durable != nil && cfg.Durable.Enabled {
		return restate.NewAdapter(restate.Config{IngressURL: cfg.Durable.Endpoint}, logger)
	}
	if cfg == nil || db == nil {
		return nil, invalidOptions("the durable runtime needs a config and open storage")
	}
	runtime, err := sqlite.New(durableHostOptions(db, logger, cfg.Runtime))
	if err != nil {
		return nil, err
	}
	declareRunHandlers(runtime)
	return runtime, nil
}

type runHandlerDeclarer interface {
	DeclareHandlers(names ...string)
}

func declareRunHandlers(runtime runHandlerDeclarer) {
	runtime.DeclareHandlers(broadcast.HandlerName, schedule.HandlerName, child.HandlerName, workflow.HandlerName)
}

type durableListenerInput struct {
	result      *config.LoadResult
	db          *sql.DB
	logger      *slog.Logger
	engine      *runtimeengine.Engine
	interpreter *workflow.Interpreter
}

func buildDurableListener(in *durableListenerInput) (*durableListener, error) {
	if in == nil || in.result == nil || in.result.Config == nil || in.result.Config.Durable == nil || in.db == nil || in.interpreter == nil {
		return nil, invalidOptions("durable listener input must carry a config with durable settings, storage and an interpreter")
	}
	durableCfg, db, logger, engine, interpreter := in.result.Config.Durable, in.db, in.logger, in.engine, in.interpreter
	endpoint, err := restate.NewEndpoint(restate.EndpointConfig{HandlerAddr: durableCfg.HandlerAddr}, logger)
	if err != nil {
		return nil, err
	}
	endpoint.RegisterSessionTurns(restate.SessionStores{
		Events: store.NewEventStore(db),
		Dedupe: store.NewDedupeStore(db),
	})
	endpoint.RegisterHandler(runtimeengine.HandlerName, func(ctx context.Context, inv durable.Invocation) error {
		if engine == nil {
			return errors.New("durable turn drive requires an engine")
		}
		var desc runtimesessions.Descriptor
		if err := json.Unmarshal(inv.Payload(), &desc); err != nil {
			return fmt.Errorf("decode durable turn payload: %w", err)
		}
		return engine.DriveTurn(ctx, inv, &desc)
	})
	endpoint.RegisterHandler(workflow.HandlerName, interpreter.HandleSerial)
	return &durableListener{config: durableCfg, endpoint: endpoint, logger: logger, interpreter: interpreter}, nil
}

type durableListener struct {
	config      *config.Durable
	endpoint    *restate.Endpoint
	logger      *slog.Logger
	interpreter *workflow.Interpreter
}

func (l *durableListener) Name() string { return "durable" }

func (l *durableListener) RegisterHandler(name string, fn durable.Handler) {
	if l == nil || l.endpoint == nil || fn == nil {
		return
	}
	l.endpoint.RegisterHandler(name, fn)
}

func (l *durableListener) Start(ctx context.Context) error {
	if l.config.Mode == config.DurableModeExternal {
		gateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := restate.CheckExternal(gateCtx, l.config.Endpoint, l.config.AdminEndpoint); err != nil {
			return err
		}
		if l.config.HandlerURL == "" {
			return l.serveEndpointUntilDone(ctx)
		}
		return l.serveAndRegister(ctx)
	}
	supervisor, err := restate.NewSupervisor(&restate.SuperviseConfig{
		BinaryPath:     l.config.BinaryPath,
		DataDir:        l.config.DataDir,
		IngressURL:     l.config.Endpoint,
		AdminURL:       l.config.AdminEndpoint,
		HandlerURL:     "http://" + l.config.HandlerAddr,
		NodeAddr:       l.config.NodeAddr,
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

const externalRegistrationTimeout = time.Minute

func (l *durableListener) serveAndRegister(ctx context.Context) error {
	serveCtx, stop := context.WithCancel(ctx)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- l.serveEndpointUntilDone(serveCtx) }()
	if err := restate.AwaitRegistration(ctx, l.config.AdminEndpoint, l.config.HandlerURL, externalRegistrationTimeout); err != nil {
		stop()
		serveErr := <-served
		if ctx.Err() != nil {
			return serveErr
		}
		return err
	}
	l.logger.InfoContext(ctx, "durable handler deployment registered", "handler_url", l.config.HandlerURL)
	return <-served
}

func (l *durableListener) serveEndpointUntilDone(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- l.endpoint.Start(ctx) }()
	select {
	case err := <-done:
		if ctx.Err() == nil {
			return err
		}
	case <-ctx.Done():
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			return &restate.Error{Code: restate.ErrorCodeUnreachable, Detail: "durable handler endpoint did not stop"}
		}
	}
	return nil
}

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

func (s *durableSessionStore) Status(ctx context.Context, sessionID string) (runtimesessions.StatusResult, error) {
	return callSessionObject[runtimesessions.StatusResult](ctx, s.runtime, sessionID, restate.SessionStatusHandler, json.RawMessage(`{}`))
}

func (s *durableSessionStore) Abort(ctx context.Context, sessionID string) (runtimesessions.AbortResult, error) {
	return callSessionObject[runtimesessions.AbortResult](ctx, s.runtime, sessionID, restate.SessionAbortHandler, json.RawMessage(`{}`))
}

func (s *durableSessionStore) ActiveTurns(context.Context) ([]runtimesessions.QueuedTurn, error) {
	return []runtimesessions.QueuedTurn{}, nil
}
