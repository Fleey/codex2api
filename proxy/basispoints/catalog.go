package basispoints

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// describeCatalog presents tool contracts as documentation, not native tool definitions.
func describeCatalog(catalog []any) string {
	var lines []string
	for _, raw := range catalog {
		// collectTools constructs every catalog entry as an object.
		entry, _ := raw.(object)
		if summarized, _ := entry[catalogSummaryKey].(bool); summarized {
			lines = append(lines, describeSummary(entry))
			continue
		}
		line := "Client tool " + quoted(entry["name"]) + " (" + text(entry["type"]) + ")."
		if description := text(entry["description"]); description != "" {
			line += " " + description
		}
		if text(entry["type"]) == "custom" {
			line += " Set run_officejs summary to " + quoted("codex2api.custom/"+text(entry["name"])) + " and pass its exact raw text directly in code."
			if format := entry["format"]; format != nil {
				line += " Input format: " + quoted(format) + "."
			}
		} else {
			line += " Pass a JSON object in the envelope's arguments field. Argument contract: " + describeSchema(entry["parameters"], 0) +
				rawTransportNote(entry)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n\n")
}

// rawTransportNote tells how to fill an entry's raw field, if it has one.
func rawTransportNote(entry object) string {
	field := text(entry[catalogRawFieldKey])
	if field == "" {
		return ""
	}
	value := "the exact raw " + quoted(field) + " value"
	if list, _ := entry[catalogRawListKey].(bool); list {
		value = "exactly one raw " + quoted(field) + " entry (one command line)"
	}
	return " Raw transport: set run_officejs summary to " + quoted(rawFieldTransportPrefix+text(entry["name"])+"/"+field) +
		" and put " + value + " directly in code; other arguments keep their defaults."
}

func quoted(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// Preserve constraints that are not expanded into prose, including schema references.
func describeSchema(value any, depth int) string {
	schema, ok := value.(object)
	if !ok || depth >= 8 {
		if value == nil {
			return "Use the arguments described by the tool."
		}
		return quoted(value)
	}
	var parts []string
	if kind := schema["type"]; kind != nil {
		parts = append(parts, "Value type: "+quoted(kind)+".")
	}
	if description := text(schema["description"]); description != "" {
		parts = append(parts, description)
	}
	required := make(map[string]bool)
	if names, ok := schema["required"].([]any); ok {
		for _, name := range names {
			required[text(name)] = true
		}
	}
	if properties, ok := schema["properties"].(object); ok {
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			presence := "optional"
			if required[name] {
				presence = "required"
			}
			parts = append(parts, fmt.Sprintf("Field %s (%s): %s", quoted(name), presence, describeSchema(properties[name], depth+1)))
		}
	}
	if items := schema["items"]; items != nil {
		parts = append(parts, "Each array item: "+describeSchema(items, depth+1))
	}
	constraints := make(object)
	for key, value := range schema {
		switch key {
		// title and $schema only label the schema for validators.
		case "type", "description", "properties", "items", "title", "$schema":
		default:
			constraints[key] = value
		}
	}
	if len(constraints) > 0 {
		parts = append(parts, "Additional constraints: "+quoted(constraints)+".")
	}
	if len(parts) == 0 {
		return "Any JSON value."
	}
	return strings.Join(parts, " ")
}
