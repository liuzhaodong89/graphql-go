package graphql

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// 本文件固化 checkAndCompileParentKeyFieldNames 的父子关联 key 推断契约：
//   - 有 id: ID! 时取 id；
//   - 没有 id 但恰好一个 ID 字段时取该字段；
//   - 有多个 ID 字段时不推断，交由调用方处理，绝不依赖 map 遍历顺序任选一个。

func spkScope(t testing.TB, object *Object) *FieldTypeScope {
	t.Helper()
	scope, err := wrapStaticFieldTypeScope(object)
	if err != nil {
		t.Fatalf("wrap field type scope failed: %v", err)
	}
	return scope
}

// ---------------------------------------------------------------------------
// 推断规则本身
// ---------------------------------------------------------------------------

func TestSGraphParentKey_InferenceRules(t *testing.T) {
	compiler := &PlanCompiler{}

	t.Run("explicit_id_field_wins_over_other_id_fields", func(t *testing.T) {
		object := NewObject(ObjectConfig{Name: "SPKExplicitID", Fields: Fields{
			"id":      &Field{Type: NewNonNull(ID)},
			"alphaId": &Field{Type: NewNonNull(ID)},
			"betaId":  &Field{Type: NewNonNull(ID)},
		}})
		name, candidates := compiler.checkAndCompileParentKeyFieldNames(true, spkScope(t, object))
		if name != ParentKeyFieldNameAsID || candidates != nil {
			t.Fatalf("expected id with no candidates, got %q %v", name, candidates)
		}
	})

	t.Run("single_id_field_is_used", func(t *testing.T) {
		object := NewObject(ObjectConfig{Name: "SPKSingleID", Fields: Fields{
			"groupId": &Field{Type: NewNonNull(ID)},
			"name":    &Field{Type: String},
			"count":   &Field{Type: Int},
		}})
		name, candidates := compiler.checkAndCompileParentKeyFieldNames(true, spkScope(t, object))
		if name != "groupId" || candidates != nil {
			t.Fatalf("expected groupId with no candidates, got %q %v", name, candidates)
		}
	})

	t.Run("no_id_field_yields_empty_name_and_no_candidates", func(t *testing.T) {
		object := NewObject(ObjectConfig{Name: "SPKNoID", Fields: Fields{
			"name":  &Field{Type: String},
			"count": &Field{Type: Int},
		}})
		name, candidates := compiler.checkAndCompileParentKeyFieldNames(true, spkScope(t, object))
		if name != "" || len(candidates) != 0 {
			t.Fatalf("expected empty name and no candidates, got %q %v", name, candidates)
		}
	})

	t.Run("multiple_id_fields_are_reported_as_candidates", func(t *testing.T) {
		object := NewObject(ObjectConfig{Name: "SPKAmbiguous", Fields: Fields{
			"alphaId": &Field{Type: NewNonNull(ID)},
			"betaId":  &Field{Type: NewNonNull(ID)},
			"gammaId": &Field{Type: ID},
			"name":    &Field{Type: String},
		}})
		name, candidates := compiler.checkAndCompileParentKeyFieldNames(true, spkScope(t, object))
		if name != "" {
			t.Fatalf("ambiguous parent type must not infer a key field, got %q", name)
		}
		want := []string{"alphaId", "betaId", "gammaId"}
		if len(candidates) != len(want) {
			t.Fatalf("candidates mismatch: got %v want %v", candidates, want)
		}
		for index := range want {
			if candidates[index] != want[index] {
				t.Fatalf("candidates must be sorted: got %v want %v", candidates, want)
			}
		}
	})

	t.Run("interface_parent_follows_the_same_rules", func(t *testing.T) {
		single := NewInterface(InterfaceConfig{Name: "SPKIfaceSingle", Fields: Fields{
			"nodeId": &Field{Type: NewNonNull(ID)},
			"name":   &Field{Type: String},
		}})
		scope := &FieldTypeScope{declaredType: single}
		if name, candidates := compiler.checkAndCompileParentKeyFieldNames(true, scope); name != "nodeId" || candidates != nil {
			t.Fatalf("expected nodeId, got %q %v", name, candidates)
		}

		ambiguous := NewInterface(InterfaceConfig{Name: "SPKIfaceAmbiguous", Fields: Fields{
			"alphaId": &Field{Type: NewNonNull(ID)},
			"betaId":  &Field{Type: NewNonNull(ID)},
		}})
		scope = &FieldTypeScope{declaredType: ambiguous}
		if name, candidates := compiler.checkAndCompileParentKeyFieldNames(true, scope); name != "" || len(candidates) != 2 {
			t.Fatalf("expected ambiguity on interface parent, got %q %v", name, candidates)
		}
	})

	t.Run("non_list_parent_is_never_inferred", func(t *testing.T) {
		object := NewObject(ObjectConfig{Name: "SPKNonList", Fields: Fields{
			"id": &Field{Type: NewNonNull(ID)},
		}})
		if name, candidates := compiler.checkAndCompileParentKeyFieldNames(false, spkScope(t, object)); name != "" || candidates != nil {
			t.Fatalf("expected no inference for a non-list parent, got %q %v", name, candidates)
		}
	})
}

