package terminal

import (
	"bytes"
	"context"
	"io"
	"iter"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// approvalRunner asks one approval through the bridge mid-turn, mirroring
// the runtime worker's position inside the event stream.
type approvalRunner struct {
	bridge *ApprovalBridge
	card   *ApprovalCard

	mu       sync.Mutex
	accepted bool
	askErr   error
}

func (r *approvalRunner) Run(ctx context.Context, req *Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		accepted, err := r.bridge.Decide(ctx, r.card)
		r.mu.Lock()
		r.accepted = accepted
		r.askErr = err
		r.mu.Unlock()
		yield(Event{Kind: "model.delta", Payload: delta("working")}, nil)
		if accepted {
			yield(Event{Kind: "tool.completed", Author: req.SessionID}, nil)
		}
		yield(Event{Kind: "turn.completed"}, nil)
	}
}

func (r *approvalRunner) decision() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accepted, r.askErr
}

func testCard() *ApprovalCard {
	return &ApprovalCard{
		ToolName:       "exec",
		ToolVersion:    "v1",
		SessionID:      "sess_1",
		TurnID:         "turn_1",
		PrincipalID:    "owner",
		Arguments:      `{"command":["ls","-la"]}`,
		Network:        false,
		Timeout:        30 * time.Second,
		MaxOutputBytes: 65536,
		PolicyVersion:  "builtin-tools-v1",
		ReasonCode:     "approval_required",
		ExpiresAt:      time.Now().Add(5 * time.Minute),
	}
}

func newApprovalConsole(runner Runner, sess *fakeSessions, in io.Reader) (*Console, *ApprovalBridge, *bytes.Buffer) {
	bridge := NewApprovalBridge()
	out := &bytes.Buffer{}
	diag := &bytes.Buffer{}
	console := NewConsole(runner, sess, PlainRenderer{}, in, out, diag, Config{
		MaxInputBytes:       1000,
		InMemoryHistory:     100,
		SecondInterruptTime: 2 * time.Second,
	}, "owner")
	console.SetTTY(NewTTYRenderer(TTYOptions{
		Out:     testWriteCloser{Writer: out},
		Width:   func() int { return 80 },
		Hz:      500,
		Styling: false,
	}))
	console.SetApprovalBridge(bridge)
	return console, bridge, out
}

func TestApprovalAcceptedExecutesTool(t *testing.T) {
	runner := &approvalRunner{bridge: NewApprovalBridge(), card: testCard()}
	console, bridge, out := newApprovalConsole(runner, newFakeSessions(), strings.NewReader("hi\ny\n"))
	runner.bridge = bridge
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	accepted, askErr := runner.decision()
	if askErr != nil || !accepted {
		t.Fatalf("decision = %v, %v; want accepted", accepted, askErr)
	}
	if !strings.Contains(out.String(), "approval required: exec@v1") {
		t.Errorf("output = %q, want the approval card", out.String())
	}
	if !strings.Contains(out.String(), "approval accepted: exec@v1") {
		t.Errorf("output = %q, want the accepted outcome", out.String())
	}
}

func TestApprovalCardDisplaysCanonicalScope(t *testing.T) {
	runner := &approvalRunner{bridge: NewApprovalBridge(), card: testCard()}
	console, bridge, out := newApprovalConsole(runner, newFakeSessions(), strings.NewReader("hi\nyes\n"))
	runner.bridge = bridge
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rendered := out.String()
	for _, want := range []string{
		`arguments: {"command":["ls","-la"]}`,
		"session: sess_1",
		"principal: owner",
		"effect: network=false timeout=30s max_output_bytes=65536",
		"policy: builtin-tools-v1 (approval_required)",
		"approve exactly this request? [y/N]",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("card = %q, want %q", rendered, want)
		}
	}
	if accepted, _ := runner.decision(); !accepted {
		t.Errorf("decision not accepted for explicit yes")
	}
}

func TestApprovalDefaultActionIsReject(t *testing.T) {
	runner := &approvalRunner{bridge: NewApprovalBridge(), card: testCard()}
	console, bridge, out := newApprovalConsole(runner, newFakeSessions(), strings.NewReader("hi\n\n"))
	runner.bridge = bridge
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if accepted, _ := runner.decision(); accepted {
		t.Errorf("empty answer must reject")
	}
	if !strings.Contains(out.String(), "approval rejected: exec@v1") {
		t.Errorf("output = %q, want rejection outcome", out.String())
	}
}

