package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

const defaultModuleMapFile = "testdata/golden/module-map.txt"

type moduleMapSection struct {
	header string
	root   string
}

var moduleMapSections = []moduleMapSection{
	{header: "cmd/", root: "cmd"},
	{header: "internal/app/", root: "internal/app"},
	{header: "internal/cli/", root: "internal/cli"},
	{header: "internal/platform/", root: "internal/platform"},
	{header: "internal/modules/", root: "internal/modules"},
	{header: "internal/channels/", root: "internal/channels"},
	{header: "internal/store/", root: "internal/store"},
	{header: "plugins/", root: "plugins"},
}

func renderModuleMap(packages []repoPackage) (string, error) {
	buckets := make([][]string, len(moduleMapSections))
	for _, pkg := range packages {
		if !within(pkg.path, "cmd") && !within(pkg.path, "internal") && !within(pkg.path, "plugins") {
			continue
		}
		index := slices.IndexFunc(moduleMapSections, func(sec moduleMapSection) bool {
			return within(pkg.path, sec.root)
		})
		if index < 0 {
			return "", fmt.Errorf("modulemap: package %s matches no section", pkg.path)
		}
		line := pkg.path
		if pkg.name != "" && pkg.name != "main" && pkg.name != path.Base(pkg.path) {
			line += " (package " + pkg.name + ")"
		}
		buckets[index] = append(buckets[index], line)
	}
	var out strings.Builder
	written := 0
	for index, sec := range moduleMapSections {
		lines := uniqueSorted(buckets[index])
		if len(lines) == 0 {
			continue
		}
		if written > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(sec.header)
		out.WriteByte('\n')
		for _, line := range lines {
			out.WriteString("  ")
			out.WriteString(line)
			out.WriteByte('\n')
		}
		written++
	}
	return out.String(), nil
}

func compareModuleMap(rendered, expected, file string) string {
	if rendered == expected {
		return ""
	}
	gotLines := strings.Split(rendered, "\n")
	wantLines := strings.Split(expected, "\n")
	limit := min(len(gotLines), len(wantLines))
	for i := range limit {
		if gotLines[i] != wantLines[i] {
			return fmt.Sprintf("modulemap: %s differs at line %d: got %q, want %q", file, i+1, gotLines[i], wantLines[i])
		}
	}
	if len(gotLines) > len(wantLines) {
		return fmt.Sprintf("modulemap: %s differs at line %d: unexpected extra line %q", file, limit+1, gotLines[limit])
	}
	return fmt.Sprintf("modulemap: %s differs at line %d: missing line %q", file, limit+1, wantLines[limit])
}

func readFileRooted(target string) ([]byte, error) {
	clean := filepath.Clean(target)
	root, err := os.OpenRoot(filepath.Dir(clean))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", target, err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(filepath.Base(clean))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", target, err)
	}
	return data, nil
}

func writeFileRooted(target string, data []byte) error {
	clean := filepath.Clean(target)
	dir := filepath.Dir(clean)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", target, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(filepath.Base(clean), data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

func checkModuleMap(packages []repoPackage, file string) ([]string, error) {
	rendered, err := renderModuleMap(packages)
	if err != nil {
		return nil, err
	}
	expected, err := readFileRooted(file)
	if err != nil {
		return nil, err
	}
	if diff := compareModuleMap(rendered, string(expected), file); diff != "" {
		return []string{diff}, nil
	}
	return nil, nil
}

func runModuleMap(ctx context.Context, args []string, stdout, stderr io.Writer, defaultFile string, load func(context.Context) (repository, error)) int {
	flags := flag.NewFlagSet("modulemap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	check := flags.Bool("check", false, "verify module map matches file")
	write := flags.Bool("write", false, "write module map to file")
	file := flags.String("file", defaultFile, "path to module map file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "archcheck modulemap: unexpected arguments %q\n", flags.Args())
		return 2
	}
	if *check && *write {
		_, _ = fmt.Fprintln(stderr, "archcheck modulemap: -check and -write cannot be used together")
		return 2
	}
	repo, err := load(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archcheck: %v\n", err)
		return 1
	}
	rendered, err := renderModuleMap(repo.packages)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archcheck: %v\n", err)
		return 1
	}
	switch {
	case *write:
		if err := writeFileRooted(*file, []byte(rendered)); err != nil {
			_, _ = fmt.Fprintf(stderr, "archcheck: %v\n", err)
			return 1
		}
		return 0
	case *check:
		expected, err := readFileRooted(*file)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "archcheck: %v\n", err)
			return 1
		}
		if diff := compareModuleMap(rendered, string(expected), *file); diff != "" {
			_, _ = fmt.Fprintln(stdout, diff)
			return 1
		}
		return 0
	default:
		_, _ = io.WriteString(stdout, rendered)
		return 0
	}
}
