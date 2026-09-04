package workflow

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type specFile struct {
	ID      string          `yaml:"id"`
	Version int             `yaml:"version"`
	Goal    string          `yaml:"goal"`
	Source  string          `yaml:"source"`
	Steps   []stepFileEntry `yaml:"steps"`
}

type stepFileEntry struct {
	ID        string        `yaml:"id"`
	DependsOn []string      `yaml:"depends_on"`
	Condition *string       `yaml:"condition"`
	Timeout   time.Duration `yaml:"timeout"`
	Executor  executorFile  `yaml:"executor"`
	Retry     retryFile     `yaml:"retry"`
}

type executorFile struct {
	Kind     string   `yaml:"kind"`
	AgentID  *string  `yaml:"agent_id"`
	Requires []string `yaml:"requires"`
	ToolID   *string  `yaml:"tool"`
	Event    *string  `yaml:"event"`
}

type retryFile struct {
	Attempts int           `yaml:"attempts"`
	Backoff  time.Duration `yaml:"backoff"`
}

// maxDefinitionBytes bounds one definition file read.
const maxDefinitionBytes = 1 << 20

// LoadSpecFile parses one definition file with strict unknown-key
// rejection. The read is confined to the file's own directory.
func LoadSpecFile(path string) (*Spec, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, "open definitions root: "+err.Error())
	}
	defer func() { _ = root.Close() }()
	return readSpec(root, filepath.Base(path))
}

func readSpec(root *os.Root, name string) (*Spec, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, fmt.Sprintf("read definition %s: %s", name, err.Error()))
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maxDefinitionBytes))
	if err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, fmt.Sprintf("read definition %s: %s", name, err.Error()))
	}
	return parseSpec(content)
}

func parseSpec(content []byte) (*Spec, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	decoder.KnownFields(true)
	var file specFile
	if err := decoder.Decode(&file); err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, "invalid definition: "+err.Error())
	}
	spec := &Spec{
		ID:      file.ID,
		Goal:    file.Goal,
		Version: file.Version,
		Source:  Source(file.Source),
	}
	if spec.Source == "" {
		spec.Source = SourceDefined
	}
	for index := range file.Steps {
		entry := &file.Steps[index]
		step := StepSpec{
			ID:        entry.ID,
			DependsOn: entry.DependsOn,
			Condition: entry.Condition,
			Timeout:   entry.Timeout,
			Retry:     RetryPolicy{Attempts: entry.Retry.Attempts, Backoff: entry.Retry.Backoff},
		}
		step.Executor.Kind = Kind(entry.Executor.Kind)
		step.Executor.AgentID = entry.Executor.AgentID
		step.Executor.RequiredCapabilities = entry.Executor.Requires
		step.Executor.ToolID = entry.Executor.ToolID
		step.Executor.Event = entry.Executor.Event
		spec.Steps = append(spec.Steps, step)
	}
	return spec, nil
}

// LoadDefinitionsDir loads every .yaml/.yml file under dir in sorted
// filename order; a missing directory loads nothing.
func LoadDefinitionsDir(dir string) ([]*Spec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, codedError(ErrorCodeSpecInvalid, "read definitions dir: "+err.Error())
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, "open definitions root: "+err.Error())
	}
	defer func() { _ = root.Close() }()
	var specs []*Spec
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		spec, err := readSpec(root, name)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	slices.SortFunc(specs, func(a, b *Spec) int {
		if a.ID != b.ID {
			return strings.Compare(a.ID, b.ID)
		}
		return a.Version - b.Version
	})
	return specs, nil
}
