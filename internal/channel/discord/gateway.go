package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/websocket"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
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
	session, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan intakeItem, intakeQueueCap)
	dispatchDone := make(chan error, 1)
	go a.dispatchIntake(session, cancel, queue, dispatchDone)
	err = a.pump(session, ws, interval, queue)
	close(queue)
	if dispatchErr := <-dispatchDone; err == nil {
		err = dispatchErr
	}
	return err
}

type intakeItem struct {
	msg *messagePayload
	seq *int64
}

func (a *Adapter) dispatchIntake(ctx context.Context, cancel context.CancelFunc, queue <-chan intakeItem, done chan<- error) {
	defer close(done)
	for item := range queue {
		admitted, err := a.admit(ctx, item.msg)
		if err != nil {
			a.logger.WarnContext(ctx, "intake dispatch failed", "component", "discord", "error", err)
			cancel()
			done <- err
			return
		}
		if admitted && item.seq != nil {
			a.saveCursor(ctx, a.sessionID, *item.seq)
		}
	}
	done <- nil
}

func (a *Adapter) pump(ctx context.Context, ws *websocket.Conn, interval time.Duration, queue chan<- intakeItem) error {
	session, cancel := context.WithCancel(ctx)
	defer cancel()
	beat := make(chan struct{}, 1)
	acked := make(chan struct{}, 16)
	done := make(chan struct{})
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-session.Done():
			_ = ws.Close()
		case <-stopped:
		}
	}()
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
			if err := a.handleDispatch(session, envelope, queue); err != nil {
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

func (a *Adapter) handleDispatch(ctx context.Context, envelope *gatewayEnvelope, queue chan<- intakeItem) error {
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
		select {
		case queue <- intakeItem{msg: &msg, seq: envelope.Seq}:
			return nil
		case <-ctx.Done():
			return nil
		}
	case eventInteractionCreate:
		var interaction interactionPayload
		if err := json.Unmarshal(envelope.Data, &interaction); err != nil {
			return Errorf(ErrorCodeProtocolInvalid, "interaction payload is not decodable")
		}
		a.resolveComponentClick(ctx, &interaction)
		return nil
	default:
		return nil
	}
}

func (a *Adapter) admit(ctx context.Context, msg *messagePayload) (bool, error) {
	if msg == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "account and message must not be nil")
	}
	if route, ok := a.lookupThread(msg.ChannelID); ok {
		return a.admitThreadMessage(ctx, msg, msg.ChannelID, route)
	}
	envelope, admitted, err := normalizeMessage(a.account, a.owners, a.selfID, msg)
	if err != nil {
		return false, err
	}
	if a.threadClassificationNeeded(ctx, msg, admitted) {
		probe, probeErr := a.probeChannel(ctx, msg.ChannelID)
		if probeErr != nil {
			a.logger.DebugContext(ctx, "thread classification failed", "component", "discord", "error", probeErr)
			return false, nil
		}
		if probe.isThread {
			if !probe.owned {
				return false, nil
			}
			return a.admitThreadRoute(ctx, msg, threadRoute{parent: probe.parent})
		}
		if msg.ChannelType != nil && isThreadChannelType(*msg.ChannelType) {
			return false, nil
		}
	}
	if !admitted {
		return false, nil
	}
	persona, routed := a.routePersona(ctx, &auraagent.RouteScope{GuildID: msg.GuildID, ChannelID: msg.ChannelID, UserID: msg.Author.ID})
	if !routed {
		return false, nil
	}
	envelope.AgentID = persona
	if inv, ok := a.commandInvocation(msg); ok {
		if handled, err := a.runCommand(ctx, msg, &inv, envelope); handled || err != nil {
			return handled, err
		}
	}
	if msg.GuildID != "" && !skipThreadCreate(msg.ChannelType) {
		if threadID, threadErr := a.createThreadForTrigger(ctx, msg, msg.ChannelID, persona); threadErr == nil {
			if rewriteErr := rewriteEnvelopeForThread(envelope, msg, threadID, msg.ChannelID); rewriteErr == nil {
				return a.acceptEnvelope(ctx, msg, envelope, threadID)
			}
		} else {
			a.logger.WarnContext(ctx, "thread create fallback", "component", "discord", "error", threadErr)
		}
	}
	return a.acceptEnvelope(ctx, msg, envelope, msg.ChannelID)
}