func TestApprovalExplicitReject(t *testing.T) {
	runner := &approvalRunner{bridge: NewApprovalBridge(), card: testCard()}
	console, bridge, out := newApprovalConsole(runner, newFakeSessions(), strings.NewReader("hi\nn\n"))
	runner.bridge = bridge
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if accepted, _ := runner.decision(); accepted {
		t.Errorf("explicit n must reject")
	}
	if !strings.Contains(out.String(), "approval rejected: exec@v1") {
		t.Errorf("output = %q, want rejection outcome", out.String())
	}
}

func TestApprovalEOFRejects(t *testing.T) {
	runner := &approvalRunner{bridge: NewApprovalBridge(), card: testCard()}
	console, bridge, out := newApprovalConsole(runner, newFakeSessions(), strings.NewReader("hi\n"))
	runner.bridge = bridge
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if accepted, _ := runner.decision(); accepted {
		t.Errorf("EOF must reject")
	}
	if !strings.Contains(out.String(), "approval rejected (input closed): exec@v1") {
		t.Errorf("output = %q, want input-closed rejection", out.String())
	}
}

// syncBuffer serializes writes and reads across goroutines, for tests that
// poll the output while the console runs on another goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type syncOutput struct{ s *syncBuffer }

func (o syncOutput) WriteContext(_ context.Context, p []byte) (int, error) { return o.s.Write(p) }
func (o syncOutput) Close() error                                          { return nil }

func TestApprovalExpiryRejects(t *testing.T) {
	runner := &approvalRunner{bridge: NewApprovalBridge()}
	card := testCard()
	card.ExpiresAt = time.Now().Add(50 * time.Millisecond)
	runner.card = card
	pr, pw := io.Pipe()
	bridge := NewApprovalBridge()
	runner.bridge = bridge
	out := &syncBuffer{}
	diag := &bytes.Buffer{}
	console := NewConsole(runner, newFakeSessions(), PlainRenderer{}, pr, out, diag, Config{
		MaxInputBytes:       1000,
		InMemoryHistory:     100,
		SecondInterruptTime: 2 * time.Second,
	}, "owner")
	console.SetTTY(NewTTYRenderer(TTYOptions{
		Out:     syncOutput{s: out},
		Width:   func() int { return 80 },
		Hz:      500,
		Styling: false,
	}))
	console.SetApprovalBridge(bridge)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- console.Run(ctx) }()
	if _, err := pw.Write([]byte("hi\n")); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "approval rejected (expired): exec@v1") {
			break
		}
		goruntime.Gosched()
	}
	if !strings.Contains(out.String(), "approval rejected (expired): exec@v1") {
		t.Fatalf("output = %q, want expired rejection", out.String())
	}
	if accepted, _ := runner.decision(); accepted {
		t.Errorf("expired card must reject")
	}
	cancel()
	if err := pw.Close(); err != nil {
		t.Fatalf("close input: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("console did not stop after cancellation")
	}
}

type lateApprovalRunner struct {
	bridge *ApprovalBridge
	card   *ApprovalCard

	mu      sync.Mutex
	prompts []string
	turns   int
}

func (r *lateApprovalRunner) Run(ctx context.Context, req *Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		r.mu.Lock()
		r.prompts = append(r.prompts, req.Parts[0].Text)
		r.turns++
		turn := r.turns
		r.mu.Unlock()
		if turn == 1 {
			_, _ = r.bridge.Decide(ctx, r.card)
		}
		yield(Event{Kind: "turn.completed"}, nil)
	}
}

func (r *lateApprovalRunner) promptList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prompts...)
}

