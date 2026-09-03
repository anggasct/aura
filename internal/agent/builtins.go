package agent

// DefaultID is the default turn target: the conversational agent that every
// trigger reaches when no explicit definition is requested.
const DefaultID = "main"

const defaultModelRoute = "primary"

var builtins = []Definition{
	{
		ID: DefaultID,
		// The default target must keep the prompt surface it had before
		// definitions existed: the executor maps these fields straight onto
		// the ADK agent, so any text here would silently change every
		// default turn's system prompt.
		Tools: []string{"read_file", "write_file", "list_dir", "exec", "web_fetch", "web_search"},
	},
	{
		ID:           "engineer",
		Description:  "Software engineering agent for inspecting, modifying, and validating source repositories.",
		Instructions: "You are a software engineering agent. Inspect the repository before changing it, keep edits scoped to the task, and validate your work with the project's own build and test commands before reporting done.",
		Tools:        []string{"read_file", "write_file", "list_dir", "exec"},
		Capabilities: []string{CapabilityRepositoryRead, CapabilityRepositoryWrite, CapabilityShellExecute, CapabilityGitDiff},
		ModelRoute:   defaultModelRoute,
	},
	{
		ID:           "reviewer",
		Description:  "Code review agent for reading changes and reporting defects and risks.",
		Instructions: "You are a code review agent. Read the change under review in full, report concrete defects with file and line references, and rank findings by severity. You do not modify code.",
		Tools:        []string{"read_file", "list_dir"},
		Capabilities: []string{CapabilityRepositoryRead, CapabilityCodeReview, CapabilityGitDiff},
		ModelRoute:   defaultModelRoute,
	},
	{
		ID:           "researcher",
		Description:  "Research agent for gathering and summarizing sources from the web and local documents.",
		Instructions: "You are a research agent. Gather sources before concluding, attribute every claim to a source, and distinguish verified facts from inference.",
		Tools:        []string{"web_fetch", "web_search", "read_file"},
		Capabilities: []string{CapabilityWebSearch, CapabilityWebRead, CapabilityDocumentRead},
		ModelRoute:   defaultModelRoute,
	},
}

func builtinByID(id string) (Definition, bool) {
	for _, definition := range builtins {
		if definition.ID == id {
			return definition, true
		}
	}
	return Definition{}, false
}
