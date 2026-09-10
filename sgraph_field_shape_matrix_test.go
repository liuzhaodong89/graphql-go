package graphql

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

// 本文件按「实现维度」而非「规范章节」枚举用例，覆盖 spec2025 矩阵照不到的组合：
//
//	{父字段有无 resolver} × {父字段形状} × {子字段类型}
//
// spec2025 的 526 条用例按 GraphQL 规范章节组织，而「字段有没有配 resolver」是纯实现细节，
// 规范里没有这个概念，因此那套语料天然不会枚举这些组合。实测确认语料中：
//   - 元素是 Object 的 list 字段（people/nodes/search/badNode）全部带 Resolve，且全在根层级；
//   - 唯一无 Resolve 的 list 字段（wrapped）元素是 String，没有子选择集。
//
// 于是「无 resolver 的中间层 list + Object 元素 + 子选择集」这一形态从未被覆盖，
// 而它正是 "field N depends on field M which does not produce a FieldResponse" 的触发条件。
//
// 用例以原生链路（ExecuteGraphQLGo）为 oracle。已知分歧的用例同样写成断言（aligned=false），
// 一旦缺陷修复、两条链路对齐，该断言会失败并提示把 aligned 改为 true——
// 避免修复后测试静默漂移。

// ---------------------------------------------------------------------------
// 执行与比较
// ---------------------------------------------------------------------------

type shapeOutcome struct {
	data      any
	hasErrors bool
	raw       string
}

func shapeRunSGraph(t *testing.T, schema Schema, query string) shapeOutcome {
	t.Helper()
	return shapeNormalize(t, Do(Params{Schema: schema, RequestString: query}))
}

func shapeRunNative(t *testing.T, schema Schema, query string) shapeOutcome {
	t.Helper()
	document, parseErr := parser.Parse(parser.ParseParams{
		Source: source.NewSource(&source.Source{Body: []byte(query)}),
	})
	if parseErr != nil {
		t.Fatalf("parse %s: %v", query, parseErr)
	}
	return shapeNormalize(t, ExecuteGraphQLGo(ExecuteParams{Schema: schema, AST: document}))
}

// shapeNormalize 把两条链路的结果归一化成可比较形态。
// sgraph 返回 *SGraphResponseOrderedMap、原生返回 map[string]interface{}，
// 统一经 JSON 往返后比较，因此只比较内容不比较字段顺序（顺序另有专项用例覆盖）。
func shapeNormalize(t *testing.T, result *Result) shapeOutcome {
	t.Helper()
	if result == nil {
		t.Fatalf("result is nil")
	}
	rawBytes, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatalf("marshal result: %v", marshalErr)
	}
	dataBytes, dataMarshalErr := json.Marshal(result.Data)
	if dataMarshalErr != nil {
		t.Fatalf("marshal data: %v", dataMarshalErr)
	}
	var data any
	if unmarshalErr := json.Unmarshal(dataBytes, &data); unmarshalErr != nil {
		t.Fatalf("unmarshal data: %v", unmarshalErr)
	}
	return shapeOutcome{data: data, hasErrors: len(result.Errors) != 0, raw: string(rawBytes)}
}

func shapeRegisterEngine(t *testing.T, schema *Schema) {
	t.Helper()
	engine, engineErr := NewSGraphEngine(schema, NewDirectiveRegistry(), NewParamRegistry())
	if engineErr != nil {
		t.Fatalf("new sgraph engine: %v", engineErr)
	}
	if registerErr := RegisterSGraphEngine(engine); registerErr != nil {
		t.Fatalf("register sgraph engine: %v", registerErr)
	}
}

// ---------------------------------------------------------------------------
// 矩阵用例
// ---------------------------------------------------------------------------

