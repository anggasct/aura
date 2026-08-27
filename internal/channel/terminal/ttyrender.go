package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// maxDisplayLines bounds how many physical lines one frame may repaint,
	// so a tiny or zero-width terminal cannot grow the repaint region.
	maxDisplayLines = 32
	// maxProgressLines bounds the tool/approval progress tail per frame.
	maxProgressLines = 8
	defaultRenderHz  = 20
	maxRenderHz      = int(time.Second / time.Nanosecond)
)

// TTYOptions configures the interactive presentation renderer.
type TTYOptions struct {
	Out     io.Writer
	Width   func() int // probe per paint; zero or nil means unknown
	Hz      int        // paint frequency; zero selects the default
	Styling bool       // false (NO_COLOR) disables all escape sequences
	// Stdin and Stdout wire the multiline editor gesture to the real
	// terminal; nil disables the gesture.
	Stdin  *os.File
	Stdout *os.File
}

// TTYRenderer paints streamed runtime events as in-place terminal frames.
// Untrusted text is sanitized before entering render state; only the
// renderer's own escape sequences reach the output. Frames coalesce at the
// configured rate, the display is bounded regardless of stream size, and the
// completed durable message wins over streamed partials.
type TTYRenderer struct {
	opt TTYOptions
	hz  time.Duration

	writeMu sync.Mutex // serializes frame writes

	mu          sync.Mutex
	partial     bytes.Buffer
	final       string
	finalSet    bool
	progress    []string
	terminalHit bool
	dirty       bool
	frameLines  int
	painted     bool
	err         error
}

// NewTTYRenderer builds the interactive renderer. A nil Width probe or a
// non-positive hz degrades presentation only.
func NewTTYRenderer(opt TTYOptions) *TTYRenderer {
	hz := opt.Hz
	if hz <= 0 {
		hz = defaultRenderHz
	}
	if hz > maxRenderHz {
		hz = maxRenderHz
	}
	return &TTYRenderer{
		opt: opt,
		hz:  time.Second / time.Duration(hz),
	}
}

// Begin resets per-turn render state.
func (r *TTYRenderer) Begin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partial.Reset()
	r.final = ""
	r.finalSet = false
	r.progress = nil
	r.terminalHit = false
	r.dirty = false
	r.frameLines = 0
	r.painted = false
}

// Observe folds one runtime event into render state; it never blocks the
// event stream and never grows past the render bounds.
func (r *TTYRenderer) Observe(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Kind {
	case "model.delta":
		if !r.finalSet {
			r.partial.Write(decodeDelta(ev.Payload))
			r.capLocked()
		}
	case "message.completed":
		r.finalSet = true
		r.final = limitText(string(decodeDelta(ev.Payload)))
	case "adk_event":
		if len(ev.Payload) == 0 || len(ev.Payload) > 4*maxRenderBytes {
			return
		}
		var adk struct {
			Content *struct {
				Role  string `json:"role"`
				Parts []struct {
					Text         *string `json:"text"`
					FunctionCall *struct {
						Name string `json:"name"`
					} `json:"functionCall"`
					FunctionResponse *struct {
						Name string `json:"name"`
					} `json:"functionResponse"`
				} `json:"parts"`
			} `json:"content"`
			Actions *struct {
				TransferToAgent string `json:"transferToAgent"`
				Escalate        bool   `json:"escalate"`
			} `json:"actions"`
			LongRunningToolIDs []string `json:"longRunningToolIds"`
			Partial            bool     `json:"partial"`
		}
		if err := json.Unmarshal(ev.Payload, &adk); err != nil {
			return
		}
		var hasText bool
		if adk.Content != nil {
			for _, part := range adk.Content.Parts {
				if part.FunctionCall != nil && part.FunctionCall.Name != "" {
					r.appendProgress("tool requested: " + part.FunctionCall.Name)
				}
				if part.FunctionResponse != nil && part.FunctionResponse.Name != "" {
					r.appendProgress("tool completed: " + part.FunctionResponse.Name)
				}
				if part.Text != nil && adk.Content.Role != "user" {
					hasText = true
				}
			}
		}
		if adk.Actions != nil {
			if adk.Actions.TransferToAgent != "" {
				r.appendProgress("agent transfer requested: " + adk.Actions.TransferToAgent)
			}
			if adk.Actions.Escalate {
				r.appendProgress("agent escalation requested")
			}
		}
		if len(adk.LongRunningToolIDs) > 0 {
			r.appendProgress("long-running tool active")
		}
		if !hasText {
			r.dirty = true
			return
		}
		var text strings.Builder
		if adk.Content != nil {
			for _, part := range adk.Content.Parts {
				if part.Text != nil && adk.Content.Role != "user" {
					text.WriteString(*part.Text)
				}
			}
		}
		if adk.Partial {
			if !r.finalSet {
				r.partial.WriteString(text.String())
				r.capLocked()
			}
		} else {
			r.final = limitText(text.String())
			r.finalSet = true
		}
	case "tool.requested":
		var request struct {
			Operation string `json:"operation"`
		}
		if err := json.Unmarshal(ev.Payload, &request); err != nil {
			return
		}
		if request.Operation == "" {
			request.Operation = "unknown"
		}
		r.appendProgress("tool requested: " + request.Operation)
	case "tool.started":
		r.appendProgress("tool started: " + ev.Author)
	case "tool.completed":
		r.appendProgress("tool completed: " + ev.Author)
	case "approval.required":
		r.appendProgress("approval required but denied")
	case "turn.completed", "turn.failed", "turn.cancelled":
		r.terminalHit = true
	default:
		return
	}
	r.dirty = true
}

