package approval

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/principal"
)

const (
	CallbackPrefix = "aura"
	tokenBytes     = 8
	macBytes       = 8

	ActionApprove  = "a"
	ActionReject   = "r"
	ActionStanding = "s"
)

const (
	MaxPending   = 1024
	MaxSeen      = 4096
	FallbackTTL  = 5 * time.Minute
	CardArgRunes = 1200
)

const (
	RoutedApproved    = "approved"
	RoutedStanding    = "standing"
	RoutedRejected    = "rejected"
	RoutedTimeout     = "timeout"
	RoutedUnavailable = "unavailable"
)

type Prompt struct {
	ToolName       string
	ToolVersion    string
	SessionID      string
	TurnID         string
	PrincipalID    string
	Arguments      string
	Network        bool
	Timeout        time.Duration
	MaxOutputBytes int64
	PolicyVersion  string
	ReasonCode     string
	ExpiresAt      time.Time
}

type Requester struct {
	Principal string
	Class     principal.Class
	Persona   string
}

type Binding struct {
	Principal string
	Class     principal.Class
	Persona   string
	Actor     string
	Scope     [2]string
	Standing  bool
}

type Presser struct {
	Actor string
	Scope [2]string
}

type Outcome struct {
	Accepted bool
	Standing bool
}

type Pending struct {
	binding   Binding
	expiresAt time.Time
	decided   bool
	result    chan Outcome
}

func (p *Pending) Binding() Binding {
	return p.binding
}

func (p *Pending) ExpiresAt() time.Time {
	return p.expiresAt
}

type Callbacks struct {
	Approve  string
	Reject   string
	Standing string
}

type Registry struct {
	key     [32]byte
	mu      sync.Mutex
	pending map[string]*Pending
	seen    map[string]struct{}
}

func NewRegistry() (*Registry, error) {
	registry := &Registry{pending: map[string]*Pending{}, seen: map[string]struct{}{}}
	if _, err := rand.Read(registry.key[:]); err != nil {
		return nil, ErrorWrapf(ErrorCodeEntropyUnavailable, err, "approval token key is unavailable")
	}
	return registry, nil
}

func (r *Registry) Issue(binding *Binding, expiresAt time.Time) (tokenID string, pending *Pending, err error) {
	if r == nil || binding == nil {
		return "", nil, Errorf(ErrorCodeInvalidArgument, "approval registry and binding must not be nil")
	}
	var raw [tokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, ErrorWrapf(ErrorCodeEntropyUnavailable, err, "approval token mint failed")
	}
	tokenID = hex.EncodeToString(raw[:])
	pending = &Pending{binding: *binding, expiresAt: expiresAt, result: make(chan Outcome, 1)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) >= MaxPending {
		for drop := range r.pending {
			delete(r.pending, drop)
			break
		}
	}
	r.pending[tokenID] = pending
	return tokenID, pending, nil
}

func (r *Registry) Drop(tokenID string, pending *Pending) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.pending[tokenID]; ok && current == pending {
		delete(r.pending, tokenID)
	}
}

func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

func (r *Registry) NoteSeen(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.seen[id]; ok {
		return false
	}
	if len(r.seen) >= MaxSeen {
		for drop := range r.seen {
			delete(r.seen, drop)
			break
		}
	}
	r.seen[id] = struct{}{}
	return true
}

func (r *Registry) mac(tokenID, action string, binding *Binding, expiryUnix int64) string {
	mac := hmac.New(sha256.New, r.key[:])
	_, _ = mac.Write([]byte(tokenID + "|" + action + "|" + binding.Actor + "|" + binding.Scope[0] + "|" + binding.Scope[1] + "|" + strconv.FormatInt(expiryUnix, 10)))
	return hex.EncodeToString(mac.Sum(nil)[:macBytes])
}

func (r *Registry) Callbacks(tokenID string, binding *Binding, expiresAt time.Time) Callbacks {
	expiry := expiresAt.Unix()
	callbacks := Callbacks{
		Approve: CallbackData(tokenID, ActionApprove, r.mac(tokenID, ActionApprove, binding, expiry)),
		Reject:  CallbackData(tokenID, ActionReject, r.mac(tokenID, ActionReject, binding, expiry)),
	}
	if binding.Standing {
		callbacks.Standing = CallbackData(tokenID, ActionStanding, r.mac(tokenID, ActionStanding, binding, expiry))
	}
	return callbacks
}

func CallbackData(tokenID, action, mac string) string {
	return CallbackPrefix + ":" + tokenID + ":" + action + ":" + mac
}

