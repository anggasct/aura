package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strings"
)

type sourcePackage struct {
	path    string
	name    string
	imports []string
	files   []*ast.File
	current []*ast.File
	fset    *token.FileSet
	info    *types.Info
}

func isTestKit(name string) bool {
	return strings.HasSuffix(name, "test")
}

func parseSources(packages []repoPackage) ([]sourcePackage, error) {
	fset := token.NewFileSet()
	sources := make([]sourcePackage, 0, len(packages))
	for index := range packages {
		pkg := &packages[index]
		source := sourcePackage{path: pkg.path, name: pkg.name, imports: pkg.productionImports, fset: fset}
		for _, path := range pkg.files {
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			source.files = append(source.files, file)
			if slices.Contains(pkg.built, path) {
				source.current = append(source.current, file)
			}
		}
		if source.name == "" && len(source.files) > 0 {
			source.name = source.files[0].Name.Name
		}
		sources = append(sources, source)
	}
	return sources, nil
}

func declaredNames(file *ast.File) []string {
	return slices.Concat(declaredTypes(file), declaredValues(file), declaredFuncs(file))
}

func declaredTypes(file *ast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			if typeSpec, ok := spec.(*ast.TypeSpec); ok {
				names = append(names, typeSpec.Name.Name)
			}
		}
	}
	return names
}

func declaredValues(file *ast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			if valueSpec, ok := spec.(*ast.ValueSpec); ok {
				for _, name := range valueSpec.Names {
					names = append(names, name.Name)
				}
			}
		}
	}
	return names
}

func declaredFuncs(file *ast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			names = append(names, fn.Name.Name)
		}
	}
	return names
}
