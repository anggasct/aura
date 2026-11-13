package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
)

const (
	baselineDir = "scripts/archcheck/"
	allowFile   = baselineDir + "layers.allow"
)

var checkNames = []string{"layout", "stdnames", "layers", "fakes", "setters", "codederrors"}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string) int {
	selected := args
	if len(selected) == 0 {
		selected = checkNames
	}
	for _, name := range selected {
		if !slices.Contains(checkNames, name) {
			fmt.Fprintf(os.Stderr, "archcheck: unknown check %q\n", name)
			return 2
		}
	}
	violations, err := collect(ctx, selected)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archcheck: %v\n", err)
		return 1
	}
	for _, violation := range violations {
		fmt.Println(violation)
	}
	if len(violations) > 0 {
		return 1
	}
	return 0
}

func collect(ctx context.Context, selected []string) ([]string, error) {
	var violations []string
	if slices.Contains(selected, "layout") {
		entries, err := readGroupDirs()
		if err != nil {
			return nil, err
		}
		violations = append(violations, checkLayout(entries)...)
	}
	if !slices.Contains(selected, "stdnames") && !slices.Contains(selected, "layers") && !hasSourceCheck(selected) {
		return violations, nil
	}
	repo, err := loadRepository(ctx)
	if err != nil {
		return nil, err
	}
	if slices.Contains(selected, "stdnames") {
		violations = append(violations, checkStdNames(repo.packages, repo.stdNames, repo.importedStd)...)
	}
	if slices.Contains(selected, "layers") {
		content, err := readOptional(allowFile)
		if err != nil {
			return nil, err
		}
		allowed, problems := parseAllow(content)
		violations = append(violations, problems...)
		violations = append(violations, checkLayers(repo.packages, allowed)...)
	}
	if hasSourceCheck(selected) {
		found, err := checkSources(selected, repo.packages)
		if err != nil {
			return nil, err
		}
		violations = append(violations, found...)
	}
	return violations, nil
}

func readOptional(path string) (string, error) {
	content, err := os.ReadFile(filepath.Clean(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(content), nil
}

func readGroupDirs() (map[string][]dirEntry, error) {
	result := map[string][]dirEntry{}
	for _, dir := range []string{"internal", "internal/platform", "internal/modules", "internal/channels"} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, entry := range entries {
			result[dir] = append(result[dir], dirEntry{name: entry.Name(), isDir: entry.IsDir()})
		}
	}
	return result, nil
}