func (a *Adapter) routePersona(ctx context.Context, scope *auraagent.RouteScope) (string, bool) {
	if a.router != nil {
		scope.Channel = config.BindingChannelDiscord
		scope.Account = a.account.Name
		if match, ok := a.router.Resolve(scope); ok {
			return match.Persona, true
		}
	}
	a.logger.DebugContext(ctx, "persona unresolved", "component", "discord", "account", a.account.Name)
	return "", false
}

func (a *Adapter) sessionKey(persona, conversationID string) string {
	return runtimeingress.SessionKey(persona, a.source(), conversationID)
}

func (a *Adapter) admitThreadMessage(ctx context.Context, msg *messagePayload, threadID string, route threadRoute) (bool, error) {
	if msg.GuildID == "" || !validThreadAuthor(a.account, a.owners, a.selfID, msg) {
		return false, nil
	}
	if len(a.account.AllowedGuildIDs) > 0 && !slices.Contains(a.account.AllowedGuildIDs, msg.GuildID) {
		return false, nil
	}
	if len(a.account.AllowedChannelIDs) > 0 && !slices.Contains(a.account.AllowedChannelIDs, route.parent) {
		return false, nil
	}
	persona, routed, err := a.threadPersona(ctx, msg, threadID, route)
	if err != nil || !routed {
		return false, err
	}
	principalID, class, _ := resolveAuthor(a.account, a.owners, msg.Author.ID)
	envelope, err := buildThreadEnvelope(a.account.Name, principalID, class, msg, threadID, route.parent)
	if err != nil {
		return false, err
	}
	envelope.AgentID = persona
	if inv, ok := a.commandInvocation(msg); ok {
		if handled, err := a.runCommand(ctx, msg, &inv, envelope); handled || err != nil {
			return handled, err
		}
	}
	return a.acceptEnvelope(ctx, msg, envelope, threadID)
}

func (a *Adapter) threadPersona(ctx context.Context, msg *messagePayload, threadID string, route threadRoute) (persona string, routed bool, err error) {
	if route.persona != "" {
		return route.persona, true, nil
	}
	persona, routed, err = a.existingThreadPersona(ctx, threadID)
	if err != nil {
		return "", false, fmt.Errorf("thread persona lookup: %w", err)
	}
	if !routed {
		persona, routed = a.routePersona(ctx, &auraagent.RouteScope{GuildID: msg.GuildID, ChannelID: route.parent, ThreadID: threadID, UserID: msg.Author.ID})
		if !routed {
			return "", false, nil
		}
	}
	a.rememberThread("", threadID, threadRoute{parent: route.parent, persona: persona})
	return persona, true, nil
}

func (a *Adapter) existingThreadPersona(ctx context.Context, threadID string) (persona string, found bool, err error) {
	if a.router == nil {
		return "", false, nil
	}
	for _, candidate := range a.router.Personas() {
		exists, lookupErr := a.sessions.SessionExists(ctx, a.sessionKey(candidate, "thread:"+threadID))
		if lookupErr != nil {
			return "", false, lookupErr
		}
		if exists {
			return candidate, true, nil
		}
	}
	return "", false, nil
}

type channelProbe struct {
	parent   string
	isThread bool
	owned    bool
}

func (a *Adapter) threadClassificationNeeded(ctx context.Context, msg *messagePayload, admitted bool) bool {
	if msg == nil || msg.GuildID == "" || !validThreadAuthor(a.account, a.owners, a.selfID, msg) {
		return false
	}
	if msg.ChannelType != nil {
		return isThreadChannelType(*msg.ChannelType)
	}
	if !admitted || len(a.account.AllowedChannelIDs) == 0 {
		return true
	}
	_, found, err := a.existingThreadPersona(ctx, msg.ChannelID)
	return err == nil && found
}

