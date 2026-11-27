package main

import (
	"context"
	"fmt"
	"go/ast"
	"path/filepath"
	"regexp"
	"slices"
)

const faultPackage = "internal/platform/fault"

var (
	fakeName   = regexp.MustCompile(`^(New)?(Fake|Stub|Mock)([A-Z_]|$)`)
	setterName = regexp.MustCompile(`^Set[A-Z]`)
)

var basicTypes = []string{
	"bool", "string", "byte", "rune",
	"int", "int8", "int16", "int32", "int64",
	"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
	"float32", "float64", "complex64", "complex128",
}

type sourceCheck struct {
	name string
	find func([]sourcePackage) []string
}

var sourceChecks = []sourceCheck{
	{name: "fakes", find: findFakes},
	{name: "setters", find: findSetters},
	{name: "codederrors", find: findCodedErrors},
	{name: operationDefinitionsCheck, find: findOperationDefinitions},
}

func findFakes(packages []sourcePackage) []string {
	kits := map[string]bool{}
	for _, pkg := range packages {
		if isTestKit(pkg.name) {
			kits[pkg.path] = true
		}
	}
	var violations []string
	for _, pkg := range packages {
		if isTestKit(pkg.name) {
			continue
		}
		for _, file := range pkg.files {
			for _, name := range declaredNames(file) {
				if fakeName.MatchString(name) {
					violations = append(violations, fmt.Sprintf("fakes: %s declares %s in a production file", pkg.path, name))
				}
			}
		}
		for _, imported := range pkg.imports {
			if kits[imported] {
				violations = append(violations, fmt.Sprintf("fakes: %s imports the test kit %s from a production file", pkg.path, imported))
			}
		}
	}
	return uniqueSorted(violations)
}

func findSetters(packages []sourcePackage) []string {
	var violations []string
	for _, pkg := range packages {
		if isTestKit(pkg.name) {
			continue
		}
		basic := basicNamedTypes(pkg.files)
		for _, file := range pkg.files {
			for _, decl := range file.Decls {
				method, ok := decl.(*ast.FuncDecl)
				if !ok || !isDependencySetter(method, basic) {
					continue
				}
				name := receiverName(method.Recv.List[0].Type) + "." + method.Name.Name
				violations = append(violations, fmt.Sprintf("setters: %s %s injects a dependency after construction", pkg.path, name))
			}
		}
	}
	return uniqueSorted(violations)
}

func isDependencySetter(method *ast.FuncDecl, basic map[string]bool) bool {
	if method.Recv == nil || len(method.Recv.List) != 1 || !setterName.MatchString(method.Name.Name) {
		return false
	}
	params := method.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}
	switch kind := params[0].Type.(type) {
	case *ast.Ellipsis:
		return false
	case *ast.Ident:
		return !slices.Contains(basicTypes, kind.Name) && !basic[kind.Name]
	default:
		return true
	}
}

func basicNamedTypes(files []*ast.File) map[string]bool {
	declared := map[string]ast.Expr{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				if typeSpec, ok := spec.(*ast.TypeSpec); ok {
					declared[typeSpec.Name.Name] = typeSpec.Type
				}
			}
		}
	}
	basic := map[string]bool{}
	for name := range declared {
		current := name
		for range len(declared) + 1 {
			ident, ok := declared[current].(*ast.Ident)
			if !ok {
				break
			}
			if slices.Contains(basicTypes, ident.Name) {
				basic[name] = true
				break
			}
			current = ident.Name
		}
	}
	return basic
}

func receiverName(expr ast.Expr) string {
	for {
		switch node := expr.(type) {
		case *ast.StarExpr:
			expr = node.X
		case *ast.ParenExpr:
			expr = node.X
		case *ast.IndexExpr:
			expr = node.X
		case *ast.IndexListExpr:
			expr = node.X
		case *ast.Ident:
			return node.Name
		default:
			return ""
		}
	}
}

func findCodedErrors(packages []sourcePackage) []string {
	var violations []string
	for _, pkg := range packages {
		if isTestKit(pkg.name) || within(pkg.path, faultPackage) || within(pkg.path, pluginWire) || classify(pkg.path).kind == kindPlugin {
			continue
		}
		for _, file := range pkg.files {
			if slices.Contains(declaredTypes(file), "Error") {
				violations = append(violations, fmt.Sprintf("codederrors: %s declares its own Error type; use platform/fault", pkg.path))
			}
			if slices.Contains(declaredFuncs(file), "CodeOf") {
				violations = append(violations, fmt.Sprintf("codederrors: %s declares its own CodeOf; use fault.CodeOf", pkg.path))
			}
		}
	}
	return uniqueSorted(violations)
}

func checkSources(ctx context.Context, selected []string, packages []repoPackage, deps runDeps) ([]string, error) {
	sources, err := parseSources(packages)
	if err != nil {
		return nil, err
	}
	if slices.Contains(selected, operationDefinitionsCheck) {
		if err := typeOperationCandidates(ctx, sources, deps.loadExports); err != nil {
			return nil, err
		}
	}
	allowDir := deps.allowDir
	var violations []string
	for _, check := range sourceChecks {
		if !slices.Contains(selected, check.name) {
			continue
		}
		absent, err := checkAllowListAbsent(filepath.Join(allowDir, check.name+".allow"))
		if err != nil {
			return nil, err
		}
		violations = append(violations, absent...)
		violations = append(violations, check.find(sources)...)
	}
	return violations, nil
}

func hasSourceCheck(selected []string) bool {
	return slices.ContainsFunc(sourceChecks, func(check sourceCheck) bool {
		return slices.Contains(selected, check.name)
	})
}
