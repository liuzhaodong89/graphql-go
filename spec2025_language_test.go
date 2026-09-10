package graphql

// spec2025_language_test.go
//
// GraphQL September 2025 规范 §2 Language 一致性用例。
// https://spec.graphql.org/September2025/#sec-Language
//
// 期望值全部按规范推导，而非按当前实现的行为反推。

import (
	"fmt"
	"testing"
)

// ---------------------------------------------------------------------------
// 本文件私有 helper（统一 s25lang 前缀）
// ---------------------------------------------------------------------------

// s25langCore 返回一个全新的 core schema。每次请求都取一份新的，
// 避免复用已经执行过的 schema。
func s25langCore(t testing.TB) Schema {
	t.Helper()
	schema, _ := s25NewCoreSchema(t, nil)
	return schema
}

// s25langRequireRequestError 断言这是一个 request error：
// 规范 §7.1.2 —— 请求在执行开始前失败时，响应中不得出现 data 键。
func s25langRequireRequestError(t testing.TB, result *Result) {
	t.Helper()
	if result == nil {
		t.Fatalf("result is nil")
	}
	if len(result.Errors) == 0 {
		t.Fatalf("expected a request error, got none; data=%#v", s25Plain(result.Data))
	}
	if result.Data != nil {
		t.Fatalf("request error must not carry data, got %#v", s25Plain(result.Data))
	}
}

// s25langSameData 断言两份文档在 core schema 上产生逐字节相同的 data。
func s25langSameData(t testing.TB, label, queryA, queryB string, variables map[string]any) {
	t.Helper()
	resultA := s25Do(t, s25Request{Schema: s25langCore(t), Query: queryA, Variables: variables})
	s25RequireNoErrors(t, resultA)
	resultB := s25Do(t, s25Request{Schema: s25langCore(t), Query: queryB, Variables: variables})
	s25RequireNoErrors(t, resultB)
	gotA, gotB := s25MarshalData(t, resultA), s25MarshalData(t, resultB)
	if gotA != gotB {
		t.Fatalf("%s: documents must produce identical data\n A: %s\n B: %s", label, gotA, gotB)
	}
}

// s25langRequireParseError 断言源文本不是合法的 GraphQL 文档。
func s25langRequireParseError(t testing.TB, query string) {
	t.Helper()
	if _, err := s25Parse(query); err == nil {
		t.Fatalf("expected a parse error for %q, got none", query)
	}
}

// s25langRequireParses 断言源文本可以被解析。
func s25langRequireParses(t testing.TB, query string) {
	t.Helper()
	if _, err := s25Parse(query); err != nil {
		t.Fatalf("expected %q to parse, got: %v", query, err)
	}
}

// s25langBackslash 是单个反斜杠，用于在 Go 源码里安全拼出 GraphQL 转义序列。
const s25langBackslash = "\\"

// s25langBOM 是 §2.1.2 的 UnicodeBOM（U+FEFF），属于 Ignored token。
var s25langBOM = string(rune(0xFEFF))

// s25langEsc 拼出一个 GraphQL 字符串转义序列（反斜杠 + body）。
func s25langEsc(body string) string { return s25langBackslash + body }

// s25langNameSchema 是 §2.1.8 名字词法用的专用 schema。
func s25langNameSchema(t testing.TB) Schema {
	t.Helper()
	return s25NewSchema(t, Fields{
		"_a":  &Field{Type: String, Resolve: s25Const("underscore")},
		"a1":  &Field{Type: String, Resolve: s25Const("alnum")},
		"A_1": &Field{Type: String, Resolve: s25Const("mixed")},
	})
}

// s25langFloatSchema 提供一个 Float 入参字段。core schema 没有 Float 入参，
// §2.10.2 FloatValue 需要单独的落点。
func s25langFloatSchema(t testing.TB) Schema {
	t.Helper()
	return s25NewSchema(t, Fields{
		"floatEcho": &Field{
			Type: String,
			Args: FieldConfigArgument{"value": &ArgumentConfig{Type: Float}},
			Resolve: func(p ResolveParams) (any, error) {
				value, ok := p.Args["value"]
				if !ok {
					return "<missing>", nil
				}
				if value == nil {
					return "<null>", nil
				}
				return fmt.Sprintf("%v", value), nil
			},
		},
	})
}

// ---------------------------------------------------------------------------
// §2.1.1 - §2.1.6 Ignored Tokens
// ---------------------------------------------------------------------------

func TestSpec2025_Language_IgnoredTokensAreInsignificant(t *testing.T) {
	// §2.1.1 SourceCharacter / §2.1.2 UnicodeBOM / §2.1.3 WhiteSpace /
	// §2.1.4 LineTerminator / §2.1.5 Comment / §2.1.6 Insignificant Commas.
	// 全部属于 Ignored token：出现与否不得改变文档含义。

	const compact = `{greeting echo(text:"a",suffix:"b") echoList(values:[1,2,3])}`

	t.Run("bom_whitespace_line_terminators_and_comments", func(t *testing.T) {
		// BOM(U+FEFF) + 空格 + 水平制表符 + LF + CR + CRLF + 注释 + 逗号
		pretty := s25langBOM + "\t# leading comment\r\n" +
			"{\r\n" +
			"\tgreeting ,\n" +
			"  # a comment may contain } and it is still a comment\r" +
			"  echo( text : \"a\" , suffix : \"b\" ) ,\r\n" +
			"  echoList ( values : [ 1 , 2 , 3 ] )\n" +
			"}\r\n"

		resultPretty := s25Do(t, s25Request{Schema: s25langCore(t), Query: pretty})
		s25RequireNoErrors(t, resultPretty)
		resultCompact := s25Do(t, s25Request{Schema: s25langCore(t), Query: compact})
		s25RequireNoErrors(t, resultCompact)

		gotPretty, gotCompact := s25MarshalData(t, resultPretty), s25MarshalData(t, resultCompact)
		if gotPretty != gotCompact {
			t.Fatalf("ignored tokens changed the response\npretty : %s\ncompact: %s", gotPretty, gotCompact)
		}
	})

	t.Run("expected_values", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: compact}, map[string]any{
			"greeting": "hello",
			"echo":     "ab",
			"echoList": "[1 2 3]",
		})
	})

	t.Run("comment_containing_closing_brace", func(t *testing.T) {
		// §2.1.5：Comment 一直延伸到 LineTerminator，其中的 } 不是 punctuator。
		query := "{\n  greeting # this comment closes nothing: } } }\n}"
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: query}, map[string]any{
			"greeting": "hello",
		})
	})

	t.Run("comment_terminated_by_carriage_return", func(t *testing.T) {
		query := "{ greeting # comment ends at CR }\r a }"
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: query}, map[string]any{
			"greeting": "hello",
			"a":        "a",
		})
	})

	t.Run("commas_between_arguments_list_items_and_fields", func(t *testing.T) {
		s25langSameData(t, "insignificant commas",
			`{ greeting, a, echo(text: "a", suffix: "b"), echoList(values: [1, 2, 3]) }`,
			`{ greeting a echo(text: "a" suffix: "b") echoList(values: [1 2 3]) }`,
			nil)
	})

	t.Run("leading_and_trailing_commas", func(t *testing.T) {
		s25langSameData(t, "leading/trailing commas",
			`{ , greeting , a , }`,
			`{ greeting a }`,
			nil)
	})
}

