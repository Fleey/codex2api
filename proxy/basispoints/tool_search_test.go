package basispoints

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

const toolSearchTool = `{"type":"tool_search","execution":"client",` +
	`"description":"Search for app and MCP tools.\n- GitHub: Work with repositories, issues and pull requests. It can also manage releases, workflows, discussions, gists, projects, code scanning alerts, secret scanning alerts and many other things that make this line far longer than any catalog needs to quote in every single request.",` +
	`"parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}}`

const loadedEchoTools = `[{"type":"namespace","name":"mcp__fake__","tools":[{"type":"function","name":"echo","description":"Echo text back to the caller.",` +
	`"parameters":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]}]`

func protocolText(t *testing.T, prepared object) string {
	t.Helper()
	for _, raw := range prepared["input"].([]any) {
		item := raw.(object)
		if text(item["role"]) != "developer" {
			continue
		}
		body := text(item["content"].([]any)[0].(object)["text"])
		if strings.Contains(body, "external Responses client") {
			return body
		}
	}
	t.Fatal("prepared body has no protocol message")
	return ""
}

func TestToolSearchIsACatalogToolWithShortSources(t *testing.T) {
	prepared, bridge := preparedBody(t, `{"model":"gpt-5.5","input":"find a tool","tools":[`+toolSearchTool+`]}`, &ReplayCache{})
	protocol := protocolText(t, prepared)
	if !strings.Contains(protocol, `Client tool "tool_search" (function).`) || !strings.Contains(protocol, "tool_search loads more tools") {
		t.Fatalf("tool_search missing from the catalog: %s", protocol)
	}
	if !strings.Contains(protocol, "- GitHub: Work with repositories, issues and pull requests.") || strings.Contains(protocol, "secret scanning") {
		t.Fatalf("source line not shortened to its opening sentences: %s", protocol)
	}
	if len(bridge.Warnings) != 0 || bridge.tools[toolSearchKind].Kind != toolSearchKind {
		t.Fatalf("client tool_search treated as unsupported: %v", bridge.Warnings)
	}

	hosted, bridge := preparedBody(t, `{"model":"gpt-5.5","input":"x","tools":[{"type":"tool_search","execution":"server"}]}`, &ReplayCache{})
	if !strings.Contains(protocolText(t, hosted), "Hosted tools unavailable through Basispoints: tool_search") || bridge.tools[toolSearchKind].Kind != "" {
		t.Fatalf("hosted tool_search was not reported as unavailable: %v", bridge.Warnings)
	}
}

func TestToolSearchCallReachesCodexAndReplaysAsItsNativeCall(t *testing.T) {
	replay := &ReplayCache{}
	request := `{"model":"gpt-5.5","input":"find a tool","tools":[` + toolSearchTool + `]}`
	_, bridge := preparedBody(t, request, replay)
	native := transportCall("call_s", toolSearchKind, `{"query":"github","limit":"3"}`)
	out := readStream(t, bridge, io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":["+native+"]}}\n\n")))
	if strings.Contains(out, "function_call_arguments") || strings.Count(out, `"type":"tool_search_call"`) < 2 {
		t.Fatalf("tool_search_call was not streamed as a finished item: %s", out)
	}
	items := streamItems(t, out)
	if len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	call := items[0]
	arguments, _ := json.Marshal(call["arguments"])
	if call["type"] != toolSearchCallType || call["execution"] != "client" || call["call_id"] != "call_s" || string(arguments) != `{"limit":3,"query":"github"}` {
		t.Fatalf("client call = %v", call)
	}

	// Codex echoes the call and adds its result; the native run_officejs item
	// replays exactly, and the loaded tool becomes callable.
	history := `[{"type":"message","role":"user","content":[{"type":"input_text","text":"find a tool"}]},` +
		`{"type":"tool_search_call","id":"` + text(call["id"]) + `","call_id":"call_s","execution":"client","status":"completed","arguments":{"query":"github","limit":3}},` +
		`{"type":"tool_search_output","call_id":"call_s","execution":"client","status":"completed","tools":` + loadedEchoTools + `}]`
	prepared, bridge := preparedBody(t, `{"model":"gpt-5.5","input":`+history+`,"tools":[`+toolSearchTool+`]}`, replay)
	input := prepared["input"].([]any)
	replayed, output := input[len(input)-2].(object), input[len(input)-1].(object)
	if replayed["id"] != "fc_call_s" || replayed["name"] != "run_officejs" {
		t.Fatalf("native call was not replayed: %v", replayed)
	}
	result := text(output["output"])
	if output["type"] != "function_call_output" || output["call_id"] != "call_s" ||
		!strings.Contains(result, `Client tool "mcp__fake__.echo" (function). Echo text back to the caller.`) {
		t.Fatalf("search result not replayed as the loaded tools: %v", output)
	}
	if meta := prepared["metadata"].(object); meta["agent_iteration"] != "2" {
		t.Fatalf("search result did not count as a round of results: %v", meta)
	}
	if strings.Contains(protocolText(t, prepared), "mcp__fake__") {
		t.Fatal("loaded tool was added to the catalog prefix")
	}
	echo := readStream(t, bridge, io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":["+
		transportCall("call_e", "mcp__fake__.echo", `{"text":"hi"}`)+"]}}\n\n")))
	if items := streamItems(t, echo); len(items) != 1 || items[0]["name"] != "echo" || items[0]["namespace"] != "mcp__fake__" {
		t.Fatalf("loaded tool was not relayed: %s", echo)
	}
}

