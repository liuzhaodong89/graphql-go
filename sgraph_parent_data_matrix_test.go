package graphql

import (
	"reflect"
	"sync/atomic"
	"testing"
)

// 本文件补齐 sgraph_field_shape_matrix_test.go 未枚举的实现维度。
// 那份矩阵枚举的是「resolver 有无 × 字段形状 × 子字段类型」，覆盖依赖图构建期的缺陷；
// 但另有几类缺陷的成因落在完全不同的维度上，需要单独枚举：
//
//	1. 父对象的数据形态     map[string]any / 命名 map / struct / FieldResolver
//	2. 父子绑定的成败       业务 key 命中 / key 缺失 / key 值重复 / key 跨类型冲突
//	3. 父值的状态           正常 / null / resolver 报错
//	4. 类型解析的来源       声明类型（Interface/Union）vs 运行时 Object
//	5. 同名字段的选择顺序   无条件在前 / 内联片段在前
//
// 复用 shapeCase / runShapeCases / shapeRegisterEngine 等 helper（同 package）。

// ---------------------------------------------------------------------------
// 维度 1+2：父对象数据形态 × 业务 key 绑定成败
//
// list 元素类型只要声明了 ID 字段，checkAndCompileParentKeyFieldNames 就会推断出
// parentKeyFieldName，使该父类型下所有带 resolver 的子字段改走 composite key 绑定
// （plan.go 的 bindIterationResponse）。该分支硬性要求父元素是 map[string]any 且携带该 key，
// 因此父对象形态与 key 取值共同决定成败——而 sgraph 在其他取值路径上是支持 struct 的
// （result_assembler 的 DefaultResolveFn 兜底），构成同一引擎内的不一致。
// ---------------------------------------------------------------------------

type parentDataRow struct {
	ID  string `json:"id" graphql:"id"`
	Seq string `json:"seq" graphql:"seq"`
}

type parentDataNamedMap map[string]any

func buildParentDataSchema(namePrefix string, rows func() any, declareIDField bool) func(t *testing.T) Schema {
	return func(t *testing.T) Schema {
		t.Helper()
		rowFields := Fields{
			// seq 带 resolver，因此会走 IterationCallStep 与父子绑定。
			"seq": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				return "SEQ", nil
			}},
		}
		if declareIDField {
			// 声明 ID 字段即启用业务 key 绑定，影响该父类型下的全部子字段。
			rowFields["id"] = &Field{Type: NewNonNull(ID)}
		} else {
			rowFields["id"] = &Field{Type: String}
		}
		row := NewObject(ObjectConfig{Name: namePrefix + "Row", Fields: rowFields})
		schema, schemaErr := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"rows": &Field{Type: NewList(row), Resolve: func(ResolveParams) (any, error) {
					return rows(), nil
				}},
			}}),
			Types: []Type{row},
		})
		if schemaErr != nil {
			t.Fatalf("build schema: %v", schemaErr)
		}
		return schema
	}
}