// ---------------------------------------------------------------------------
// §2.1.8 Names
// ---------------------------------------------------------------------------

func TestSpec2025_Language_NameLexing(t *testing.T) {
	// Name :: NameStart NameContinue*
	// NameStart :: Letter | `_`      NameContinue :: Letter | Digit | `_`

	t.Run("valid_names", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langNameSchema(t),
			Query:  `{ _a a1 A_1 }`,
		}, map[string]any{
			"_a":  "underscore",
			"a1":  "alnum",
			"A_1": "mixed",
		})
	})

	t.Run("names_are_case_sensitive_response_keys", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langNameSchema(t),
			Query:  `{ _under: _a A_1 }`,
		}, map[string]any{
			"_under": "underscore",
			"A_1":    "mixed",
		})
	})

	t.Run("name_may_not_start_with_digit", func(t *testing.T) {
		s25langRequireParseError(t, `{ 1abc }`)
	})

	t.Run("name_may_not_contain_hyphen", func(t *testing.T) {
		s25langRequireParseError(t, `{ a-b }`)
	})

	t.Run("fragment_and_variable_names_follow_the_same_grammar", func(t *testing.T) {
		s25langRequireParses(t, `query Q_1($_v: Int) { a1 ..._F1 } fragment _F1 on Query { A_1 }`)
		s25langRequireParseError(t, `query Q($1v: Int) { greeting }`)
		s25langRequireParseError(t, `{ ...1Frag } fragment 1Frag on Query { greeting }`)
	})
}

// ---------------------------------------------------------------------------
// §2.1.7 Punctuators
// ---------------------------------------------------------------------------

func TestSpec2025_Language_PunctuatorsParse(t *testing.T) {
	// Punctuator :: one of ! $ & ( ) ... : = @ [ ] { | }

	t.Run("executable_document_uses_bang_dollar_paren_spread_colon_equals_at_bracket_brace", func(t *testing.T) {
		query := `query S25Punctuators(
			$text: String!,
			$suffix: String = "?",
			$values: [[Int!]!]!
		) {
			echo(text: $text, suffix: $suffix) @include(if: true)
			echoNestedList(values: $values)
			...S25PunctFrag
			... on Query {
				a
			}
		}

		fragment S25PunctFrag on Query {
			m @skip(if: false)
		}`

		s25langRequireParses(t, query)
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  query,
			Variables: map[string]any{
				"text":   "hi",
				"values": []any{[]any{1, 2}, []any{3}},
			},
		}, map[string]any{
			"echo":           "hi?",
			"echoNestedList": "[[1 2] [3]]",
			"a":              "a",
			"m":              "m",
		})
	})

	t.Run("ampersand_and_pipe_in_type_system_document", func(t *testing.T) {
		// & 只出现在 ImplementsInterfaces，| 出现在 UnionMemberTypes / SDL 中。
		s25langRequireParses(t, `
			interface S25P_A { id: ID! }
			interface S25P_B { name: String }
			type S25P_Impl implements S25P_A & S25P_B {
				id: ID!
				name: String
			}
			type S25P_Other { id: ID! }
			union S25P_Union = S25P_Impl | S25P_Other
		`)
	})
}

// ---------------------------------------------------------------------------
// §2.2 Document (Descriptions on executable definitions, new in September 2025)
// ---------------------------------------------------------------------------

