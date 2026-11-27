package main

import (
	"fmt"
	"go/ast"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	faultPackage      = "internal/platform/fault"
	operationPackage  = "internal/platform/operation"
	toolsPackage      = "internal/platform/tools"
	toolsDefinition   = "Definition"
	operationOwnedKey = "Operation"
	noticeOwnedKey    = "OwnerNotice"
)

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
	{name: "operation-definitions", find: findOperationDefinitions},
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

func checkSources(selected []string, packages []repoPackage, allowDir string) ([]string, error) {
	sources, err := parseSources(packages)
	if err != nil {
		return nil, err
	}
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

func findOperationDefinitions(packages []sourcePackage) []string {
	var violations []string
	for _, pkg := range packages {
		if isTestKit(pkg.name) || within(pkg.path, operationPackage) {
			continue
		}
		for _, file := range pkg.files {
			alias, ok := toolsAlias(pkg.path, file)
			if !ok {
				continue
			}
			for _, field := range operationOwnedFields(file, alias) {
				violations = append(violations, fmt.Sprintf("operation-definitions: %s sets Definition.%s outside %s", pkg.path, field, operationPackage))
			}
		}
	}
	return uniqueSorted(violations)
}

func toolsAlias(pkgPath string, file *ast.File) (string, bool) {
	if pkgPath == toolsPackage {
		return "", true
	}
	for _, spec := range file.Imports {
		imported := strings.Trim(spec.Path.Value, `"`)
		if imported != toolsPackage && !strings.HasSuffix(imported, "/"+toolsPackage) {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name, spec.Name.Name != "_" && spec.Name.Name != "."
		}
		return path.Base(imported), true
	}
	return "", false
}

func operationOwnedFields(file *ast.File, alias string) []string {
	var fields []string
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CompositeLit:
			fields = append(fields, definitionLiteralFields(node, alias)...)
		case *ast.AssignStmt:
			for _, target := range node.Lhs {
				if selector, ok := target.(*ast.SelectorExpr); ok && isOperationOwned(selector.Sel.Name) {
					fields = append(fields, selector.Sel.Name)
				}
			}
		}
		return true
	})
	return fields
}

func definitionLiteralFields(literal *ast.CompositeLit, alias string) []string {
	var fields []string
	elements := literal.Elts
	switch kind := literal.Type.(type) {
	case *ast.ArrayType:
		if !isToolsDefinition(kind.Elt, alias) {
			return nil
		}
		for _, element := range elements {
			if inner, ok := element.(*ast.CompositeLit); ok && inner.Type == nil {
				fields = append(fields, keyedOperationFields(inner.Elts)...)
			}
		}
		return fields
	case *ast.MapType:
		if !isToolsDefinition(kind.Value, alias) {
			return nil
		}
		for _, element := range elements {
			if pair, ok := element.(*ast.KeyValueExpr); ok {
				if inner, ok := pair.Value.(*ast.CompositeLit); ok && inner.Type == nil {
					fields = append(fields, keyedOperationFields(inner.Elts)...)
				}
			}
		}
		return fields
	}
	if !isToolsDefinition(literal.Type, alias) {
		return nil
	}
	return keyedOperationFields(elements)
}

func keyedOperationFields(elements []ast.Expr) []string {
	var fields []string
	for _, element := range elements {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := pair.Key.(*ast.Ident); ok && isOperationOwned(key.Name) {
			fields = append(fields, key.Name)
		}
	}
	return fields
}

func isToolsDefinition(expr ast.Expr, alias string) bool {
	switch kind := expr.(type) {
	case *ast.Ident:
		return alias == "" && kind.Name == toolsDefinition
	case *ast.SelectorExpr:
		pkg, ok := kind.X.(*ast.Ident)
		return ok && alias != "" && pkg.Name == alias && kind.Sel.Name == toolsDefinition
	}
	return false
}

func isOperationOwned(name string) bool {
	return name == operationOwnedKey || name == noticeOwnedKey
}
