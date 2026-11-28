package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const DefaultRelabelBatchSize = 256

type RelabelEventReader interface {
	ListAllSessionEvents(ctx context.Context, sessionID string) ([]turn.Event, error)
}

type RelabelOptions struct {
	TaintMarks   TaintMarksReader
	Events       RelabelEventReader
	ToolMayTaint func(name string) bool
	BatchSize    int
}

type dbRelabelEventReader struct {
	db *sql.DB
}

func (r dbRelabelEventReader) ListAllSessionEvents(ctx context.Context, sessionID string) ([]turn.Event, error) {
	return turnstore.ListAllSessionEvents(ctx, r.db, sessionID)
}

func boolPtr(v bool) *bool {
	return &v
}

type batchTaintContext struct {
	req                 *turn.TurnRequest
	firstMarkBySession  map[string]uint64
	requestTaintedTurns map[string]bool
	taintMarksFailed    bool
}

func (s *Service) buildBatchTaintContext(ctx context.Context, req *turn.TurnRequest, events []*Event) *batchTaintContext {
	taintCtx := &batchTaintContext{
		req:                 req,
		firstMarkBySession:  map[string]uint64{},
		requestTaintedTurns: map[string]bool{},
	}
	seen := map[string]struct{}{}
	var sessionIDs []string
	if req != nil && req.SessionID != "" {
		seen[req.SessionID] = struct{}{}
		sessionIDs = append(sessionIDs, req.SessionID)
	}
	for _, ev := range events {
		if ev == nil {
			continue
		}
		if ev.SessionID != "" {
			if _, ok := seen[ev.SessionID]; !ok {
				seen[ev.SessionID] = struct{}{}
				sessionIDs = append(sessionIDs, ev.SessionID)
			}
		}
		taintCtx.observeEvent(ev.SessionID, ev.Sequence, ev.TurnID, ev.Author, ev.Kind, ev.Payload)
	}
	if s != nil && s.taintMarks != nil && len(sessionIDs) > 0 {
		marks, err := s.taintMarks.SessionMarks(ctx, sessionIDs)
		if err != nil {
			taintCtx.taintMarksFailed = true
		} else {
			for sid, seq := range marks {
				if existing, ok := taintCtx.firstMarkBySession[sid]; !ok || seq < existing {
					taintCtx.firstMarkBySession[sid] = seq
				}
			}
		}
	}
	return taintCtx
}

func (c *batchTaintContext) observeEvent(sessionID string, sequence uint64, turnID, author, kind string, payload json.RawMessage) {
	switch kind {
	case turn.EventKindSessionTainted:
		mark, ok := turn.DecodeTaintMark(payload)
		if !ok {
			mark = turn.TaintMark{Source: turn.TaintSourceLegacy}
		}
		if mark.Source == turn.TaintSourceOtherPrincipal && mark.Principal == principal.OwnerID {
			return
		}
		effSeq := sequence
		if mark.AtSequence > 0 {
			effSeq = uint64(mark.AtSequence)
		}
		if sessionID != "" && effSeq > 0 {
			if existing, exists := c.firstMarkBySession[sessionID]; !exists || effSeq < existing {
				c.firstMarkBySession[sessionID] = effSeq
			}
		}
		if turnID != "" && turn.IsRequestTaintSource(mark.Source) {
			c.requestTaintedTurns[turnID] = true
		}
	case turn.EventKindTurnAccepted:
		if turnID != "" && acceptedTurnHasRequestTaint(author, payload) {
			c.requestTaintedTurns[turnID] = true
		}
	}
}

func (c *batchTaintContext) isEventTainted(ownerID string, event *Event) bool {
	if event == nil {
		return true
	}
	if event.Kind == turnstore.EventKindADK && event.Author == "user" {
		return c.isUserEventTainted(ownerID, event)
	}
	return c.isOtherEventTainted(ownerID, event.SessionID, event.Sequence)
}