func TestSpec2025_Language_DescriptionsOnExecutableDefinitions(t *testing.T) {
	// September 2025 允许 OperationDefinition / FragmentDefinition /
	// VariableDefinition 前带 Description。Description 是文档层面的元数据，
	// 不参与执行，也不得改变响应。

	t.Run("operation_description", func(t *testing.T) {
		t.Run("single_quoted", func(t *testing.T) {
			s25langRequireParses(t, `"Greets the caller." query Greet { greeting }`)
			s25langSameData(t, "operation description",
				`"Greets the caller." query Greet { greeting }`,
				`query Greet { greeting }`, nil)
		})
		t.Run("block_quoted", func(t *testing.T) {
			withDescription := "\"\"\"\nGreets the caller.\n\"\"\"\nquery Greet { greeting }"
			s25langRequireParses(t, withDescription)
			s25langSameData(t, "operation block description",
				withDescription, `query Greet { greeting }`, nil)
		})
		t.Run("anonymous_operation_with_description", func(t *testing.T) {
			// 简写形式没有 `query` 关键字，因此描述只能加在带关键字的形式上。
			s25langRequireParses(t, `"An anonymous query." query { greeting }`)
			s25langSameData(t, "anonymous operation description",
				`"An anonymous query." query { greeting }`,
				`query { greeting }`, nil)
		})
	})

	t.Run("fragment_description", func(t *testing.T) {
		t.Run("single_quoted", func(t *testing.T) {
			withDescription := `{ user { ...S25LangUserFields } } "The fields of a user." fragment S25LangUserFields on S25User { id name }`
			s25langRequireParses(t, withDescription)
			s25langSameData(t, "fragment description",
				withDescription,
				`{ user { ...S25LangUserFields } } fragment S25LangUserFields on S25User { id name }`,
				nil)
		})
		t.Run("block_quoted", func(t *testing.T) {
			withDescription := "{ user { ...S25LangUserFields } }\n\"\"\"\nThe fields of a user.\n\"\"\"\nfragment S25LangUserFields on S25User { id name }"
			s25langRequireParses(t, withDescription)
			s25langSameData(t, "fragment block description",
				withDescription,
				`{ user { ...S25LangUserFields } } fragment S25LangUserFields on S25User { id name }`,
				nil)
		})
	})

	t.Run("variable_description", func(t *testing.T) {
		t.Run("single_quoted", func(t *testing.T) {
			withDescription := `query Echo("The text to echo." $text: String! = "hi") { echo(text: $text) }`
			s25langRequireParses(t, withDescription)
			s25langSameData(t, "variable description",
				withDescription,
				`query Echo($text: String! = "hi") { echo(text: $text) }`,
				nil)
		})
		t.Run("block_quoted", func(t *testing.T) {
			withDescription := "query Echo(\n\"\"\"\nThe text to echo.\n\"\"\"\n$text: String! = \"hi\"\n) { echo(text: $text) }"
			s25langRequireParses(t, withDescription)
			s25langSameData(t, "variable block description",
				withDescription,
				`query Echo($text: String! = "hi") { echo(text: $text) }`,
				nil)
		})
		t.Run("with_supplied_variable_values", func(t *testing.T) {
			s25langSameData(t, "described variable with value",
				`query Echo("The text to echo." $text: String!) { echo(text: $text) }`,
				`query Echo($text: String!) { echo(text: $text) }`,
				map[string]any{"text": "given"})
		})
	})
}

// ---------------------------------------------------------------------------
// §2.3 Document shapes
// ---------------------------------------------------------------------------

func TestSpec2025_Language_DocumentShapes(t *testing.T) {
	t.Run("multiple_operations_selected_by_operationName", func(t *testing.T) {
		const document = `query GetGreeting { greeting } query GetA { a } query GetM { m }`

		s25RequireData(t, s25Request{
			Schema: s25langCore(t), Query: document, OperationName: "GetGreeting",
		}, map[string]any{"greeting": "hello"})

		s25RequireData(t, s25Request{
			Schema: s25langCore(t), Query: document, OperationName: "GetA",
		}, map[string]any{"a": "a"})

		s25RequireData(t, s25Request{
			Schema: s25langCore(t), Query: document, OperationName: "GetM",
		}, map[string]any{"m": "m"})
	})

	t.Run("multiple_operations_without_operationName_is_request_error", func(t *testing.T) {
		// §6.1 GetOperation：文档含多个 operation 而未指定 operationName 时请求失败。
		result := s25Do(t, s25Request{
			Schema: s25langCore(t),
			Query:  `query GetGreeting { greeting } query GetA { a }`,
		})
		s25langRequireRequestError(t, result)
	})

	t.Run("unknown_operationName_is_request_error", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:        s25langCore(t),
			Query:         `query GetGreeting { greeting } query GetA { a }`,
			OperationName: "Nope",
		})
		s25langRequireRequestError(t, result)
	})

	t.Run("document_with_only_fragments_is_request_error", func(t *testing.T) {
		// §6.1：请求必须能选出一个 operation；只有 FragmentDefinition 的文档不能执行。
		result := s25Do(t, s25Request{
			Schema: s25langCore(t),
			Query:  `fragment S25LangOnly on Query { greeting }`,
		})
		s25langRequireRequestError(t, result)
	})

	t.Run("operation_with_unused_fragment_is_request_error", func(t *testing.T) {
		// §5.5.1.4 Fragments Must Be Used：文档中定义的每个 fragment 都必须被使用。
		const document = `{ greeting } fragment S25LangUnused on Query { a }`
		s25langRequireParses(t, document)
		s25RequireInvalid(t, s25langCore(t), document)
		result := s25Do(t, s25Request{Schema: s25langCore(t), Query: document})
		s25langRequireRequestError(t, result)
	})

	t.Run("operation_with_used_fragment_executes", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ greeting ...S25LangUsed } fragment S25LangUsed on Query { a }`,
		}, map[string]any{"greeting": "hello", "a": "a"})
	})

	t.Run("fragment_may_be_defined_before_the_operation", func(t *testing.T) {
		// §2.3：ExecutableDefinition 的顺序不影响含义。
		s25langSameData(t, "definition order",
			`fragment S25LangFirst on Query { a } { greeting ...S25LangFirst }`,
			`{ greeting ...S25LangFirst } fragment S25LangFirst on Query { a }`,
			nil)
	})
}

// ---------------------------------------------------------------------------
// §2.4 Operations
// ---------------------------------------------------------------------------

func TestSpec2025_Language_OperationForms(t *testing.T) {
	t.Run("anonymous_shorthand", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ greeting }`},
			map[string]any{"greeting": "hello"})
	})

	t.Run("explicit_query_keyword", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `query { greeting }`},
			map[string]any{"greeting": "hello"})
	})

	t.Run("named_query", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `query GetGreeting { greeting }`},
			map[string]any{"greeting": "hello"})
	})

	t.Run("shorthand_and_explicit_forms_are_equivalent", func(t *testing.T) {
		s25langSameData(t, "shorthand vs explicit",
			`{ greeting a m }`, `query S25LangNamed { greeting a m }`, nil)
	})

	t.Run("query_with_variable_definitions_and_directives", func(t *testing.T) {
		// @s25Tag 声明在 QUERY 位置（见 s25NewIntrospectSchema）。
		s25RequireData(t, s25Request{
			Schema: s25NewIntrospectSchema(t),
			Query: `query S25LangTagged($first: Int = 3) @s25Tag(label: "tagged") {
				item { withArgs(first: $first) }
			}`,
			MetadataDirectives: []string{"s25Tag"},
		}, map[string]any{
			"item": map[string]any{"withArgs": "first=3"},
		})
	})

	t.Run("named_query_with_variable_definitions_only", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:    s25langCore(t),
			Query:     `query S25LangEcho($text: String!, $suffix: String) { echo(text: $text, suffix: $suffix) }`,
			Variables: map[string]any{"text": "hi", "suffix": "?"},
		}, map[string]any{"echo": "hi?"})
	})
}

