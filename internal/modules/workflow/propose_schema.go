package workflow

const (
	proposeRetrySchema = `{"type":"object","properties":{"attempts":{"type":"integer","minimum":0,"maximum":5}},"additionalProperties":false}`

	proposeToolExecutorSchema     = `{"type":"object","properties":{"kind":{"const":"tool"},"tool":{"type":"string","minLength":1},"args":{"type":"object"}},"required":["kind","tool"],"additionalProperties":false}`
	proposeAgentExecutorSchema    = `{"type":"object","properties":{"kind":{"const":"agent"},"agent_id":{"type":"string","minLength":1},"prompt":{"type":"string","minLength":1,"maxLength":2000},"tools":{"type":"array","items":{"enum":["read_file","list_dir","web_search","web_fetch"]}},"output":{"enum":["text","json"]},"max_tokens":{"type":"integer","minimum":1,"maximum":200000}},"required":["kind","agent_id","prompt"],"additionalProperties":false}`
	proposeApprovalExecutorSchema = `{"type":"object","properties":{"kind":{"const":"approval"}},"required":["kind"],"additionalProperties":false}`
	proposeWaitExecutorSchema     = `{"type":"object","properties":{"kind":{"const":"wait"},"event":{"type":"string","minLength":1,"maxLength":128},"source":{"type":"string","minLength":1},"external_ref":{"type":"string","minLength":1}},"required":["kind","event"],"additionalProperties":false}`
	proposeWorkflowExecutorSchema = `{"type":"object","properties":{"kind":{"const":"workflow"},"workflow":{"type":"string","minLength":1}},"required":["kind","workflow"],"additionalProperties":false}`

	proposeExecutorSchema = `{"oneOf":[` + proposeToolExecutorSchema + `,` + proposeAgentExecutorSchema + `,` + proposeApprovalExecutorSchema + `,` + proposeWaitExecutorSchema + `,` + proposeWorkflowExecutorSchema + `]}`

	proposeStepSchema = `{"type":"object","properties":{"id":{"type":"string","pattern":"^[a-z][a-z0-9_-]{0,63}$"},"executor":` + proposeExecutorSchema +
		`,"depends_on":{"type":"array","items":{"type":"string"}},"condition":{"type":"string","minLength":1,"maxLength":256,"description":"steps.<id>.status or steps.<id>.output.<key> compared with == or != to a literal, joined by &&"},"retry":` + proposeRetrySchema +
		`},"required":["id","executor"],"additionalProperties":false}`

	proposeSpecSchema = `{"type":"object","properties":{"goal":{"type":"string","minLength":1,"maxLength":200},"run_as":{"type":"string","minLength":1},"steps":{"type":"array","minItems":1,"items":` + proposeStepSchema +
		`}},"required":["goal","steps"],"additionalProperties":false}`

	proposeToolSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_propose/v1","type":"object","properties":{"spec":` + proposeSpecSchema + `},"required":["spec"],"additionalProperties":false}`

	proposeExample = `{"spec":{"goal":"Summarize the latest Go release news","steps":[` +
		`{"id":"search","executor":{"kind":"tool","tool":"web_search","args":{"query":"Go release news"}}},` +
		`{"id":"summary","depends_on":["search"],"executor":{"kind":"agent","agent_id":"main","prompt":"Summarize the search results in five bullet points."}}]}}`

	proposeDescription = "Propose a workflow for the owner to approve; nothing runs until they do. " +
		"Send spec with goal and steps. Each step has an id, an executor, and optional depends_on, condition and retry. " +
		"Executor kinds: tool runs a registered tool with args matching that tool's schema; agent runs one read-only turn of persona agent_id over its dependencies' outputs; approval waits for the owner; wait pauses for an event; workflow runs another workflow. " +
		"An agent step's tools come from read_file, list_dir, web_search, web_fetch, and may not mix workspace tools (read_file, list_dir) with web tools. " +
		"A step with web tools may not depend on a private step: any tool step other than web_search and web_fetch, an agent step with workspace tools, or an agent step downstream of one. " +
		"A tool step that changes things needs an approval step among its predecessors. " +
		"Example: " + proposeExample

	listDescription        = "List the workflows you may start."
	startDescription       = "Start a workflow by id, optionally with an objective. Returns the run id; read progress with workflow_status."
	statusDescription      = "Show the status of a workflow run and its steps."
	cancelDescription      = "Cancel a workflow run by run id."
	saveRoutineDescription = "Save the workflow of a succeeded run as a named routine the owner can start again."
)
