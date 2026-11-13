package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
)

type sourcePackage struct {
	path    string
	name    string
	imports []string
	files   []*ast.File
}

type finding struct {
	entry   string
	message string
}

func isTestKit(name string) bool {
	return strings.HasSuffix(name, "test")
}

func parseSources(packages []repoPackage) ([]sourcePackage, error) {
	fset := token.NewFileSet()
	sources := make([]sourcePackage, 0, len(packages))
	for index := range packages {
		pkg := &packages[index]
		source := sourcePackage{path: pkg.path, name: pkg.name, imports: pkg.productionImports}
		for _, path := range pkg.files {
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			source.files = append(source.files, file)
		}
		if source.name == "" && len(source.files) > 0 {
			source.name = source.files[0].Name.Name
		}
		sources = append(sources, source)
	}
	return sources, nil
}

type baseline struct {
	file   string
	fields int
}

type baselineEntry struct {
	key  string
	line int
}

func (b baseline) parse(content string) (entries []baselineEntry, problems []string) {
	seen := map[string]bool{}
	for index, raw := range strings.Split(content, "\n") {
		text, _, _ := strings.Cut(raw, "#")
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != b.fields {
			problems = append(problems, fmt.Sprintf("%s:%d: malformed entry %q", b.file, index+1, raw))
			continue
		}
		key := strings.Join(fields, " ")
		if seen[key] {
			problems = append(problems, fmt.Sprintf("%s:%d: duplicate entry %s", b.file, index+1, key))
			continue
		}
		seen[key] = true
		entries = append(entries, baselineEntry{key: key, line: index + 1})
	}
	return entries, problems
}

func (b baseline) apply(findings []finding, content string) []string {
	entries, violations := b.parse(content)
	allowed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		allowed[entry.key] = true
	}
	matched := map[string]bool{}
	for _, f := range findings {
		if f.entry != "" && allowed[f.entry] {
			matched[f.entry] = true
			continue
		}
		violations = append(violations, f.message)
	}
	for _, entry := range entries {
		if !matched[entry.key] {
			violations = append(violations, fmt.Sprintf("%s:%d: stale allow entry %s", b.file, entry.line, entry.key))
		}
	}
	return uniqueSorted(violations)
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
