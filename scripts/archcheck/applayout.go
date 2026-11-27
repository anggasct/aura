package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

const appDir = "internal/app"

var (
	allowedAppFiles   = []string{"app.go", "modules.go", "commands.go", "migrations.go"}
	allowedAppSubdirs = []string{"config", "testdata"}
)

func readAppEntries() ([]dirEntry, error) {
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", appDir, err)
	}
	result := make([]dirEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, dirEntry{name: entry.Name(), isDir: entry.IsDir()})
	}
	return result, nil
}

func checkAppLayout(entries []dirEntry) []string {
	var violations []string
	for _, entry := range entries {
		if entry.isDir {
			if !slices.Contains(allowedAppSubdirs, entry.name) {
				violations = append(violations, fmt.Sprintf("app-layout: %s/%s is not an allowed subdirectory (config)", appDir, entry.name))
			}
			continue
		}
		if strings.HasSuffix(entry.name, "_test.go") {
			continue
		}
		if !slices.Contains(allowedAppFiles, entry.name) {
			violations = append(violations, fmt.Sprintf("app-layout: %s/%s is not in the allowed composition-root file set (app.go, modules.go, commands.go, migrations.go)", appDir, entry.name))
		}
	}
	return uniqueSorted(violations)
}