// 推断结果不得依赖 map 遍历顺序：同一 scope 反复推断必须得到同一结果。
func TestSGraphParentKey_InferenceIsDeterministic(t *testing.T) {
	compiler := &PlanCompiler{}
	cases := map[string]*Object{
		"ambiguous": NewObject(ObjectConfig{Name: "SPKDetAmbiguous", Fields: Fields{
			"alphaId": &Field{Type: NewNonNull(ID)},
			"betaId":  &Field{Type: NewNonNull(ID)},
			"gammaId": &Field{Type: NewNonNull(ID)},
			"deltaId": &Field{Type: NewNonNull(ID)},
			"name":    &Field{Type: String},
		}}),
		"single": NewObject(ObjectConfig{Name: "SPKDetSingle", Fields: Fields{
			"alphaId": &Field{Type: NewNonNull(ID)},
			"name":    &Field{Type: String},
		}}),
	}
	for label, object := range cases {
		scope := spkScope(t, object)
		firstName, firstCandidates := compiler.checkAndCompileParentKeyFieldNames(true, scope)
		for i := 0; i < 500; i++ {
			name, candidates := compiler.checkAndCompileParentKeyFieldNames(true, scope)
			if name != firstName {
				t.Fatalf("%s: inferred key field is not deterministic: %q vs %q", label, name, firstName)
			}
			if len(candidates) != len(firstCandidates) {
				t.Fatalf("%s: candidate list length is not deterministic", label)
			}
			for index := range candidates {
				if candidates[index] != firstCandidates[index] {
					t.Fatalf("%s: candidate list order is not deterministic: %v vs %v", label, candidates, firstCandidates)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// bulk：候选歧义在编译期报错，并列出候选字段
// ---------------------------------------------------------------------------

func spkAmbiguousBulkSchema(t testing.TB) Schema {
	t.Helper()
	userType := NewObject(ObjectConfig{Name: "SPKBulkUser", Fields: Fields{
		"uid":     &Field{Type: Int},
		"ownerId": &Field{Type: NewNonNull(String)},
	}})
	groupType := NewObject(ObjectConfig{Name: "SPKBulkGroup", Fields: Fields{
		"alphaId": &Field{Type: NewNonNull(ID)},
		"betaId":  &Field{Type: NewNonNull(ID)},
		"users": &Field{Type: NewList(userType), Resolve: func(ResolveParams) (any, error) {
			return []map[string]any{}, nil
		}},
	}})
	usersDefinition := groupType.Fields()["users"]
	usersDefinition.BulkResultMappedFieldName = "ownerId"
	usersDefinition.BulkResolve = func(ResolveParams) (any, error) {
		return []map[string]any{{"uid": 1, "ownerId": "A1"}}, nil
	}
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"groups": &Field{Type: NewList(groupType), Resolve: func(ResolveParams) (any, error) {
			return []map[string]any{{"alphaId": "A1", "betaId": "B1"}}, nil
		}},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{groupType, userType}})
	if err != nil {
		t.Fatalf("build schema failed: %v", err)
	}
	return schema
}

func TestSGraphParentKey_AmbiguousBulkFailsAtCompileTime(t *testing.T) {
	schema := spkAmbiguousBulkSchema(t)
	query := `query SPKBulk { groups { alphaId betaId users { uid ownerId } } }`
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	engine := newQueryFoldingEngine(t, &schema, nil, nil)
	operationName := "SPKBulk"
	result := engine.Execute(document, nil, &operationName, nil, context.Background()).toGraphQLResult()

	if len(result.Errors) != 1 {
		t.Fatalf("expected exactly one request error, got %d: %v", len(result.Errors), result.Errors)
	}
	message := result.Errors[0].Message
	for _, fragment := range []string{
		"parent key field name for bulk resolver users result binding is ambiguous",
		"SPKBulkGroup",
		"[alphaId betaId]",
		"declare an id: ID! field on the parent type",
	} {
		if !strings.Contains(message, fragment) {
			t.Fatalf("error message missing %q: %s", fragment, message)
		}
	}
	if result.Data != nil {
		t.Fatalf("request error must not carry data, got %#v", toPlainValue(result.Data))
	}
}

// 同一份 schema 反复编译，报错文案必须逐字一致（候选列表已排序）。
func TestSGraphParentKey_AmbiguousBulkErrorIsStable(t *testing.T) {
	query := `query SPKBulk { groups { alphaId betaId users { uid ownerId } } }`
	operationName := "SPKBulk"
	var firstMessage string
	for i := 0; i < 50; i++ {
		schema := spkAmbiguousBulkSchema(t)
		document := parseAndValidateQueryFoldingDocument(t, schema, query)
		engine := newQueryFoldingEngine(t, &schema, nil, nil)
		result := engine.Execute(document, nil, &operationName, nil, context.Background()).toGraphQLResult()
		if len(result.Errors) != 1 {
			t.Fatalf("iteration %d: expected one error, got %v", i, result.Errors)
		}
		if i == 0 {
			firstMessage = result.Errors[0].Message
			continue
		}
		if result.Errors[0].Message != firstMessage {
			t.Fatalf("iteration %d: error message is not stable\n got: %s\nwant: %s", i, result.Errors[0].Message, firstMessage)
		}
	}
}

// 补上 id: ID! 之后，同一形状的 schema 恢复正常。
func TestSGraphParentKey_ExplicitIDUnblocksBulk(t *testing.T) {
	userType := NewObject(ObjectConfig{Name: "SPKFixedUser", Fields: Fields{
		"uid":     &Field{Type: Int},
		"ownerId": &Field{Type: NewNonNull(String)},
	}})
	groupType := NewObject(ObjectConfig{Name: "SPKFixedGroup", Fields: Fields{
		"id":      &Field{Type: NewNonNull(ID)},
		"alphaId": &Field{Type: NewNonNull(ID)},
		"betaId":  &Field{Type: NewNonNull(ID)},
		"users": &Field{Type: NewList(userType), Resolve: func(ResolveParams) (any, error) {
			return []map[string]any{}, nil
		}},
	}})
	usersDefinition := groupType.Fields()["users"]
	usersDefinition.BulkResultMappedFieldName = "ownerId"
	usersDefinition.BulkResolve = func(ResolveParams) (any, error) {
		return []map[string]any{
			{"uid": 2, "ownerId": "g2"},
			{"uid": 1, "ownerId": "g1"},
		}, nil
	}
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"groups": &Field{Type: NewList(groupType), Resolve: func(ResolveParams) (any, error) {
			return []map[string]any{
				{"id": "g1", "alphaId": "A1", "betaId": "B1"},
				{"id": "g2", "alphaId": "A2", "betaId": "B2"},
			}, nil
		}},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{groupType, userType}})
	if err != nil {
		t.Fatal(err)
	}
	query := `query SPKFixed { groups { id users { uid ownerId } } }`
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	engine := newQueryFoldingEngine(t, &schema, nil, nil)
	result := executeQueryFoldingOperation(t, engine, document, "SPKFixed", nil, context.Background())
	requireQueryFoldingData(t, result, map[string]any{
		"groups": []any{
			map[string]any{"id": "g1", "users": []any{map[string]any{"uid": 1, "ownerId": "g1"}}},
			map[string]any{"id": "g2", "users": []any{map[string]any{"uid": 2, "ownerId": "g2"}}},
		},
	})
}

// ---------------------------------------------------------------------------
// 普通迭代：候选歧义时回退到 responsePath 绑定，不报错也不丢数据
// ---------------------------------------------------------------------------

const spkIterationQuery = `query SPKIter { items { alphaId label(seed: "unused") } }`

func spkAmbiguousIterationSchema(t testing.TB) Schema {
	t.Helper()
	itemType := NewObject(ObjectConfig{Name: "SPKIterItem", Fields: Fields{
		"alphaId": &Field{Type: NewNonNull(ID)},
		"betaId":  &Field{Type: NewNonNull(ID)},
		"label": &Field{
			Type: String,
			Args: FieldConfigArgument{"seed": &ArgumentConfig{Type: ID}},
			Resolve: func(p ResolveParams) (any, error) {
				return "label-" + valueToString(p.Args["seed"]), nil
			},
		},
	}})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"items": &Field{Type: NewList(itemType), Resolve: func(ResolveParams) (any, error) {
			// 故意只返回 alphaId：betaId 若被选为父 key 会触发 "parent key field is missing"。
			return []map[string]any{{"alphaId": "a1"}, {"alphaId": "a2"}}, nil
		}},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{itemType}})
	if err != nil {
		t.Fatalf("build schema failed: %v", err)
	}
	return schema
}

