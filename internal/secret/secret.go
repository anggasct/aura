package secret

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// withoutPath strips the filesystem path from an *fs.PathError so an error
// string keeps the reason without disclosing where the file lives.
func withoutPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

type ErrorCode string

const (
	ErrorCodeScopeViolation ErrorCode = "secret_scope_violation"
)

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

func Errorf(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Reference points at where a secret value lives — an environment variable
// or a file — never at the value itself. Configuration carries references;
// resolution happens only inside the owning adapter.
type Reference struct {
	Env  string
	File string
}

// Resolve reads the referenced value. It must be called only by the
// adapter that owns the secret.
func (r Reference) Resolve() (string, error) {
	switch {
	case r.Env != "" && r.File != "":
		return "", Errorf(ErrorCodeScopeViolation, "secret reference has both env and file sources")
	case r.Env != "":
		value, ok := os.LookupEnv(r.Env)
		if !ok {
			return "", Errorf(ErrorCodeScopeViolation, "secret environment variable %q is unavailable", r.Env)
		}
		return value, nil
	case r.File != "":
		value, err := os.ReadFile(r.File)
		if err != nil {
			return "", Errorf(ErrorCodeScopeViolation, "cannot read secret file %q: %v", filepath.Base(r.File), withoutPath(err))
		}
		secret := strings.TrimRight(string(value), "\r\n")
		if secret == "" {
			return "", Errorf(ErrorCodeScopeViolation, "secret file is empty")
		}
		return secret, nil
	default:
		return "", Errorf(ErrorCodeScopeViolation, "secret reference has no source")
	}
}

const placeholder = "[redacted]"

// Vault holds the secrets of one adapter. Raw values are returned only to
// the adapter itself; every other path — prompts, logs, traces, errors,
// artifacts, child environments — goes through Redact.
type Vault struct {
	mu      sync.RWMutex
	values  map[string]string
	ordered []string
}

func New() *Vault {
	return &Vault{values: make(map[string]string)}
}

// Set registers a resolved value under a name. Called by the owning
// adapter after resolving a Reference.
func (v *Vault) Set(name, value string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.values[name]; !ok {
		v.ordered = append(v.ordered, name)
	}
	v.values[name] = value
}

// Raw returns the value for the owning adapter. Callers outside the
// adapter must use Redact instead.
func (v *Vault) Raw(name string) string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.values[name]
}

// Redact replaces every registered secret in text with a placeholder, so
// values can never reach prompts, logs, traces, errors, artifacts, or
// child environments.
func (v *Vault) Redact(text string) string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, name := range v.ordered {
		text = strings.ReplaceAll(text, v.values[name], placeholder)
	}
	return text
}

// Contains reports whether any registered secret value appears in text.
// Used by canary checks to prove redaction covered every channel.
func (v *Vault) Contains(text string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, name := range v.ordered {
		if strings.Contains(text, v.values[name]) {
			return true
		}
	}
	return false
}
