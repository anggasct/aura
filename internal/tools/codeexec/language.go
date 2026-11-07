package codeexec

import (
	"path/filepath"
	"strings"

	"github.com/anggasct/aura/internal/tools/interpreter"
)

const (
	goScratchDir = ".go"
	goTempName   = "tmp"
	goCacheName  = "cache"
	goPathName   = "path"
	goModName    = "go.mod"
	goModModule  = "aurarun"
)

type workspaceFile struct {
	name    string
	content string
}

type language struct {
	source           string
	probe            string
	directories      []string
	scratch          []string
	workspace        func(bundle *interpreter.Bundle) []workspaceFile
	buildEnvironment func(dir string, bundle *interpreter.Bundle) map[string]string
}

func languageFor(runtime string) (language, bool) {
	switch runtime {
	case "python":
		return language{source: "main.py", probe: "import sys\nprint('%d.%d.%d' % sys.version_info[:3])\n"}, true
	case "javascript":
		return language{source: "main.js", probe: "console.log(process.versions.node)\n"}, true
	case "go":
		return language{
			source:           "main.go",
			probe:            "package main\nimport (\"fmt\"; \"runtime\"; \"strings\")\nfunc main() { fmt.Println(strings.TrimPrefix(runtime.Version(), \"go\")) }\n",
			directories:      []string{filepath.Join(goScratchDir, goTempName)},
			scratch:          []string{filepath.Join(goScratchDir, goCacheName), filepath.Join(goScratchDir, goPathName), filepath.Join(goScratchDir, goTempName)},
			workspace:        goWorkspace,
			buildEnvironment: goBuildEnvironment,
		}, true
	default:
		return language{}, false
	}
}

func goWorkspace(bundle *interpreter.Bundle) []workspaceFile {
	return []workspaceFile{{name: goModName, content: "module " + goModModule + "\n\ngo " + coreVersion(bundle.Runtime.Version) + "\n"}}
}

func goBuildEnvironment(dir string, bundle *interpreter.Bundle) map[string]string {
	binDir := filepath.Dir(bundle.Executable)
	scratch := filepath.Join(dir, goScratchDir)
	return map[string]string{
		"GOROOT":  filepath.Dir(binDir),
		"GOCACHE": filepath.Join(scratch, goCacheName),
		"GOPATH":  filepath.Join(scratch, goPathName),
		"PATH":    binDir,
		"TMPDIR":  filepath.Join(scratch, goTempName),
		"HOME":    "",
	}
}

func coreVersion(version string) string {
	core, _, _ := strings.Cut(version, "+")
	return core
}