type shapeCase struct {
	name  string
	build func(t *testing.T) Schema
	query string
	// aligned 为 true 表示两条链路必须产出相同 data 且错误有无一致（回归保护）；
	// 为 false 表示当前存在已知分歧，note 说明原因。
	aligned bool
	note    string
	// unorderedLists 用于结果中列表顺序本身不确定的查询（例如 __schema.types 由
	// TypeMap 的 map 遍历决定顺序，两条链路都不保证稳定）。开启后比较前对列表排序。
	// 响应字段与列表的保序语义由 spec2025 的 §7 专项用例覆盖，此处不重复。
	unorderedLists bool
}

// shapeSortLists 递归地把结果中的列表按元素的 JSON 表示排序，用于顺序不确定的比较。
func shapeSortLists(value any) any {
	switch typed := value.(type) {
	case []any:
		sorted := make([]any, 0, len(typed))
		for _, element := range typed {
			sorted = append(sorted, shapeSortLists(element))
		}
		sort.SliceStable(sorted, func(left, right int) bool {
			leftBytes, _ := json.Marshal(sorted[left])
			rightBytes, _ := json.Marshal(sorted[right])
			return string(leftBytes) < string(rightBytes)
		})
		return sorted
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, element := range typed {
			result[key] = shapeSortLists(element)
		}
		return result
	}
	return value
}

func runShapeCases(t *testing.T, cases []shapeCase) {
	t.Helper()
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			schema := testCase.build(t)
			shapeRegisterEngine(t, &schema)

			sgraphOutcome := shapeRunSGraph(t, schema, testCase.query)

			// -race 下不调用原生 oracle：原生 executor 在并发子字段时对共享
			// executionContext.Context 有已知竞态（extensions.go:196/197 ←
			// executor.go:400 executeSubFields），只要查询里有 list 加多个子字段就会命中。
			// 那是 CONFORMANCE_MATRIX.md 已记录的原生缺陷，与被测的 sgraph 无关。
			// 此处仍完整执行 sgraph 链路，race detector 照样观察 sgraph 的代码路径；
			// 两条链路的数据一致性交给非 -race 运行负责。
			if raceDetectorEnabled {
				return
			}

			nativeOutcome := shapeRunNative(t, schema, testCase.query)

			sgraphData := sgraphOutcome.data
			nativeData := nativeOutcome.data
			if testCase.unorderedLists {
				sgraphData = shapeSortLists(sgraphData)
				nativeData = shapeSortLists(nativeData)
			}

			aligned := reflect.DeepEqual(sgraphData, nativeData) &&
				sgraphOutcome.hasErrors == nativeOutcome.hasErrors

			if testCase.aligned && !aligned {
				t.Errorf("两条链路应当一致但出现分歧\n  query : %s\n  sgraph: %s\n  native: %s",
					testCase.query, sgraphOutcome.raw, nativeOutcome.raw)
				return
			}
			if !testCase.aligned && aligned {
				t.Errorf("已知分歧已消失（%s），请把该用例的 aligned 改为 true\n  query : %s\n  两链路: %s",
					testCase.note, testCase.query, sgraphOutcome.raw)
				return
			}
			if !testCase.aligned {
				t.Logf("已知分歧（%s）\n  sgraph: %s\n  native: %s",
					testCase.note, sgraphOutcome.raw, nativeOutcome.raw)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// schema 工厂：父字段形状 × 父字段 resolver 有无 × 子字段类型
// ---------------------------------------------------------------------------

// shapeLeafFields 构造叶子对象的字段集。childKind 决定子字段形态：
//
//	"plain"    无 resolver 的标量，走结果组装阶段的属性提取
//	"resolver" 带 resolver 的标量，需要在执行阶段拿到父数据
//	"typename" 只选 __typename
func shapeLeafFields(childKind string) Fields {
	fields := Fields{
		"id": &Field{Type: String},
	}
	if childKind == "resolver" {
		fields["computed"] = &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
			return "COMPUTED", nil
		}}
	}
	return fields
}