func TestLateApprovalInputIsDiscardedInsteadOfBecomingPrompt(t *testing.T) {
	pr, pw := io.Pipe()
	bridge := NewApprovalBridge()
	runner := &lateApprovalRunner{bridge: bridge, card: testCard()}
	runner.card.ExpiresAt = time.Now().Add(50 * time.Millisecond)
	out := &syncBuffer{}
	console := NewConsole(runner, newFakeSessions(), PlainRenderer{}, pr, out, &bytes.Buffer{}, Config{
		MaxInputBytes:       1000,
		InMemoryHistory:     100,
		SecondInterruptTime: 2 * time.Second,
	}, "owner")
	console.SetTTY(NewTTYRenderer(TTYOptions{
		Out:     syncOutput{s: out},
		Width:   func() int { return 80 },
		Hz:      500,
		Styling: false,
	}))
	console.SetApprovalBridge(bridge)

	done := make(chan error, 1)
	go func() { done <- console.Run(context.Background()) }()
	if _, err := pw.Write([]byte("first\n")); err != nil {
		t.Fatalf("write first prompt: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), "approval rejected (expired): exec@v1") {
		goruntime.Gosched()
	}
	if !strings.Contains(out.String(), "approval rejected (expired): exec@v1") {
		t.Fatalf("output = %q, want expired rejection", out.String())
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := pw.Write([]byte("y\nsecond\n"))
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write late input: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late input was not consumed")
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("close input: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("console did not stop")
	}
	got := runner.promptList()
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("prompts = %v, want [first second]", got)
	}
}

func TestApprovalCardSanitizesInjectedSequences(t *testing.T) {
	card := testCard()
	card.Arguments = `{"command":["\u001b[31mevil\u0007"]}`
	rendered := renderApprovalCard(card, 80)
	if strings.ContainsAny(rendered, "\x1b\x07") {
		t.Errorf("card = %q, want control sequences stripped", rendered)
	}
	if !strings.Contains(rendered, "arguments:") {
		t.Errorf("card = %q, want arguments section", rendered)
	}
}

func TestApprovalAcceptedTokens(t *testing.T) {
	for _, line := range []string{"y", "Y", "yes", "YES", " y ", "y\t"} {
		if !approvalAccepted(line) {
			t.Errorf("approvalAccepted(%q) = false, want true", line)
		}
	}
	for _, line := range []string{"", "n", "N", "no", "yeah", "approve", "1"} {
		if approvalAccepted(line) {
			t.Errorf("approvalAccepted(%q) = true, want false", line)
		}
	}
}

func TestApprovalBridgeWithoutAskerIsIdle(t *testing.T) {
	bridge := NewApprovalBridge()
	select {
	case <-bridge.readyCh():
		t.Fatal("fresh bridge must not signal readiness")
	default:
	}
	if ask := bridge.take(); ask != nil {
		t.Fatalf("take = %+v, want nil", ask)
	}
}

func TestApprovalBridgeSecondAskFailsClosed(t *testing.T) {
	bridge := NewApprovalBridge()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := bridge.Decide(ctx, testCard()); err == nil {
			t.Error("cancelled ask must return an error")
		}
	}()
	<-bridge.readyCh()
	if _, err := bridge.Decide(context.Background(), testCard()); err == nil {
		t.Fatal("second concurrent ask must fail")
	}
	cancel()
	<-done
}

func TestApprovalBridgeSecondAskFailsClosedWhileServing(t *testing.T) {
	bridge := NewApprovalBridge()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() {
		_, err := bridge.Decide(ctx, testCard())
		first <- err
	}()
	<-bridge.readyCh()
	ask := bridge.take()
	if ask == nil {
		t.Fatal("take = nil, want the pending ask")
	}
	if again := bridge.take(); again != nil {
		t.Fatal("take must hand the ask to the renderer exactly once")
	}
	secondErr := make(chan error, 1)
	go func() {
		_, err := bridge.Decide(context.Background(), testCard())
		secondErr <- err
	}()
	select {
	case err := <-secondErr:
		if err == nil {
			t.Fatal("second concurrent ask must fail while the first is being served")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second concurrent ask blocked instead of failing closed")
	}
	ask.answer(true)
	if err := <-first; err != nil {
		t.Fatalf("first ask ended with error: %v", err)
	}
	second := make(chan error, 1)
	secondCtx, secondCancel := context.WithCancel(context.Background())
	go func() {
		_, err := bridge.Decide(secondCtx, testCard())
		second <- err
	}()
	<-bridge.readyCh()
	secondCancel()
	if err := <-second; err == nil {
		t.Fatal("cancelled second ask must return an error")
	}
}