// label 的 seed 参数绑定到父元素的 alphaId，因此返回值能反映"这一次调用挂在哪个父元素上"，
// 可以直接验证回退到 responsePath 绑定之后父子映射依然逐元素正确。
func spkIterationParamRegistry(t testing.TB) *ParamRegistry {
	t.Helper()
	registry := NewParamRegistry()
	if err := registry.RegisterQuery(QueryParamConfig{
		DocumentBody:  spkIterationQuery,
		OperationName: "SPKIter",
		FieldParams: []FieldParamBinding{{
			Target: FieldParamTarget{
				ResponsePath:   []string{"items", "label"},
				ParentTypeName: "SPKIterItem",
				FieldName:      "label",
				ParamName:      "seed",
			},
			Source: ParamSource{Kind: ParamSourceFieldResponse, FieldResponse: &FieldResponseParamSource{
				ResponsePath:   []string{"items"},
				ParentTypeName: "Query",
				FieldName:      "items",
				ResultPath:     []string{"alphaId"},
			}},
		}},
	}); err != nil {
		t.Fatalf("register param bindings failed: %v", err)
	}
	return registry
}

func TestSGraphParentKey_AmbiguousIterationFallsBackToResponsePath(t *testing.T) {
	expected := map[string]any{
		"items": []any{
			map[string]any{"alphaId": "a1", "label": "label-a1"},
			map[string]any{"alphaId": "a2", "label": "label-a2"},
		},
	}
	// 每轮重建 schema 与 Engine，等价于反复重启进程：修复前这里会随机出现
	// "parent key field \"betaId\" is missing for field label"。
	for i := 0; i < 50; i++ {
		schema := spkAmbiguousIterationSchema(t)
		document := parseAndValidateQueryFoldingDocument(t, schema, spkIterationQuery)
		engine := newQueryFoldingEngine(t, &schema, nil, spkIterationParamRegistry(t))
		result := executeQueryFoldingOperation(t, engine, document, "SPKIter", nil, context.Background())
		requireQueryFoldingData(t, result, expected)
	}
}

