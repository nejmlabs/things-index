package toolschema

import (
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestStripNullTypesCollapsesEverywhere(t *testing.T) {
	schema := &jsonschema.Schema{
		Types: []string{"null", "object"},
		Properties: map[string]*jsonschema.Schema{
			"direct": {Types: []string{"null", "array"}, Items: &jsonschema.Schema{Types: []string{"null", "string"}}},
		},
		PatternProperties: map[string]*jsonschema.Schema{
			"^x-": {Types: []string{"null", "integer"}},
		},
		Defs: map[string]*jsonschema.Schema{
			"Shared": {Types: []string{"null", "object"}},
		},
		AnyOf:       []*jsonschema.Schema{{Types: []string{"null", "string"}}},
		OneOf:       []*jsonschema.Schema{{Types: []string{"null", "number"}}},
		AllOf:       []*jsonschema.Schema{{Types: []string{"null", "boolean"}}},
		PrefixItems: []*jsonschema.Schema{{Types: []string{"null", "string"}}},
		Contains:    &jsonschema.Schema{Types: []string{"null", "object"}},
		Not:         &jsonschema.Schema{Types: []string{"null", "object"}},
	}
	StripNullTypes(schema)

	var assertSingle func(location string, node *jsonschema.Schema)
	assertSingle = func(location string, node *jsonschema.Schema) {
		if node == nil {
			return
		}
		if len(node.Types) != 0 {
			t.Errorf("%s: type array survived: %v", location, node.Types)
		}
		if node.Type == "null" || node.Type == "" {
			t.Errorf("%s: collapsed to %q", location, node.Type)
		}
	}
	assertSingle("root", schema)
	assertSingle("properties.direct", schema.Properties["direct"])
	assertSingle("properties.direct.items", schema.Properties["direct"].Items)
	assertSingle("patternProperties", schema.PatternProperties["^x-"])
	assertSingle("$defs.Shared", schema.Defs["Shared"])
	assertSingle("anyOf[0]", schema.AnyOf[0])
	assertSingle("oneOf[0]", schema.OneOf[0])
	assertSingle("allOf[0]", schema.AllOf[0])
	assertSingle("prefixItems[0]", schema.PrefixItems[0])
	assertSingle("contains", schema.Contains)
	assertSingle("not", schema.Not)
}

func TestStripNullTypesEdgeCases(t *testing.T) {
	nullOnly := &jsonschema.Schema{Types: []string{"null"}}
	StripNullTypes(nullOnly)
	if len(nullOnly.Types) != 1 || nullOnly.Types[0] != "null" {
		t.Errorf("null-only type was altered: %v", nullOnly.Types)
	}

	multi := &jsonschema.Schema{Types: []string{"null", "string", "integer"}}
	StripNullTypes(multi)
	if len(multi.Types) != 2 || multi.Types[0] != "string" || multi.Types[1] != "integer" {
		t.Errorf("multi-type collapse wrong: %v", multi.Types)
	}

	StripNullTypes(nil) // must not panic
}
