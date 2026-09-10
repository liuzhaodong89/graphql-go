package graphql

// spec2025_introspection_test.go
//
// GraphQL September 2025 §4 Introspection。
// 全部断言以规范为准，不以当前实现现状为准。
// 断言使用 t.Errorf 累积，保证一次运行能报出全部缺口。

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// 局部 helper
// ---------------------------------------------------------------------------

func s25introRun(t *testing.T, query string) *Result {
	t.Helper()
	schema := s25NewIntrospectSchema(t)
	return s25Do(t, s25Request{Schema: schema, Query: query})
}

// s25introData 执行内省查询并要求完全没有错误。内省字段缺失会在校验期报错，
// 这里直接失败，失败本身就是规范缺口的证据。
func s25introData(t *testing.T, query string) map[string]any {
	t.Helper()
	result := s25introRun(t, query)
	if len(result.Errors) != 0 {
		t.Fatalf("introspection query returned errors: %v\nquery: %s", s25ErrorMessages(result), query)
	}
	return s25DataMap(t, result)
}

func s25introMap(t *testing.T, value any, path ...string) map[string]any {
	t.Helper()
	current := value
	for _, key := range path {
		asMap, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("path %v: expected map at %q, got %T", path, key, current)
		}
		current = asMap[key]
	}
	result, ok := current.(map[string]any)
	if !ok {
		t.Fatalf("path %v: expected map, got %T (%v)", path, current, current)
	}
	return result
}

func s25introList(t *testing.T, value any, path ...string) []any {
	t.Helper()
	current := value
	for _, key := range path {
		asMap, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("path %v: expected map at %q, got %T", path, key, current)
		}
		current = asMap[key]
	}
	result, ok := current.([]any)
	if !ok {
		t.Fatalf("path %v: expected list, got %T (%v)", path, current, current)
	}
	return result
}

