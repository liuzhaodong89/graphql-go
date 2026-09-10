package graphql

import (
	"context"
	"testing"

	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

func executeParentMaterialization(t *testing.T, schema Schema, query string, variables map[string]any) *Result {
	t.Helper()
	document, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(query), Name: "parent-materialization"})})
	if err != nil {
		t.Fatal(err)
	}
	validation := ValidateDocument(&schema, document, nil)
	if !validation.IsValid {
		t.Fatalf("validation failed: %v", validation.Errors)
	}
	engine, err := NewSGraphEngine(&schema, NewDirectiveRegistry(), NewParamRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return engine.Execute(document, variables, nil, nil, context.Background()).toGraphQLResult()
}

func parentMaterializationSchema(t *testing.T) Schema {
	t.Helper()
	query := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"hello": &Field{Type: String, Resolve: func(ResolveParams) (any, error) { return "world", nil }},
	}})
	schema, err := NewSchema(SchemaConfig{Query: query})
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestParentMaterializationIntrospection(t *testing.T) {
	schema := parentMaterializationSchema(t)
	tests := []struct {
		name  string
		query string
		vars  map[string]any
	}{
		{"schema-types", `{ __schema { types { __typename name } } }`, nil},
		{"type-fields-default", `{ __type(name:"Query") { fields { __typename name } } }`, nil},
		{"type-fields-explicit", `{ __type(name:"Query") { fields(includeDeprecated:true) { __typename name } } }`, nil},
		{"type-fields-variable", `query Q($include:Boolean!){ __type(name:"Query") { fields(includeDeprecated:$include) { __typename name } } }`, map[string]any{"include": true}},
		{"schema-types-alias", `{ __schema { myTypes: types { __typename name } } }`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeParentMaterialization(t, schema, tc.query, tc.vars)
			if len(result.Errors) != 0 {
				t.Fatalf("errors: %#v", result.Errors)
			}
			if result.Data == nil {
				t.Fatal("data is nil")
			}
			plain := toPlainValue(result.Data).(map[string]any)
			if tc.name == "schema-types-alias" {
				schemaValue := plain["__schema"].(map[string]any)
				types, ok := schemaValue["myTypes"].([]any)
				if !ok || len(types) == 0 {
					t.Fatalf("aliased introspection types missing: %#v", plain)
				}
			}
			if tc.name == "type-fields-default" || tc.name == "type-fields-explicit" || tc.name == "type-fields-variable" {
				typeValue := plain["__type"].(map[string]any)
				fields, ok := typeValue["fields"].([]any)
				if !ok || len(fields) == 0 {
					t.Fatalf("introspection fields missing: %#v", plain)
				}
			}
		})
	}
}

func TestParentMaterializationParentKeyAndNullParent(t *testing.T) {
	item := NewObject(ObjectConfig{Name: "ParentMaterializationItem", Fields: Fields{"name": &Field{Type: String}}})
	holder := NewObject(ObjectConfig{Name: "ParentMaterializationHolder", Fields: Fields{"items": &Field{Type: NewList(item)}}})
	parent := NewObject(ObjectConfig{Name: "ParentMaterializationParent", Fields: Fields{
		"id":     &Field{Type: NewNonNull(ID)},
		"holder": &Field{Type: NewNonNull(holder)},
	}})
	query := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"parents": &Field{Type: NewList(parent), Resolve: func(ResolveParams) (any, error) {
			return []any{map[string]any{"holder": map[string]any{"items": []any{map[string]any{"name": "one"}}}}}, nil
		}},
		"nullable": &Field{Type: parent, Resolve: func(ResolveParams) (any, error) { return nil, nil }},
	}})
	schema, err := NewSchema(SchemaConfig{Query: query})
	if err != nil {
		t.Fatal(err)
	}

	result := executeParentMaterialization(t, schema, `{ parents { holder { items { __typename name } } } }`, nil)
	if len(result.Errors) != 0 {
		t.Fatalf("parent-key errors: %#v", result.Errors)
	}
	result = executeParentMaterialization(t, schema, `{ nullable { holder { items { __typename } } } }`, nil)
	if len(result.Errors) != 0 {
		t.Fatalf("null-parent errors: %#v", result.Errors)
	}
}
