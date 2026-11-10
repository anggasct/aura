package interpreter

import (
	"context"
	"errors"
)

var ErrProbeUnsupported = errors.New("runtime has no version probe")

type Prober interface {
	Probe(ctx context.Context, bundle *Bundle) error
}

type VerifierOption func(*Verifier)

func WithProber(prober Prober) VerifierOption {
	return func(v *Verifier) { v.prober = prober }
}

type Verifier struct {
	store  *Store
	set    *VerifiedSet
	prober Prober
}

func NewVerifier(store *Store, set *VerifiedSet, options ...VerifierOption) (*Verifier, error) {
	if store == nil || set == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "store and verified set are required")
	}
	verifier := &Verifier{store: store, set: set}
	for _, apply := range options {
		apply(verifier)
	}
	return verifier, nil
}

func (v *Verifier) Run(ctx context.Context) error {
	for _, id := range v.store.Manifest().IDs() {
		verification, err := v.store.Verify(ctx, id)
		if err != nil {
			return err
		}
		if verification.Verified() && v.prober != nil {
			verification, err = v.probe(ctx, id, verification)
			if err != nil {
				return err
			}
		}
		v.set.record(verification)
	}
	return nil
}

func (v *Verifier) probe(ctx context.Context, id string, verification Verification) (Verification, error) {
	bundle, err := v.store.Bundle(id)
	if err != nil {
		return Verification{}, err
	}
	probeErr := v.prober.Probe(ctx, &bundle)
	switch {
	case probeErr == nil, errors.Is(probeErr, ErrProbeUnsupported):
		return verification, nil
	case ctx.Err() != nil:
		return Verification{}, ctx.Err()
	default:
		verification.State, verification.Reason = StateInvalid, ReasonProbeFailed
		return verification, nil
	}
}
