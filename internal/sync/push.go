package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/anggasct/aura/internal/effect"
)

const (
	PushProvider  = "git-sync"
	PushOperation = "push"
)

type PushRequest struct {
	Remote      string `json:"remote"`
	Branch      string `json:"branch"`
	Digest      string `json:"digest"`
	IntendedRef string `json:"intended_ref,omitempty"`
}

type PushReceipt struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	Digest string `json:"digest"`
	Ref    string `json:"ref"`
}

type Runner interface {
	Execute(ctx context.Context, req *effect.PrepareRequest, provider effect.Provider) (*effect.Intent, error)
	Reconcile(ctx context.Context, id string, provider effect.Provider, reconciler effect.Reconciler) (*effect.Intent, error)
}

type ObserveFunc func(ctx context.Context, remote, branch string) (string, error)

type PushProviderAdapter struct {
	Observe ObserveFunc
}

func (p *PushProviderAdapter) SupportsIdempotency() bool {
	return true
}

func (p *PushProviderAdapter) Invoke(ctx context.Context, inv *effect.Invocation) (effect.Outcome, error) {
	if ctx == nil || inv == nil {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "invocation must not be nil")
	}
	var request PushRequest
	if err := json.Unmarshal(inv.Request, &request); err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "push request is not decodable")
	}
	if strings.TrimSpace(request.Remote) == "" || strings.TrimSpace(request.Branch) == "" || strings.TrimSpace(request.Digest) == "" {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "push request is incomplete")
	}
	if strings.TrimSpace(request.IntendedRef) == "" {
		return effect.Outcome{Ambiguous: true}, nil
	}
	if p.Observe == nil {
		return effect.Outcome{Ambiguous: true}, nil
	}
	ref, err := p.Observe(ctx, request.Remote, request.Branch)
	if err != nil {
		return effect.Outcome{}, err
	}
	if ref == "" || ref != request.IntendedRef {
		return effect.Outcome{Ambiguous: true}, nil
	}
	receipt, err := json.Marshal(PushReceipt{Remote: request.Remote, Branch: request.Branch, Digest: request.Digest, Ref: ref})
	if err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "push receipt is not serializable")
	}
	return effect.Outcome{Succeeded: true, Receipt: receipt}, nil
}

type PushReconciler struct {
	Observe ObserveFunc
}

func (r *PushReconciler) Reconcile(ctx context.Context, intent *effect.Intent) (effect.Evidence, error) {
	if ctx == nil || intent == nil {
		return effect.Evidence{}, Errorf(ErrorCodeInvalidArgument, "intent must not be nil")
	}
	var request PushRequest
	if err := json.Unmarshal(intent.RequestJSON, &request); err != nil {
		return effect.Evidence{}, Errorf(ErrorCodeInvalidArgument, "push request is not decodable")
	}
	if strings.TrimSpace(request.IntendedRef) == "" {
		return effect.Evidence{}, nil
	}
	if r.Observe == nil {
		return effect.Evidence{}, nil
	}
	ref, err := r.Observe(ctx, request.Remote, request.Branch)
	if err != nil {
		return effect.Evidence{}, err
	}
	if ref == "" || ref != request.IntendedRef {
		return effect.Evidence{}, nil
	}
	var prior PushReceipt
	if len(intent.ProviderReceipt) > 0 {
		if err := json.Unmarshal(intent.ProviderReceipt, &prior); err == nil && prior.Digest == request.Digest && prior.Ref == ref {
			return effect.Evidence{Definitive: true, Succeeded: true, Receipt: intent.ProviderReceipt}, nil
		}
	}
	receipt, err := json.Marshal(PushReceipt{Remote: request.Remote, Branch: request.Branch, Digest: request.Digest, Ref: ref})
	if err != nil {
		return effect.Evidence{}, Errorf(ErrorCodeInvalidArgument, "push receipt is not serializable")
	}
	return effect.Evidence{Definitive: true, Succeeded: true, Receipt: receipt}, nil
}

func PushIntentsKey(remote, digest, branch string) string {
	normalized := strings.TrimSpace(remote)
	sum := sha256.Sum256([]byte("sync-push/v1\n" + normalized + "\x00" + digest + "\x00" + branch))
	return "sync-push:" + branch + ":" + hex.EncodeToString(sum[:])[:32]
}

func PushRequestJSON(remote, branch, digest, intendedRef string) ([]byte, error) {
	raw, err := json.Marshal(PushRequest{Remote: remote, Branch: branch, Digest: digest, IntendedRef: intendedRef})
	if err != nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "push request is not serializable")
	}
	return raw, nil
}

func StartPush(ctx context.Context, runner Runner, sessionID, remote, branch, digest, intendedRef, turnID, toolCallID string, sequence uint64, observe ObserveFunc) (*effect.Intent, error) {
	if ctx == nil {
		return nil, errNilArgument("ctx")
	}
	if runner == nil {
		return nil, errNilArgument("runner")
	}
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(remote) == "" || strings.TrimSpace(branch) == "" || strings.TrimSpace(digest) == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "push identity must not be empty")
	}
	request, err := PushRequestJSON(remote, branch, digest, intendedRef)
	if err != nil {
		return nil, err
	}
	return runner.Execute(ctx, &effect.PrepareRequest{
		SessionID:      sessionID,
		TurnID:         turnID,
		ToolCallID:     toolCallID,
		IdempotencyKey: PushIntentsKey(remote, digest, branch),
		Provider:       PushProvider,
		Operation:      PushOperation,
		Classification: effect.ClassificationIdempotent,
		Request:        request,
		EventKind:      effect.EventKindToolRequested,
		EventSequence:  sequence,
	}, &PushProviderAdapter{Observe: observe})
}

func ReconcilePush(ctx context.Context, runner Runner, id string, observe ObserveFunc) (*effect.Intent, error) {
	if ctx == nil {
		return nil, errNilArgument("ctx")
	}
	if runner == nil {
		return nil, errNilArgument("runner")
	}
	if strings.TrimSpace(id) == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "intent id must not be empty")
	}
	return runner.Reconcile(ctx, id, &PushProviderAdapter{Observe: observe}, &PushReconciler{Observe: observe})
}
