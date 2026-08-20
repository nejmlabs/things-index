// Package toolschema registers MCP tools with schemas normalized for maximum
// client compatibility. Both ThingsIndex servers (the HTTP queue server and
// the stdio server) must register through it so their advertised schemas
// stay identical.
package toolschema

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AddTool registers a tool with explicitly normalized input and output
// schemas. jsonschema-go infers optional pointer and slice fields as type
// ["null","object"]/["null","array"]; several MCP client runtimes only accept
// a single type string when converting tool schemas to their LLM's
// function-call format, and reject the whole tool otherwise — surfacing as
// "invalid tool call" for every use (observed live with Pebble Index).
// Optionality is already conveyed by the field's absence from "required", so
// the null member carries no information.
func AddTool[In, Out any](server *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) error {
	inputSchema, err := jsonschema.For[In](nil)
	if err != nil {
		return fmt.Errorf("infer input schema for %s: %w", tool.Name, err)
	}
	StripNullTypes(inputSchema)
	tool.InputSchema = inputSchema

	outputSchema, err := jsonschema.For[Out](nil)
	if err != nil {
		return fmt.Errorf("infer output schema for %s: %w", tool.Name, err)
	}
	StripNullTypes(outputSchema)
	tool.OutputSchema = outputSchema

	mcp.AddTool(server, tool, handler)
	return nil
}

// StripNullTypes collapses ["null", T] type arrays to plain T everywhere in
// the schema. It walks every construct jsonschema-go emits today plus the
// combinators, so a future Go type that renders through $defs or anyOf
// cannot silently reintroduce the arrays. A null-only type is left alone:
// there is nothing sensible to collapse it to.
func StripNullTypes(schema *jsonschema.Schema) {
	if schema == nil {
		return
	}
	if len(schema.Types) > 0 {
		kept := make([]string, 0, len(schema.Types))
		for _, entry := range schema.Types {
			if entry != "null" {
				kept = append(kept, entry)
			}
		}
		switch len(kept) {
		case 0:
			// null-only: leave as-is
		case 1:
			schema.Type, schema.Types = kept[0], nil
		default:
			schema.Types = kept
		}
	}
	for _, property := range schema.Properties {
		StripNullTypes(property)
	}
	for _, property := range schema.PatternProperties {
		StripNullTypes(property)
	}
	for _, definition := range schema.Defs {
		StripNullTypes(definition)
	}
	for _, sub := range schema.AnyOf {
		StripNullTypes(sub)
	}
	for _, sub := range schema.OneOf {
		StripNullTypes(sub)
	}
	for _, sub := range schema.AllOf {
		StripNullTypes(sub)
	}
	for _, sub := range schema.PrefixItems {
		StripNullTypes(sub)
	}
	StripNullTypes(schema.Items)
	StripNullTypes(schema.AdditionalProperties)
	StripNullTypes(schema.Contains)
	StripNullTypes(schema.Not)
}
