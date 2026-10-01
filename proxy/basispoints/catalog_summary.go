package basispoints

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode"
)

// The catalog sits in every request's prompt, and BPS accounts share one
// tokens-per-minute budget. Codex's own tools (no namespace) and the
// namespaces below are quoted in full; the desktop app's tools, plugins and
// MCP servers are summarized: their opening sentences and each parameter's
// type. A call that does not match a summarized tool gets its full definition
// with the result, so the model can correct the next call.
var fullNamespaces = map[string]bool{"collaboration": true, "image_gen": true, "web": true}

const (
	// catalogSummaryKey marks a catalog entry describeCatalog summarizes; it
	// is never sent upstream as tool schema.
	catalogSummaryKey = "_codex2api_summary"

	summaryDescriptionRunes = 160
	summaryParameterRunes   = 60
	// mismatchProblems bounds the problems named for one mismatched call.
	mismatchProblems = 6
)

// summarizeCatalog marks the namespaced function tools outside fullNamespaces
// for summary in the prompt catalog.
func (b *Bridge) summarizeCatalog(catalog []any) {
	for _, raw := range catalog {
		entry, _ := raw.(object)
		key := text(entry["name"])
		info, ok := b.tools[key]
		if !ok || info.Kind != "function" || info.Namespace == "" {
			continue
		}
		top, _, _ := strings.Cut(info.Namespace, ".")
		if fullNamespaces[top] {
			continue
		}
		entry[catalogSummaryKey] = true
		info.Summarized = true
		b.tools[key] = info
	}
}

// catalogNotes explains the catalog's summarized entries and tool_search,
// when it has them.
func catalogNotes(catalog []any) string {
	var notes string
	summarized := slices.ContainsFunc(catalog, func(raw any) bool {
		entry, _ := raw.(object)
		marked, _ := entry[catalogSummaryKey].(bool)
		return marked
	})
	if summarized {
		notes += "A summarized catalog entry gives each parameter as \"type[, required]: meaning\" (a ? after a field name marks it optional); " +
			"the result of a call that does not match such a tool includes its full definition. "
	}
	searchable := slices.ContainsFunc(catalog, func(raw any) bool {
		entry, _ := raw.(object)
		return text(entry["name"]) == toolSearchKind
	})
	if searchable {
		notes += "tool_search loads more tools (such as app and MCP tools) that this catalog leaves out; its result defines the tools it loaded, " +
			"which you then call through run_officejs like catalog tools, by the full name it gives. "
	}
	return notes
}

// describeSummary is the catalog line of a summarized function tool.
func describeSummary(entry object) string {
	line := "Client tool " + quoted(entry["name"]) + " (function, summarized)."
	if description := leadingSentences(text(entry["description"]), summaryDescriptionRunes); description != "" {
		line += " " + description
	}
	parameters, _ := entry["parameters"].(object)
	return line + " Parameters: " + parameterSummary(parameters) + "." + rawTransportNote(entry)
}

// fullEntry is the unsummarized catalog entry of a summarized tool.
func fullEntry(info tool) object {
	entry := object{"type": "function", "name": info.key(), "parameters": info.Parameters}
	if info.Description != "" {
		entry["description"] = info.Description
	}
	if info.RawField != "" {
		entry[catalogRawFieldKey] = info.RawField
	}
	return entry
}

// leadingSentences returns the whole sentences value starts with that fit in
// limit runes, or, when even the first does not fit, its start cut to fit.
func leadingSentences(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	kept := 0
	for i := 0; i < limit; i++ {
		switch runes[i] {
		case '。', '！', '？':
			kept = i + 1
		case '.', '!', '?':
			// A sentence ends before a space and a character that is not a
			// lowercase letter, so "e.g. the" and "v1.2" do not end one.
			if i+2 < len(runes) && runes[i+1] == ' ' && !unicode.IsLower(runes[i+2]) {
				kept = i + 1
			}
		}
	}
	if kept > 0 {
		return string(runes[:kept])
	}
	return strings.TrimRight(string(runes[:limit-3]), " ") + "..."
}

// typeSummary is a short type for schema: string, number[], "a"|"b",
// {name: string; size?: number}.
func typeSummary(value any, depth int) string {
	schema, ok := value.(object)
	if !ok || depth > 4 {
		return "any"
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		parts := make([]string, len(values))
		for i, v := range values {
			parts[i] = quoted(v)
		}
		return strings.Join(parts, "|")
	}
	if value, ok := schema["const"]; ok {
		return quoted(value)
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if variants, ok := schema[key].([]any); ok && len(variants) > 0 {
			parts := make([]string, len(variants))
			for i, variant := range variants {
				parts[i] = typeSummary(variant, depth+1)
			}
			return strings.Join(parts, "|")
		}
	}
	switch kind := schema["type"].(type) {
	case string:
		switch kind {
		case "array":
			return typeSummary(schema["items"], depth+1) + "[]"
		case "object":
			if fields := objectSummary(schema, depth); fields != "" {
				return fields
			}
		}
		return kind
	case []any:
		parts := make([]string, 0, len(kind))
		for _, part := range kind {
			parts = append(parts, text(part))
		}
		return strings.Join(parts, "|")
	}
	if fields := objectSummary(schema, depth); fields != "" {
		return fields
	}
	return "any"
}

