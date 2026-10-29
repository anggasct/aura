package principal

import (
	"errors"
	"fmt"
	"strings"
)

type Class string

const (
	ClassOwner   Class = "owner"
	ClassTrusted Class = "trusted"
	ClassPublic  Class = "public"
)

const OwnerID = "local-owner"

const ChannelDiscord = "discord"

type ErrorCode string

const ErrorCodeIdentityInvalid ErrorCode = "owner_identity_invalid"

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

// ParseClass fails closed: anything that is not a known class, including the
// empty value carried by turns recorded before classes existed, is trusted.
func ParseClass(raw string) Class {
	switch Class(raw) {
	case ClassOwner, ClassTrusted, ClassPublic:
		return Class(raw)
	default:
		return ClassTrusted
	}
}

func ClassForPrincipal(id string) Class {
	if id == OwnerID {
		return ClassOwner
	}
	return ClassTrusted
}

func Namespaced(channel, externalID string) string {
	return channel + ":" + externalID
}

func ParseIdentity(raw string) (channel, externalID string, err error) {
	channel, externalID, found := strings.Cut(raw, ":")
	if !found {
		return "", "", &Error{Code: ErrorCodeIdentityInvalid, Detail: "identity must be <channel>:<id>"}
	}
	switch channel {
	case ChannelDiscord:
		if !isSnowflake(externalID) {
			return "", "", &Error{Code: ErrorCodeIdentityInvalid, Detail: "discord identity must be a numeric user id"}
		}
	default:
		return "", "", &Error{Code: ErrorCodeIdentityInvalid, Detail: "identity channel is not supported"}
	}
	return channel, externalID, nil
}

func isSnowflake(id string) bool {
	if id == "" || len(id) > 20 {
		return false
	}
	for i := range len(id) {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

type Resolver struct {
	owners map[string]struct{}
}

func NewResolver(identities []string) (*Resolver, error) {
	r := &Resolver{owners: make(map[string]struct{}, len(identities))}
	var problems []error
	for i, raw := range identities {
		channel, externalID, err := ParseIdentity(raw)
		if err != nil {
			problems = append(problems, fmt.Errorf("owner.identities[%d]: %w", i, err))
			continue
		}
		key := Namespaced(channel, externalID)
		if _, dup := r.owners[key]; dup {
			problems = append(problems, fmt.Errorf("owner.identities[%d]: %w", i, &Error{Code: ErrorCodeIdentityInvalid, Detail: "identity is listed more than once"}))
			continue
		}
		r.owners[key] = struct{}{}
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Resolver) IsOwnerIdentity(channel, externalID string) bool {
	if r == nil {
		return false
	}
	_, ok := r.owners[Namespaced(channel, externalID)]
	return ok
}

func (r *Resolver) Count(channel string) int {
	if r == nil {
		return 0
	}
	prefix := channel + ":"
	n := 0
	for key := range r.owners {
		if strings.HasPrefix(key, prefix) {
			n++
		}
	}
	return n
}

func (r *Resolver) Resolve(channel, externalID string, allowlisted bool) (id string, class Class, ok bool) {
	if externalID == "" {
		return "", "", false
	}
	if r.IsOwnerIdentity(channel, externalID) {
		return OwnerID, ClassOwner, true
	}
	if allowlisted {
		return Namespaced(channel, externalID), ClassTrusted, true
	}
	return "", "", false
}
