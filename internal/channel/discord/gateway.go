package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/websocket"
)

func (a *Adapter) dialGateway(ctx context.Context, url string) (*websocket.Conn, error) {
	dialer := &net.Dialer{Timeout: writeTimeout}
	wsConfig, err := websocket.NewConfig(url, "https://discord.com")
	if err != nil {
		return nil, err
	}
	wsConfig.Dialer = dialer
	return wsConfig.DialContext(ctx)
}

func (a *Adapter) connectLoop(ctx context.Context) error {
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		token, err := a.tokenRef.Resolve()
		if err != nil {
			a.logger.WarnContext(ctx, "bot token unavailable", "component", "discord")
			if !sleepContext(ctx, a.retryDelay(attempt)) {
				return nil
			}
			attempt++
			continue
		}
		cursor, resume, err := a.loadCursor(ctx)
		if err != nil {
			a.logger.WarnContext(ctx, "resume cursor unreadable", "component", "discord")
		}
		ws, err := a.dial(ctx, a.activeURL())
		if err != nil {
			a.logger.WarnContext(ctx, "gateway dial failed", "component", "discord")
			if !sleepContext(ctx, a.retryDelay(attempt)) {
				return nil
			}
			attempt++
			continue
		}
		started := time.Now()
		sessionErr := a.runSession(ctx, ws, token, cursor, resume)
		_ = ws.Close()
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if sessionErr != nil {
			a.logger.WarnContext(ctx, "gateway session ended", "component", "discord")
		}
		if time.Since(started) > stableSession {
			attempt = 0
		}
		if !sleepContext(ctx, a.retryDelay(attempt)) {
			return nil
		}
		attempt++
	}
}

func (a *Adapter) retryDelay(attempt int) time.Duration {
	shift := attempt
	if shift > 6 {
		shift = 6
	}
	limit := a.retryBase << shift
	if limit > a.retryCap || limit <= 0 {
		limit = a.retryCap
	}
	half := int64(limit / 2)
	return time.Duration(half + rand.Int64N(half+1))
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (a *Adapter) activeURL() string {
	if url, ok := a.resumeURL.Load().(string); ok && url != "" {
		return url
	}
	return a.gatewayURL
}

func (a *Adapter) loadCursor(ctx context.Context) (ResumeCursor, bool, error) {
	if a.resumes == nil {
		return ResumeCursor{}, false, nil
	}
	cursor, found, err := a.resumes.Load(ctx)
	if err != nil || !found {
		return ResumeCursor{}, false, err
	}
	if cursor.ConfigDigest != "" && cursor.ConfigDigest != a.digest {
		if err := a.resumes.Delete(ctx); err != nil {
			return ResumeCursor{}, false, err
		}
		return ResumeCursor{}, false, nil
	}
	if cursor.SessionID == "" {
		return ResumeCursor{}, false, nil
	}
	return cursor, true, nil
}

func (a *Adapter) runSession(ctx context.Context, ws *websocket.Conn, token string, cursor ResumeCursor, resume bool) error {
	ws.MaxPayloadBytes = maxFrameBytes
	if err := ws.SetReadDeadline(time.Now().Add(helloTimeout)); err != nil {
		return err
	}
	envelope, err := receiveEnvelope(ws)
	if err != nil {
		return err
	}
	if envelope.Op != opHello {
		return Errorf(ErrorCodeProtocolInvalid, "first gateway frame is not hello")
	}
	var hello helloPayload
	if err := json.Unmarshal(envelope.Data, &hello); err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "hello payload is not decodable")
	}
	if hello.HeartbeatIntervalMs <= 0 {
		return Errorf(ErrorCodeProtocolInvalid, "hello carries no heartbeat interval")
	}
	interval := time.Duration(hello.HeartbeatIntervalMs) * time.Millisecond
	if !resume {
		a.sessionID = ""
		a.sequence.Store(0)
		a.hasSeq.Store(false)
		if err := sendPayload(ws, opIdentify, identifyPayload{
			Token:   token,
			Intents: requiredIntents,
			Properties: identifyProperties{
				OS:      "linux",
				Browser: "aura",
				Device:  "aura",
			},
		}); err != nil {
			return err
		}
	} else {
		a.sessionID = cursor.SessionID
		a.sequence.Store(cursor.Sequence)
		a.hasSeq.Store(true)
		if a.selfID == "" {
			if selfID, err := a.fetchSelfID(ctx, token); err != nil {
				a.logger.WarnContext(ctx, "self lookup failed", "component", "discord")
			} else {
				a.selfID = selfID
			}
		}
		if err := sendPayload(ws, opResume, resumePayload{
			Token:     token,
			SessionID: cursor.SessionID,
			Seq:       cursor.Sequence,
		}); err != nil {
			return err
		}
	}
	if !a.gap.Load() {
		a.state.Store(int32(stateLive))
	}
	a.logger.InfoContext(ctx, "gateway session running", "component", "discord", "resumed", resume)
	return a.pump(ctx, ws, interval)
}

func (a *Adapter) pump(ctx context.Context, ws *websocket.Conn, interval time.Duration) error {
	session, cancel := context.WithCancel(ctx)
	defer cancel()
	beat := make(chan struct{}, 1)
	acked := make(chan struct{}, 16)
	done := make(chan struct{})
	go a.heartbeatLoop(session, ws, interval, beat, acked, done)
	defer func() {
		cancel()
		<-done
	}()
	for {
		if err := ws.SetReadDeadline(time.Now().Add(readMultiplier * interval)); err != nil {
			return err
		}
		envelope, err := receiveEnvelope(ws)
		if err != nil {
			return err
		}
		if envelope.Seq != nil {
			a.sequence.Store(*envelope.Seq)
			a.hasSeq.Store(true)
		}
		switch envelope.Op {
		case opDispatch:
			if err := a.handleDispatch(session, envelope); err != nil {
				return err
			}
		case opHeartbeat:
			select {
			case beat <- struct{}{}:
			default:
			}
		case opReconnect:
			return nil
		case opInvalidSession:
			return a.handleInvalidSession(session, envelope)
		case opHeartbeatACK:
			select {
			case acked <- struct{}{}:
			default:
			}
		default:
			continue
		}
	}
}

