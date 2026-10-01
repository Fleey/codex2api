package basispoints

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Codex's tool discovery. For a model whose catalog supports it, Codex keeps
// app and MCP tools out of the request and declares only a client-executed
// tool_search; the tools a search finds come back in its result and stay
// callable from then on, though the request's tools never list them. BPS
// takes no client tools, so the search is relayed through run_officejs like
// any catalog tool and its result is replayed as that call's text output.
const (
	toolSearchKind       = "tool_search"
	toolSearchCallType   = "tool_search_call"
	toolSearchOutputType = "tool_search_output"
	// toolSearchCallOutputType is the result's name in some client versions.
	toolSearchCallOutputType = "tool_search_call_output"
	// searchSourceRunes bounds each source (app, MCP server) tool_search's
	// description quotes; Codex quotes what each one says of itself in full.
	searchSourceRunes = 240
)

// defaultToolSearchParameters stands in when the client declares none.
var defaultToolSearchParameters = object{
	"type": "object",
	"properties": object{
		"query": object{"type": "string", "description": "What the needed tool should do."},
		"limit": object{"type": "integer", "description": "Most tools to load."},
	},
	"required": []any{"query"},
}

func isClientExecution(item object) bool {
	execution := text(item["execution"])
	return execution == "" || execution == "client"
}

func isToolSearchCall(item object) bool {
	return text(item["type"]) == toolSearchCallType
}

func isToolSearchOutput(item object) bool {
	kind := text(item["type"])
	return kind == toolSearchOutputType || kind == toolSearchCallOutputType
}

// registerToolSearch adds the client's tool_search to the catalog as a
// function the model calls through run_officejs. A client function already
// named tool_search keeps the name.
func (b *Bridge) registerToolSearch(item object) (object, bool) {
	if _, taken := b.tools[toolSearchKind]; taken {
		return nil, false
	}
	parameters, _ := item["parameters"].(object)
	if parameters == nil {
		parameters = defaultToolSearchParameters
	}
	description := text(item["description"])
	info := tool{Name: toolSearchKind, Kind: toolSearchKind, Definition: fingerprint(item), Parameters: parameters,
		Description: description, RawField: rawFieldFor(parameters)}
	entry := object{"type": "function", "name": toolSearchKind, "parameters": parameters}
	if description != "" {
		entry["description"] = searchDescription(description)
	}
	if info.RawField != "" {
		entry[catalogRawFieldKey] = info.RawField
	}
	b.tools[toolSearchKind] = info
	return entry, true
}

// searchDescription shortens each "- source: ..." line of tool_search's
// description to its opening sentences.
func searchDescription(description string) string {
	lines := strings.Split(description, "\n")
	for i, line := range lines {
		if rest, ok := strings.CutPrefix(line, "- "); ok {
			lines[i] = "- " + leadingSentences(rest, searchSourceRunes)
		}
	}
	return strings.Join(lines, "\n")
}

// toolSearchArguments returns tool_search's arguments as Codex reads them: a
// nonempty query and, when given, a whole positive limit. A quoted number is
// accepted because the model writes the arguments inside JSON text.
func toolSearchArguments(value any) (object, error) {
	if raw, ok := value.(string); ok {
		if decode([]byte(raw), &value) != nil {
			return nil, fmt.Errorf("basispoints tool_search arguments are invalid JSON")
		}
	}
	args, ok := value.(object)
	if !ok {
		return nil, fmt.Errorf("basispoints tool_search arguments must be an object")
	}
	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("basispoints tool_search requires a nonempty query")
	}
	search := object{"query": query}
	var limit json.Number
	switch v := args["limit"].(type) {
	case json.Number:
		limit = v
	case string:
		limit = json.Number(strings.TrimSpace(v))
	}
	if n, err := limit.Int64(); err == nil && n >= 1 {
		search["limit"] = json.Number(strconv.FormatInt(n, 10))
	}
	return search, nil
}

// toolSearchCallItem is the client's tool_search_call for a relayed search.
func toolSearchCallItem(callID string, value any) (object, error) {
	search, err := toolSearchArguments(value)
	if err != nil {
		return nil, err
	}
	return object{
		"type": toolSearchCallType, "id": "tsc_" + fingerprint(callID), "call_id": callID,
		"execution": "client", "arguments": search, "status": "completed",
	}, nil
}

// toolSearchCallFingerprint identifies a tool_search call by its call ID and
// normalized arguments, mirroring historyCallFingerprint.
func toolSearchCallFingerprint(item object) string {
	id := text(item["call_id"])
	if id == "" || strings.TrimSpace(id) != id {
		return ""
	}
	search, err := toolSearchArguments(item["arguments"])
	if err != nil {
		return ""
	}
	return fingerprint(object{"type": toolSearchCallType, "call_id": id, "arguments": search})
}

// toolSearchOutputText renders a tool_search result for the model: the full
// definition of every tool it loaded, which become callable for this request.
// The prompt catalog stays as it was, so the cached prefix still matches.
func (b *Bridge) toolSearchOutputText(item object) string {
	entries := b.loadSearchedTools(item["tools"])
	if len(entries) == 0 {
		if output := strings.TrimSpace(text(item["output"])); output != "" {
			return output
		}
		return "tool_search found no matching tools."
	}
	return "tool_search loaded these tools. Call them through run_officejs like catalog tools, by the name given here:\n\n" +
		describeCatalog(entries)
}

// loadSearchedTools registers the tools a search loaded, without letting
// them displace a declared tool, and returns their catalog entries. Results
// are client history, so a malformed one is skipped rather than failing
// every later turn of the conversation.
func (b *Bridge) loadSearchedTools(value any) []any {
	if value == nil {
		return nil
	}
	scratch := &Bridge{tools: make(map[string]tool), unsupportedTools: make(map[string]bool)}
	entries, err := scratch.collectTools(value, "")
	if err != nil {
		b.logf("ignored the tools a tool_search loaded: %v", err)
		return nil
	}
	if b.toolsOff {
		return entries
	}
	for key, info := range scratch.tools {
		if _, declared := b.tools[key]; declared {
			continue
		}
		if b.allowedTools != nil && !b.allowedTools[key] && !b.allowedTools[info.Name] {
			continue
		}
		b.tools[key] = info
	}
	return entries
}