// s25introNames 从 [{name: ...}] 形状的列表中抽出全部 name。
func s25introNames(items []any) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		if asMap, ok := item.(map[string]any); ok {
			if name, ok := asMap["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// s25introFind 在 [{name: ...}] 列表中按 name 找到对应元素。
func s25introFind(items []any, name string) map[string]any {
	for _, item := range items {
		if asMap, ok := item.(map[string]any); ok {
			if got, ok := asMap["name"].(string); ok && got == name {
				return asMap
			}
		}
	}
	return nil
}

func s25introContains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// §4.1 Type Name Introspection
// ---------------------------------------------------------------------------

func TestSpec2025_Introspection_TypeNameField(t *testing.T) {
	t.Run("root_typename_is_query_type_name", func(t *testing.T) {
		data := s25introData(t, `{ __typename }`)
		if data["__typename"] != "Query" {
			t.Errorf("__typename at root = %v, want %q", data["__typename"], "Query")
		}
	})

	t.Run("typename_on_object_field", func(t *testing.T) {
		data := s25introData(t, `{ item { __typename id } }`)
		item := s25introMap(t, data, "item")
		if item["__typename"] != "S25IntroItem" {
			t.Errorf("item.__typename = %v, want S25IntroItem", item["__typename"])
		}
	})

	t.Run("typename_on_interface_field_reports_runtime_object_type", func(t *testing.T) {
		data := s25introData(t, `{ named { __typename name } }`)
		named := s25introMap(t, data, "named")
		if named["__typename"] != "S25IntroItem" {
			t.Errorf("named.__typename = %v, want the runtime object type S25IntroItem", named["__typename"])
		}
	})

	t.Run("typename_on_union_field_reports_runtime_object_type", func(t *testing.T) {
		data := s25introData(t, `{ union { __typename ... on S25IntroItem { id } } }`)
		union := s25introMap(t, data, "union")
		if union["__typename"] != "S25IntroItem" {
			t.Errorf("union.__typename = %v, want S25IntroItem", union["__typename"])
		}
	})

	t.Run("typename_inside_a_list", func(t *testing.T) {
		data := s25introData(t, `{ items { __typename id } }`)
		items := s25introList(t, data, "items")
		if len(items) != 1 {
			t.Fatalf("items length = %d, want 1", len(items))
		}
		first := items[0].(map[string]any)
		if first["__typename"] != "S25IntroItem" {
			t.Errorf("items[0].__typename = %v, want S25IntroItem", first["__typename"])
		}
	})

	t.Run("typename_with_alias", func(t *testing.T) {
		data := s25introData(t, `{ item { kind: __typename } }`)
		item := s25introMap(t, data, "item")
		if item["kind"] != "S25IntroItem" {
			t.Errorf("aliased __typename = %v, want S25IntroItem", item["kind"])
		}
		if _, present := item["__typename"]; present {
			t.Errorf("aliased __typename must not also appear under its field name")
		}
	})

	t.Run("typename_inside_named_and_inline_fragments", func(t *testing.T) {
		data := s25introData(t, `
			{ item { ...F ... on S25IntroItem { inline: __typename } } }
			fragment F on S25IntroItem { named: __typename }
		`)
		item := s25introMap(t, data, "item")
		if item["named"] != "S25IntroItem" || item["inline"] != "S25IntroItem" {
			t.Errorf("__typename in fragments = %v / %v, want S25IntroItem twice", item["named"], item["inline"])
		}
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 The __Schema Type
// ---------------------------------------------------------------------------

func TestSpec2025_Introspection_SchemaType(t *testing.T) {
	// §4.2.1: __Schema 必须提供 description、types、queryType、mutationType、
	// subscriptionType、directives。description 是 September 2025 的必备字段。
	result := s25introRun(t, `
		{
			__schema {
				description
				queryType { name }
				mutationType { name }
				subscriptionType { name }
				types { name }
				directives { name }
			}
		}
	`)
	if len(result.Errors) != 0 {
		t.Fatalf("__schema query must succeed, got errors: %v", s25ErrorMessages(result))
	}
	data := s25DataMap(t, result)
	schemaMeta := s25introMap(t, data, "__schema")

	if _, present := schemaMeta["description"]; !present {
		t.Errorf("__Schema.description is required by September 2025 but is absent from the response")
	}
	if got := s25introMap(t, schemaMeta, "queryType")["name"]; got != "Query" {
		t.Errorf("__schema.queryType.name = %v, want Query", got)
	}
	if schemaMeta["mutationType"] != nil {
		t.Errorf("__schema.mutationType = %v, want null for a query-only schema", schemaMeta["mutationType"])
	}
	if schemaMeta["subscriptionType"] != nil {
		t.Errorf("__schema.subscriptionType = %v, want null for a query-only schema", schemaMeta["subscriptionType"])
	}

	typeNames := s25introNames(s25introList(t, schemaMeta, "types"))
	for _, want := range []string{
		"Query", "S25IntroItem", "S25IntroOther", "S25IntroUnion", "S25IntroNamed",
		"S25IntroEntity", "S25IntroInput", "S25IntroInner", "S25Mode",
		"String", "Int", "Boolean", "ID",
		"__Schema", "__Type", "__Field", "__InputValue", "__EnumValue", "__Directive", "__TypeKind", "__DirectiveLocation",
	} {
		if !s25introContains(typeNames, want) {
			t.Errorf("__schema.types is missing %q", want)
		}
	}

	directiveNames := s25introNames(s25introList(t, schemaMeta, "directives"))
	for _, want := range []string{"skip", "include", "deprecated", "specifiedBy", "oneOf"} {
		if !s25introContains(directiveNames, want) {
			t.Errorf("__schema.directives is missing the specified directive @%s", want)
		}
	}
}

// ---------------------------------------------------------------------------
// §4.2.2 The __Type Type
// ---------------------------------------------------------------------------

func TestSpec2025_Introspection_TypeType(t *testing.T) {
	t.Run("object_type", func(t *testing.T) {
		data := s25introData(t, `
			{
				__type(name: "S25IntroItem") {
					kind
					name
					description
					fields { name }
					interfaces { name }
					possibleTypes { name }
					enumValues { name }
					inputFields { name }
					ofType { name }
				}
			}
		`)
		target := s25introMap(t, data, "__type")
		if target["kind"] != "OBJECT" {
			t.Errorf("kind = %v, want OBJECT", target["kind"])
		}
		if target["description"] != "An introspectable item." {
			t.Errorf("description = %v, want the declared description", target["description"])
		}
		fieldNames := s25introNames(s25introList(t, target, "fields"))
		for _, want := range []string{"id", "name", "wrapped", "withArgs"} {
			if !s25introContains(fieldNames, want) {
				t.Errorf("fields is missing %q", want)
			}
		}
		if s25introContains(fieldNames, "legacy") {
			t.Errorf("fields must exclude deprecated fields by default, but legacy is present")
		}
		interfaceNames := s25introNames(s25introList(t, target, "interfaces"))
		for _, want := range []string{"S25IntroNamed", "S25IntroEntity"} {
			if !s25introContains(interfaceNames, want) {
				t.Errorf("interfaces is missing %q", want)
			}
		}
		if target["possibleTypes"] != nil {
			t.Errorf("possibleTypes on OBJECT must be null, got %v", target["possibleTypes"])
		}
		if target["enumValues"] != nil {
			t.Errorf("enumValues on OBJECT must be null, got %v", target["enumValues"])
		}
		if target["inputFields"] != nil {
			t.Errorf("inputFields on OBJECT must be null, got %v", target["inputFields"])
		}
		if target["ofType"] != nil {
			t.Errorf("ofType on a named type must be null, got %v", target["ofType"])
		}
	})

	t.Run("interface_implementing_another_interface", func(t *testing.T) {
		// §3.7 / §4.2.2: interface 可以实现 interface，并且必须能被内省出来。
		data := s25introData(t, `
			{
				__type(name: "S25IntroEntity") {
					kind
					interfaces { name }
					possibleTypes { name }
					fields { name }
				}
			}
		`)
		target := s25introMap(t, data, "__type")
		if target["kind"] != "INTERFACE" {
			t.Errorf("kind = %v, want INTERFACE", target["kind"])
		}
		if target["interfaces"] == nil {
			t.Errorf("__Type.interfaces on an INTERFACE must be a list (September 2025), got null")
		}
		possible := s25introNames(s25introList(t, target, "possibleTypes"))
		if !s25introContains(possible, "S25IntroItem") {
			t.Errorf("possibleTypes = %v, want it to contain S25IntroItem", possible)
		}
	})

	t.Run("union_type", func(t *testing.T) {
		data := s25introData(t, `
			{ __type(name: "S25IntroUnion") { kind name description possibleTypes { name } fields { name } } }
		`)
		target := s25introMap(t, data, "__type")
		if target["kind"] != "UNION" {
			t.Errorf("kind = %v, want UNION", target["kind"])
		}
		possible := s25introNames(s25introList(t, target, "possibleTypes"))
		if !reflect.DeepEqual(possible, []string{"S25IntroItem", "S25IntroOther"}) {
			t.Errorf("possibleTypes = %v, want [S25IntroItem S25IntroOther]", possible)
		}
		if target["fields"] != nil {
			t.Errorf("fields on UNION must be null, got %v", target["fields"])
		}
	})

	t.Run("enum_type", func(t *testing.T) {
		data := s25introData(t, `{ __type(name: "S25Mode") { kind name description enumValues { name } } }`)
		target := s25introMap(t, data, "__type")
		if target["kind"] != "ENUM" {
			t.Errorf("kind = %v, want ENUM", target["kind"])
		}
		values := s25introNames(s25introList(t, target, "enumValues"))
		if !reflect.DeepEqual(values, []string{"A", "B"}) {
			t.Errorf("enumValues = %v, want [A B] (deprecated values excluded by default)", values)
		}
	})

	t.Run("input_object_type", func(t *testing.T) {
		data := s25introData(t, `
			{ __type(name: "S25IntroInput") { kind name description inputFields { name } fields { name } } }
		`)
		target := s25introMap(t, data, "__type")
		if target["kind"] != "INPUT_OBJECT" {
			t.Errorf("kind = %v, want INPUT_OBJECT", target["kind"])
		}
		inputNames := s25introNames(s25introList(t, target, "inputFields"))
		want := []string{"count", "mode", "nested", "tags", "text"}
		if !reflect.DeepEqual(inputNames, want) {
			t.Errorf("inputFields = %v, want %v", inputNames, want)
		}
		if target["fields"] != nil {
			t.Errorf("fields on INPUT_OBJECT must be null, got %v", target["fields"])
		}
	})

	t.Run("scalar_type_exposes_specifiedByURL", func(t *testing.T) {
		// §4.2.2 + §3.13.4: __Type.specifiedByURL 是 September 2025 的必备字段。
		data := s25introData(t, `{ __type(name: "String") { kind name specifiedByURL } }`)
		target := s25introMap(t, data, "__type")
		if target["kind"] != "SCALAR" {
			t.Errorf("kind = %v, want SCALAR", target["kind"])
		}
		if _, present := target["specifiedByURL"]; !present {
			t.Errorf("__Type.specifiedByURL is required by September 2025 but is absent")
		}
	})

	t.Run("input_object_exposes_isOneOf", func(t *testing.T) {
		// §4.2.2 + §3.10.1: __Type.isOneOf 是 September 2025 的必备字段。
		data := s25introData(t, `{ __type(name: "S25IntroInput") { name isOneOf } }`)
		target := s25introMap(t, data, "__type")
		if _, present := target["isOneOf"]; !present {
			t.Errorf("__Type.isOneOf is required by September 2025 but is absent")
		}
		if target["isOneOf"] != false {
			t.Errorf("isOneOf on a non-oneOf input object = %v, want false", target["isOneOf"])
		}
	})

	t.Run("inputFields_accepts_includeDeprecated", func(t *testing.T) {
		// §4.2.2: __Type.inputFields(includeDeprecated: Boolean = false)
		data := s25introData(t, `{ __type(name: "S25IntroInput") { inputFields(includeDeprecated: true) { name } } }`)
		target := s25introMap(t, data, "__type")
		if len(s25introList(t, target, "inputFields")) == 0 {
			t.Errorf("inputFields(includeDeprecated: true) returned nothing")
		}
	})

	t.Run("wrapping_types_unwrap_through_ofType", func(t *testing.T) {
		data := s25introData(t, `
			{
				__type(name: "S25IntroItem") {
					fields {
						name
						type { kind name ofType { kind name ofType { kind name ofType { kind name } } } }
					}
				}
			}
		`)
		fields := s25introList(t, s25introMap(t, data, "__type"), "fields")
		wrapped := s25introFind(fields, "wrapped")
		if wrapped == nil {
			t.Fatalf("field wrapped not found")
		}
		// NON_NULL(LIST(NON_NULL(String)))
		level0 := s25introMap(t, wrapped, "type")
		if level0["kind"] != "NON_NULL" || level0["name"] != nil {
			t.Errorf("level0 = %v/%v, want NON_NULL with null name", level0["kind"], level0["name"])
		}
		level1 := s25introMap(t, level0, "ofType")
		if level1["kind"] != "LIST" {
			t.Errorf("level1 kind = %v, want LIST", level1["kind"])
		}
		level2 := s25introMap(t, level1, "ofType")
		if level2["kind"] != "NON_NULL" {
			t.Errorf("level2 kind = %v, want NON_NULL", level2["kind"])
		}
		level3 := s25introMap(t, level2, "ofType")
		if level3["kind"] != "SCALAR" || level3["name"] != "String" {
			t.Errorf("level3 = %v/%v, want SCALAR/String", level3["kind"], level3["name"])
		}
	})
}

// ---------------------------------------------------------------------------
// §4.2.3 The __Field Type
// ---------------------------------------------------------------------------

func TestSpec2025_Introspection_FieldType(t *testing.T) {
	t.Run("field_metadata", func(t *testing.T) {
		data := s25introData(t, `
			{
				__type(name: "S25IntroItem") {
					fields(includeDeprecated: true) {
						name
						description
						isDeprecated
						deprecationReason
						type { kind }
						args { name }
					}
				}
			}
		`)
		fields := s25introList(t, s25introMap(t, data, "__type"), "fields")

		name := s25introFind(fields, "name")
		if name == nil {
			t.Fatalf("field name not found")
		}
		if name["description"] != "The name." {
			t.Errorf("__Field.description = %v, want %q", name["description"], "The name.")
		}
		if name["isDeprecated"] != false {
			t.Errorf("name.isDeprecated = %v, want false", name["isDeprecated"])
		}
		if name["deprecationReason"] != nil {
			t.Errorf("name.deprecationReason = %v, want null", name["deprecationReason"])
		}

		legacy := s25introFind(fields, "legacy")
		if legacy == nil {
			t.Fatalf("deprecated field legacy not returned by fields(includeDeprecated: true)")
		}
		if legacy["isDeprecated"] != true {
			t.Errorf("legacy.isDeprecated = %v, want true", legacy["isDeprecated"])
		}
		if legacy["deprecationReason"] != "use name" {
			t.Errorf("legacy.deprecationReason = %v, want %q", legacy["deprecationReason"], "use name")
		}

		withArgs := s25introFind(fields, "withArgs")
		if withArgs == nil {
			t.Fatalf("field withArgs not found")
		}
		argNames := s25introNames(s25introList(t, withArgs, "args"))
		want := []string{"filter", "first", "mode", "tags"}
		if !reflect.DeepEqual(argNames, want) {
			t.Errorf("withArgs.args = %v, want %v", argNames, want)
		}
	})

	t.Run("fields_includeDeprecated_false_filters", func(t *testing.T) {
		data := s25introData(t, `{ __type(name: "S25IntroItem") { fields(includeDeprecated: false) { name } } }`)
		names := s25introNames(s25introList(t, s25introMap(t, data, "__type"), "fields"))
		if s25introContains(names, "legacy") {
			t.Errorf("fields(includeDeprecated: false) must exclude legacy, got %v", names)
		}
	})

	t.Run("args_accepts_includeDeprecated", func(t *testing.T) {
		// §4.2.3: __Field.args(includeDeprecated: Boolean = false)
		data := s25introData(t, `
			{ __type(name: "S25IntroItem") { fields { name args(includeDeprecated: true) { name } } } }
		`)
		fields := s25introList(t, s25introMap(t, data, "__type"), "fields")
		if s25introFind(fields, "withArgs") == nil {
			t.Errorf("args(includeDeprecated: true) query did not return withArgs")
		}
	})
}

// ---------------------------------------------------------------------------
// §4.2.4 The __InputValue Type
// ---------------------------------------------------------------------------

func TestSpec2025_Introspection_InputValueType(t *testing.T) {
	// §4.2.4: defaultValue 是 "A GraphQL-formatted string representing the default
	// value for this input value" —— 必须是 GraphQL 字面量，不是 Go 的 %v 输出。
	data := s25introData(t, `
		{
			__type(name: "S25IntroItem") {
				fields {
					name
					args {
						name
						description
						defaultValue
						isDeprecated
						deprecationReason
						type { kind name ofType { name } }
					}
				}
			}
		}
	`)
	fields := s25introList(t, s25introMap(t, data, "__type"), "fields")
	withArgs := s25introFind(fields, "withArgs")
	if withArgs == nil {
		t.Fatalf("field withArgs not found")
	}
	args := s25introList(t, withArgs, "args")

	first := s25introFind(args, "first")
	if first == nil {
		t.Fatalf("arg first not found")
	}
	if first["description"] != "How many." {
		t.Errorf("first.description = %v, want %q", first["description"], "How many.")
	}
	if first["defaultValue"] != "10" {
		t.Errorf("Int default must print as the GraphQL literal %q, got %v", "10", first["defaultValue"])
	}
	if _, present := first["isDeprecated"]; !present {
		t.Errorf("__InputValue.isDeprecated is required by September 2025 but is absent")
	}
	if _, present := first["deprecationReason"]; !present {
		t.Errorf("__InputValue.deprecationReason is required by September 2025 but is absent")
	}

	mode := s25introFind(args, "mode")
	if mode == nil {
		t.Fatalf("arg mode not found")
	}
	if mode["defaultValue"] != "A" {
		t.Errorf("Enum default must print as the unquoted GraphQL literal %q, got %v", "A", mode["defaultValue"])
	}

	tags := s25introFind(args, "tags")
	if tags == nil {
		t.Fatalf("arg tags not found")
	}
	if tags["defaultValue"] != `["x"]` {
		t.Errorf("List default must print as the GraphQL literal %q, got %v", `["x"]`, tags["defaultValue"])
	}

	filter := s25introFind(args, "filter")
	if filter == nil {
		t.Fatalf("arg filter not found")
	}
	if filter["defaultValue"] != nil {
		t.Errorf("an argument without a default must report defaultValue null, got %v", filter["defaultValue"])
	}

	// input object field 的默认值同样必须是 GraphQL 字面量。
	inputData := s25introData(t, `
		{ __type(name: "S25IntroInput") { inputFields { name defaultValue description isDeprecated } } }
	`)
	inputFields := s25introList(t, s25introMap(t, inputData, "__type"), "inputFields")

	count := s25introFind(inputFields, "count")
	if count == nil {
		t.Fatalf("input field count not found")
	}
	if count["defaultValue"] != "7" {
		t.Errorf("input field Int default = %v, want %q", count["defaultValue"], "7")
	}
	inputMode := s25introFind(inputFields, "mode")
	if inputMode == nil {
		t.Fatalf("input field mode not found")
	}
	if inputMode["defaultValue"] != "A" {
		t.Errorf("input field Enum default = %v, want the unquoted literal %q", inputMode["defaultValue"], "A")
	}
	inputTags := s25introFind(inputFields, "tags")
	if inputTags == nil {
		t.Fatalf("input field tags not found")
	}
	if inputTags["defaultValue"] != `["x", "y"]` {
		t.Errorf("input field List default = %v, want the literal %q", inputTags["defaultValue"], `["x", "y"]`)
	}
	nested := s25introFind(inputFields, "nested")
	if nested == nil {
		t.Fatalf("input field nested not found")
	}
	if nested["defaultValue"] != "{flag: true}" {
		t.Errorf("input field input-object default = %v, want the literal %q", nested["defaultValue"], "{flag: true}")
	}
	text := s25introFind(inputFields, "text")
	if text == nil {
		t.Fatalf("input field text not found")
	}
	if text["description"] != "Required text." {
		t.Errorf("input field description = %v, want %q", text["description"], "Required text.")
	}
}

// ---------------------------------------------------------------------------
// §4.2.5 The __EnumValue Type
// ---------------------------------------------------------------------------

func TestSpec2025_Introspection_EnumValueType(t *testing.T) {
	data := s25introData(t, `
		{
			all: __type(name: "S25Mode") {
				enumValues(includeDeprecated: true) { name description isDeprecated deprecationReason }
			}
			active: __type(name: "S25Mode") {
				enumValues(includeDeprecated: false) { name }
			}
		}
	`)

	all := s25introList(t, s25introMap(t, data, "all"), "enumValues")
	allNames := s25introNames(all)
	want := []string{"A", "B", "DEPRECATED_C"}
	if !reflect.DeepEqual(allNames, want) {
		t.Errorf("enumValues(includeDeprecated: true) = %v, want %v", allNames, want)
	}

	valueA := s25introFind(all, "A")
	if valueA == nil {
		t.Fatalf("enum value A not found")
	}
	if valueA["description"] != "Mode A" {
		t.Errorf("A.description = %v, want %q", valueA["description"], "Mode A")
	}
	if valueA["isDeprecated"] != false {
		t.Errorf("A.isDeprecated = %v, want false", valueA["isDeprecated"])
	}
	if valueA["deprecationReason"] != nil {
		t.Errorf("A.deprecationReason = %v, want null", valueA["deprecationReason"])
	}

	valueC := s25introFind(all, "DEPRECATED_C")
	if valueC == nil {
		t.Fatalf("enum value DEPRECATED_C not found")
	}
	if valueC["isDeprecated"] != true {
		t.Errorf("DEPRECATED_C.isDeprecated = %v, want true", valueC["isDeprecated"])
	}
	if valueC["deprecationReason"] != "use B" {
		t.Errorf("DEPRECATED_C.deprecationReason = %v, want %q", valueC["deprecationReason"], "use B")
	}

	activeNames := s25introNames(s25introList(t, s25introMap(t, data, "active"), "enumValues"))
	if !reflect.DeepEqual(activeNames, []string{"A", "B"}) {
		t.Errorf("enumValues(includeDeprecated: false) = %v, want [A B]", activeNames)
	}
}

// ---------------------------------------------------------------------------
// §4.2.6 The __Directive Type
// ---------------------------------------------------------------------------

func TestSpec2025_Introspection_DirectiveType(t *testing.T) {
	data := s25introData(t, `
		{
			__schema {
				directives {
					name
					description
					locations
					isRepeatable
					args { name defaultValue }
				}
			}
		}
	`)
	directives := s25introList(t, s25introMap(t, data, "__schema"), "directives")

	// §3.13 规定的内置指令及其 locations。
	expected := map[string][]string{
		"skip":        {"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"},
		"include":     {"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"},
		"deprecated":  {"ARGUMENT_DEFINITION", "ENUM_VALUE", "FIELD_DEFINITION", "INPUT_FIELD_DEFINITION"},
		"specifiedBy": {"SCALAR"},
		"oneOf":       {"INPUT_OBJECT"},
	}
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		directive := s25introFind(directives, name)
		if directive == nil {
			t.Errorf("specified directive @%s is missing from __schema.directives", name)
			continue
		}
		if _, present := directive["isRepeatable"]; !present {
			t.Errorf("@%s: __Directive.isRepeatable is required by September 2025 but is absent", name)
		}
		locations := make([]string, 0)
		for _, item := range s25introList(t, directive, "locations") {
			locations = append(locations, fmt.Sprintf("%v", item))
		}
		sort.Strings(locations)
		if !reflect.DeepEqual(locations, expected[name]) {
			t.Errorf("@%s locations = %v, want %v", name, locations, expected[name])
		}
	}

	t.Run("directive_args_accept_includeDeprecated", func(t *testing.T) {
		inner := s25introData(t, `{ __schema { directives { name args(includeDeprecated: true) { name } } } }`)
		if len(s25introList(t, s25introMap(t, inner, "__schema"), "directives")) == 0 {
			t.Errorf("__Directive.args(includeDeprecated:) query returned no directives")
		}
	})

	t.Run("skip_argument_shape", func(t *testing.T) {
		skip := s25introFind(directives, "skip")
		if skip == nil {
			t.Fatalf("@skip is missing")
		}
		argNames := s25introNames(s25introList(t, skip, "args"))
		if !reflect.DeepEqual(argNames, []string{"if"}) {
			t.Errorf("@skip args = %v, want [if]", argNames)
		}
	})
}

// ---------------------------------------------------------------------------
// §4 交叉场景
// ---------------------------------------------------------------------------

func TestSpec2025_Introspection_CrossScenarios(t *testing.T) {
	t.Run("introspection_mixed_with_business_fields", func(t *testing.T) {
		data := s25introData(t, `
			{
				__typename
				mode
				item { id __typename }
				__type(name: "S25Mode") { name kind }
			}
		`)
		if data["__typename"] != "Query" {
			t.Errorf("__typename = %v, want Query", data["__typename"])
		}
		if data["mode"] != "A" {
			t.Errorf("mode = %v, want A", data["mode"])
		}
		meta := s25introMap(t, data, "__type")
		if meta["name"] != "S25Mode" || meta["kind"] != "ENUM" {
			t.Errorf("__type = %v/%v, want S25Mode/ENUM", meta["name"], meta["kind"])
		}
	})

	t.Run("introspection_through_named_fragment", func(t *testing.T) {
		data := s25introData(t, `
			{ __type(name: "S25IntroItem") { ...TypeParts } }
			fragment TypeParts on __Type { name kind fields { name } }
		`)
		target := s25introMap(t, data, "__type")
		if target["name"] != "S25IntroItem" || target["kind"] != "OBJECT" {
			t.Errorf("fragment on __Type produced %v/%v", target["name"], target["kind"])
		}
	})

	t.Run("introspection_through_inline_fragment", func(t *testing.T) {
		data := s25introData(t, `{ __type(name: "S25IntroItem") { ... on __Type { name kind } } }`)
		target := s25introMap(t, data, "__type")
		if target["name"] != "S25IntroItem" {
			t.Errorf("inline fragment on __Type produced %v", target["name"])
		}
	})

	t.Run("aliases_on_introspection_fields", func(t *testing.T) {
		data := s25introData(t, `
			{
				a: __type(name: "S25IntroItem") { typeName: name typeKind: kind }
				b: __type(name: "S25Mode") { typeName: name }
			}
		`)
		first := s25introMap(t, data, "a")
		second := s25introMap(t, data, "b")
		if first["typeName"] != "S25IntroItem" || first["typeKind"] != "OBJECT" {
			t.Errorf("aliased __type a = %v", first)
		}
		if second["typeName"] != "S25Mode" {
			t.Errorf("aliased __type b = %v", second)
		}
	})

	t.Run("skip_directive_on_introspection_field", func(t *testing.T) {
		data := s25introData(t, `{ __typename @skip(if: true) mode }`)
		if _, present := data["__typename"]; present {
			t.Errorf("@skip(if: true) must remove __typename from the response, got %v", data)
		}
		if data["mode"] != "A" {
			t.Errorf("mode = %v, want A", data["mode"])
		}
	})

	t.Run("variable_argument_on_introspection_field", func(t *testing.T) {
		schema := s25NewIntrospectSchema(t)
		result := s25Do(t, s25Request{
			Schema:    schema,
			Query:     `query Q($name: String!) { __type(name: $name) { name kind } }`,
			Variables: map[string]any{"name": "S25IntroUnion"},
		})
		if len(result.Errors) != 0 {
			t.Fatalf("variable argument on __type failed: %v", s25ErrorMessages(result))
		}
		target := s25introMap(t, s25DataMap(t, result), "__type")
		if target["name"] != "S25IntroUnion" || target["kind"] != "UNION" {
			t.Errorf("__type via variable = %v/%v, want S25IntroUnion/UNION", target["name"], target["kind"])
		}
	})

	t.Run("unknown_type_returns_null", func(t *testing.T) {
		data := s25introData(t, `{ __type(name: "S25DoesNotExist") { name } }`)
		if data["__type"] != nil {
			t.Errorf("__type for an unknown name = %v, want null", data["__type"])
		}
	})

	t.Run("typename_and_meta_fields_are_not_in_type_fields", func(t *testing.T) {
		// §4.2.2: __schema / __type / __typename 是 meta field，不出现在 __Type.fields 中。
		data := s25introData(t, `{ __type(name: "Query") { fields { name } } }`)
		names := s25introNames(s25introList(t, s25introMap(t, data, "__type"), "fields"))
		for _, forbidden := range []string{"__typename", "__type", "__schema"} {
			if s25introContains(names, forbidden) {
				t.Errorf("__Type.fields must not expose the meta field %q", forbidden)
			}
		}
	})
}