func (c *batchTaintContext) isUserEventTainted(ownerID string, event *Event) bool {
	if principal.ClassForPrincipal(ownerID) != principal.ClassOwner {
		return true
	}
	if c != nil && c.req != nil {
		if c.req.PrincipalClass != "" && c.req.PrincipalClass != principal.ClassOwner {
			return true
		}
		if mark, ok := c.req.AdmissionTaint(); ok && turn.IsRequestTaintSource(mark.Source) {
			return true
		}
	}
	if hasUserRequestTaint(event.Payload) {
		return true
	}
	if c != nil && event.TurnID != "" && c.requestTaintedTurns[event.TurnID] {
		return true
	}
	return false
}

func (c *batchTaintContext) isOtherEventTainted(ownerID, sessionID string, toSeq uint64) bool {
	if principal.ClassForPrincipal(ownerID) != principal.ClassOwner {
		return true
	}
	if c != nil && c.req != nil && c.req.PrincipalClass != "" && c.req.PrincipalClass != principal.ClassOwner {
		return true
	}
	if c != nil && c.taintMarksFailed {
		return true
	}
	if c != nil {
		if firstSeq, ok := c.firstMarkBySession[sessionID]; ok && firstSeq <= toSeq {
			return true
		}
		if c.req != nil {
			if _, ok := c.req.AdmissionTaint(); ok {
				return true
			}
		}
	}
	return false
}

func acceptedTurnHasRequestTaint(author string, payload json.RawMessage) bool {
	var accepted struct {
		Origin    string `json:"origin"`
		Principal string `json:"principal"`
	}
	if err := json.Unmarshal(payload, &accepted); err != nil {
		return true
	}
	origin := turn.Origin(accepted.Origin)
	if origin == turn.OriginHTTP {
		return true
	}
	if !isAuthoredTurnOrigin(origin) {
		return true
	}
	actor := accepted.Principal
	if actor == "" {
		actor = author
	}
	if actor != "" && actor != turn.AuthorSystem && principal.ClassForPrincipal(actor) != principal.ClassOwner {
		return true
	}
	return false
}

func isAuthoredTurnOrigin(origin turn.Origin) bool {
	if origin == turn.OriginTerminal || origin == turn.OriginSchedule || origin == turn.OriginHTTP {
		return true
	}
	return origin.OnChannel(principal.ChannelTelegram) ||
		origin.OnChannel(principal.ChannelDiscord) ||
		origin.OnChannel(principal.ChannelEmail)
}

