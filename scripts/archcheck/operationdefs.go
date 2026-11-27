package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	operationDefinitionsCheck = "operation-definitions"
	operationPackage          = "internal/platform/operation"
	toolsPackage              = "internal/platform/tools"
	toolsDefinition           = "Definition"
)

var operationOwnedFields = []string{"Operation", "OwnerNotice"}

type exportLoader func(ctx context.Context) (map[string]string, error)

func isOperationOwned(name string) bool {
	return slices.Contains(operationOwnedFields, name)
}

func isOperationCandidate(pkg *sourcePackage) bool {
	if isTestKit(pkg.name) || within(pkg.path, operationPackage) {
		return false
	}
	if slices.Contains(pkg.imports, toolsPackage) || pkg.path == toolsPackage {
		return true
	}
	for _, file := range pkg.files {
		found := false
		ast.Inspect(file, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && isOperationOwned(ident.Name) {
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

func typeOperationCandidates(ctx context.Context, sources []sourcePackage, load exportLoader) error {
	var candidates []*sourcePackage
	for i := range sources {
		if isOperationCandidate(&sources[i]) {
			candidates = append(candidates, &sources[i])
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	exports, err := load(ctx)
	if err != nil {
		return err
	}
	for _, pkg := range candidates {
		if _, err := typeCheck(pkg, exportImporter(pkg.fset, exports)); err != nil {
			return err
		}
	}
	return nil
}

func exportImporter(fset *token.FileSet, exports map[string]string) types.Importer {
	return importer.ForCompiler(fset, "gc", func(importPath string) (io.ReadCloser, error) {
		file, ok := exports[importPath]
		if !ok || file == "" {
			return nil, fmt.Errorf("no export data for %s", importPath)
		}
		return os.Open(filepath.Clean(file))
	})
}

func typeCheck(pkg *sourcePackage, imp types.Importer) (*types.Package, error) {
	pkg.info = &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Uses:  map[*ast.Ident]types.Object{},
		Defs:  map[*ast.Ident]types.Object{},
	}
	var unresolved []error
	config := types.Config{
		Importer:    imp,
		FakeImportC: true,
		Error: func(err error) {
			var typeErr types.Error
			if errors.As(err, &typeErr) && strings.HasPrefix(typeErr.Msg, "could not import") {
				unresolved = append(unresolved, err)
			}
		},
	}
	checked, _ := config.Check(modulePath+"/"+pkg.path, pkg.fset, pkg.files, pkg.info)
	if len(unresolved) > 0 {
		return nil, fmt.Errorf("type-check %s: %w", pkg.path, errors.Join(unresolved...))
	}
	return checked, nil
}

func listExports(ctx context.Context) (map[string]string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-export", "-deps", "-json=ImportPath,Export", "-tags", "durable,integration,e2e,load", "./...")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list -export: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	decoder := json.NewDecoder(&stdout)
	exports := map[string]string{}
	for {
		var listed struct {
			ImportPath string `json:"ImportPath"`
			Export     string `json:"Export"`
		}
		err := decoder.Decode(&listed)
		if errors.Is(err, io.EOF) {
			return exports, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode go list -export: %w", err)
		}
		exports[listed.ImportPath] = listed.Export
	}
}

func findOperationDefinitions(packages []sourcePackage) []string {
	var violations []string
	for i := range packages {
		pkg := &packages[i]
		if pkg.info == nil || isTestKit(pkg.name) || within(pkg.path, operationPackage) {
			continue
		}
		for _, file := range pkg.files {
			for _, field := range definitionWrites(pkg.info, file) {
				violations = append(violations, fmt.Sprintf("operation-definitions: %s sets Definition.%s outside %s", pkg.path, field, operationPackage))
			}
			if unkeyed(pkg.info, file) {
				violations = append(violations, fmt.Sprintf("operation-definitions: %s builds a Definition without field names outside %s", pkg.path, operationPackage))
			}
		}
	}
	return uniqueSorted(violations)
}

func definitionWrites(info *types.Info, file *ast.File) []string {
	var fields []string
	record := func(expr ast.Expr) {
		var name *ast.Ident
		switch node := ast.Unparen(expr).(type) {
		case *ast.Ident:
			name = node
		case *ast.SelectorExpr:
			name = node.Sel
		}
		if name != nil && isOperationOwned(name.Name) && isDefinitionField(info.Uses[name]) {
			fields = append(fields, name.Name)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CompositeLit:
			for _, element := range node.Elts {
				if pair, ok := element.(*ast.KeyValueExpr); ok {
					record(pair.Key)
				}
			}
		case *ast.AssignStmt:
			for _, target := range node.Lhs {
				record(target)
			}
		case *ast.IncDecStmt:
			record(node.X)
		case *ast.UnaryExpr:
			if node.Op == token.AND {
				record(node.X)
			}
		}
		return true
	})
	return fields
}

func unkeyed(info *types.Info, file *ast.File) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok || len(literal.Elts) == 0 {
			return true
		}
		if _, keyed := literal.Elts[0].(*ast.KeyValueExpr); keyed {
			return true
		}
		if isDefinitionType(info.Types[literal].Type) {
			found = true
		}
		return true
	})
	return found
}

func isDefinitionType(t types.Type) bool {
	for t != nil {
		pointer, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = pointer.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return named.Obj().Pkg().Path() == modulePath+"/"+toolsPackage && named.Obj().Name() == toolsDefinition
}

func isDefinitionField(obj types.Object) bool {
	field, ok := obj.(*types.Var)
	if !ok || !field.IsField() || field.Pkg() == nil || field.Pkg().Path() != modulePath+"/"+toolsPackage {
		return false
	}
	declared := field.Pkg().Scope().Lookup(toolsDefinition)
	if declared == nil {
		return true
	}
	structure, ok := declared.Type().Underlying().(*types.Struct)
	if !ok {
		return true
	}
	for i := range structure.NumFields() {
		if structure.Field(i) == field {
			return true
		}
	}
	return false
}