func ParseCallbackData(raw string) (tokenID, action, mac string, ok bool) {
	parts := strings.Split(raw, ":")
	if len(parts) != 4 || parts[0] != CallbackPrefix {
		return "", "", "", false
	}
	tokenID, action, mac = parts[1], parts[2], parts[3]
	if len(tokenID) != tokenBytes*2 || len(mac) != macBytes*2 {
		return "", "", "", false
	}
	if action != ActionApprove && action != ActionReject && action != ActionStanding {
		return "", "", "", false
	}
	if !isHex(tokenID) || !isHex(mac) {
		return "", "", "", false
	}
	return tokenID, action, mac, true
}

func isHex(value string) bool {
	for i := range len(value) {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (r *Registry) Resolve(data string, presser *Presser) bool {
	if r == nil || presser == nil {
		return false
	}
	tokenID, action, mac, ok := ParseCallbackData(data)
	if !ok {
		return false
	}
	r.mu.Lock()
	pending, found := r.pending[tokenID]
	if !found {
		r.mu.Unlock()
		return false
	}
	if pending.decided || !time.Now().Before(pending.expiresAt) {
		if !pending.decided {
			delete(r.pending, tokenID)
		}
		r.mu.Unlock()
		return false
	}
	binding := pending.binding
	expected := r.mac(tokenID, action, &binding, pending.expiresAt.Unix())
	r.mu.Unlock()
	if subtle.ConstantTimeCompare([]byte(mac), []byte(expected)) != 1 {
		return false
	}
	if presser.Actor != binding.Actor || !scopeMatches(&binding, presser) {
		return false
	}
	if action == ActionStanding && !binding.Standing {
		return false
	}
	r.mu.Lock()
	if pending.decided {
		r.mu.Unlock()
		return false
	}
	pending.decided = true
	r.mu.Unlock()
	pending.result <- Outcome{Accepted: action == ActionApprove || action == ActionStanding, Standing: action == ActionStanding}
	return true
}

func scopeMatches(binding *Binding, presser *Presser) bool {
	for i := range binding.Scope {
		if binding.Scope[i] != "" && binding.Scope[i] != presser.Scope[i] {
			return false
		}
	}
	return true
}

func (r *Registry) Await(ctx context.Context, tokenID string, pending *Pending) (outcome Outcome, timedOut bool, err error) {
	if r == nil || pending == nil {
		return Outcome{}, false, Errorf(ErrorCodeInvalidArgument, "approval registry and pending approval must not be nil")
	}
	timer := time.NewTimer(time.Until(pending.expiresAt))
	defer timer.Stop()
	defer r.Drop(tokenID, pending)
	select {
	case outcome = <-pending.result:
		return outcome, false, nil
	case <-timer.C:
		return Outcome{}, true, nil
	case <-ctx.Done():
		return Outcome{}, false, ctx.Err()
	}
}

type StandingGrantRecorder interface {
	RecordStandingGrant(ctx context.Context, grant *StandingGrantRequest) error
}

type StandingGrantRequest struct {
	PrincipalID string
	AgentID     string
	ToolName    string
	OwnerID     string
	TTL         time.Duration
}

func OffersStandingGrant(class principal.Class, grants StandingGrantRecorder, ttl time.Duration) bool {
	return class == principal.ClassTrusted && grants != nil && ttl > 0
}

func StandingGrantLabel(ttl time.Duration) string {
	switch {
	case ttl == 24*time.Hour:
		return "Allow 1 day"
	case ttl%(24*time.Hour) == 0:
		return fmt.Sprintf("Allow %d days", ttl/(24*time.Hour))
	case ttl%time.Hour == 0:
		return fmt.Sprintf("Allow %d hours", ttl/time.Hour)
	default:
		return "Allow " + ttl.String()
	}
}

func OwnerTimeout(persona, configured time.Duration) time.Duration {
	return cmp.Or(persona, configured, FallbackTTL)
}

func PersonaLabel(id, name string) string {
	if name == "" || name == id {
		return id
	}
	return name + " (" + id + ")"
}

func RenderCard(prompt *Prompt, persona string, clean func(string) string) string {
	if prompt == nil {
		return ""
	}
	if clean == nil {
		clean = func(text string) string { return text }
	}
	arguments := clean(strings.TrimSpace(prompt.Arguments))
	if arguments == "" {
		arguments = "{}"
	}
	if runes := []rune(arguments); len(runes) > CardArgRunes {
		arguments = string(runes[:CardArgRunes]) + "...[truncated]"
	}
	var card strings.Builder
	card.WriteString("approval required: " + clean(prompt.ToolName))
	if prompt.ToolVersion != "" {
		card.WriteString("@" + clean(prompt.ToolVersion))
	}
	if persona != "" {
		card.WriteString("\npersona: " + clean(persona))
	}
	card.WriteString("\nsession: " + clean(prompt.SessionID))
	card.WriteString("\nprincipal: " + clean(prompt.PrincipalID))
	card.WriteString("\narguments: " + arguments)
	card.WriteString("\napprove exactly this request?")
	return card.String()
}
