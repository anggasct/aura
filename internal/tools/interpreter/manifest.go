package interpreter

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	PlaceholderExecutable = "{executable}"
	PlaceholderSource     = "{source}"

	maxManifestBytes     = 64 << 10
	maxManifestRuntimes  = 8
	maxArgvEntries       = 32
	maxArgvEntryLength   = 256
	maxEnvironmentKeys   = 32
	maxEnvironmentValue  = 1024
	maxExecutablePathLen = 256
)

//go:embed manifest.json
var embeddedManifest []byte

var (
	runtimeIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	versionPattern      = regexp.MustCompile(`^\d[0-9A-Za-z.+_-]{0,63}$`)
	archiveNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}\.tar\.gz$`)
	sha256HexPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	executablePattern   = regexp.MustCompile(`^[A-Za-z0-9._+-]+(?:/[A-Za-z0-9._+-]+)*$`)
	environmentKeyRegex = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

type Runtime struct {
	ID          string            `json:"id"`
	Version     string            `json:"version"`
	Archive     string            `json:"archive"`
	SHA256      string            `json:"sha256"`
	Executable  string            `json:"executable"`
	Argv        []string          `json:"argv"`
	Environment map[string]string `json:"environment"`
}

func (r *Runtime) clone() Runtime {
	out := *r
	out.Argv = slices.Clone(r.Argv)
	out.Environment = maps.Clone(r.Environment)
	return out
}

type manifestFile struct {
	Runtimes []Runtime `json:"runtimes"`
}

type Manifest struct {
	runtimes []Runtime
}

func DefaultManifest() (*Manifest, error) {
	return ParseManifest(embeddedManifest)
}

func ParseManifest(data []byte) (*Manifest, error) {
	if len(data) > maxManifestBytes {
		return nil, Errorf(ErrorCodeManifestInvalid, "manifest exceeds %d bytes", maxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file manifestFile
	if err := decoder.Decode(&file); err != nil {
		return nil, Errorf(ErrorCodeManifestInvalid, "manifest is not valid: %v", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, Errorf(ErrorCodeManifestInvalid, "manifest has trailing content")
	}
	if len(file.Runtimes) == 0 || len(file.Runtimes) > maxManifestRuntimes {
		return nil, Errorf(ErrorCodeManifestInvalid, "manifest must list between 1 and %d runtimes", maxManifestRuntimes)
	}
	seen := make(map[string]struct{}, len(file.Runtimes))
	var problems []error
	for i := range file.Runtimes {
		runtime := &file.Runtimes[i]
		if _, duplicate := seen[runtime.ID]; duplicate {
			problems = append(problems, fmt.Errorf("runtime %q is listed twice", runtime.ID))
		}
		seen[runtime.ID] = struct{}{}
		problems = append(problems, validateRuntime(runtime)...)
	}
	if err := errors.Join(problems...); err != nil {
		return nil, &Error{Code: ErrorCodeManifestInvalid, Detail: err.Error()}
	}
	return &Manifest{runtimes: file.Runtimes}, nil
}

func (m *Manifest) Runtimes() []Runtime {
	out := make([]Runtime, 0, len(m.runtimes))
	for i := range m.runtimes {
		out = append(out, m.runtimes[i].clone())
	}
	return out
}

func (m *Manifest) Lookup(id string) (Runtime, bool) {
	for i := range m.runtimes {
		if m.runtimes[i].ID == id {
			return m.runtimes[i].clone(), true
		}
	}
	return Runtime{}, false
}

func (m *Manifest) IDs() []string {
	ids := make([]string, 0, len(m.runtimes))
	for i := range m.runtimes {
		ids = append(ids, m.runtimes[i].ID)
	}
	return ids
}

func validateRuntime(runtime *Runtime) []error {
	var problems []error
	label := fmt.Sprintf("runtime %q", runtime.ID)
	if !runtimeIDPattern.MatchString(runtime.ID) {
		problems = append(problems, errors.New("runtime id is not a plain lowercase name"))
	}
	if !versionPattern.MatchString(runtime.Version) {
		problems = append(problems, fmt.Errorf("%s version is not a plain version string", label))
	}
	if !archiveNamePattern.MatchString(runtime.Archive) {
		problems = append(problems, fmt.Errorf("%s archive is not a plain .tar.gz name", label))
	}
	if !sha256HexPattern.MatchString(runtime.SHA256) {
		problems = append(problems, fmt.Errorf("%s sha256 must be 64 lowercase hex characters", label))
	}
	if !validExecutablePath(runtime.Executable) {
		problems = append(problems, fmt.Errorf("%s executable must be a clean relative path", label))
	}
	problems = append(problems, validateArgv(label, runtime.Argv)...)
	problems = append(problems, validateEnvironment(label, runtime.Environment)...)
	return problems
}

func validExecutablePath(path string) bool {
	return len(path) <= maxExecutablePathLen && fs.ValidPath(path) && path != "." && executablePattern.MatchString(path)
}

func validateArgv(label string, argv []string) []error {
	if len(argv) == 0 || len(argv) > maxArgvEntries {
		return []error{fmt.Errorf("%s argv must have between 1 and %d entries", label, maxArgvEntries)}
	}
	var problems []error
	if argv[0] != PlaceholderExecutable {
		problems = append(problems, fmt.Errorf("%s argv must start with %s", label, PlaceholderExecutable))
	}
	sources := 0
	for _, entry := range argv {
		switch {
		case entry == "" || len(entry) > maxArgvEntryLength || !printable(entry):
			problems = append(problems, fmt.Errorf("%s argv entries must be short printable text", label))
		case entry == PlaceholderSource:
			sources++
		case entry == PlaceholderExecutable:
		case strings.ContainsAny(entry, "{}"):
			problems = append(problems, fmt.Errorf("%s argv entry carries an unknown placeholder", label))
		}
	}
	if sources != 1 {
		problems = append(problems, fmt.Errorf("%s argv must contain %s exactly once", label, PlaceholderSource))
	}
	if slices.Index(argv, PlaceholderExecutable) != 0 || slices.Index(argv[1:], PlaceholderExecutable) >= 0 {
		problems = append(problems, fmt.Errorf("%s argv must contain %s only first", label, PlaceholderExecutable))
	}
	return problems
}

func validateEnvironment(label string, environment map[string]string) []error {
	if len(environment) > maxEnvironmentKeys {
		return []error{fmt.Errorf("%s environment has more than %d entries", label, maxEnvironmentKeys)}
	}
	var problems []error
	for _, key := range slices.Sorted(maps.Keys(environment)) {
		value := environment[key]
		switch {
		case !environmentKeyRegex.MatchString(key):
			problems = append(problems, fmt.Errorf("%s environment key %q is not a plain upper-case name", label, key))
		case key == "PATH" || strings.HasPrefix(key, "LD_"):
			problems = append(problems, fmt.Errorf("%s environment must not set %s", label, key))
		case len(value) > maxEnvironmentValue || !printable(value):
			problems = append(problems, fmt.Errorf("%s environment value for %s must be short printable text", label, key))
		}
	}
	return problems
}

func printable(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