// ---------------------------------------------------------------------------
// §2.5 Selection Sets / §2.6 Fields
// ---------------------------------------------------------------------------

func TestSpec2025_Language_SelectionSetsAndFields(t *testing.T) {
	t.Run("nested_selection_sets", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ user { id name } people { id name } }`,
		}, map[string]any{
			"user": map[string]any{"id": "u1", "name": "Ada"},
			"people": []any{
				map[string]any{"id": "p1", "name": "Ada"},
				map[string]any{"id": "p2", "name": "Bob"},
				map[string]any{"id": "p3", "name": "Cid"},
			},
		})
	})

	t.Run("leaf_field_without_selection_set", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ greeting }`},
			map[string]any{"greeting": "hello"})
	})

	t.Run("leaf_field_with_selection_set_is_invalid", func(t *testing.T) {
		// §5.3.3 Leaf Field Selections：标量字段不得带选择集。
		s25RequireInvalid(t, s25langCore(t), `{ greeting { a } }`)
	})

	t.Run("composite_field_without_selection_set_is_invalid", func(t *testing.T) {
		s25RequireInvalid(t, s25langCore(t), `{ user }`)
	})

	t.Run("same_field_repeated_is_merged_into_one_response_key", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ greeting greeting greeting }`},
			map[string]any{"greeting": "hello"})
	})

	t.Run("same_nested_field_repeated_is_merged", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ user { id id name } user { name } }`,
		}, map[string]any{
			"user": map[string]any{"id": "u1", "name": "Ada"},
		})
	})

	t.Run("response_key_order_follows_selection_order", func(t *testing.T) {
		// §2.5 选择集是有序的；§6.3 要求响应 map 按查询书写顺序建立。
		result := s25Do(t, s25Request{Schema: s25langCore(t), Query: `{ z a m }`})
		s25RequireNoErrors(t, result)
		s25RequireKeyOrder(t, s25MarshalData(t, result), "z", "a", "m")
	})
}

// ---------------------------------------------------------------------------
// §2.7 Arguments
// ---------------------------------------------------------------------------

func TestSpec2025_Language_Arguments(t *testing.T) {
	t.Run("literal_arguments", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ echo(text: "hi", suffix: "?") }`},
			map[string]any{"echo": "hi?"})
	})

	t.Run("variable_arguments", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:    s25langCore(t),
			Query:     `query S25LangArgs($text: String!, $suffix: String) { echo(text: $text, suffix: $suffix) }`,
			Variables: map[string]any{"text": "hi", "suffix": "??"},
		}, map[string]any{"echo": "hi??"})
	})

	t.Run("omitted_argument_uses_its_default_value", func(t *testing.T) {
		// echo.suffix 默认 "!"；echoOptional.value 默认 "argDefault"。
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echo(text: "hi") echoOptional }`,
		}, map[string]any{"echo": "hi!", "echoOptional": "argDefault"})
	})

	t.Run("omitted_argument_without_default_is_absent", func(t *testing.T) {
		// §6.4.1：既无字面量也无默认值时，该参数不出现在 coerced argument map 中。
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echoNoDefault }`,
		}, map[string]any{"echoNoDefault": "<missing>"})
	})

	t.Run("explicit_null_argument", func(t *testing.T) {
		// §2.10.5：null 字面量与"缺省"语义不同，显式 null 覆盖默认值。
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echo(text: "hi", suffix: null) echoOptional(value: null) echoNoDefault(value: null) }`,
		}, map[string]any{
			"echo":          "hi<null>",
			"echoOptional":  "<null>",
			"echoNoDefault": "<null>",
		})
	})

	t.Run("argument_order_is_independent", func(t *testing.T) {
		s25langSameData(t, "argument order",
			`{ echo(text: "hi", suffix: "?") }`,
			`{ echo(suffix: "?", text: "hi") }`,
			nil)
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ echo(suffix: "?", text: "hi") }`},
			map[string]any{"echo": "hi?"})
	})

	t.Run("argument_names_must_be_unique", func(t *testing.T) {
		// §5.4.2 Argument Uniqueness。
		s25RequireInvalid(t, s25langCore(t), `{ echo(text: "a", text: "b") }`)
	})

	t.Run("required_argument_must_be_provided", func(t *testing.T) {
		// §5.4.2.1 Required Arguments：echo.text 是 String!，没有默认值。
		s25RequireInvalid(t, s25langCore(t), `{ echo(suffix: "?") }`)
	})

	t.Run("undefined_argument_is_invalid", func(t *testing.T) {
		// §5.4.1 Argument Names。
		s25RequireInvalid(t, s25langCore(t), `{ echo(text: "a", nope: 1) }`)
	})
}

// ---------------------------------------------------------------------------
// §2.8 Field Alias
// ---------------------------------------------------------------------------

func TestSpec2025_Language_FieldAlias(t *testing.T) {
	t.Run("alias_renames_the_response_key", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ hi: greeting }`},
			map[string]any{"hi": "hello"})
	})

	t.Run("same_field_twice_under_different_aliases_with_different_arguments", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ first: echo(text: "a", suffix: "1") second: echo(text: "b", suffix: "2") }`,
		}, map[string]any{"first": "a1", "second": "b2"})
	})

	t.Run("alias_may_equal_another_fields_name", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ greeting: a }`},
			map[string]any{"greeting": "a"})
	})

	t.Run("alias_on_nested_field", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ me: user { identifier: id displayName: name } }`,
		}, map[string]any{
			"me": map[string]any{"identifier": "u1", "displayName": "Ada"},
		})
	})

	t.Run("conflicting_aliases_are_invalid", func(t *testing.T) {
		// §5.3.2 Field Selection Merging：同一 response key 必须能合并。
		s25RequireInvalid(t, s25langCore(t), `{ same: greeting same: a }`)
	})
}

// ---------------------------------------------------------------------------
// §2.9 Fragments
// ---------------------------------------------------------------------------

func TestSpec2025_Language_Fragments(t *testing.T) {
	t.Run("named_fragment_spread", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ user { ...S25LangUser } } fragment S25LangUser on S25User { id name }`,
		}, map[string]any{
			"user": map[string]any{"id": "u1", "name": "Ada"},
		})
	})

	t.Run("inline_fragment_with_type_condition", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ nodes { id ... on S25User { name } ... on S25Robot { serial } } }`,
		}, map[string]any{
			"nodes": []any{
				map[string]any{"id": "u1", "name": "Ada"},
				map[string]any{"id": "r1", "serial": "RX-2"},
			},
		})
	})

	t.Run("inline_fragment_without_type_condition", func(t *testing.T) {
		// §2.9.2：省略类型条件时，内联片段继承外层类型。
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ user { ... { id name } } }`,
		}, map[string]any{
			"user": map[string]any{"id": "u1", "name": "Ada"},
		})
	})

	t.Run("inline_fragment_without_type_condition_at_root", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ ... { greeting } a }`,
		}, map[string]any{"greeting": "hello", "a": "a"})
	})

	t.Run("inline_fragment_with_directive_and_no_type_condition", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ ... @include(if: false) { greeting } a }`,
		}, map[string]any{"a": "a"})
	})

	t.Run("three_level_fragment_nesting", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query: `{ user { ...S25LangL1 } }
				fragment S25LangL1 on S25User { id ...S25LangL2 }
				fragment S25LangL2 on S25User { name ...S25LangL3 }
				fragment S25LangL3 on S25User { legacy }`,
		}, map[string]any{
			"user": map[string]any{"id": "u1", "name": "Ada", "legacy": "legacy"},
		})
	})

	t.Run("same_fragment_spread_twice_is_merged", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ user { ...S25LangUser ...S25LangUser } } fragment S25LangUser on S25User { id name }`,
		}, map[string]any{
			"user": map[string]any{"id": "u1", "name": "Ada"},
		})
	})

	t.Run("fragment_on_interface_type", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ nodes { ...S25LangNode } } fragment S25LangNode on S25Node { id __typename }`,
		}, map[string]any{
			"nodes": []any{
				map[string]any{"id": "u1", "__typename": "S25User"},
				map[string]any{"id": "r1", "__typename": "S25Robot"},
			},
		})
	})

	t.Run("fragment_cycles_are_invalid", func(t *testing.T) {
		// §5.5.2.2 Fragment Spreads Must Not Form Cycles。
		// 该用例可能触发无限递归，放到子进程里跑，避免拖垮整个测试二进制。
		if !s25Isolated(t, "fragment-cycles") {
			return
		}
		s25RequireInvalid(t, s25langCore(t),
			`{ user { ...S25LangCycleA } }
			 fragment S25LangCycleA on S25User { id ...S25LangCycleB }
			 fragment S25LangCycleB on S25User { name ...S25LangCycleA }`)
	})

	t.Run("fragment_names_must_be_unique", func(t *testing.T) {
		// §5.5.1.1 Fragment Name Uniqueness。
		s25RequireInvalid(t, s25langCore(t),
			`{ user { ...S25LangDup } }
			 fragment S25LangDup on S25User { id }
			 fragment S25LangDup on S25User { name }`)
	})

	t.Run("fragment_may_not_be_named_on", func(t *testing.T) {
		// §2.9.1 FragmentName :: Name but not `on`
		s25langRequireParseError(t, `{ ...on } fragment on on Query { greeting }`)
	})
}