func hasUserRequestTaint(payload json.RawMessage) bool {
	if len(bytes.TrimSpace(payload)) == 0 {
		return false
	}
	var envelope struct {
		Content *struct {
			Parts []struct {
				Text       string          `json:"text"`
				InlineData json.RawMessage `json:"inlineData"`
				FileData   json.RawMessage `json:"fileData"`
			} `json:"parts"`
		} `json:"content"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return true
	}
	if envelope.Content != nil {
		for i := range envelope.Content.Parts {
			part := &envelope.Content.Parts[i]
			if hasJSONValue(part.InlineData) || hasJSONValue(part.FileData) {
				return true
			}
			if textHasRelayedOrAttachment(part.Text) {
				return true
			}
		}
		return false
	}
	return textHasRelayedOrAttachment(envelope.Text)
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func textHasRelayedOrAttachment(text string) bool {
	if text == "" {
		return false
	}
	if strings.Contains(text, "[untrusted_input") {
		return true
	}
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[attachment ") && strings.Contains(trimmed, "artifact:") {
			return true
		}
	}
	return false
}

func RelabelUnlabelled(ctx context.Context, db *sql.DB, opts *RelabelOptions) (int, error) {
	if ctx == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if db == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	batchSize := DefaultRelabelBatchSize
	var taintMarks TaintMarksReader
	var eventsReader RelabelEventReader
	var toolMayTaint func(string) bool
	if opts != nil {
		if opts.BatchSize > 0 {
			batchSize = opts.BatchSize
		}
		taintMarks = opts.TaintMarks
		eventsReader = opts.Events
		toolMayTaint = opts.ToolMayTaint
	}
	if taintMarks == nil {
		taintMarks = turnstore.NewTaintReader(db)
	}
	if eventsReader == nil {
		eventsReader = dbRelabelEventReader{db: db}
	}
	eventCount, err := relabelKindBatches(ctx, db, KindEventText, batchSize, taintMarks, eventsReader, toolMayTaint)
	if err != nil {
		return eventCount, err
	}
	summaryCount, err := relabelKindBatches(ctx, db, KindSummary, batchSize, taintMarks, eventsReader, toolMayTaint)
	if err != nil {
		return eventCount + summaryCount, err
	}
	return eventCount + summaryCount, nil
}

type sessionRelabelInfo struct {
	hasMark             bool
	firstOwnerMarkSeq   uint64
	requestTaintedTurns map[string]bool
	eventsBySeq         map[uint64]turn.Event
}

func relabelKindBatches(ctx context.Context, db *sql.DB, kind string, batchSize int, taintMarks TaintMarksReader, eventsReader RelabelEventReader, toolMayTaint func(string) bool) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		batch, err := listUnlabelledByKind(ctx, db, kind, batchSize)
		if err != nil {
			return total, err
		}
		if len(batch) == 0 {
			return total, nil
		}
		infoBySession, err := loadSessionRelabelInfo(ctx, batch, taintMarks, eventsReader, toolMayTaint)
		if err != nil {
			return total, err
		}
		labels := make(map[string]bool, len(batch))
		for i := range batch {
			doc := &batch[i]
			tainted, err := deriveRelabelledTaint(ctx, db, doc, infoBySession[doc.SessionID])
			if err != nil {
				return total, err
			}
			labels[doc.ID] = tainted
		}
		updated, err := applyMemoryLabels(ctx, db, labels)
		if err != nil {
			return total, err
		}
		total += updated
		if updated == 0 {
			return total, nil
		}
	}
}

func listUnlabelledByKind(ctx context.Context, db *sql.DB, kind string, limit int) ([]StoredDocument, error) {
	rows, err := sqlite.Conn(ctx, db).QueryContext(ctx,
		`SELECT `+selectMemoryDocumentColumns+` FROM memory_document d
		WHERE d.kind = ? AND d.tainted IS NULL
		ORDER BY d.session_id ASC, d.source_from_sequence ASC, d.id ASC
		LIMIT ?`,
		kind, limit,
	)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list unlabelled memory documents: %w", err))
	}
	defer func() { _ = rows.Close() }()
	out := make([]StoredDocument, 0, limit)
	for rows.Next() {
		doc, err := scanMemoryDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list unlabelled memory documents: %w", err))
	}
	return out, nil
}

func loadSessionRelabelInfo(ctx context.Context, batch []StoredDocument, taintMarks TaintMarksReader, eventsReader RelabelEventReader, toolMayTaint func(string) bool) (map[string]*sessionRelabelInfo, error) {
	ownerBySession := map[string]string{}
	var sessionIDs []string
	for i := range batch {
		sid := batch[i].SessionID
		if _, ok := ownerBySession[sid]; !ok {
			ownerBySession[sid] = batch[i].OwnerID
			sessionIDs = append(sessionIDs, sid)
		}
	}
	marks, err := taintMarks.SessionMarks(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*sessionRelabelInfo, len(sessionIDs))
	for _, sid := range sessionIDs {
		info := &sessionRelabelInfo{
			requestTaintedTurns: map[string]bool{},
			eventsBySeq:         map[uint64]turn.Event{},
		}
		if seq, ok := marks[sid]; ok {
			info.hasMark = true
			info.firstOwnerMarkSeq = seq
		}
		events, err := eventsReader.ListAllSessionEvents(ctx, sid)
		if err != nil {
			return nil, err
		}
		taintCtx := &batchTaintContext{
			firstMarkBySession:  map[string]uint64{},
			requestTaintedTurns: info.requestTaintedTurns,
		}
		for i := range events {
			ev := &events[i]
			info.eventsBySeq[ev.Sequence] = *ev
			taintCtx.observeEvent(sid, ev.Sequence, ev.TurnID, ev.Author, ev.Kind, ev.Payload)
		}
		if seq, ok := taintCtx.firstMarkBySession[sid]; ok && (!info.hasMark || seq < info.firstOwnerMarkSeq) {
			info.hasMark = true
			info.firstOwnerMarkSeq = seq
		}
		if !info.hasMark && len(events) > 0 {
			if legacySeq, found := turn.FirstLegacyUntrustedSequence(ownerBySession[sid], events, toolMayTaint); found {
				info.hasMark = true
				info.firstOwnerMarkSeq = legacySeq
			}
		}
		out[sid] = info
	}
	return out, nil
}

func deriveRelabelledTaint(ctx context.Context, db *sql.DB, doc *StoredDocument, info *sessionRelabelInfo) (bool, error) {
	if doc == nil {
		return true, nil
	}
	if doc.Kind == KindSummary {
		hasAny, hasTainted, err := coveredEventTextTaint(ctx, db, doc.OwnerID, doc.SessionID, doc.FromSequence, doc.ToSequence)
		if err != nil {
			return true, err
		}
		if hasAny {
			return hasTainted, nil
		}
		if principal.ClassForPrincipal(doc.OwnerID) != principal.ClassOwner {
			return true, nil
		}
		return info != nil && info.hasMark && doc.ToSequence >= 0 && info.firstOwnerMarkSeq <= uint64(doc.ToSequence), nil
	}
	if principal.ClassForPrincipal(doc.OwnerID) != principal.ClassOwner {
		return true, nil
	}
	if doc.TrustLabel == string(approval.TrustOwnerInput) {
		if textHasRelayedOrAttachment(doc.Content) {
			return true, nil
		}
		if info != nil && doc.FromSequence > 0 && doc.ToSequence >= 0 && doc.ToSequence >= doc.FromSequence {
			from := uint64(doc.FromSequence)
			to := uint64(doc.ToSequence)
			for seq := from; seq <= to; seq++ {
				ev, ok := info.eventsBySeq[seq]
				if !ok {
					continue
				}
				if hasUserRequestTaint(ev.Payload) {
					return true, nil
				}
				if ev.TurnID != "" && info.requestTaintedTurns[ev.TurnID] {
					return true, nil
				}
			}
		}
		return false, nil
	}
	return info != nil && info.hasMark && doc.ToSequence >= 0 && info.firstOwnerMarkSeq <= uint64(doc.ToSequence), nil
}

func coveredEventTextTaint(ctx context.Context, db *sql.DB, ownerID, sessionID string, from, to int64) (hasAny, hasTainted bool, err error) {
	var total, tainted int64
	err = sqlite.Conn(ctx, db).QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN tainted IS NULL OR tainted != 0 THEN 1 ELSE 0 END), 0)
		FROM memory_document
		WHERE owner_id = ? AND session_id = ? AND kind = ?
		  AND source_from_sequence >= ? AND source_to_sequence <= ?`,
		ownerID, sessionID, KindEventText, from, to,
	).Scan(&total, &tainted)
	if err != nil {
		return false, false, sqlite.Classify(fmt.Errorf("check covered memory taint: %w", err))
	}
	return total > 0, tainted > 0, nil
}

func applyMemoryLabels(ctx context.Context, db *sql.DB, labels map[string]bool) (int, error) {
	if len(labels) == 0 {
		return 0, nil
	}
	ids := slices.Sorted(maps.Keys(labels))
	updated := 0
	err := sqlite.RunInTx(ctx, db, func(txCtx context.Context) error {
		conn := sqlite.Conn(txCtx, db)
		for _, id := range ids {
			val := 0
			if labels[id] {
				val = 1
			}
			result, err := conn.ExecContext(txCtx,
				`UPDATE memory_document SET tainted = ? WHERE id = ? AND tainted IS NULL`,
				val, id,
			)
			if err != nil {
				return sqlite.Classify(fmt.Errorf("relabel memory document: %w", err))
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return sqlite.Classify(fmt.Errorf("relabel memory document: %w", err))
			}
			updated += int(affected)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return updated, nil
}