func (a *Adapter) heartbeatLoop(ctx context.Context, ws *websocket.Conn, interval time.Duration, beat, acked <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	awaiting := false
	send := func() bool {
		var seq *int64
		if a.hasSeq.Load() {
			current := a.sequence.Load()
			seq = &current
		}
		if err := sendHeartbeat(ws, seq); err != nil {
			return false
		}
		awaiting = true
		return true
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-beat:
			if !send() {
				return
			}
		case <-acked:
			awaiting = false
		case <-ticker.C:
			if awaiting {
				_ = ws.Close()
				return
			}
			if !send() {
				return
			}
		}
	}
}

func (a *Adapter) handleInvalidSession(ctx context.Context, envelope *gatewayEnvelope) error {
	var resumable bool
	if len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, &resumable); err != nil {
			return Errorf(ErrorCodeProtocolInvalid, "invalid session payload is not decodable")
		}
	}
	if resumable {
		return nil
	}
	a.markGap(ctx)
	if a.resumes != nil {
		if err := a.resumes.Delete(ctx); err != nil {
			a.logger.WarnContext(ctx, "resume cursor deletion failed", "component", "discord")
		}
	}
	sleepContext(ctx, a.invalidDelay)
	return nil
}

func (a *Adapter) handleDispatch(ctx context.Context, envelope *gatewayEnvelope) error {
	switch envelope.Type {
	case eventReady:
		var ready readyPayload
		if err := json.Unmarshal(envelope.Data, &ready); err != nil {
			return Errorf(ErrorCodeProtocolInvalid, "ready payload is not decodable")
		}
		if ready.SessionID == "" || ready.User.ID == "" {
			return Errorf(ErrorCodeProtocolInvalid, "ready payload is missing session or user")
		}
		a.sessionID = ready.SessionID
		a.selfID = ready.User.ID
		if ready.ResumeGatewayURL != "" {
			a.resumeURL.Store(ready.ResumeGatewayURL)
		}
		a.saveCursor(ctx, ready.SessionID, a.sequence.Load())
		return nil
	case eventMessageCreate:
		var msg messagePayload
		if err := json.Unmarshal(envelope.Data, &msg); err != nil {
			return Errorf(ErrorCodeProtocolInvalid, "message payload is not decodable")
		}
		admitted, err := a.admit(ctx, &msg)
		if err != nil {
			return err
		}
		if admitted && envelope.Seq != nil {
			a.saveCursor(ctx, a.sessionID, *envelope.Seq)
		}
		return nil
	default:
		return nil
	}
}

func (a *Adapter) admit(ctx context.Context, msg *messagePayload) (bool, error) {
	envelope, admitted, err := normalizeMessage(a.cfg, a.selfID, a.cfg.Instance, msg)
	if err != nil {
		return false, err
	}
	if !admitted {
		return false, nil
	}
	if _, err := a.sink.Accept(ctx, envelope); err != nil {
		return false, fmt.Errorf("accept ingress: %w", err)
	}
	return true, nil
}

func (a *Adapter) saveCursor(ctx context.Context, sessionID string, sequence int64) {
	if a.resumes == nil || sessionID == "" {
		return
	}
	err := a.resumes.Save(ctx, &ResumeCursor{
		SessionID:    sessionID,
		Sequence:     sequence,
		ConfigDigest: a.digest,
		UpdatedAt:    time.Now().UTC(),
	})
	if err != nil {
		a.logger.WarnContext(ctx, "resume cursor save failed", "component", "discord")
	}
}

func (a *Adapter) fetchSelfID(ctx context.Context, token string) (string, error) {
	rest, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rest, http.MethodGet, a.restBase+"/users/@me", http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+token)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", Errorf(ErrorCodeConnectionFailed, "self lookup did not succeed")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRestBytes))
	if err != nil {
		return "", Errorf(ErrorCodeProtocolInvalid, "self payload is not readable")
	}
	var user userPayload
	if err := json.Unmarshal(body, &user); err != nil {
		return "", Errorf(ErrorCodeProtocolInvalid, "self payload is not decodable")
	}
	if user.ID == "" {
		return "", Errorf(ErrorCodeProtocolInvalid, "self payload is missing the user id")
	}
	return user.ID, nil
}

func receiveEnvelope(ws *websocket.Conn) (*gatewayEnvelope, error) {
	var raw []byte
	if err := websocket.Message.Receive(ws, &raw); err != nil {
		return nil, err
	}
	if len(raw) > maxFrameBytes {
		return nil, Errorf(ErrorCodeProtocolInvalid, "gateway frame exceeds size bound")
	}
	var envelope gatewayEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, Errorf(ErrorCodeProtocolInvalid, "gateway frame is not decodable")
	}
	return &envelope, nil
}

func sendPayload(ws *websocket.Conn, op int, payload any) error {
	raw, err := json.Marshal(sendEnvelope{Op: op, Data: payload})
	if err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "gateway payload is not serializable")
	}
	if err := ws.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return websocket.Message.Send(ws, raw)
}

func sendHeartbeat(ws *websocket.Conn, seq *int64) error {
	raw, err := json.Marshal(sendEnvelope{Op: opHeartbeat, Data: seq})
	if err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "heartbeat is not serializable")
	}
	if err := ws.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return websocket.Message.Send(ws, raw)
}