// 父 key 值在多个父元素上重复时，回退到 responsePath 绑定不会误报 duplicate。
func TestSGraphParentKey_AmbiguousIterationToleratesDuplicateIDValues(t *testing.T) {
	itemType := NewObject(ObjectConfig{Name: "SPKDupItem", Fields: Fields{
		"alphaId": &Field{Type: NewNonNull(ID)},
		"betaId":  &Field{Type: NewNonNull(ID)},
		"label":   &Field{Type: String, Resolve: func(ResolveParams) (any, error) { return "L", nil }},
	}})
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"items": &Field{Type: NewList(itemType), Resolve: func(ResolveParams) (any, error) {
			return []map[string]any{
				{"alphaId": "a1", "betaId": "shared"},
				{"alphaId": "a2", "betaId": "shared"},
			}, nil
		}},
	}})
	query := `query SPKDup { items { alphaId label } }`
	expected := map[string]any{
		"items": []any{
			map[string]any{"alphaId": "a1", "label": "L"},
			map[string]any{"alphaId": "a2", "label": "L"},
		},
	}
	for i := 0; i < 50; i++ {
		schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{itemType}})
		if err != nil {
			t.Fatal(err)
		}
		document := parseAndValidateQueryFoldingDocument(t, schema, query)
		engine := newQueryFoldingEngine(t, &schema, nil, nil)
		result := executeQueryFoldingOperation(t, engine, document, "SPKDup", nil, context.Background())
		requireQueryFoldingData(t, result, expected)
	}
}