// shapeBuildObjectParent 父字段是单个 Object。
func shapeBuildObjectParent(namePrefix string, parentHasResolver bool, childKind string) func(t *testing.T) Schema {
	return func(t *testing.T) Schema {
		t.Helper()
		leaf := NewObject(ObjectConfig{Name: namePrefix + "Leaf", Fields: shapeLeafFields(childKind)})
		holder := &Field{Type: leaf}
		if parentHasResolver {
			holder.Resolve = func(ResolveParams) (any, error) {
				return map[string]any{"id": "leaf-1"}, nil
			}
		}
		wrapper := NewObject(ObjectConfig{Name: namePrefix + "Wrap", Fields: Fields{"holder": holder}})
		schema, schemaErr := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"wrap": &Field{Type: wrapper, Resolve: func(ResolveParams) (any, error) {
					return map[string]any{"holder": map[string]any{"id": "leaf-1"}}, nil
				}},
			}}),
			Types: []Type{leaf},
		})
		if schemaErr != nil {
			t.Fatalf("build schema: %v", schemaErr)
		}
		return schema
	}
}

// shapeBuildListParent 父字段是 Object 列表。
func shapeBuildListParent(namePrefix string, parentHasResolver bool, childKind string) func(t *testing.T) Schema {
	return func(t *testing.T) Schema {
		t.Helper()
		leaf := NewObject(ObjectConfig{Name: namePrefix + "Leaf", Fields: shapeLeafFields(childKind)})
		holder := &Field{Type: NewList(leaf)}
		if parentHasResolver {
			holder.Resolve = func(ResolveParams) (any, error) {
				return []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}, nil
			}
		}
		wrapper := NewObject(ObjectConfig{Name: namePrefix + "Wrap", Fields: Fields{"holder": holder}})
		schema, schemaErr := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"wrap": &Field{Type: wrapper, Resolve: func(ResolveParams) (any, error) {
					return map[string]any{"holder": []any{
						map[string]any{"id": "a"},
						map[string]any{"id": "b"},
					}}, nil
				}},
			}}),
			Types: []Type{leaf},
		})
		if schemaErr != nil {
			t.Fatalf("build schema: %v", schemaErr)
		}
		return schema
	}
}

// shapeBuildNestedListParent 父字段是二维 Object 列表，覆盖 occurrence 路径的嵌套下标。
func shapeBuildNestedListParent(namePrefix string, parentHasResolver bool, childKind string) func(t *testing.T) Schema {
	return func(t *testing.T) Schema {
		t.Helper()
		leaf := NewObject(ObjectConfig{Name: namePrefix + "Leaf", Fields: shapeLeafFields(childKind)})
		holder := &Field{Type: NewList(NewList(leaf))}
		rows := []any{
			[]any{map[string]any{"id": "a0"}, map[string]any{"id": "a1"}},
			[]any{map[string]any{"id": "b0"}},
		}
		if parentHasResolver {
			holder.Resolve = func(ResolveParams) (any, error) { return rows, nil }
		}
		wrapper := NewObject(ObjectConfig{Name: namePrefix + "Wrap", Fields: Fields{"holder": holder}})
		schema, schemaErr := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"wrap": &Field{Type: wrapper, Resolve: func(ResolveParams) (any, error) {
					return map[string]any{"holder": rows}, nil
				}},
			}}),
			Types: []Type{leaf},
		})
		if schemaErr != nil {
			t.Fatalf("build schema: %v", schemaErr)
		}
		return schema
	}
}