func TestSGraphParentDataMatrix_ShapeAndKeyBinding(t *testing.T) {
	runShapeCases(t, []shapeCase{
		{
			name: "map父元素/声明ID且携带key",
			build: buildParentDataSchema("PDMapOK", func() any {
				return []any{
					map[string]any{"id": "r1"},
					map[string]any{"id": "r2"},
				}
			}, true),
			query:   `{ rows { id seq } }`,
			aligned: true,
		},
		{
			name: "map父元素/声明ID但缺少key",
			build: buildParentDataSchema("PDMapMissing", func() any {
				// 父 resolver 未返回 id，业务 key 绑定失败。
				return []any{map[string]any{}, map[string]any{}}
			}, true),
			query:   `{ rows { seq } }`,
			aligned: false,
			note:    "P1-4：业务 key 缺失导致逐 occurrence 绑定失败，标量正确变 null，但 list 分支会变成 []",
		},
		{
			name: "map父元素/声明ID但key值重复",
			build: buildParentDataSchema("PDMapDup", func() any {
				return []any{
					map[string]any{"id": "same"},
					map[string]any{"id": "same"},
				}
			}, true),
			query:   `{ rows { id seq } }`,
			aligned: false,
			note:    "业务 key 在父元素间重复时报 duplicate parent binding key，原生按下标逐元素解析不受影响",
		},
		{
			name: "map父元素/声明ID但key跨类型冲突",
			build: buildParentDataSchema("PDMapCross", func() any {
				// ID 同时接受 int 与 string；generateCompositeKey 经 valueToString 后
				// int(1) 与 string("1") 折叠成同一个 key。
				return []any{
					map[string]any{"id": 1},
					map[string]any{"id": "1"},
				}
			}, true),
			query:   `{ rows { id seq } }`,
			aligned: false,
			note:    "P2-7：composite key 由 valueToString 生成，int(1) 与 \"1\" 折叠成同一 key",
		},
		{
			name: "struct父元素/声明ID",
			build: buildParentDataSchema("PDStruct", func() any {
				return []any{
					parentDataRow{ID: "r1"},
					parentDataRow{ID: "r2"},
				}
			}, true),
			query:   `{ rows { id seq } }`,
			aligned: false,
			note:    "P1-3：composite key 绑定硬要求 map[string]any，struct 父元素在其他取值路径可用却在此失败",
		},
		{
			name: "命名map父元素/声明ID",
			build: buildParentDataSchema("PDNamedMap", func() any {
				return []any{
					parentDataNamedMap{"id": "r1"},
					parentDataNamedMap{"id": "r2"},
				}
			}, true),
			query:   `{ rows { id seq } }`,
			aligned: false,
			note:    "P1-3 同源：命名 map 类型不是 map[string]any，composite key 断言同样失败",
		},
		{
			name: "map父元素/未声明ID字段",
			build: buildParentDataSchema("PDNoID", func() any {
				return []any{
					map[string]any{"id": "r1"},
					map[string]any{"id": "r2"},
				}
			}, false),
			query:   `{ rows { id seq } }`,
			aligned: true,
		},
		{
			name: "struct父元素/未声明ID字段",
			build: buildParentDataSchema("PDStructNoID", func() any {
				return []any{
					parentDataRow{ID: "r1"},
					parentDataRow{ID: "r2"},
				}
			}, false),
			query:   `{ rows { id seq } }`,
			aligned: true,
			note:    "对照：不启用业务 key 时走 occurrence 路径绑定，struct 父元素可用",
		},
	})
}

// ---------------------------------------------------------------------------
// 维度 2 续：绑定失败时 list 与标量的补全形态必须一致
//
// 规范 §6.4.3 / §7.1.2 要求产生字段错误的可空字段补 null。
// 同一个绑定失败原因下，标量子字段正确变 null，而 list 子字段返回空列表，
// 后者对调用方而言与「确实是空集合」不可区分，属于静默错误。
// ---------------------------------------------------------------------------

