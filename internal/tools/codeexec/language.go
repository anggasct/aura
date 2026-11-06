package codeexec

import "strings"

type language struct {
	source string
	probe  string
}

func languageFor(runtime string) (language, bool) {
	switch runtime {
	case "python":
		return language{source: "main.py", probe: "import sys\nprint('%d.%d.%d' % sys.version_info[:3])\n"}, true
	case "javascript":
		return language{source: "main.js", probe: "console.log(process.versions.node)\n"}, true
	default:
		return language{}, false
	}
}

func coreVersion(version string) string {
	core, _, _ := strings.Cut(version, "+")
	return core
}