func (r *TTYRenderer) appendProgress(line string) {
	r.progress = append(r.progress, limitText(sanitizeText(line)))
	if len(r.progress) > maxProgressLines {
		r.progress = r.progress[len(r.progress)-maxProgressLines:]
	}
}

func (r *TTYRenderer) capLocked() {
	if r.partial.Len() > maxRenderBytes {
		trimmed := limitText(r.partial.String())
		r.partial.Reset()
		r.partial.WriteString(trimmed)
	}
}

// StartPump runs the paint loop until ctx is cancelled. Paints coalesce: a
// slow consumer naturally skips ticks because the loop is single-threaded,
// and a clean frame state means no write at all.
func (r *TTYRenderer) StartPump(ctx context.Context, onError func(error)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(r.hz)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.Paint(); err != nil {
					r.setErr(err)
					if onError != nil {
						onError(err)
					}
					return
				}
			}
		}
	}()
	return done
}

// Err reports the first paint failure, e.g. a closed output.
func (r *TTYRenderer) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *TTYRenderer) setErr(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
}

// Paint writes one frame when state changed since the last paint. It is
// called from the pump only.
func (r *TTYRenderer) Paint() error {
	r.mu.Lock()
	if r.err != nil {
		err := r.err
		r.mu.Unlock()
		return err
	}
	if !r.dirty {
		r.mu.Unlock()
		return nil
	}
	frame, lines := r.buildFrame(false, false)
	r.dirty = false
	r.mu.Unlock()
	return r.writeFrame(frame, lines)
}

// Finalize paints the terminal frame with the authoritative text: the
// completed durable message when present, otherwise the streamed partial,
// and a failure marker replaces partial text for failed turns.
func (r *TTYRenderer) Finalize(failed, cancelled bool) error {
	r.mu.Lock()
	frame, lines := r.buildFrame(failed, cancelled)
	r.mu.Unlock()
	return r.writeFrame(frame, lines)
}

