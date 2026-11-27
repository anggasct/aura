package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const baselineDir = "scripts/archcheck"

var checkNames = []string{"layout", "app-layout", "stdnames", "layers", "fakes", "setters", "codederrors", operationDefinitionsCheck, "modulemap"}

type runDeps struct {
	allowDir      string
	moduleMapFile string
	readGroups    func() (map[string][]dirEntry, error)
	readApp       func() ([]dirEntry, error)
	loadRepo      func(context.Context) (repository, error)
	loadExports   exportLoader
}

func defaultRunDeps() runDeps {
	return runDeps{
		allowDir:      baselineDir,
		moduleMapFile: defaultModuleMapFile,
		readGroups:    readGroupDirs,
		readApp:       readAppEntries,
		loadRepo:      loadRepository,
		loadExports:   listExports,
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string) int {
	return runWithDeps(ctx, args, os.Stdout, os.Stderr, defaultRunDeps())
}

func runWithDeps(ctx context.Context, args []string, stdout, stderr io.Writer, deps runDeps) int {
	if len(args) > 0 && args[0] == "modulemap" {
		return runModuleMap(ctx, args[1:], stdout, stderr, deps.moduleMapFile, deps.loadRepo)
	}
	selected := args
	if len(selected) == 0 {
		selected = checkNames
	}
	for _, name := range selected {
		if strings.HasPrefix(name, "-") {
			_, _ = fmt.Fprintf(stderr, "archcheck: flags are only accepted with \"modulemap\" as the first argument (got %q)\n", name)
			return 2
		}
		if !slices.Contains(checkNames, name) {
			_, _ = fmt.Fprintf(stderr, "archcheck: unknown check %q\n", name)
			return 2
		}
	}
	violations, err := collect(ctx, selected, deps)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archcheck: %v\n", err)
		return 1
	}
	for _, violation := range violations {
		_, _ = fmt.Fprintln(stdout, violation)
	}
	if len(violations) > 0 {
		return 1
	}
	return 0
}

func collect(ctx context.Context, selected []string, deps runDeps) ([]string, error) {
	var violations []string
	if slices.Contains(selected, "layout") {
		entries, err := deps.readGroups()
		if err != nil {
			return nil, err
		}
		violations = append(violations, checkLayout(entries)...)
	}
	if slices.Contains(selected, "app-layout") {
		entries, err := deps.readApp()
		if err != nil {
			return nil, err
		}
		violations = append(violations, checkAppLayout(entries)...)
	}
	needsRepo := slices.Contains(selected, "stdnames") ||
		slices.Contains(selected, "layers") ||
		hasSourceCheck(selected) ||
		slices.Contains(selected, "modulemap")
	if !needsRepo {
		return violations, nil
	}
	repo, err := deps.loadRepo(ctx)
	if err != nil {
		return nil, err
	}
	if slices.Contains(selected, "stdnames") {
		violations = append(violations, checkStdNames(repo.packages, repo.stdNames, repo.importedStd)...)
	}
	if slices.Contains(selected, "layers") {
		absent, err := checkAllowListAbsent(filepath.Join(deps.allowDir, "layers.allow"))
		if err != nil {
			return nil, err
		}
		violations = append(violations, absent...)
		violations = append(violations, checkLayers(repo.packages)...)
	}
	if hasSourceCheck(selected) {
		found, err := checkSources(ctx, selected, repo.packages, deps)
		if err != nil {
			return nil, err
		}
		violations = append(violations, found...)
	}
	if slices.Contains(selected, "modulemap") {
		found, err := checkModuleMap(repo.packages, deps.moduleMapFile)
		if err != nil {
			return nil, err
		}
		violations = append(violations, found...)
	}
	return violations, nil
}

func checkAllowListAbsent(path string) ([]string, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	return []string{filepath.ToSlash(path) + ": allow-list files are forbidden after close-out"}, nil
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
