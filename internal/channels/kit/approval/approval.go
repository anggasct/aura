package approval

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	policy "github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	CallbackPrefix = "aura"
	tokenBytes     = 8
	macBytes       = 8

	ActionApprove  = "a"
	ActionReject   = "r"
	ActionStanding = "s"
	ActionStop     = "x"
)

const (
	MaxPending  = 1024
	MaxSeen     = 4096
	FallbackTTL = 5 * time.Minute
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
	TaintSource    string
	ExpiresAt      time.Time

	StandingGrantAllowed bool
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
	SessionID string
	TurnID    string
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
		return nil, fault.Wrap(ErrorCodeEntropyUnavailable, "approval token key is unavailable", err)
	}
	return registry, nil
}

func (r *Registry) Issue(binding *Binding, expiresAt time.Time) (tokenID string, pending *Pending, err error) {
	if r == nil || binding == nil {
		return "", nil, fault.New(ErrorCodeInvalidArgument, "approval registry and binding must not be nil")
	}
	var raw [tokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, fault.Wrap(ErrorCodeEntropyUnavailable, "approval token mint failed", err)
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

func (r *Registry) Callback(tokenID, action string, binding *Binding, expiresAt time.Time) string {
	return CallbackData(tokenID, action, r.mac(tokenID, action, binding, expiresAt.Unix()))
}

func (r *Registry) Callbacks(tokenID string, binding *Binding, expiresAt time.Time) Callbacks {
	callbacks := Callbacks{
		Approve: r.Callback(tokenID, ActionApprove, binding, expiresAt),
		Reject:  r.Callback(tokenID, ActionReject, binding, expiresAt),
	}
	if binding.Standing {
		callbacks.Standing = r.Callback(tokenID, ActionStanding, binding, expiresAt)
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
	if action != ActionApprove && action != ActionReject && action != ActionStanding && action != ActionStop {
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
	_, _, ok := r.resolve(data, presser, false)
	return ok
}

func (r *Registry) ResolveAction(data string, presser *Presser) (action string, binding Binding, ok bool) {
	return r.resolve(data, presser, true)
}

func (r *Registry) resolve(data string, presser *Presser, allowStop bool) (action string, binding Binding, ok bool) {
	if r == nil || presser == nil {
		return "", Binding{}, false
	}
	tokenID, parsedAction, mac, valid := ParseCallbackData(data)
	if !valid || (!allowStop && parsedAction == ActionStop) {
		return "", Binding{}, false
	}
	r.mu.Lock()
	pending, found := r.pending[tokenID]
	if !found {
		r.mu.Unlock()
		return "", Binding{}, false
	}
	if pending.decided || !time.Now().Before(pending.expiresAt) {
		if !pending.decided {
			delete(r.pending, tokenID)
		}
		r.mu.Unlock()
		return "", Binding{}, false
	}
	binding = pending.binding
	expected := r.mac(tokenID, parsedAction, &binding, pending.expiresAt.Unix())
	r.mu.Unlock()
	if subtle.ConstantTimeCompare([]byte(mac), []byte(expected)) != 1 {
		return "", Binding{}, false
	}
	if presser.Actor != binding.Actor || !scopeMatches(&binding, presser) {
		return "", Binding{}, false
	}
	if parsedAction == ActionStanding && !binding.Standing {
		return "", Binding{}, false
	}
	r.mu.Lock()
	if pending.decided {
		r.mu.Unlock()
		return "", Binding{}, false
	}
	pending.decided = true
	if parsedAction == ActionStop {
		delete(r.pending, tokenID)
	}
	r.mu.Unlock()
	if parsedAction != ActionStop {
		pending.result <- Outcome{Accepted: parsedAction == ActionApprove || parsedAction == ActionStanding, Standing: parsedAction == ActionStanding}
	}
	return parsedAction, binding, true
}

func scopeMatches(binding *Binding, presser *Presser) bool {
	return binding.Scope == presser.Scope
}

func (r *Registry) Await(ctx context.Context, tokenID string, pending *Pending) (outcome Outcome, timedOut bool, err error) {
	if r == nil || pending == nil {
		return Outcome{}, false, fault.New(ErrorCodeInvalidArgument, "approval registry and pending approval must not be nil")
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

type StandingGrantRequest = principal.StandingGrantRequest

type StandingOffer struct {
	Grants StandingGrantRecorder
	TTL    time.Duration
}

func OffersStandingGrant(prompt *Prompt, class principal.Class, offer StandingOffer) bool {
	return prompt != nil &&
		prompt.ReasonCode != policy.ReasonUntrustedInput &&
		prompt.ReasonCode != policy.ReasonUntrustedTrustGrant &&
		class == principal.ClassTrusted &&
		offer.Grants != nil &&
		offer.TTL > 0 &&
		prompt.StandingGrantAllowed
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