func TestToolSearchHistoryIsRebuiltWithoutItsNativeCall(t *testing.T) {
	history := `[{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]},` +
		`{"type":"tool_search_call","call_id":"call_lost","execution":"client","arguments":"{\"query\":\"mail\"}"},` +
		`{"type":"tool_search_output","call_id":"call_lost","execution":"client","tools":[]}]`
	prepared, _ := preparedBody(t, `{"model":"gpt-5.5","input":`+history+`,"tools":[`+toolSearchTool+`]}`, &ReplayCache{})
	input := prepared["input"].([]any)
	rebuilt, output := input[len(input)-2].(object), input[len(input)-1].(object)
	var arguments object
	if err := decode([]byte(text(rebuilt["arguments"])), &arguments); err != nil {
		t.Fatalf("rebuilt arguments: %v", err)
	}
	if rebuilt["name"] != "run_officejs" || text(arguments["code"]) != `{"arguments":{"query":"mail"},"name":"tool_search"}` {
		t.Fatalf("rebuilt call = %v", rebuilt)
	}
	if text(output["output"]) != "tool_search found no matching tools." {
		t.Fatalf("empty result = %v", output)
	}
}

func TestToolSearchRejectsQuerylessCallsAndHonoursToolChoice(t *testing.T) {
	_, bridge := preparedBody(t, `{"model":"gpt-5.5","input":"x","tools":[`+toolSearchTool+`]}`, &ReplayCache{})
	out := readStream(t, bridge, io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":["+
		transportCall("call_q", toolSearchKind, `{"limit":2}`)+"]}}\n\n")))
	if !strings.Contains(out, `"type":"response.failed"`) || strings.Contains(out, toolSearchCallType) {
		t.Fatalf("query-less search was relayed: %s", out)
	}
	forced, _ := preparedBody(t, `{"model":"gpt-5.5","input":"x","tools":[`+toolSearchTool+`],"tool_choice":{"type":"tool_search"}}`, &ReplayCache{})
	if !strings.Contains(protocolText(t, forced), `call client tool "tool_search"`) {
		t.Fatalf("tool_choice tool_search not enforced: %s", protocolText(t, forced))
	}
	off, bridge := preparedBody(t, `{"model":"gpt-5.5","input":[{"type":"tool_search_call","call_id":"s","arguments":{"query":"q"}},{"type":"tool_search_output","call_id":"s","tools":`+loadedEchoTools+`}],"tool_choice":"none"}`, &ReplayCache{})
	if _, loaded := bridge.tools["mcp__fake__.echo"]; loaded || off == nil {
		t.Fatal("tool_choice none still made a searched tool callable")
	}
}

const mcpTools = `"tools":[` +
	`{"type":"function","name":"exec_command","description":"Runs a command. It returns the output.","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},` +
	`{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","description":"Start a helper agent with a task. The helper works on its own and reports back.","parameters":{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}}]},` +
	`{"type":"namespace","name":"mcp__tracker__","tools":[{"type":"function","name":"create_issue",` +
	`"description":"Create an issue in the tracker. The issue gets a number, a title, labels and an assignee; it notifies watchers, links commits and applies workflow rules configured by an administrator for the project.",` +
	`"parameters":{"$schema":"http://json-schema.org/draft-07/schema#","title":"CreateIssue","type":"object","additionalProperties":false,"required":["title"],` +
	`"properties":{"title":{"type":"string","description":"Short title of the issue. Keep it under eighty characters please, the tracker truncates it."},` +
	`"labels":{"type":"array","items":{"type":"string"}},"priority":{"enum":["low","high"]},"assignee":{"type":"object","properties":{"login":{"type":"string"}},"required":["login"]}}}}]}]`