// ---------------------------------------------------------------------------
// §2.10 Input Values
// ---------------------------------------------------------------------------

func TestSpec2025_Language_InputValueLiterals(t *testing.T) {
	t.Run("int_value", func(t *testing.T) {
		// §2.10.1 IntValue
		result := s25Do(t, s25Request{Schema: s25langCore(t), Query: `{ odd(value: 3) }`})
		s25RequireNoErrors(t, result)
		if got, want := s25MarshalData(t, result), `{"odd":3}`; got != want {
			t.Fatalf("odd literal: got %s, want %s", got, want)
		}

		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echoList(values: [1, -2, 0]) }`,
		}, map[string]any{"echoList": "[1 -2 0]"})
	})

	t.Run("int_value_must_not_have_leading_zero", func(t *testing.T) {
		s25langRequireParseError(t, `{ echoList(values: [013]) }`)
		s25langRequireParseError(t, `{ echoList(values: [-01]) }`)
	})

	t.Run("int_value_must_not_be_followed_by_a_name_start", func(t *testing.T) {
		s25langRequireParseError(t, `{ echoList(values: [1abc]) }`)
	})

	t.Run("float_value", func(t *testing.T) {
		// §2.10.2 FloatValue :: IntegerPart FractionalPart
		//                     | IntegerPart ExponentPart
		//                     | IntegerPart FractionalPart ExponentPart
		s25RequireData(t, s25Request{
			Schema: s25langFloatSchema(t),
			Query: `{
				fraction: floatEcho(value: 1.5)
				exponent: floatEcho(value: 1e2)
				both: floatEcho(value: 1.5e2)
				upperPlus: floatEcho(value: 1.5E+2)
				negativeExponent: floatEcho(value: -1.5e-2)
				intoFloat: floatEcho(value: 2)
			}`,
		}, map[string]any{
			"fraction":         "1.5",
			"exponent":         "100",
			"both":             "150",
			"upperPlus":        "150",
			"negativeExponent": "-0.015",
			"intoFloat":        "2",
		})
	})

	t.Run("malformed_float_values_are_parse_errors", func(t *testing.T) {
		s25langRequireParseError(t, `{ floatEcho(value: 1.) }`)
		s25langRequireParseError(t, `{ floatEcho(value: .5) }`)
		s25langRequireParseError(t, `{ floatEcho(value: 1.0e) }`)
	})

	t.Run("boolean_value", func(t *testing.T) {
		// §2.10.3 BooleanValue :: one of `true` `false`
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echoFilter(filter: {text: "t", nested: {flag: true}}) }`,
		}, map[string]any{
			"echoFilter": "text=t|count=7|mode=A|tags=<missing>|nested=map[flag:true]",
		})

		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echoFilter(filter: {text: "t", nested: {flag: false}}) }`,
		}, map[string]any{
			"echoFilter": "text=t|count=7|mode=A|tags=<missing>|nested=map[flag:false]",
		})
	})

	t.Run("string_value", func(t *testing.T) {
		// §2.10.4 StringValue
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echo(text: "plain", suffix: "") }`,
		}, map[string]any{"echo": "plain"})
	})

	t.Run("string_escaped_characters", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echo(text: "a\tb\"c\\d\/e\nf", suffix: "") }`,
		}, map[string]any{"echo": "a\tb\"c\\d/e\nf"})
	})

	t.Run("string_unicode_escape", func(t *testing.T) {
		// EscapedUnicode: U+0041 -> "A", U+00E9 -> "é"
		query := `{ echo(text: "` + s25langEsc("u0041") + s25langEsc("u00e9") + `", suffix: "") }`
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: query},
			map[string]any{"echo": "Aé"})
	})

	t.Run("string_surrogate_pair_escape", func(t *testing.T) {
		// §2.10.4：EscapedUnicode 的代理对必须组合成单个码点 U+1F600。
		query := `{ echo(text: "` + s25langEsc("ud83d") + s25langEsc("ude00") + `", suffix: "") }`
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: query},
			map[string]any{"echo": "\U0001F600"})
	})

	t.Run("string_braced_unicode_escape", func(t *testing.T) {
		// §2.10.4：\u{1F600} 是合法的 EscapedUnicode 形式。
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echo(text: "\u{1F600}", suffix: "") }`,
		}, map[string]any{"echo": "😀"})
	})

	t.Run("string_literal_supplementary_character", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echo(text: "😀", suffix: "") }`,
		}, map[string]any{"echo": "😀"})
	})

	t.Run("block_string_value", func(t *testing.T) {
		// §2.10.4 BlockString：去掉公共缩进、首尾空行。
		query := "{ echo(text: \"\"\"\n  block\n  value\n\"\"\", suffix: \"\") }"
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: query},
			map[string]any{"echo": "block\nvalue"})
	})

	t.Run("block_string_keeps_quotes_and_backslashes_literal", func(t *testing.T) {
		query := "{ echo(text: \"\"\"a \"quoted\" \\n b\"\"\", suffix: \"\") }"
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: query},
			map[string]any{"echo": `a "quoted" \n b`})
	})

	t.Run("block_string_escaped_triple_quote", func(t *testing.T) {
		query := "{ echo(text: \"\"\"a \\\"\"\" b\"\"\", suffix: \"\") }"
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: query},
			map[string]any{"echo": `a """ b`})
	})

	t.Run("null_value", func(t *testing.T) {
		// §2.10.5 NullValue :: `null`
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echo(text: "x", suffix: null) }`,
		}, map[string]any{"echo": "x<null>"})
	})

	t.Run("null_is_not_an_enum_value", func(t *testing.T) {
		// §2.10.6 EnumValue :: Name but not `true`, `false` or `null`
		s25RequireInvalid(t, s25langCore(t), `{ echoMode(mode: true) }`)
		s25RequireInvalid(t, s25langCore(t), `{ echoMode(mode: NOT_A_MODE) }`)
	})

	t.Run("enum_value", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ echoMode(mode: B) }`},
			map[string]any{"echoMode": "B"})
		// DEPRECATED_C 的内部值是 "C"。
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ echoMode(mode: DEPRECATED_C) }`},
			map[string]any{"echoMode": "C"})
		// 省略时使用参数默认值 "A"。
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ echoMode }`},
			map[string]any{"echoMode": "A"})
	})

	t.Run("enum_value_must_not_be_quoted", func(t *testing.T) {
		s25RequireInvalid(t, s25langCore(t), `{ echoMode(mode: "B") }`)
	})

	t.Run("list_value", func(t *testing.T) {
		// §2.10.7 ListValue
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ echoList(values: [1, 2, 3]) }`},
			map[string]any{"echoList": "[1 2 3]"})
	})

	t.Run("empty_list_value", func(t *testing.T) {
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ echoList(values: []) }`},
			map[string]any{"echoList": "[]"})
	})

	t.Run("single_value_is_coerced_into_a_one_element_list", func(t *testing.T) {
		// §6.4.4 Input Coercion：非列表值在列表位置上被包成单元素列表。
		s25RequireData(t, s25Request{Schema: s25langCore(t), Query: `{ echoList(values: 5) }`},
			map[string]any{"echoList": "[5]"})
	})

	t.Run("nested_list_value", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echoNestedList(values: [[1, 2], [3]]) }`,
		}, map[string]any{"echoNestedList": "[[1 2] [3]]"})
	})

	t.Run("nested_empty_list_value", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echoNestedList(values: []) }`,
		}, map[string]any{"echoNestedList": "[]"})
	})

	t.Run("input_object_value", func(t *testing.T) {
		// §2.10.8 ObjectValue；未提供且有默认值的输入字段取默认值，
		// 未提供且无默认值的输入字段不出现。
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echoFilter(filter: {text: "t"}) }`,
		}, map[string]any{
			"echoFilter": "text=t|count=7|mode=A|tags=<missing>|nested=<missing>",
		})
	})

	t.Run("input_object_field_order_is_insignificant", func(t *testing.T) {
		s25langSameData(t, "input object field order",
			`{ echoFilter(filter: {text: "t", count: 2, mode: B}) }`,
			`{ echoFilter(filter: {mode: B, count: 2, text: "t"}) }`,
			nil)
	})

	t.Run("empty_input_object_value", func(t *testing.T) {
		// S25Inner 的所有字段都可省略，因此 {} 是合法的。flag 取默认值 false。
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echoFilter(filter: {text: "t", nested: {}}) }`,
		}, map[string]any{
			"echoFilter": "text=t|count=7|mode=A|tags=<missing>|nested=map[flag:false]",
		})
	})

	t.Run("fully_populated_nested_input_object", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query: `{ echoFilter(filter: {
				text: "t"
				count: 2
				mode: B
				tags: ["x", "y"]
				nested: {flag: true, values: [1, 2]}
			}) }`,
		}, map[string]any{
			"echoFilter": "text=t|count=2|mode=B|tags=[x y]|nested=map[flag:true values:[1 2]]",
		})
	})

	t.Run("explicit_null_input_object_field", func(t *testing.T) {
		// §2.10.5 / §6.4.1：显式 null 会覆盖字段默认值。
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ echoFilter(filter: {text: "t", count: null, tags: null}) }`,
		}, map[string]any{
			"echoFilter": "text=t|count=<null>|mode=A|tags=<null>|nested=<missing>",
		})
	})

	t.Run("input_object_required_field_must_be_present", func(t *testing.T) {
		// §5.6.4 Input Object Required Fields：S25Filter.text 是 String!。
		s25RequireInvalid(t, s25langCore(t), `{ echoFilter(filter: {count: 1}) }`)
	})

	t.Run("input_object_field_names_must_be_unique", func(t *testing.T) {
		// §5.6.3 Input Object Field Uniqueness。
		s25RequireInvalid(t, s25langCore(t), `{ echoFilter(filter: {text: "a", text: "b"}) }`)
	})

	t.Run("unknown_input_object_field_is_invalid", func(t *testing.T) {
		// §5.6.2 Input Object Field Names。
		s25RequireInvalid(t, s25langCore(t), `{ echoFilter(filter: {text: "a", nope: 1}) }`)
	})

	t.Run("custom_scalar_literal", func(t *testing.T) {
		// S25Odd 只接受奇数字面量。
		result := s25Do(t, s25Request{Schema: s25langCore(t), Query: `{ odd(value: 7) }`})
		s25RequireNoErrors(t, result)
		if got, want := s25MarshalData(t, result), `{"odd":7}`; got != want {
			t.Fatalf("odd literal: got %s, want %s", got, want)
		}
		// 偶数不能被 ParseLiteral 接受 —— §5.6.1 Values of Correct Type。
		s25RequireInvalid(t, s25langCore(t), `{ odd(value: 4) }`)
	})
}

// ---------------------------------------------------------------------------
// §2.11 Variables / §2.12 Type References
// ---------------------------------------------------------------------------

func TestSpec2025_Language_VariablesAndTypeReferences(t *testing.T) {
	t.Run("variable_with_default_value", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `query S25LangDefault($text: String! = "fallback") { echo(text: $text, suffix: "") }`,
		}, map[string]any{"echo": "fallback"})
	})

	t.Run("supplied_value_overrides_the_variable_default", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:    s25langCore(t),
			Query:     `query S25LangDefault($text: String! = "fallback") { echo(text: $text, suffix: "") }`,
			Variables: map[string]any{"text": "given"},
		}, map[string]any{"echo": "given"})
	})

	t.Run("non_null_variable", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:    s25langCore(t),
			Query:     `query S25LangNonNull($text: String!) { echo(text: $text, suffix: "") }`,
			Variables: map[string]any{"text": "given"},
		}, map[string]any{"echo": "given"})
	})

	t.Run("missing_non_null_variable_is_a_request_error", func(t *testing.T) {
		// §6.1.2 CoerceVariableValues：非空变量缺值时请求失败，响应不含 data。
		result := s25Do(t, s25Request{
			Schema: s25langCore(t),
			Query:  `query S25LangNonNull($text: String!) { echo(text: $text) }`,
		})
		s25langRequireRequestError(t, result)
	})

	t.Run("null_for_non_null_variable_is_a_request_error", func(t *testing.T) {
		result := s25Do(t, s25Request{
			Schema:    s25langCore(t),
			Query:     `query S25LangNonNull($text: String!) { echo(text: $text) }`,
			Variables: map[string]any{"text": nil},
		})
		s25langRequireRequestError(t, result)
	})

	t.Run("nullable_variable_default_is_used_when_absent", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `query S25LangOpt($value: String = "fromVariableDefault") { echoNoDefault(value: $value) }`,
		}, map[string]any{"echoNoDefault": "fromVariableDefault"})
	})

	t.Run("nested_non_null_list_type_reference", func(t *testing.T) {
		// §2.12 TypeReference: [[Int!]!]!
		s25RequireData(t, s25Request{
			Schema:    s25langCore(t),
			Query:     `query S25LangMatrix($values: [[Int!]!]!) { echoNestedList(values: $values) }`,
			Variables: map[string]any{"values": []any{[]any{1, 2}, []any{3}}},
		}, map[string]any{"echoNestedList": "[[1 2] [3]]"})
	})

	t.Run("nested_list_type_reference_with_default", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `query S25LangMatrix($values: [[Int!]!]! = [[4, 5]]) { echoNestedList(values: $values) }`,
		}, map[string]any{"echoNestedList": "[[4 5]]"})
	})

	t.Run("variable_inside_a_list_literal", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:    s25langCore(t),
			Query:     `query S25LangInList($n: Int!) { echoList(values: [1, $n, 3]) }`,
			Variables: map[string]any{"n": 2},
		}, map[string]any{"echoList": "[1 2 3]"})
	})

	t.Run("variable_inside_an_input_object_literal", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:    s25langCore(t),
			Query:     `query S25LangInObject($text: String!, $count: Int) { echoFilter(filter: {text: $text, count: $count}) }`,
			Variables: map[string]any{"text": "t", "count": 3},
		}, map[string]any{
			"echoFilter": "text=t|count=3|mode=A|tags=<missing>|nested=<missing>",
		})
	})

	t.Run("variable_inside_a_nested_input_object_literal", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:    s25langCore(t),
			Query:     `query S25LangNestedVar($values: [Int!]) { echoFilter(filter: {text: "t", nested: {values: $values}}) }`,
			Variables: map[string]any{"values": []any{1, 2}},
		}, map[string]any{
			"echoFilter": "text=t|count=7|mode=A|tags=<missing>|nested=map[flag:false values:[1 2]]",
		})
	})

	t.Run("variable_names_must_be_unique", func(t *testing.T) {
		// §5.8.1 Variable Uniqueness。
		s25RequireInvalid(t, s25langCore(t),
			`query S25LangDup($text: String!, $text: String!) { echo(text: $text) }`)
	})

	t.Run("undefined_variable_is_invalid", func(t *testing.T) {
		// §5.8.3 All Variable Uses Defined。
		s25RequireInvalid(t, s25langCore(t), `query S25LangUndef { echo(text: $nope) }`)
	})

	t.Run("unused_variable_is_invalid", func(t *testing.T) {
		// §5.8.4 All Variables Used。
		s25RequireInvalid(t, s25langCore(t),
			`query S25LangUnused($text: String!) { greeting }`)
	})

	t.Run("variable_default_must_be_a_constant", func(t *testing.T) {
		// §2.11：DefaultValue 使用 Value[Const]，不能引用另一个变量。
		s25langRequireParseError(t,
			`query S25LangConst($a: String!, $b: String! = $a) { echo(text: $b) }`)
	})

	t.Run("variables_may_only_be_input_types", func(t *testing.T) {
		// §5.8.2 Variables Are Input Types。
		s25RequireInvalid(t, s25langCore(t),
			`query S25LangBadType($u: S25User) { greeting }`)
	})
}

// ---------------------------------------------------------------------------
// §2.13 Directives
// ---------------------------------------------------------------------------

func TestSpec2025_Language_DirectivesAtEveryExecutableLocation(t *testing.T) {
	t.Run("skip_and_include_on_field", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ greeting @skip(if: true) a @include(if: false) m @include(if: true) z @skip(if: false) }`,
		}, map[string]any{"m": "m", "z": "z"})
	})

	t.Run("skip_and_include_with_variables_on_field", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:    s25langCore(t),
			Query:     `query S25LangCond($skip: Boolean!, $include: Boolean!) { a @skip(if: $skip) m @include(if: $include) z }`,
			Variables: map[string]any{"skip": true, "include": false},
		}, map[string]any{"z": "z"})
	})

	t.Run("skip_and_include_on_fragment_spread", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query: `{ ...S25LangSkipped @skip(if: true) ...S25LangIncluded @include(if: true) m }
				fragment S25LangSkipped on Query { a }
				fragment S25LangIncluded on Query { z }`,
		}, map[string]any{"z": "z", "m": "m"})
	})

	t.Run("skip_and_include_on_inline_fragment", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query: `{
				... on Query @include(if: false) { a }
				... on Query @skip(if: false) { z }
				m
			}`,
		}, map[string]any{"z": "z", "m": "m"})
	})

	t.Run("skip_wins_over_include", func(t *testing.T) {
		// §3.13.1 / §3.13.2：@skip(if: true) 时字段一定不被包含。
		s25RequireData(t, s25Request{
			Schema: s25langCore(t),
			Query:  `{ a @skip(if: true) @include(if: true) m }`,
		}, map[string]any{"m": "m"})
	})

	t.Run("custom_directive_on_query", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:             s25NewIntrospectSchema(t),
			Query:              `query S25LangTaggedQuery @s25Tag(label: "query") { mode }`,
			MetadataDirectives: []string{"s25Tag"},
		}, map[string]any{"mode": "A"})
	})

	t.Run("custom_directive_on_fragment_definition", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25NewIntrospectSchema(t),
			Query: `{ ...S25LangTaggedFrag }
				fragment S25LangTaggedFrag on Query @s25Tag(label: "fragment") { mode }`,
			MetadataDirectives: []string{"s25Tag"},
		}, map[string]any{"mode": "A"})
	})

	t.Run("custom_directive_on_fragment_spread_and_inline_fragment", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema: s25NewIntrospectSchema(t),
			Query: `{
				...S25LangSpread @s25Tag(label: "spread")
				... on Query @s25Tag(label: "inline") { deprecat }
			}
			fragment S25LangSpread on Query { mode }`,
			MetadataDirectives: []string{"s25Tag"},
		}, map[string]any{"mode": "A", "deprecat": "x"})
	})

	t.Run("custom_directive_on_field", func(t *testing.T) {
		s25RequireData(t, s25Request{
			Schema:             s25NewIntrospectSchema(t),
			Query:              `{ mode @s25Tag(label: "field") }`,
			MetadataDirectives: []string{"s25Tag"},
		}, map[string]any{"mode": "A"})
	})

	t.Run("custom_directive_on_variable_definition", func(t *testing.T) {
		// §2.13 / §3.13：VARIABLE_DEFINITION 是一个可执行指令位置。
		const query = `query S25LangTaggedVar($first: Int = 1 @s25Tag(label: "variable")) {
			item { withArgs(first: $first) }
		}`
		s25langRequireParses(t, query)
		result := s25Do(t, s25Request{Schema: s25NewIntrospectSchema(t), Query: query})
		s25RequireNoErrors(t, result)
	})

	t.Run("directive_in_a_location_it_does_not_declare_is_invalid", func(t *testing.T) {
		// §5.7.2 Directives Are In Valid Locations：@skip 只声明了
		// FIELD / FRAGMENT_SPREAD / INLINE_FRAGMENT。
		s25RequireInvalid(t, s25NewIntrospectSchema(t),
			`query S25LangBadLoc @skip(if: true) { mode }`)
	})

	t.Run("unknown_directive_is_invalid", func(t *testing.T) {
		// §5.7.1 Directives Are Defined。
		s25RequireInvalid(t, s25langCore(t), `{ greeting @s25NotADirective }`)
	})

	t.Run("directive_required_argument_must_be_provided", func(t *testing.T) {
		// §5.4.2.1：@skip(if:) 是 Boolean!。
		s25RequireInvalid(t, s25langCore(t), `{ greeting @skip }`)
	})

	t.Run("directives_are_not_repeatable_by_default", func(t *testing.T) {
		// §5.7.3 Directives Are Unique Per Location。
		s25RequireInvalid(t, s25langCore(t), `{ greeting @skip(if: true) @skip(if: false) }`)
	})

	t.Run("skip_and_include_are_order_independent", func(t *testing.T) {
		// §2.13：指令的书写顺序在文档中被保留，但 @skip / @include 的结果与顺序无关。
		s25langSameData(t, "directive order",
			`{ a @skip(if: false) @include(if: true) }`,
			`{ a @include(if: true) @skip(if: false) }`,
			nil)
	})
}