func TestSGraphParentDataMatrix_FailedBindingListVersusScalar(t *testing.T) {
	build := func(t *testing.T) Schema {
		t.Helper()
		item := NewObject(ObjectConfig{Name: "PDBindItem", Fields: Fields{
			"id":   &Field{Type: NewNonNull(ID)}, // 启用业务 key 绑定
			"name": &Field{Type: String},         // 无 resolver，走组装期属性提取
			"tags": &Field{Type: NewList(String), Resolve: func(ResolveParams) (any, error) {
				return []any{"t1"}, nil
			}},
			"label": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				return "L", nil
			}},
		}})
		schema, schemaErr := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"items": &Field{Type: NewList(item), Resolve: func(ResolveParams) (any, error) {
					// 故意不返回 id，使 tags 与 label 的绑定同时失败。
					return []any{
						map[string]any{"name": "a"},
						map[string]any{"name": "b"},
					}, nil
				}},
			}}),
			Types: []Type{item},
		})
		if schemaErr != nil {
			t.Fatalf("build schema: %v", schemaErr)
		}
		return schema
	}

	schema := build(t)
	shapeRegisterEngine(t, &schema)
	outcome := shapeRunSGraph(t, schema, `{ items { name tags label } }`)

	if !outcome.hasErrors {
		t.Fatalf("绑定失败时应产生字段错误，实际: %s", outcome.raw)
	}
	dataMap, ok := outcome.data.(map[string]any)
	if !ok {
		t.Fatalf("data 不是 map: %s", outcome.raw)
	}
	itemList, ok := dataMap["items"].([]any)
	if !ok || len(itemList) == 0 {
		t.Fatalf("items 不是非空列表: %s", outcome.raw)
	}
	first, ok := itemList[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] 不是 map: %s", outcome.raw)
	}

	// 标量分支：绑定失败 -> null（符合规范）
	if first["label"] != nil {
		t.Errorf("绑定失败的标量应为 null，实际 label=%v\n  %s", first["label"], outcome.raw)
	}

	// list 分支：当前返回 []，与标量分支不一致。修复后应同为 null。
	tags := first["tags"]
	if tags == nil {
		t.Logf("P1-4 已修复：绑定失败的 list 现在为 null，请把本用例的断言改为要求 null；实际: %s", outcome.raw)
		return
	}
	tagList, isList := tags.([]any)
	if !isList || len(tagList) != 0 {
		t.Errorf("预期 P1-4 现状为空列表，实际 tags=%v\n  %s", tags, outcome.raw)
		return
	}
	t.Logf("P1-4 现状固化：同一绑定失败下 label 为 null 而 tags 为 []（应同为 null）\n  %s", outcome.raw)
}

// ---------------------------------------------------------------------------
// 维度 3：父值状态 —— 正常 / null / resolver 报错
//
// 非 List 的具体 Object 父字段不会获得对父 FieldResponse 的依赖边
// （plan_compiler.go 的 needParentFieldResponseRaw 只在父是 List 或抽象类型时成立），
// 子字段因此进入 batch 0 无条件执行。父为 null 或父 resolver 报错时，
// 规范 §6.3 不会进入该子树，原生也不会调用子 resolver。
// ---------------------------------------------------------------------------

func TestSGraphParentDataMatrix_NullAndErroredParent(t *testing.T) {
	buildWithParent := func(namePrefix string, parentResult func() (any, error)) (Schema, *int64) {
		var childCalls int64
		child := NewObject(ObjectConfig{Name: namePrefix + "Child", Fields: Fields{
			// 子 resolver 报错，以便同时固化 errors[].path 指向 data 中不存在位置的现象：
			// 父为 null 时 data 里没有 obj.value 这个位置，规范 §7.1.2 要求 path 指向响应中的实际位置。
			"value": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				atomic.AddInt64(&childCalls, 1)
				return nil, errChildBoom{}
			}},
		}})
		schema, schemaErr := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"obj": &Field{Type: child, Resolve: func(ResolveParams) (any, error) {
					return parentResult()
				}},
			}}),
			Types: []Type{child},
		})
		if schemaErr != nil {
			t.Fatalf("build schema: %v", schemaErr)
		}
		return schema, &childCalls
	}

	for _, testCase := range []struct {
		name         string
		namePrefix   string
		parentResult func() (any, error)
	}{
		{
			name:         "父返回null",
			namePrefix:   "PDNullParent",
			parentResult: func() (any, error) { return nil, nil },
		},
		{
			name:         "父resolver报错",
			namePrefix:   "PDErrParent",
			parentResult: func() (any, error) { return nil, errParentBoom{} },
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			schema, childCalls := buildWithParent(testCase.namePrefix, testCase.parentResult)
			shapeRegisterEngine(t, &schema)

			atomic.StoreInt64(childCalls, 0)
			sgraphOutcome := shapeRunSGraph(t, schema, `{ obj { value } }`)
			sgraphCalls := atomic.LoadInt64(childCalls)

			// -race 下不调用原生 oracle，原因见 runShapeCases 中的说明。
			// 本用例的核心观测（sgraph 是否调用子 resolver）不依赖原生侧。
			if raceDetectorEnabled {
				if sgraphCalls == 0 {
					t.Logf("P1-6 已修复：父不可用时 sgraph 不再调用子 resolver\n  sgraph: %s", sgraphOutcome.raw)
				} else {
					t.Logf("P1-6 现状固化：sgraph 调用子 resolver %d 次\n  sgraph: %s", sgraphCalls, sgraphOutcome.raw)
				}
				return
			}

			atomic.StoreInt64(childCalls, 0)
			nativeOutcome := shapeRunNative(t, schema, `{ obj { value } }`)
			nativeCalls := atomic.LoadInt64(childCalls)

			if nativeCalls != 0 {
				t.Fatalf("原生链路不应调用子 resolver，实际 %d 次", nativeCalls)
			}
			if sgraphCalls == 0 {
				t.Logf("P1-6 已修复：父不可用时 sgraph 也不再调用子 resolver，请把断言改为要求 0 次\n  sgraph: %s",
					sgraphOutcome.raw)
				return
			}
			t.Logf("P1-6 现状固化：sgraph 调用子 resolver %d 次，原生 0 次\n  sgraph: %s\n  native: %s",
				sgraphCalls, sgraphOutcome.raw, nativeOutcome.raw)

			// 子 resolver 被执行且报错，因此 sgraph 会带上一条 path 指向 obj.value 的错误，
			// 而 data 中 obj 为 null、并不存在该位置（违反 §7.1.2）；原生侧无此错误。
			if !sgraphOutcome.hasErrors {
				t.Errorf("子 resolver 已被调用且报错，sgraph 侧应带错误，实际: %s", sgraphOutcome.raw)
			}
			if nativeOutcome.hasErrors && testCase.name == "父返回null" {
				t.Errorf("父返回 null 时原生侧不应有错误，实际: %s", nativeOutcome.raw)
			}
		})
	}
}