func TestCatalogSummarizesAppAndMCPToolsOnly(t *testing.T) {
	prepared, bridge := preparedBody(t, `{"model":"gpt-5.5","input":"x",`+mcpTools+`}`, &ReplayCache{})
	protocol := protocolText(t, prepared)
	want := `Client tool "mcp__tracker__.create_issue" (function, summarized). Create an issue in the tracker. Parameters: ` +
		`{"assignee":"{login: string}","labels":"string[]","priority":"\"low\"|\"high\"","title":"string, required: Short title of the issue."}.`
	if !strings.Contains(protocol, want) {
		t.Fatalf("MCP tool not summarized as expected:\nwant %s\nin   %s", want, protocol)
	}
	for _, full := range []string{"Runs a command. It returns the output.", "The helper works on its own and reports back."} {
		if !strings.Contains(protocol, full) {
			t.Fatalf("core or collaboration tool was summarized (missing %q): %s", full, protocol)
		}
	}
	if strings.Contains(protocol, "workflow rules") || strings.Contains(protocol, "CreateIssue") || !strings.Contains(protocol, "A summarized catalog entry") {
		t.Fatalf("summary leaked the full definition or lacks its note: %s", protocol)
	}
	if !bridge.tools["mcp__tracker__.create_issue"].Summarized || bridge.tools["collaboration.spawn_agent"].Summarized {
		t.Fatalf("Summarized flags wrong: %+v", bridge.tools)
	}
}

func TestMismatchedCallOfSummarizedToolGetsItsFullDefinition(t *testing.T) {
	call := func(arguments string) string {
		return `[{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]},` +
			`{"type":"function_call","call_id":"c1","namespace":"mcp__tracker__","name":"create_issue","arguments":` + arguments + `},` +
			`{"type":"function_call_output","call_id":"c1","output":"invalid params"}]`
	}
	output := func(arguments string) string {
		prepared, _ := preparedBody(t, `{"model":"gpt-5.5","input":`+call(arguments)+`,`+mcpTools+`}`, &ReplayCache{})
		input := prepared["input"].([]any)
		return text(input[len(input)-1].(object)["output"])
	}
	got := output(`"{\"name\":\"bug\",\"labels\":\"x\"}"`)
	for _, want := range []string{"invalid params\n\nGateway note:", `missing required \"title\"`, `undeclared \"name\"`, `\"labels\" should be array`, "workflow rules configured by an administrator"} {
		if !strings.Contains(got, strings.ReplaceAll(want, `\"`, `"`)) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	if got := output(`{"title":"bug","labels":["x"]}`); got != "invalid params" {
		t.Fatalf("matching call got a note: %s", got)
	}
}

func TestLeadingSentences(t *testing.T) {
	cases := []struct {
		in    string
		limit int
		want  string
	}{
		{"Short.", 20, "Short."},
		{"Use e.g. the flag. Then more text follows here.", 30, "Use e.g. the flag."},
		{"创建问题。然后通知关注者并应用工作流规则。", 8, "创建问题。"},
		{"averyveryverylongwordwithoutanybreaks", 12, "averyvery..."},
		{"Version v1.2 is out. More.", 22, "Version v1.2 is out."},
	}
	for _, tc := range cases {
		if got := leadingSentences(tc.in, tc.limit); got != tc.want {
			t.Fatalf("leadingSentences(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}
	}
}

func TestLeakedRelayCallIsReplayedWithoutNesting(t *testing.T) {
	leaked := `{"code":"{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"ls\"}}","summary":"s"}`
	history := `[{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]},` +
		`{"type":"function_call","id":"fc_leak","call_id":"c1","name":"run_officejs","arguments":` + quoted(leaked) + `},` +
		`{"type":"function_call_output","call_id":"c1","output":"unsupported call: run_officejs"}]`
	prepared, _ := preparedBody(t, `{"model":"gpt-5.5","input":`+history+`,`+execCommandTools+`}`, &ReplayCache{})
	input := prepared["input"].([]any)
	replayed := input[len(input)-2].(object)
	if replayed["id"] != "fc_leak" || replayed["name"] != "run_officejs" || replayed["arguments"] != leaked {
		t.Fatalf("leaked relay call was rewrapped: %v", replayed)
	}
}