func (a *Adapter) probeChannel(ctx context.Context, channelID string) (channelProbe, error) {
	if a.selfID == "" || channelID == "" {
		return channelProbe{}, nil
	}
	token, _ := a.tokenRef.Resolve()
	if token == "" {
		return channelProbe{}, nil
	}
	info, err := a.fetchThreadParent(ctx, token, channelID)
	if err != nil {
		return channelProbe{}, err
	}
	if info.ID != channelID || !isThreadChannelType(info.Type) || info.ParentID == "" {
		return channelProbe{}, nil
	}
	return channelProbe{parent: info.ParentID, isThread: true, owned: info.OwnerID == a.selfID}, nil
}

func (a *Adapter) admitThreadRoute(ctx context.Context, msg *messagePayload, route threadRoute) (bool, error) {
	if len(a.account.AllowedChannelIDs) > 0 && !slices.Contains(a.account.AllowedChannelIDs, route.parent) {
		return false, nil
	}
	a.rememberThread("", msg.ChannelID, route)
	return a.admitThreadMessage(ctx, msg, msg.ChannelID, route)
}

func (a *Adapter) acceptEnvelope(ctx context.Context, msg *messagePayload, envelope *runtimeingress.IngressEnvelope, bindingChannel string) (bool, error) {
	active, err := a.activeSession(ctx, a.sessionKey(envelope.AgentID, envelope.ConversationID))
	if err != nil {
		return false, err
	}
	envelope.ConversationID = active
	a.rememberTarget(envelope.ConversationID, approvalBinding{principal: envelope.PrincipalID, actor: msg.Author.ID, guildID: msg.GuildID, channelID: bindingChannel})
	if err := a.sessions.EnsureSession(ctx, envelope.ConversationID, envelope.PrincipalID, envelope.AgentID); err != nil {
		return false, fmt.Errorf("ensure session: %w", err)
	}
	if len(msg.Attachments) > 0 && !a.intakeAttachments(ctx, msg, envelope) {
		return false, nil
	}
	if _, err := a.sink.Accept(ctx, envelope); err != nil {
		return false, fmt.Errorf("accept ingress: %w", err)
	}
	if a.admitted != nil {
		a.admitted(ctx, principal.ParseClass(envelope.PrincipalClass))
	}
	channelID := msg.ChannelID
	go func() {
		call, cancel := context.WithTimeout(context.WithoutCancel(ctx), restTimeout)
		defer cancel()
		if err := a.triggerTyping(call, channelID); err != nil {
			a.logger.WarnContext(call, "typing trigger failed", "component", "discord")
		}
	}()
	return true, nil
}

func (a *Adapter) intakeAttachments(ctx context.Context, msg *messagePayload, envelope *runtimeingress.IngressEnvelope) bool {
	summaries, images, err := a.storeAttachments(ctx, msg.Attachments, envelope.ConversationID, envelope.PrincipalID, envelope.AgentID, msg.ID)
	if err != nil {
		a.logger.WarnContext(ctx, "attachment intake failed", "component", "discord")
		return false
	}
	if err := attachArtifacts(envelope, summaries); err != nil {
		a.logger.WarnContext(ctx, "attachment reference failed", "component", "discord")
		return false
	}
	for _, raw := range images {
		envelope.Parts = append(envelope.Parts, runtimeingress.InputPart{Image: raw})
	}
	return true
}

func isVisionImageMIME(contentType string) bool {
	media := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch media {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func attachArtifacts(envelope *runtimeingress.IngressEnvelope, summaries []artifactSummary) error {
	var reference replyReference
	if err := json.Unmarshal(envelope.ReplyContext, &reference); err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "reply context is not decodable")
	}
	reference.Artifacts = summaries
	raw, err := json.Marshal(reference)
	if err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "reply context is not serializable")
	}
	envelope.ReplyContext = raw
	return nil
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
