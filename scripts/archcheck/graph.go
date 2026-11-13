package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const modulePath = "github.com/anggasct/aura"

type listedPackage struct {
	ImportPath     string   `json:"ImportPath"`
	Name           string   `json:"Name"`
	Dir            string   `json:"Dir"`
	Standard       bool     `json:"Standard"`
	GoFiles        []string `json:"GoFiles"`
	CgoFiles       []string `json:"CgoFiles"`
	IgnoredGoFiles []string `json:"IgnoredGoFiles"`
	Imports        []string `json:"Imports"`
	TestImports    []string `json:"TestImports"`
	XTestImports   []string `json:"XTestImports"`
}

type repoPackage struct {
	path              string
	name              string
	imports           []string
	productionImports []string
	files             []string
}

type repository struct {
	packages    []repoPackage
	stdNames    map[string]string
	importedStd map[string]bool
}

func loadRepository(ctx context.Context) (repository, error) {
	repoListed, err := goList(ctx, listRepository)
	if err != nil {
		return repository{}, err
	}
	stdListed, err := goList(ctx, listStandardLibrary)
	if err != nil {
		return repository{}, err
	}
	repo := repository{stdNames: make(map[string]string, len(stdListed)), importedStd: map[string]bool{}}
	for index := range stdListed {
		repo.stdNames[stdListed[index].ImportPath] = stdListed[index].Name
	}
	for index := range repoListed {
		repo.packages = append(repo.packages, repo.newPackage(&repoListed[index]))
	}
	return repo, nil
}

func (repo *repository) newPackage(p *listedPackage) repoPackage {
	production := repo.moduleImports(p.Imports)
	return repoPackage{
		path:              relativePath(p.ImportPath),
		name:              p.Name,
		imports:           slices.Concat(production, repo.moduleImports(p.TestImports), repo.moduleImports(p.XTestImports)),
		productionImports: production,
		files:             productionFiles(p),
	}
}

func (repo *repository) moduleImports(imports []string) []string {
	var result []string
	for _, imported := range imports {
		if _, ok := repo.stdNames[imported]; ok {
			repo.importedStd[imported] = true
			continue
		}
		if imported == modulePath || strings.HasPrefix(imported, modulePath+"/") {
			result = append(result, relativePath(imported))
		}
	}
	return result
}

func productionFiles(p *listedPackage) []string {
	var files []string
	for _, name := range slices.Concat(p.GoFiles, p.CgoFiles, p.IgnoredGoFiles) {
		if !strings.HasSuffix(name, "_test.go") {
			files = append(files, filepath.Join(p.Dir, name))
		}
	}
	return files
}

func relativePath(importPath string) string {
	return strings.TrimPrefix(strings.TrimPrefix(importPath, modulePath), "/")
}

type listTarget int

const (
	listRepository listTarget = iota
	listStandardLibrary
)

func (target listTarget) String() string {
	if target == listStandardLibrary {
		return "std"
	}
	return "./..."
}

func listCommand(ctx context.Context, target listTarget) *exec.Cmd {
	if target == listStandardLibrary {
		return exec.CommandContext(ctx, "go", "list", "-e", "-json", "std")
	}
	return exec.CommandContext(ctx, "go", "list", "-e", "-json", "-tags", "durable,integration,e2e,load", "./...")
}

func goList(ctx context.Context, target listTarget) ([]listedPackage, error) {
	pattern := target.String()
	var stdout, stderr bytes.Buffer
	cmd := listCommand(ctx, target)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list %s: %w: %s", pattern, err, strings.TrimSpace(stderr.String()))
	}
	decoder := json.NewDecoder(&stdout)
	var listed []listedPackage
	for {
		var p listedPackage
		err := decoder.Decode(&p)
		if errors.Is(err, io.EOF) {
			return listed, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode go list %s: %w", pattern, err)
		}
		listed = append(listed, p)
	}
}