type errParentBoom struct{}

func (errParentBoom) Error() string { return "parent boom" }

type errChildBoom struct{}

func (errChildBoom) Error() string { return "child boom" }

// ---------------------------------------------------------------------------
// 维度 4+5：抽象类型的 resolver 来源，以及同名字段的选择顺序
//
// plan_compiler.go 的 getFieldDefinition 以「声明类型」（Interface/Union）取字段定义，
// 因此实现 Object 上配置的 Resolve 永不挂载；原生在 ResolveType 之后按运行时类型取定义。
// 另外同一 responseName 在无条件选择与内联片段中各出现一次时，会编译出两个 FieldPlan，
// 而结果写入是后写覆盖，导致结果依赖选择集书写顺序（违反 §6.3.2 CollectFields 的合并语义）。
// ---------------------------------------------------------------------------

func buildAbstractResolverSchema(namePrefix string) func(t *testing.T) Schema {
	return func(t *testing.T) Schema {
		t.Helper()
		iface := NewInterface(InterfaceConfig{
			Name: namePrefix + "Pet",
			// 接口上声明 name，但不配置 Resolve。
			Fields: Fields{"name": &Field{Type: String}},
		})
		dog := NewObject(ObjectConfig{
			Name:       namePrefix + "Dog",
			Interfaces: []*Interface{iface},
			Fields: Fields{
				// 实现 Object 上配置 Resolve，返回值与父 map 中的值不同以便区分来源。
				"name": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
					return "FROM-RESOLVER", nil
				}},
			},
		})
		iface.ResolveType = func(ResolveTypeParams) *Object { return dog }
		schema, schemaErr := NewSchema(SchemaConfig{
			Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
				"pet": &Field{Type: iface, Resolve: func(ResolveParams) (any, error) {
					return map[string]any{"name": "FROM-MAP"}, nil
				}},
			}}),
			Types: []Type{dog},
		})
		if schemaErr != nil {
			t.Fatalf("build schema: %v", schemaErr)
		}
		return schema
	}
}