// shapeBuildInterfaceParent 父字段是 Interface，类型需要运行时判定。
func shapeBuildInterfaceParent(namePrefix string, parentHasResolver bool, childKind string, asList bool) func(t *testing.T) Schema {
	return func(t *testing.T) Schema {
		t.Helper()
		iface := NewInterface(InterfaceConfig{
			Name:   namePrefix + "Node",
			Fields: Fields{"id": &Field{Type: String}},
		})
		implFields := shapeLeafFields(childKind)
		implFields["id"] = &Field{Type: String}
		impl := NewObject(ObjectConfig{
			Name:       namePrefix + "Impl",
			Interfaces: []*Interface{iface},
			Fields:     implFields,
		})
		iface.ResolveType = func(ResolveTypeParams) *Object { return impl }

		var holderType Output = iface
		payload := any(map[string]any{"id": "n1"})
		if asList {
			holderType = NewList(iface)
			payload = []any{map[string]any{"id": "n1"}, map[string]any{"id": "n2"}}
		}
		holder := &Field{Type: holderType}
		if parentHasResolver {
			holder.Resolve = func(ResolveParams) (any, error) { return payload, nil }
		}
		wrapper := NewObject(ObjectConfig{Name: namePrefix + "Wrap", Fields: Fields{"holder": holder}})
		schema, schemaErr := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"wrap": &Field{Type: wrapper, Resolve: func(ResolveParams) (any, error) {
					return map[string]any{"holder": payload}, nil
				}},
			}}),
			Types: []Type{impl},
		})
		if schemaErr != nil {
			t.Fatalf("build schema: %v", schemaErr)
		}
		return schema
	}
}

// shapeBuildUnionParent 父字段是 Union。
func shapeBuildUnionParent(namePrefix string, parentHasResolver bool, asList bool) func(t *testing.T) Schema {
	return func(t *testing.T) Schema {
		t.Helper()
		member := NewObject(ObjectConfig{Name: namePrefix + "Member", Fields: Fields{
			"id": &Field{Type: String},
		}})
		union := NewUnion(UnionConfig{
			Name:        namePrefix + "Union",
			Types:       []*Object{member},
			ResolveType: func(ResolveTypeParams) *Object { return member },
		})
		var holderType Output = union
		payload := any(map[string]any{"id": "u1"})
		if asList {
			holderType = NewList(union)
			payload = []any{map[string]any{"id": "u1"}, map[string]any{"id": "u2"}}
		}
		holder := &Field{Type: holderType}
		if parentHasResolver {
			holder.Resolve = func(ResolveParams) (any, error) { return payload, nil }
		}
		wrapper := NewObject(ObjectConfig{Name: namePrefix + "Wrap", Fields: Fields{"holder": holder}})
		schema, schemaErr := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"wrap": &Field{Type: wrapper, Resolve: func(ResolveParams) (any, error) {
					return map[string]any{"holder": payload}, nil
				}},
			}}),
			Types: []Type{member},
		})
		if schemaErr != nil {
			t.Fatalf("build schema: %v", schemaErr)
		}
		return schema
	}
}

// ---------------------------------------------------------------------------
// 1. 正交矩阵：父字段形状 × resolver 有无 × 子字段类型
// ---------------------------------------------------------------------------