// objectSummary is {name: type; optional?: type} for an object schema with
// properties, or "" without them.
func objectSummary(schema object, depth int) string {
	properties, _ := schema["properties"].(object)
	if len(properties) == 0 {
		return ""
	}
	required := requiredNames(schema)
	names := sortedKeys(properties)
	fields := make([]string, len(names))
	for i, name := range names {
		marker := "?"
		if required[name] {
			marker = ""
		}
		fields[i] = name + marker + ": " + typeSummary(properties[name], depth+1)
	}
	return "{" + strings.Join(fields, "; ") + "}"
}

// parameterSummary gives each top-level parameter as
// "type[, required][: the start of its description]".
func parameterSummary(schema object) string {
	properties, _ := schema["properties"].(object)
	required := requiredNames(schema)
	summary := make(map[string]string, len(properties))
	for name, raw := range properties {
		line := typeSummary(raw, 0)
		if required[name] {
			line += ", required"
		}
		nested, _ := raw.(object)
		if description := leadingSentences(text(nested["description"]), summaryParameterRunes); description != "" {
			line += ": " + description
		}
		summary[name] = line
	}
	// json.Marshal sorts the keys, so the prompt stays byte-identical.
	encoded, _ := json.Marshal(summary)
	return string(encoded)
}

func requiredNames(schema object) map[string]bool {
	names, _ := schema["required"].([]any)
	required := make(map[string]bool, len(names))
	for _, name := range names {
		required[text(name)] = true
	}
	return required
}

func sortedKeys(value object) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// mismatchNote returns, for a client call of a summarized tool whose
// arguments do not match its schema, the note that gives the model the full
// definition with the call's result; "" otherwise. It depends only on the
// history and the catalog, so every replay of the conversation reads the same.
func (b *Bridge) mismatchNote(item object) string {
	if text(item["type"]) != "function_call" {
		return ""
	}
	key := text(item["name"])
	if namespace := text(item["namespace"]); namespace != "" {
		key = namespace + "." + key
	}
	info, ok := b.tools[key]
	if !ok || !info.Summarized {
		return ""
	}
	arguments := item["arguments"]
	if raw, ok := arguments.(string); ok && decode([]byte(raw), &arguments) != nil {
		arguments = nil
	}
	problems := argumentMismatch(info.Parameters, arguments)
	if problems == "" {
		return ""
	}
	return "Gateway note: this call did not match the parameters of " + quoted(key) + " (" + problems + "). " +
		"The catalog only summarizes that tool; its full definition is:\n" + describeCatalog([]any{fullEntry(info)})
}

// argumentMismatch names what in arguments contradicts the top level of
// schema: a missing required field, an undeclared field, or a wrongly typed
// one. It is a hint for the model, not validation; the client still checks.
func argumentMismatch(schema object, value any) string {
	args, ok := value.(object)
	if !ok {
		return "the arguments were not a JSON object"
	}
	properties, _ := schema["properties"].(object)
	var problems []string
	required := make([]string, 0)
	for name := range requiredNames(schema) {
		required = append(required, name)
	}
	slices.Sort(required)
	for _, name := range required {
		if _, present := args[name]; !present && name != "" {
			problems = append(problems, "missing required "+quoted(name))
		}
	}
	open := len(properties) == 0 || schema["patternProperties"] != nil
	switch extra := schema["additionalProperties"].(type) {
	case bool:
		open = open || extra
	case object:
		open = true
	}
	for _, name := range sortedKeys(args) {
		property, declared := properties[name].(object)
		switch {
		case !declared && !open:
			problems = append(problems, "undeclared "+quoted(name))
		case declared && args[name] != nil && !jsonTypeMatches(text(property["type"]), args[name]):
			problems = append(problems, quoted(name)+" should be "+text(property["type"]))
		}
	}
	if len(problems) > mismatchProblems {
		problems = problems[:mismatchProblems]
	}
	return strings.Join(problems, "; ")
}

// jsonTypeMatches reports whether value has the JSON Schema type want; an
// unknown or absent type matches anything.
func jsonTypeMatches(want string, value any) bool {
	switch want {
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := number.Int64()
		return err == nil
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(object)
		return ok
	}
	return true
}

// appendOutputNote adds note to a tool result, as text after a string result
// or as one more text part of a content array.
func appendOutputNote(output any, note string) any {
	switch v := output.(type) {
	case string:
		return v + "\n\n" + note
	case []any:
		return append(v, object{"type": "input_text", "text": note})
	}
	return output
}