func TestSGraphParentDataMatrix_AbstractTypeResolverSource(t *testing.T) {
	runShapeCases(t, []shapeCase{
		{
			name:    "无条件选择",
			build:   buildAbstractResolverSchema("PDAbsPlain"),
			query:   `{ pet { name } }`,
			aligned: false,
			note:    "P1-5：按声明类型（Interface）取字段定义，实现 Object 的 Resolve 未挂载",
		},
		{
			// 对照：内联片段把类型收窄到实现 Object，因此能正确挂载其 Resolve。
			// 正是这条路径与上一条（无条件选择走声明类型）的差异，
			// 导致同名字段的结果取决于书写顺序，见 TestSGraphParentDataMatrix_RepeatedResponseNameOrder。
			name:    "内联片段选择",
			build:   buildAbstractResolverSchema("PDAbsFragment"),
			query:   `{ pet { ... on PDAbsFragmentDog { name } } }`,
			aligned: true,
		},
	})
}

// TestSGraphParentDataMatrix_RepeatedResponseNameOrder 固化「同名字段结果依赖书写顺序」。
// 两个查询的选择集在 §6.3.2 CollectFields 下应合并为同一字段，结果必须相同。
func TestSGraphParentDataMatrix_RepeatedResponseNameOrder(t *testing.T) {
	unconditionalFirst := buildAbstractResolverSchema("PDOrderA")(t)
	shapeRegisterEngine(t, &unconditionalFirst)
	outcomeUnconditionalFirst := shapeRunSGraph(t, unconditionalFirst,
		`{ pet { name ... on PDOrderADog { name } } }`)

	fragmentFirst := buildAbstractResolverSchema("PDOrderB")(t)
	shapeRegisterEngine(t, &fragmentFirst)
	outcomeFragmentFirst := shapeRunSGraph(t, fragmentFirst,
		`{ pet { ... on PDOrderBDog { name } name } }`)

	extractName := func(outcome shapeOutcome) any {
		dataMap, ok := outcome.data.(map[string]any)
		if !ok {
			return nil
		}
		petMap, ok := dataMap["pet"].(map[string]any)
		if !ok {
			return nil
		}
		return petMap["name"]
	}

	nameUnconditionalFirst := extractName(outcomeUnconditionalFirst)
	nameFragmentFirst := extractName(outcomeFragmentFirst)

	if reflect.DeepEqual(nameUnconditionalFirst, nameFragmentFirst) {
		t.Logf("P1-5 顺序依赖已消除：两种书写顺序结果一致（%v），"+
			"请把本用例改为断言一致\n  A: %s\n  B: %s",
			nameUnconditionalFirst, outcomeUnconditionalFirst.raw, outcomeFragmentFirst.raw)
		return
	}
	t.Logf("P1-5 顺序依赖现状固化：无条件在前得到 %v，内联片段在前得到 %v"+
		"（§6.3.2 要求两者合并为同一字段，结果应相同）\n  A: %s\n  B: %s",
		nameUnconditionalFirst, nameFragmentFirst,
		outcomeUnconditionalFirst.raw, outcomeFragmentFirst.raw)
}

// ---------------------------------------------------------------------------
// Plan 缓存的可观测边界
//
// planCache 以 sha256(documentBody + operationName) 为键、无容量上限与淘汰
// （sgraph_engine.go 只有 Load / LoadOrStore）。仅靠 alias 变体即可无限增长。
// 内存占用无法在单元测试中稳定断言，这里只固化「alias 变体确实产生不同缓存键」
// 这一可观测前提，为后续引入淘汰策略留下回归锚点。
// ---------------------------------------------------------------------------