// buildFrame composes the frame under lock. failed turns discard partial
// text: the durable log owns what happened, the display shows the outcome.
func (r *TTYRenderer) buildFrame(failed, cancelled bool) (frame string, lines int) {
	width := 0
	if r.opt.Width != nil {
		width = r.opt.Width()
		if width > 4096 {
			width = 4096
		}
	}
	var body string
	switch {
	case failed:
		body = "turn failed"
	case cancelled:
		body = "turn cancelled"
	case r.finalSet:
		body = r.final
	default:
		body = r.partial.String()
	}
	text := sanitizeText(body)
	physical := truncateLines(wrapText(text, width), maxDisplayLines)
	progress := r.progress

	var b strings.Builder
	if r.opt.Styling && r.painted && r.frameLines > 0 {
		fmt.Fprintf(&b, "\x1b[%dA\r\x1b[J", r.frameLines)
	}
	for _, p := range progress {
		writeStyled(&b, r.opt.Styling, dim, p)
		lines++
	}
	for _, line := range physical {
		writeStyled(&b, r.opt.Styling, bold, line.text)
		lines++
	}
	return b.String(), lines
}

func writeStyled(b *strings.Builder, styling bool, attr, text string) {
	if text == "" {
		b.WriteByte('\n')
		return
	}
	if styling {
		b.WriteString(attr)
	}
	b.WriteString(text)
	if styling {
		b.WriteString(reset)
	}
	b.WriteByte('\n')
}

const (
	dim   = "\x1b[2m"
	bold  = "\x1b[1m"
	reset = "\x1b[0m"
)

// writeFrame serializes writes so a cancelled pump cannot interleave with
// the final frame, and records the frame extent for the next in-place
// repaint.
func (r *TTYRenderer) writeFrame(frame string, lines int) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	r.frameLines = lines
	r.painted = true
	r.mu.Unlock()
	if _, err := io.WriteString(r.opt.Out, frame); err != nil {
		return fmt.Errorf("terminal: paint: %w", err)
	}
	return nil
}

// ClearScreen erases the display when styling is available; otherwise it
// degrades to a blank line.
func (r *TTYRenderer) ClearScreen() error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	r.frameLines = 0
	r.mu.Unlock()
	if !r.opt.Styling {
		_, err := io.WriteString(r.opt.Out, "\n")
		if err != nil {
			return fmt.Errorf("terminal: clear: %w", err)
		}
		return nil
	}
	if _, err := io.WriteString(r.opt.Out, "\x1b[2J\x1b[H"); err != nil {
		return fmt.Errorf("terminal: clear: %w", err)
	}
	return nil
}

// Compose opens the user's editor to collect a multi-line prompt, the
// explicit multiline gesture of the interactive surface. ok is false when the
// editor produced nothing to submit. The composition is bounded by the
// configured input cap and never written to a persistent history file.
func (r *TTYRenderer) Compose(ctx context.Context, maxBytes int) (text string, ok bool, err error) {
	if maxBytes <= 0 {
		return "", false, errors.New("terminal: multiline input is unbounded by configuration")
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	if r.opt.Stdin == nil || r.opt.Stdout == nil {
		return "", false, errors.New("terminal: multiline editing needs an interactive terminal")
	}
	tmp, err := os.CreateTemp("", "aura-prompt-*.md")
	if err != nil {
		return "", false, fmt.Errorf("terminal: create draft: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Close(); err != nil {
		return "", false, fmt.Errorf("terminal: close draft: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", false, fmt.Errorf("terminal: secure draft: %w", err)
	}
	cmd := exec.CommandContext(ctx, editor, tmp.Name())
	cmd.Stdin = r.opt.Stdin
	cmd.Stdout = r.opt.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("terminal: editor: %w", err)
	}
	draftFile, err := os.Open(tmp.Name())
	if err != nil {
		return "", false, fmt.Errorf("terminal: read draft: %w", err)
	}
	draft, err := io.ReadAll(io.LimitReader(draftFile, int64(maxBytes)+1))
	closeErr := draftFile.Close()
	if err != nil {
		return "", false, fmt.Errorf("terminal: read draft: %w", err)
	}
	if closeErr != nil {
		return "", false, fmt.Errorf("terminal: close draft: %w", closeErr)
	}
	if len(draft) > maxBytes {
		return "", false, errors.New("terminal: multiline input exceeds the configured maximum")
	}
	composed := strings.TrimRight(string(draft), "\n")
	if composed == "" {
		return "", false, nil
	}
	return composed, true, nil
}
