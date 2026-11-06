package interpreter

import "context"

type Verifier struct {
	store *Store
	set   *VerifiedSet
}

func NewVerifier(store *Store, set *VerifiedSet) (*Verifier, error) {
	if store == nil || set == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "store and verified set are required")
	}
	return &Verifier{store: store, set: set}, nil
}

func (v *Verifier) Run(ctx context.Context) error {
	for _, id := range v.store.Manifest().IDs() {
		verification, err := v.store.Verify(ctx, id)
		if err != nil {
			return err
		}
		v.set.record(verification)
	}
	return nil
}