// ---------------------------------------------------------------------------
// 消费点 3：跨分支读 bulk 生产者结果的运行期分支不可达
// ---------------------------------------------------------------------------

// 同一个歧义父类型下同时存在 bulk 生产者和普通迭代消费者时，
// 编译期的 bulk 歧义检查先触发，运行期不会走到
// resolveIterationFieldResponseAttributeParam 的 "parent key field name is empty" 分支。
func TestSGraphParentKey_AmbiguousBulkBlocksCrossBranchConsumerAtCompileTime(t *testing.T) {
	userType := NewObject(ObjectConfig{Name: "SPKCrossUser", Fields: Fields{
		"uid":     &Field{Type: Int},
		"ownerId": &Field{Type: NewNonNull(String)},
	}})
	groupType := NewObject(ObjectConfig{Name: "SPKCrossGroup", Fields: Fields{
		"alphaId": &Field{Type: NewNonNull(ID)},
		"betaId":  &Field{Type: NewNonNull(ID)},
		"users": &Field{Type: NewList(userType), Resolve: func(ResolveParams) (any, error) {
			return []map[string]any{}, nil
		}},
		"summary": &Field{
			Type: String,
			Args: FieldConfigArgument{"seed": &ArgumentConfig{Type: String}},
			Resolve: func(p ResolveParams) (any, error) {
				return valueToString(p.Args["seed"]), nil
			},
		},
	}})
	usersDefinition := groupType.Fields()["users"]
	usersDefinition.BulkResultMappedFieldName = "ownerId"
	usersDefinition.BulkResolve = func(ResolveParams) (any, error) {
		return []map[string]any{{"uid": 1, "ownerId": "A1"}}, nil
	}
	queryType := NewObject(ObjectConfig{Name: "Query", Fields: Fields{
		"groups": &Field{Type: NewList(groupType), Resolve: func(ResolveParams) (any, error) {
			return []map[string]any{{"alphaId": "A1", "betaId": "B1"}}, nil
		}},
	}})
	schema, err := NewSchema(SchemaConfig{Query: queryType, Types: []Type{groupType, userType}})
	if err != nil {
		t.Fatal(err)
	}
	query := `query SPKCross { groups { alphaId users { uid ownerId } summary } }`
	registry := NewParamRegistry()
	if err := registry.RegisterQuery(QueryParamConfig{
		DocumentBody:  query,
		OperationName: "SPKCross",
		FieldParams: []FieldParamBinding{{
			Target: FieldParamTarget{
				ResponsePath:   []string{"groups", "summary"},
				ParentTypeName: "SPKCrossGroup",
				FieldName:      "summary",
				ParamName:      "seed",
			},
			Source: ParamSource{Kind: ParamSourceFieldResponse, FieldResponse: &FieldResponseParamSource{
				ResponsePath:   []string{"groups", "users"},
				ParentTypeName: "SPKCrossGroup",
				FieldName:      "users",
				ResultPath:     []string{"ownerId"},
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	document := parseAndValidateQueryFoldingDocument(t, schema, query)
	engine := newQueryFoldingEngine(t, &schema, nil, registry)
	operationName := "SPKCross"
	result := engine.Execute(document, nil, &operationName, nil, context.Background()).toGraphQLResult()

	if len(result.Errors) != 1 {
		t.Fatalf("expected exactly one request error, got %d: %v", len(result.Errors), result.Errors)
	}
	message := result.Errors[0].Message
	if !strings.Contains(message, "is ambiguous") {
		t.Fatalf("expected the compile-time ambiguity error to fire first, got: %s", message)
	}
	if strings.Contains(message, "parent key field name is empty for field") {
		t.Fatalf("runtime empty-key branch must stay unreachable, got: %s", message)
	}
}

// ---------------------------------------------------------------------------
// 并发：多 goroutine 同时编译歧义 schema 的行为一致（配合 -race 使用）
// ---------------------------------------------------------------------------

func TestSGraphParentKey_ConcurrentCompilationIsConsistent(t *testing.T) {
	schema := spkAmbiguousBulkSchema(t)
	bulkQuery := `query SPKBulk { groups { alphaId betaId users { uid ownerId } } }`
	bulkDocument := parseAndValidateQueryFoldingDocument(t, schema, bulkQuery)
	bulkEngine := newQueryFoldingEngine(t, &schema, nil, nil)

	iterationSchema := spkAmbiguousIterationSchema(t)
	iterationDocument := parseAndValidateQueryFoldingDocument(t, iterationSchema, spkIterationQuery)
	iterationEngine := newQueryFoldingEngine(t, &iterationSchema, nil, spkIterationParamRegistry(t))

	const goroutines = 32
	const rounds = 16
	var waitGroup sync.WaitGroup
	var mutex sync.Mutex
	bulkMessages := map[string]int{}
	iterationFailures := []string{}

	waitGroup.Add(goroutines)
	for worker := 0; worker < goroutines; worker++ {
		go func() {
			defer waitGroup.Done()
			bulkOperation := "SPKBulk"
			iterationOperation := "SPKIter"
			for round := 0; round < rounds; round++ {
				bulkResult := bulkEngine.Execute(bulkDocument, nil, &bulkOperation, nil, context.Background()).toGraphQLResult()
				iterationResult := iterationEngine.Execute(iterationDocument, nil, &iterationOperation, nil, context.Background()).toGraphQLResult()

				mutex.Lock()
				if len(bulkResult.Errors) == 1 {
					bulkMessages[bulkResult.Errors[0].Message]++
				} else {
					bulkMessages["<unexpected error count>"]++
				}
				if len(iterationResult.Errors) != 0 {
					iterationFailures = append(iterationFailures, iterationResult.Errors[0].Message)
				}
				mutex.Unlock()
			}
		}()
	}
	waitGroup.Wait()

	if len(bulkMessages) != 1 {
		t.Fatalf("concurrent bulk compilation produced inconsistent errors: %v", bulkMessages)
	}
	for message, count := range bulkMessages {
		if !strings.Contains(message, "is ambiguous") {
			t.Fatalf("unexpected bulk error %q", message)
		}
		if count != goroutines*rounds {
			t.Fatalf("expected %d bulk errors, got %d", goroutines*rounds, count)
		}
	}
	if len(iterationFailures) != 0 {
		t.Fatalf("concurrent iteration execution reported errors: %v", iterationFailures)
	}
}