func TestSGraphShapeMatrix_ObjectParent(t *testing.T) {
	runShapeCases(t, []shapeCase{
		{
			name:    "有resolver/无resolver子标量",
			build:   shapeBuildObjectParent("SMObjRP", true, "plain"),
			query:   `{ wrap { holder { id } } }`,
			aligned: true,
		},
		{
			name:    "有resolver/带resolver子标量",
			build:   shapeBuildObjectParent("SMObjRR", true, "resolver"),
			query:   `{ wrap { holder { computed } } }`,
			aligned: true,
		},
		{
			name:    "有resolver/__typename",
			build:   shapeBuildObjectParent("SMObjRT", true, "typename"),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
		{
			name:    "无resolver/无resolver子标量",
			build:   shapeBuildObjectParent("SMObjNP", false, "plain"),
			query:   `{ wrap { holder { id } } }`,
			aligned: true,
		},
		{
			name:    "无resolver/带resolver子标量",
			build:   shapeBuildObjectParent("SMObjNR", false, "resolver"),
			query:   `{ wrap { holder { computed } } }`,
			aligned: true,
		},
		{
			name:    "无resolver/__typename",
			build:   shapeBuildObjectParent("SMObjNT", false, "typename"),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
	})
}

func TestSGraphShapeMatrix_ListParent(t *testing.T) {
	runShapeCases(t, []shapeCase{
		{
			name:    "有resolver/无resolver子标量",
			build:   shapeBuildListParent("SMLstRP", true, "plain"),
			query:   `{ wrap { holder { id } } }`,
			aligned: true,
		},
		{
			name:    "有resolver/带resolver子标量",
			build:   shapeBuildListParent("SMLstRR", true, "resolver"),
			query:   `{ wrap { holder { computed } } }`,
			aligned: true,
		},
		{
			name:    "有resolver/__typename",
			build:   shapeBuildListParent("SMLstRT", true, "typename"),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
		{
			name:    "无resolver/无resolver子标量",
			build:   shapeBuildListParent("SMLstNP", false, "plain"),
			query:   `{ wrap { holder { id } } }`,
			aligned: true,
		},
		{
			// P0-1 触发面之二：list 中间层无 resolver，子字段自带 resolver。
			name:    "无resolver/带resolver子标量",
			build:   shapeBuildListParent("SMLstNR", false, "resolver"),
			query:   `{ wrap { holder { computed } } }`,
			aligned: true,
		},
		{
			// P0-1 触发面之一：Apollo 默认为每个选择集注入 __typename 时命中。
			name:    "无resolver/__typename",
			build:   shapeBuildListParent("SMLstNT", false, "typename"),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
	})
}

func TestSGraphShapeMatrix_NestedListParent(t *testing.T) {
	runShapeCases(t, []shapeCase{
		{
			name:    "有resolver/无resolver子标量",
			build:   shapeBuildNestedListParent("SMNstRP", true, "plain"),
			query:   `{ wrap { holder { id } } }`,
			aligned: true,
		},
		{
			name:    "有resolver/__typename",
			build:   shapeBuildNestedListParent("SMNstRT", true, "typename"),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
		{
			name:    "无resolver/无resolver子标量",
			build:   shapeBuildNestedListParent("SMNstNP", false, "plain"),
			query:   `{ wrap { holder { id } } }`,
			aligned: true,
		},
		{
			name:    "无resolver/__typename",
			build:   shapeBuildNestedListParent("SMNstNT", false, "typename"),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
	})
}

func TestSGraphShapeMatrix_AbstractParent(t *testing.T) {
	runShapeCases(t, []shapeCase{
		{
			name:    "interface单值/有resolver/__typename",
			build:   shapeBuildInterfaceParent("SMIfcRT", true, "typename", false),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
		{
			name:    "interface单值/无resolver/无resolver子标量",
			build:   shapeBuildInterfaceParent("SMIfcNP", false, "plain", false),
			query:   `{ wrap { holder { id } } }`,
			aligned: true,
		},
		{
			name:    "interface单值/无resolver/__typename",
			build:   shapeBuildInterfaceParent("SMIfcNT", false, "typename", false),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
		{
			name:    "interface列表/无resolver/__typename",
			build:   shapeBuildInterfaceParent("SMIfcLNT", false, "typename", true),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
		{
			name:    "union单值/有resolver/__typename",
			build:   shapeBuildUnionParent("SMUniRT", true, false),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
		{
			name:    "union单值/无resolver/__typename",
			build:   shapeBuildUnionParent("SMUniNT", false, false),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
		{
			name:    "union列表/无resolver/__typename",
			build:   shapeBuildUnionParent("SMUniLNT", false, true),
			query:   `{ wrap { holder { __typename } } }`,
			aligned: true,
		},
	})
}

// ---------------------------------------------------------------------------
// 2. 内省专项
//
// spec2025 的内省用例（31 条）全部落在「__typename 直接挂在有 resolver 的字段下」，
// 没有覆盖「内省中间层字段（types/fields/enumValues）下带 __typename」这一形态。
// 而内省中间结果由 __schema/__type 的 resolver 一次性产出（GenerateTypeMetaResult），
// types/fields 本身没有独立 resolver，正是 P0-1 的触发条件。
//
// 其中 fields/enumValues/inputFields 带 includeDeprecated 默认值参数，
// compileParamPlansByArgDefs 对有默认值的未提供参数也会产出 CONST paramPlan，
// 因此它们与无参数的 types 必须分开覆盖——任何以 len(paramPlans)==0 为前提的
// 修复方案都会漏掉带参数的这一半。
// ---------------------------------------------------------------------------

func shapeIntrospectionSchema(t *testing.T) Schema {
	t.Helper()
	item := NewObject(ObjectConfig{
		Name: "SMIntroItem",
		Fields: Fields{
			"id":     &Field{Type: NewNonNull(ID)},
			"legacy": &Field{Type: String, DeprecationReason: "use id"},
		},
	})
	mode := NewEnum(EnumConfig{
		Name: "SMIntroMode",
		Values: EnumValueConfigMap{
			"A": &EnumValueConfig{Value: "A"},
			"B": &EnumValueConfig{Value: "B", DeprecationReason: "use A"},
		},
	})
	input := NewInputObject(InputObjectConfig{
		Name:   "SMIntroInput",
		Fields: InputObjectConfigFieldMap{"text": &InputObjectFieldConfig{Type: String}},
	})
	schema, schemaErr := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
			"item": &Field{Type: item, Resolve: func(ResolveParams) (any, error) {
				return map[string]any{"id": "i1"}, nil
			}},
			"mode": &Field{Type: mode, Resolve: func(ResolveParams) (any, error) { return "A", nil }},
			"echo": &Field{
				Type:    String,
				Args:    FieldConfigArgument{"in": &ArgumentConfig{Type: input}},
				Resolve: func(ResolveParams) (any, error) { return "ok", nil },
			},
		}}),
		Types: []Type{item, mode, input},
	})
	if schemaErr != nil {
		t.Fatalf("build introspection schema: %v", schemaErr)
	}
	return schema
}

func TestSGraphShapeMatrix_Introspection(t *testing.T) {
	build := func(t *testing.T) Schema { return shapeIntrospectionSchema(t) }
	runShapeCases(t, []shapeCase{
		{
			name:           "types无__typename",
			build:          build,
			query:          `{ __schema { types { name } } }`,
			aligned:        true,
			unorderedLists: true,
		},
		{
			name:           "types带__typename",
			build:          build,
			query:          `{ __schema { types { __typename name } } }`,
			aligned:        true,
			unorderedLists: true,
		},
		{
			name:           "fields带__typename且不传参",
			build:          build,
			query:          `{ __type(name: "SMIntroItem") { fields { __typename name } } }`,
			aligned:        true,
			unorderedLists: true,
		},
		{
			name:           "fields带__typename且显式传参",
			build:          build,
			query:          `{ __type(name: "SMIntroItem") { fields(includeDeprecated: true) { __typename name } } }`,
			aligned:        true,
			unorderedLists: true,
		},
		{
			name:           "enumValues带__typename且显式传参",
			build:          build,
			query:          `{ __type(name: "SMIntroMode") { enumValues(includeDeprecated: true) { __typename name } } }`,
			aligned:        true,
			unorderedLists: true,
		},
		{
			name:           "inputFields带__typename",
			build:          build,
			query:          `{ __type(name: "SMIntroInput") { inputFields { __typename name } } }`,
			aligned:        true,
			unorderedLists: true,
		},
		{
			// 内省中间结果由 GenerateTypeMetaResult 按 responseName 写入 map，
			// 因此任何按 info.FieldName / schema fieldName 取属性的物化实现都会在此取空。
			name:           "types使用alias",
			build:          build,
			query:          `{ __schema { myTypes: types { __typename name } } }`,
			aligned:        true,
			unorderedLists: true,
		},
		{
			name:           "types使用alias但不带__typename",
			build:          build,
			query:          `{ __schema { myTypes: types { name } } }`,
			aligned:        true,
			unorderedLists: true,
		},
		{
			// __Field.args 是 [__InputValue!]!，因此这里是「内省 list 中间层」的第二层，
			// 与 fields 一起构成 fields → args 的多层无 resolver 链路。
			name:           "多层内省list中间层带__typename",
			build:          build,
			query:          `{ __type(name: "Query") { fields { args { __typename name } } } }`,
			aligned:        true,
			unorderedLists: true,
		},
		{
			// 对照：中间层是单个 Object（__Field.type 返回 __Type）而非 list，
			// 且类型静态，因此不会注入父 FieldResponse 依赖，不触发 P0-1。
			// 该用例把「是否触发」与父字段的形状绑定，证明失败条件不是 __typename 本身。
			name:           "单值内省中间层带__typename不触发",
			build:          build,
			query:          `{ __type(name: "SMIntroItem") { fields { type { __typename } } } }`,
			aligned:        true,
			unorderedLists: true,
		},
	})
}

// ---------------------------------------------------------------------------
// 3. 父子关联 key 专项
//
// 无 resolver 中间层一旦被物化成 Step，若其父字段是 list 且元素类型声明了 ID 字段，
// checkAndCompileParentKeyFieldNames 会推断出非空 parentKeyFieldName，
// 使 bindIterationResponse 走 composite key 分支，从而要求父元素必须携带该业务 key。
// 属性提取本身按 occurrence 对应父元素，不需要业务 key——该用例固化这一边界。
// ---------------------------------------------------------------------------

func TestSGraphShapeMatrix_ParentKeyOnResolverlessMiddleLayer(t *testing.T) {
	build := func(withParentKeyValue bool) func(t *testing.T) Schema {
		return func(t *testing.T) Schema {
			t.Helper()
			suffix := "NoKey"
			if withParentKeyValue {
				suffix = "WithKey"
			}
			leaf := NewObject(ObjectConfig{Name: "SMPK" + suffix + "Leaf", Fields: Fields{
				"v": &Field{Type: String, Resolve: func(ResolveParams) (any, error) { return "V", nil }},
			}})
			// row 声明 id: ID，使 checkAndCompileParentKeyFieldNames 推断出业务 key。
			row := NewObject(ObjectConfig{Name: "SMPK" + suffix + "Row", Fields: Fields{
				"id":     &Field{Type: NewNonNull(ID)},
				"holder": &Field{Type: NewList(leaf)}, // 无 resolver 中间层
			}})
			rowValue := map[string]any{"holder": []any{map[string]any{}}}
			if withParentKeyValue {
				rowValue["id"] = "r1"
			}
			schema, schemaErr := NewSchema(SchemaConfig{
				Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
					"rows": &Field{Type: NewList(row), Resolve: func(ResolveParams) (any, error) {
						return []any{rowValue}, nil
					}},
				}}),
				Types: []Type{row, leaf},
			})
			if schemaErr != nil {
				t.Fatalf("build schema: %v", schemaErr)
			}
			return schema
		}
	}
	runShapeCases(t, []shapeCase{
		{
			name:    "父元素携带业务key",
			build:   build(true),
			query:   `{ rows { holder { v } } }`,
			aligned: true,
		},
		{
			name:    "父元素缺少业务key",
			build:   build(false),
			query:   `{ rows { holder { v } } }`,
			aligned: true,
		},
	})
}

// ---------------------------------------------------------------------------
// 4. 定向证伪文档中的绝对化断言
// ---------------------------------------------------------------------------

// 无resolver字段默认不产生Step；下游需要运行时结果时，编译器必须按需生成内部物化Step。
// 本用例同时覆盖默认组装路径和内部物化路径，防止再次产生悬空依赖。
func TestSGraphShapeMatrix_ResolverlessFieldNeverProducesStepClaim(t *testing.T) {
	buildSchema := func(t *testing.T) Schema {
		t.Helper()
		return shapeBuildListParent("SMClaim", false, "resolver")(t)
	}

	schema := buildSchema(t)
	shapeRegisterEngine(t, &schema)

	// 只选无 resolver 的子标量：依赖不产生，请求成功。
	plain := shapeRunSGraph(t, schema, `{ wrap { holder { id } } }`)
	if plain.hasErrors {
		t.Errorf("无 resolver 子标量不应产生错误，实际: %s", plain.raw)
	}

	// 追加带resolver的子字段后，编译器必须按需物化无resolver中间层。
	withResolverChild := shapeRunSGraph(t, schema, `{ wrap { holder { id computed } } }`)
	if withResolverChild.hasErrors {
		t.Errorf("按需物化无resolver中间层后不应报错，实际: %s", withResolverChild.raw)
	}

	// 同一个 __typename 在有/无 resolver 的父字段下结果相反，说明失败与父的 resolver 配置绑定，
	// 而不是与 __typename 本身的语义绑定。
	withParentResolver := shapeBuildListParent("SMClaimRes", true, "typename")(t)
	shapeRegisterEngine(t, &withParentResolver)
	okOutcome := shapeRunSGraph(t, withParentResolver, `{ wrap { holder { __typename } } }`)
	if okOutcome.hasErrors {
		t.Errorf("父字段有 resolver 时 __typename 应正常，实际: %s", okOutcome.raw)
	}
}

// CONFORMANCE_MATRIX.md 与 SGRAPH_USER_MANUAL.md 把「-race PASS / 无并发数据竞争」
// 列为 sgraph 相对原生的优势。该断言在 bulk 父字段下有多个带 resolver 兄弟子字段时不成立
// （iterationResponseData 曾在锁外读 iterationState、锁内写）。
// 本用例构造该拓扑，需配合 -race 运行才有意义。
func TestSGraphShapeMatrix_BulkSiblingIterationStepsRaceClaim(t *testing.T) {
	user := NewObject(ObjectConfig{Name: "SMRaceUser", Fields: Fields{
		"id":      &Field{Type: NewNonNull(Int)},
		"orderId": &Field{Type: NewNonNull(ID)},
		// 两个以上带 resolver 的兄弟子字段 → 同一 batch 内多个 IterationCallStep 并发
		"name": &Field{Type: String, Resolve: func(ResolveParams) (any, error) { return "N", nil }},
		"nick": &Field{Type: String, Resolve: func(ResolveParams) (any, error) { return "K", nil }},
		"tag":  &Field{Type: String, Resolve: func(ResolveParams) (any, error) { return "T", nil }},
	}})
	order := NewObject(ObjectConfig{Name: "SMRaceOrder", Fields: Fields{
		"id": &Field{Type: NewNonNull(ID)},
		"users": &Field{Type: NewList(user), Resolve: func(ResolveParams) (any, error) {
			return []map[string]any{}, nil
		}},
	}})
	usersDefinition := order.Fields()["users"]
	usersDefinition.BulkResolve = func(ResolveParams) (any, error) {
		return []map[string]any{
			{"id": 10, "orderId": "o1"},
			{"id": 20, "orderId": "o2"},
			{"id": 21, "orderId": "o3"},
		}, nil
	}
	usersDefinition.BulkResultMappedFieldName = "orderId"

	schema, schemaErr := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
			"orders": &Field{Type: NewList(order), Resolve: func(ResolveParams) (any, error) {
				return []map[string]any{{"id": "o1"}, {"id": "o2"}, {"id": "o3"}}, nil
			}},
		}}),
		Types: []Type{order, user},
	})
	if schemaErr != nil {
		t.Fatalf("build schema: %v", schemaErr)
	}
	shapeRegisterEngine(t, &schema)

	query := `{ orders { id users { id name nick tag } } }`
	baseline := shapeRunSGraph(t, schema, query)
	for iteration := 0; iteration < 50; iteration++ {
		current := shapeRunSGraph(t, schema, query)
		if !reflect.DeepEqual(current.data, baseline.data) {
			t.Fatalf("第 %d 次执行结果与首次不一致\n  first: %s\n  got  : %s",
				iteration, baseline.raw, current.raw)
		}
	}
	if baseline.hasErrors {
		t.Logf("bulk 拓扑返回错误（非本用例断言目标，仅记录）: %s", baseline.raw)
	}
}