func TestSGraphParentDataMatrix_PlanCacheKeyPerAliasVariant(t *testing.T) {
	schema, schemaErr := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
			"greeting": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				return "hi", nil
			}},
		}}),
	})
	if schemaErr != nil {
		t.Fatalf("build schema: %v", schemaErr)
	}
	engine, engineErr := NewSGraphEngine(&schema, NewDirectiveRegistry(), NewParamRegistry())
	if engineErr != nil {
		t.Fatalf("new engine: %v", engineErr)
	}
	if registerErr := RegisterSGraphEngine(engine); registerErr != nil {
		t.Fatalf("register engine: %v", registerErr)
	}

	cachedPlans := 0
	engine.planCache.Range(func(any, any) bool {
		cachedPlans++
		return true
	})
	if cachedPlans != 0 {
		t.Fatalf("新建 engine 的 planCache 应为空，实际 %d 条", cachedPlans)
	}

	const variantCount = 40
	for index := 0; index < variantCount; index++ {
		// 每个 alias 变体是不同的 documentBody，因此产生不同缓存键。
		query := "{ a" + itoaForTest(index) + ": greeting }"
		result := Do(Params{Schema: schema, RequestString: query})
		if len(result.Errors) != 0 {
			t.Fatalf("变体 %d 执行失败: %v", index, result.Errors)
		}
	}

	cachedPlans = 0
	engine.planCache.Range(func(any, any) bool {
		cachedPlans++
		return true
	})
	if cachedPlans < variantCount {
		t.Fatalf("planCache 应至少缓存 %d 个变体，实际 %d 条", variantCount, cachedPlans)
	}
	t.Logf("P2-8 现状固化：%d 个 alias 变体产生 %d 条常驻缓存，planCache 无容量上限与淘汰；"+
		"引入淘汰策略后本用例应改为断言缓存受限", variantCount, cachedPlans)
}

func itoaForTest(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// ---------------------------------------------------------------------------
// bulk 一对多与业务 key 推断的语义冲突
//
// 文档分别记载了两条约束：
//   1. bulk resolver 需要 BulkResultMappedFieldName 指向结果中代表父映射的字段（外键）；
//   2. list 元素类型上的 ID 字段会启用业务 key 绑定，且该 key 的值必须逐父元素唯一。
//
// 两条单独看都成立，但当元素类型的唯一 ID 候选恰好就是那个映射外键时，
// 一对多关系下外键在元素间天然重复，checkAndCompileParentKeyFieldNames 仍会推断它为业务 key，
// 于是同一父元素下的多个子元素触发 duplicate parent binding key，
// 带 resolver 的子字段只有第一个元素拿到值。文档未把这两条约束放在一起讨论过。
// ---------------------------------------------------------------------------

func TestSGraphParentDataMatrix_BulkOneToManyForeignKeyAsInferredParentKey(t *testing.T) {
	// User 上只有 orderId 是 ID 类型（id 故意用 Int），因此业务 key 推断必然落到外键上。
	user := NewObject(ObjectConfig{Name: "PDBulkUser", Fields: Fields{
		"id":      &Field{Type: NewNonNull(Int)},
		"orderId": &Field{Type: NewNonNull(ID)},
		"name": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
			return "N", nil
		}},
	}})
	order := NewObject(ObjectConfig{Name: "PDBulkOrder", Fields: Fields{
		"id": &Field{Type: NewNonNull(ID)},
		"users": &Field{Type: NewList(user), Resolve: func(ResolveParams) (any, error) {
			return []map[string]any{}, nil
		}},
	}})
	usersDefinition := order.Fields()["users"]
	usersDefinition.BulkResolve = func(ResolveParams) (any, error) {
		// o2 下挂两个 user，构成一对多。
		return []map[string]any{
			{"id": 10, "orderId": "o1"},
			{"id": 20, "orderId": "o2"},
			{"id": 21, "orderId": "o2"},
		}, nil
	}
	usersDefinition.BulkResultMappedFieldName = "orderId"

	schema, schemaErr := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
			"orders": &Field{Type: NewList(order), Resolve: func(ResolveParams) (any, error) {
				return []map[string]any{{"id": "o1"}, {"id": "o2"}}, nil
			}},
		}}),
		Types: []Type{order, user},
	})
	if schemaErr != nil {
		t.Fatalf("build schema: %v", schemaErr)
	}
	shapeRegisterEngine(t, &schema)

	outcome := shapeRunSGraph(t, schema, `{ orders { id users { id name } } }`)
	if !outcome.hasErrors {
		t.Logf("一对多外键冲突已消除：bulk 子字段不再因外键重复而报 duplicate key，"+
			"请把本用例改为断言无错误\n  %s", outcome.raw)
		return
	}
	t.Logf("现状固化：元素类型的唯一 ID 候选是映射外键时，一对多必然触发 duplicate parent binding key，"+
		"同一父元素下只有首个子元素的带 resolver 字段拿到值\n  %s", outcome.raw)
}
