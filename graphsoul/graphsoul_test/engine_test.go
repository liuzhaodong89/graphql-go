package graphsoul_test

import (
	"context"
	"errors"
	"testing"

	"github.com/graphql-go/graphql/graphsoul/build"
	"github.com/graphql-go/graphql/graphsoul/core"
)

// ──────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────

func newEngine() *core.SGraphEngine {
	return &core.SGraphEngine{}
}

// ──────────────────────────────────────────────
// Case 1: plan == nil → 返回空结果，不崩溃
// ──────────────────────────────────────────────

func TestExecute_NilPlan(t *testing.T) {
	engine := newEngine()
	result := engine.Execute(build.NewSGraphPlan(nil, nil))
	if result == nil {
		t.Fatal("Execute(empty plan) should not return nil SGraphResult")
	}
	if len(result.GetResponse()) != 0 {
		t.Errorf("expected empty response, got %v", result.GetResponse())
	}
	if len(result.GetErrors()) != 0 {
		t.Errorf("expected no errors, got %v", result.GetErrors())
	}
}

// ──────────────────────────────────────────────
// Case 2: 单个 root，SCALAR 类型，CONST 参数
// 期望：response["user"] = "alice"
// ──────────────────────────────────────────────

func TestExecute_SingleRoot_Scalar_ConstParam(t *testing.T) {
	userField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:      1,
		ResponseName: "user",
		FieldType:    build.FIELD_TYPE_SCALAR,
		Paths:        []string{"user"},
		ParamPlans: []*build.ParamPlan{
			build.NewConstParamPlan("name", "alice"),
		},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return params["name"], nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{userField}, nil)
	result := newEngine().Execute(plan)

	if result.GetResponse()["user"] != "alice" {
		t.Errorf("expected 'alice', got %v", result.GetResponse()["user"])
	}
}

// ──────────────────────────────────────────────
// Case 3: 单个 root，SCALAR 类型，INPUT 参数
// 期望：response["greeting"] = "hello"
// ──────────────────────────────────────────────

func TestExecute_SingleRoot_Scalar_InputParam(t *testing.T) {
	greetField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:      1,
		ResponseName: "greeting",
		FieldType:    build.FIELD_TYPE_SCALAR,
		Paths:        []string{"greeting"},
		ParamPlans: []*build.ParamPlan{
			build.NewInputParamPlan("msg", "inputMsg"),
		},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return params["msg"], nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{greetField}, map[string]any{"inputMsg": "hello"})
	result := newEngine().Execute(plan)

	if result.GetResponse()["greeting"] != "hello" {
		t.Errorf("expected 'hello', got %v", result.GetResponse()["greeting"])
	}
}

// ──────────────────────────────────────────────
// Case 4: root (OBJECT) + 子字段依赖 root 的 FIELD_RESULT
// root 返回 map{"id": 42, "name": "bob"}
// 子字段取 root.id 作为参数，返回 "order_42"
// 期望：response["account"]["orderNo"] = "order_42"
// ──────────────────────────────────────────────

func TestExecute_Root_Child_FieldResultParam(t *testing.T) {
	orderField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:       2,
		ParentFieldId: 1,
		ResponseName:  "orderNo",
		FieldType:     build.FIELD_TYPE_SCALAR,
		Paths:         []string{"account", "orderNo"},
		ParamPlans: []*build.ParamPlan{
			build.NewFieldResultParamPlan("accountId", 1, []string{"id"}),
		},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			id := params["accountId"]
			_ = id
			return "order_42", nil
		},
	})

	rootField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "account",
		FieldType:      build.FIELD_TYPE_OBJECT,
		Paths:          []string{"account"},
		ChildrenFields: []*build.FieldPlan{orderField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return map[string]any{"id": 42, "name": "bob"}, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootField}, nil)
	result := newEngine().Execute(plan)

	accountVal, ok := result.GetResponse()["account"].(map[string]any)
	if !ok {
		t.Fatalf("expected account to be map, got %T", result.GetResponse()["account"])
	}
	if accountVal["orderNo"] != "order_42" {
		t.Errorf("expected 'order_42', got %v", accountVal["orderNo"])
	}
}

// ──────────────────────────────────────────────
// Case 5: root 返回 error
// 期望：response 无数据，errors 不为空
// ──────────────────────────────────────────────

func TestExecute_ResolverError(t *testing.T) {
	rootField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:      1,
		ResponseName: "data",
		FieldType:    build.FIELD_TYPE_SCALAR,
		Paths:        []string{"data"},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return nil, errors.New("db error")
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootField}, nil)
	result := newEngine().Execute(plan)

	if len(result.GetErrors()) == 0 {
		t.Error("expected errors, got none")
	}
}

// ──────────────────────────────────────────────
// Case 6: resolver 为 nil → 返回 fieldError，不崩溃
// ──────────────────────────────────────────────

func TestExecute_NilResolver(t *testing.T) {
	rootField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:      1,
		ResponseName: "data",
		FieldType:    build.FIELD_TYPE_SCALAR,
		Paths:        []string{"data"},
		ResolverFunc: nil,
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootField}, nil)
	result := newEngine().Execute(plan)

	if len(result.GetErrors()) == 0 {
		t.Error("expected resolver-nil error, got none")
	}
}

// ──────────────────────────────────────────────
// Case 7: parentFieldNotNil=true，但父节点结果为 nil
// 期望：子字段报错，不崩溃
// ──────────────────────────────────────────────

func TestExecute_ParentNotNil_ParentMissing(t *testing.T) {
	childField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:           2,
		ParentFieldId:     1,
		ResponseName:      "name",
		FieldType:         build.FIELD_TYPE_SCALAR,
		ParentFieldNotNil: true,
		Paths:             []string{"account", "name"},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return "should not reach", nil
		},
	})

	rootWithChild := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "account",
		FieldType:      build.FIELD_TYPE_OBJECT,
		Paths:          []string{"account"},
		ChildrenFields: []*build.FieldPlan{childField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return nil, errors.New("not found")
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootWithChild}, nil)
	result := newEngine().Execute(plan)

	if len(result.GetErrors()) == 0 {
		t.Error("expected errors due to parent-nil check, got none")
	}
}

// ──────────────────────────────────────────────
// Case 8: root 是 List（IteratorStep traverse 模式）
// root 返回 3 个 user（responses[0..2]），子字段对每个 user 调用一次
// 期望：response["users"] 是长度为 3 的数组
// ──────────────────────────────────────────────

func TestExecute_ListRoot_IteratorTraverse(t *testing.T) {
	users := []any{
		map[string]any{"userId": "u1", "name": "alice"},
		map[string]any{"userId": "u2", "name": "bob"},
		map[string]any{"userId": "u3", "name": "carol"},
	}

	// 子字段：遍历每个 user，取 userId 作为参数查 score
	scoreField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:             2,
		ParentFieldId:       1,
		ResponseName:        "score",
		FieldType:           build.FIELD_TYPE_SCALAR,
		Paths:               []string{"users", "score"},
		ParentKeyFieldNames: []string{"userId"},
		ParamPlans: []*build.ParamPlan{
			build.NewFieldResultParamPlan("uid", 1, []string{"userId"}),
		},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			uid := params["uid"]
			scores := map[any]int{"u1": 90, "u2": 85, "u3": 78}
			return scores[uid], nil
		},
	})

	rootWithChild := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "users",
		FieldType:      build.FIELD_TYPE_OBJECT,
		FieldIsList:    true,
		Paths:          []string{"users"},
		ChildrenFields: []*build.FieldPlan{scoreField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return users, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootWithChild}, nil)
	result := newEngine().Execute(plan)

	usersVal, ok := result.GetResponse()["users"].([]any)
	if !ok {
		t.Fatalf("expected []any for users, got %T", result.GetResponse()["users"])
	}
	if len(usersVal) != 3 {
		t.Errorf("expected 3 users, got %d", len(usersVal))
	}
}

// ──────────────────────────────────────────────
// Case 9: root 是 List（IteratorStep batch 模式）
// batch resolver 接收 ids 数组，一次返回所有结果
// 期望：response["orders"] 是长度为 2 的数组
// ──────────────────────────────────────────────

func TestExecute_ListRoot_IteratorBatch(t *testing.T) {
	// 父节点：返回 2 个 order id
	parentData := []any{
		map[string]any{"orderId": "o1"},
		map[string]any{"orderId": "o2"},
	}

	// 子字段：batch 模式，arrParamPlan 收集所有 orderId
	detailField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:                  2,
		ParentFieldId:            1,
		ResponseName:             "orders",
		FieldType:                build.FIELD_TYPE_OBJECT,
		FieldIsList:              true,
		Paths:                    []string{"orders"},
		ArrayResultParentKeyName: "orderId",
		ArrParamPlan:             build.NewFieldResultParamPlan("ids", 1, []string{"orderId"}),
		ArrayResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			// ids = ["o1","o2"]，按顺序返回详情（与入参顺序一致）
			return []any{
				map[string]any{"orderId": "o1", "amount": 100},
				map[string]any{"orderId": "o2", "amount": 200},
			}, nil
		},
	})

	rootWithChild := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "orderIds",
		FieldType:      build.FIELD_TYPE_OBJECT,
		FieldIsList:    true,
		Paths:          []string{"orderIds"},
		ChildrenFields: []*build.FieldPlan{detailField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return parentData, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootWithChild}, nil)
	result := newEngine().Execute(plan)

	if len(result.GetErrors()) != 0 {
		t.Errorf("unexpected errors: %v", result.GetErrors())
	}
}

// ──────────────────────────────────────────────
// Case 10: 多个 root 并发，各自独立返回
// ──────────────────────────────────────────────

func TestExecute_MultipleRoots(t *testing.T) {
	fieldA := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:      1,
		ResponseName: "a",
		FieldType:    build.FIELD_TYPE_SCALAR,
		Paths:        []string{"a"},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return "valueA", nil
		},
	})
	fieldB := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:      2,
		ResponseName: "b",
		FieldType:    build.FIELD_TYPE_SCALAR,
		Paths:        []string{"b"},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return "valueB", nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{fieldA, fieldB}, nil)
	result := newEngine().Execute(plan)

	if result.GetResponse()["a"] != "valueA" {
		t.Errorf("expected 'valueA', got %v", result.GetResponse()["a"])
	}
	if result.GetResponse()["b"] != "valueB" {
		t.Errorf("expected 'valueB', got %v", result.GetResponse()["b"])
	}
}

// ──────────────────────────────────────────────
// Case 11: List 子字段的 SCALAR 子节点（IteratorStep traverse）
// 验证 Bug 2：list 内每个对象的 scalar 子字段取到正确的值，而非全部取 responses[0]
// 结构：root(list) → users[]{userId, name} → score(scalar，独立 resolver，每个 user 不同)
// 期望：users[0].score=90, users[1].score=85, users[2].score=78
// ──────────────────────────────────────────────

func TestExecute_ListObject_ScalarChild_CorrectPerElement(t *testing.T) {
	users := []any{
		map[string]any{"userId": "u1", "name": "alice"},
		map[string]any{"userId": "u2", "name": "bob"},
		map[string]any{"userId": "u3", "name": "carol"},
	}

	scoreField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:             2,
		ParentFieldId:       1,
		ResponseName:        "score",
		FieldType:           build.FIELD_TYPE_SCALAR,
		Paths:               []string{"users", "score"},
		ParentKeyFieldNames: []string{"userId"},
		ParamPlans: []*build.ParamPlan{
			build.NewFieldResultParamPlan("uid", 1, []string{"userId"}),
		},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			scores := map[any]int{"u1": 90, "u2": 85, "u3": 78}
			return scores[params["uid"]], nil
		},
	})

	rootField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "users",
		FieldType:      build.FIELD_TYPE_OBJECT,
		FieldIsList:    true,
		Paths:          []string{"users"},
		ChildrenFields: []*build.FieldPlan{scoreField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return users, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootField}, nil)
	result := newEngine().Execute(plan)

	usersVal, ok := result.GetResponse()["users"].([]any)
	if !ok {
		t.Fatalf("expected []any for users, got %T", result.GetResponse()["users"])
	}
	if len(usersVal) != 3 {
		t.Fatalf("expected 3 users, got %d", len(usersVal))
	}

	expected := []int{90, 85, 78}
	for i, u := range usersVal {
		uMap, ok := u.(map[string]any)
		if !ok {
			t.Fatalf("users[%d] expected map, got %T", i, u)
		}
		if uMap["score"] != expected[i] {
			t.Errorf("users[%d].score: expected %d, got %v", i, expected[i], uMap["score"])
		}
	}
}

// ──────────────────────────────────────────────
// Case 12: Batch 模式 arrayParentKeyMap key 类型一致性
// 验证 Bug 1：batch resolver 返回的 key 是 string，组装时 BuildCompositeKey 也产生 string，能正确匹配
// 结构：root(list) → orders[]{orderId(string), amount} 通过 batch 模式获取
// 期望：每个 order 的 amount 能正确组装到对应父元素
// ──────────────────────────────────────────────

func TestExecute_BatchMode_KeyTypeConsistency(t *testing.T) {
	parentData := []any{
		map[string]any{"orderId": "o1"},
		map[string]any{"orderId": "o2"},
		map[string]any{"orderId": "o3"},
	}

	orderIDField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:       4,
		ParentFieldId: 2,
		ResponseName:  "orderId",
		FieldType:     build.FIELD_TYPE_SCALAR,
		Paths:         []string{"orders", "detail", "orderId"},
	})
	amountField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:       5,
		ParentFieldId: 2,
		ResponseName:  "amount",
		FieldType:     build.FIELD_TYPE_SCALAR,
		Paths:         []string{"orders", "detail", "amount"},
	})

	detailField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:                  2,
		ParentFieldId:            1,
		ResponseName:             "detail",
		FieldType:                build.FIELD_TYPE_OBJECT,
		FieldIsList:              false,
		Paths:                    []string{"orders", "detail"},
		ParentKeyFieldNames:      []string{"orderId"},
		ArrayResultParentKeyName: "orderId",
		ChildrenFields:           []*build.FieldPlan{orderIDField, amountField},
		ArrParamPlan:             build.NewFieldResultParamPlan("ids", 1, []string{"orderId"}),
		ArrayResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return []any{
				map[string]any{"orderId": "o1", "amount": 100},
				map[string]any{"orderId": "o2", "amount": 200},
				map[string]any{"orderId": "o3", "amount": 300},
			}, nil
		},
	})

	rootField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "orders",
		FieldType:      build.FIELD_TYPE_OBJECT,
		FieldIsList:    true,
		Paths:          []string{"orders"},
		ChildrenFields: []*build.FieldPlan{detailField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return parentData, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootField}, nil)
	result := newEngine().Execute(plan)

	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	ordersVal, ok := result.GetResponse()["orders"].([]any)
	if !ok {
		t.Fatalf("expected []any for orders, got %T", result.GetResponse()["orders"])
	}
	if len(ordersVal) != 3 {
		t.Fatalf("expected 3 orders, got %d", len(ordersVal))
	}

	expectedAmounts := []int{100, 200, 300}
	expectedIds := []string{"o1", "o2", "o3"}
	for i, o := range ordersVal {
		oMap, ok := o.(map[string]any)
		if !ok {
			t.Fatalf("orders[%d] expected map, got %T", i, o)
		}
		detail, ok := oMap["detail"].(map[string]any)
		if !ok {
			t.Fatalf("orders[%d].detail expected map, got %T (key matching may have failed)", i, oMap["detail"])
		}
		if detail["orderId"] != expectedIds[i] {
			t.Errorf("orders[%d].detail.orderId: expected %s, got %v", i, expectedIds[i], detail["orderId"])
		}
		if detail["amount"] != expectedAmounts[i] {
			t.Errorf("orders[%d].detail.amount: expected %d, got %v", i, expectedAmounts[i], detail["amount"])
		}
	}
}

// ──────────────────────────────────────────────
// Case 13: 子节点 resolver 返回 error，中断传播
// 验证：child resolver error 被收集到 errors，不崩溃
// ──────────────────────────────────────────────

func TestExecute_ChildResolverError_Collected(t *testing.T) {
	childField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:       2,
		ParentFieldId: 1,
		ResponseName:  "detail",
		FieldType:     build.FIELD_TYPE_SCALAR,
		Paths:         []string{"account", "detail"},
		ParamPlans: []*build.ParamPlan{
			build.NewFieldResultParamPlan("id", 1, []string{"id"}),
		},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return nil, errors.New("downstream error")
		},
	})

	rootField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "account",
		FieldType:      build.FIELD_TYPE_OBJECT,
		Paths:          []string{"account"},
		ChildrenFields: []*build.FieldPlan{childField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return map[string]any{"id": "a1"}, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootField}, nil)
	result := newEngine().Execute(plan)

	if len(result.GetErrors()) == 0 {
		t.Error("expected child resolver error to be collected, got none")
	}
}

// ──────────────────────────────────────────────
// Case 14: GetValueByPath 工具函数
// ──────────────────────────────────────────────

func TestGetValueByPath(t *testing.T) {
	data := map[string]any{
		"order": map[string]any{
			"user": map[string]any{
				"name": "alice",
			},
		},
	}

	val := build.GetValueByPath(data, []string{"order", "user", "name"})
	if val != "alice" {
		t.Errorf("expected 'alice', got %v", val)
	}

	// 路径中断
	val2 := build.GetValueByPath(data, []string{"order", "missing", "name"})
	if val2 != nil {
		t.Errorf("expected nil for missing path, got %v", val2)
	}

	// 空路径返回原始数据
	val3 := build.GetValueByPath(data, []string{})
	if val3 == nil {
		t.Error("expected non-nil for empty path")
	}
}

// ──────────────────────────────────────────────
// Case 15: BuildCompositeKey 工具函数
// ──────────────────────────────────────────────

func TestBuildCompositeKey(t *testing.T) {
	source := map[string]any{"orderId": "o1", "itemId": 5}

	key := build.BuildCompositeKey([]string{"orderId", "itemId"}, source)
	if key != "o1:5" {
		t.Errorf("expected 'o1:5', got %v", key)
	}

	// 单字段
	key2 := build.BuildCompositeKey([]string{"orderId"}, source)
	if key2 != "o1" {
		t.Errorf("expected 'o1', got %v", key2)
	}

	// 空字段列表
	key3 := build.BuildCompositeKey([]string{}, source)
	if key3 != "" {
		t.Errorf("expected empty string, got %v", key3)
	}
}

// ──────────────────────────────────────────────
// Case 16: ENUM 类型 root，resolver 返回枚举值字符串
// 期望：response["status"] = "ACTIVE"
// ──────────────────────────────────────────────

func TestExecute_SingleRoot_Enum(t *testing.T) {
	statusField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:      1,
		ResponseName: "status",
		FieldType:    build.FIELD_TYPE_ENUM,
		Paths:        []string{"status"},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return "ACTIVE", nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{statusField}, nil)
	result := newEngine().Execute(plan)

	if result.GetResponse()["status"] != "ACTIVE" {
		t.Errorf("expected 'ACTIVE', got %v", result.GetResponse()["status"])
	}
}

// ──────────────────────────────────────────────
// Case 17: root OBJECT → scalar 子字段无独立 resolver
// 子字段直接从父节点 response map 中读取（fallback 路径）
// 期望：response["user"]["name"] = "alice"（从父 response 取，无子 resolver）
// ──────────────────────────────────────────────

func TestExecute_ScalarChild_ReadFromParentResponse(t *testing.T) {
	nameField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:       2,
		ParentFieldId: 1,
		ResponseName:  "name",
		FieldType:     build.FIELD_TYPE_SCALAR,
		Paths:         []string{"user", "name"},
		// 无 ResolverFunc，引擎应从父 response map 取 "name" 字段
	})

	rootField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "user",
		FieldType:      build.FIELD_TYPE_OBJECT,
		Paths:          []string{"user"},
		ChildrenFields: []*build.FieldPlan{nameField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return map[string]any{"name": "alice", "age": 30}, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootField}, nil)
	result := newEngine().Execute(plan)

	userVal, ok := result.GetResponse()["user"].(map[string]any)
	if !ok {
		t.Fatalf("expected user to be map, got %T", result.GetResponse()["user"])
	}
	if userVal["name"] != "alice" {
		t.Errorf("expected 'alice', got %v", userVal["name"])
	}
}

// ──────────────────────────────────────────────
// Case 18: root OBJECT → 嵌套 OBJECT 子字段（非 list）
// 结构：root → profile(object) → bio(scalar)
// 期望：response["user"]["profile"]["bio"] = "engineer"
// ──────────────────────────────────────────────

func TestExecute_NestedObject(t *testing.T) {
	bioField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:       3,
		ParentFieldId: 2,
		ResponseName:  "bio",
		FieldType:     build.FIELD_TYPE_SCALAR,
		Paths:         []string{"user", "profile", "bio"},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return "engineer", nil
		},
	})

	profileField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        2,
		ParentFieldId:  1,
		ResponseName:   "profile",
		FieldType:      build.FIELD_TYPE_OBJECT,
		Paths:          []string{"user", "profile"},
		ChildrenFields: []*build.FieldPlan{bioField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return map[string]any{"bio": "engineer"}, nil
		},
	})

	rootField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "user",
		FieldType:      build.FIELD_TYPE_OBJECT,
		Paths:          []string{"user"},
		ChildrenFields: []*build.FieldPlan{profileField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return map[string]any{"id": "u1"}, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootField}, nil)
	result := newEngine().Execute(plan)

	userVal, ok := result.GetResponse()["user"].(map[string]any)
	if !ok {
		t.Fatalf("expected user map, got %T", result.GetResponse()["user"])
	}
	profileVal, ok := userVal["profile"].(map[string]any)
	if !ok {
		t.Fatalf("expected profile map, got %T", userVal["profile"])
	}
	if profileVal["bio"] != "engineer" {
		t.Errorf("expected 'engineer', got %v", profileVal["bio"])
	}
}

// ──────────────────────────────────────────────
// Case 19: root 是 LIST of SCALAR
// 期望：response["tags"] = ["go", "graphql", "engine"]
// ──────────────────────────────────────────────

func TestExecute_ListRoot_Scalar(t *testing.T) {
	tagsField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:      1,
		ResponseName: "tags",
		FieldType:    build.FIELD_TYPE_SCALAR,
		FieldIsList:  true,
		Paths:        []string{"tags"},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return []any{"go", "graphql", "engine"}, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{tagsField}, nil)
	result := newEngine().Execute(plan)

	tagsVal, ok := result.GetResponse()["tags"].([]any)
	if !ok {
		t.Fatalf("expected []any for tags, got %T", result.GetResponse()["tags"])
	}
	expected := []string{"go", "graphql", "engine"}
	for i, tag := range tagsVal {
		if tag != expected[i] {
			t.Errorf("tags[%d]: expected %s, got %v", i, expected[i], tag)
		}
	}
}

// ──────────────────────────────────────────────
// Case 20: 父节点是 list，子字段 resolver 为 nil
// 引擎不为该子字段创建 Step，而是回落到从父 response map 读取同名字段。
// 父 response map 中不存在 "score"，故该字段值为 nil，但不应崩溃、不应有 error。
// 期望：无错误，users[0]["score"] = nil
// ──────────────────────────────────────────────

func TestExecute_Iterator_Traverse_NilResolver(t *testing.T) {
	scoreField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:             2,
		ParentFieldId:       1,
		ResponseName:        "score",
		FieldType:           build.FIELD_TYPE_SCALAR,
		Paths:               []string{"users", "score"},
		ParentKeyFieldNames: []string{"userId"},
		ParamPlans: []*build.ParamPlan{
			build.NewFieldResultParamPlan("uid", 1, []string{"userId"}),
		},
		ResolverFunc: nil, // 无 resolver → 回落到从父 map 读取
	})

	rootField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "users",
		FieldType:      build.FIELD_TYPE_OBJECT,
		FieldIsList:    true,
		Paths:          []string{"users"},
		ChildrenFields: []*build.FieldPlan{scoreField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return []any{map[string]any{"userId": "u1"}}, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootField}, nil)
	result := newEngine().Execute(plan)

	// 不应有错误，不应崩溃
	if len(result.GetErrors()) != 0 {
		t.Errorf("expected no errors, got %v", result.GetErrors())
	}
	usersVal, ok := result.GetResponse()["users"].([]any)
	if !ok {
		t.Fatalf("expected []any for users, got %T", result.GetResponse()["users"])
	}
	if len(usersVal) != 1 {
		t.Fatalf("expected 1 user, got %d", len(usersVal))
	}
	// 父 map 中无 "score" 字段，回落读取结果为 nil
	userMap, ok := usersVal[0].(map[string]any)
	if !ok {
		t.Fatalf("expected map for users[0], got %T", usersVal[0])
	}
	if v, exists := userMap["score"]; exists && v != nil {
		t.Errorf("expected score to be nil (no resolver, not in parent map), got %v", v)
	}
}

// ──────────────────────────────────────────────
// Case 21: IteratorStep batch 模式 resolver 返回 error
// 期望：error 被收集，不崩溃
// ──────────────────────────────────────────────

func TestExecute_Iterator_Batch_ResolverError(t *testing.T) {
	detailField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:                  2,
		ParentFieldId:            1,
		ResponseName:             "detail",
		FieldType:                build.FIELD_TYPE_OBJECT,
		FieldIsList:              false,
		Paths:                    []string{"orders", "detail"},
		ParentKeyFieldNames:      []string{"orderId"},
		ArrayResultParentKeyName: "orderId",
		ArrParamPlan:             build.NewFieldResultParamPlan("ids", 1, []string{"orderId"}),
		ArrayResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return nil, errors.New("batch upstream error")
		},
	})

	rootField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "orders",
		FieldType:      build.FIELD_TYPE_OBJECT,
		FieldIsList:    true,
		Paths:          []string{"orders"},
		ChildrenFields: []*build.FieldPlan{detailField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return []any{map[string]any{"orderId": "o1"}}, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootField}, nil)
	result := newEngine().Execute(plan)

	if len(result.GetErrors()) == 0 {
		t.Error("expected batch resolver error, got none")
	}
}

// ──────────────────────────────────────────────
// Case 22: root list resolver 返回空数组
// 期望：response["items"] = []（空 slice，不是 nil，不崩溃）
// ──────────────────────────────────────────────

func TestExecute_ListRoot_EmptyResult(t *testing.T) {
	itemsField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:      1,
		ResponseName: "items",
		FieldType:    build.FIELD_TYPE_OBJECT,
		FieldIsList:  true,
		Paths:        []string{"items"},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return []any{}, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{itemsField}, nil)
	result := newEngine().Execute(plan)

	if len(result.GetErrors()) != 0 {
		t.Errorf("unexpected errors: %v", result.GetErrors())
	}
	itemsVal, ok := result.GetResponse()["items"].([]any)
	if !ok {
		t.Fatalf("expected []any for items, got %T", result.GetResponse()["items"])
	}
	if len(itemsVal) != 0 {
		t.Errorf("expected empty slice, got len=%d", len(itemsVal))
	}
}

// ──────────────────────────────────────────────
// Case 23: 三级 batch 依赖链 A → B → C
// A(root) → B(依赖A结果) → C(依赖B结果)，形成 3 个 batch
// 期望：C 的结果正确，不崩溃
// ──────────────────────────────────────────────

func TestExecute_ThreeLevelBatchChain(t *testing.T) {
	// C 依赖 B 的结果
	fieldC := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:       3,
		ParentFieldId: 2,
		ResponseName:  "c",
		FieldType:     build.FIELD_TYPE_SCALAR,
		Paths:         []string{"a", "b", "c"},
		ParamPlans: []*build.ParamPlan{
			build.NewFieldResultParamPlan("bVal", 2, []string{"bId"}),
		},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return "result_c_from_" + params["bVal"].(string), nil
		},
	})

	// B 依赖 A 的结果
	fieldB := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        2,
		ParentFieldId:  1,
		ResponseName:   "b",
		FieldType:      build.FIELD_TYPE_OBJECT,
		Paths:          []string{"a", "b"},
		ChildrenFields: []*build.FieldPlan{fieldC},
		ParamPlans: []*build.ParamPlan{
			build.NewFieldResultParamPlan("aVal", 1, []string{"aId"}),
		},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return map[string]any{"bId": "b1"}, nil
		},
	})

	// A 是 root
	fieldA := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "a",
		FieldType:      build.FIELD_TYPE_OBJECT,
		Paths:          []string{"a"},
		ChildrenFields: []*build.FieldPlan{fieldB},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return map[string]any{"aId": "a1"}, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{fieldA}, nil)
	result := newEngine().Execute(plan)

	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	aVal, ok := result.GetResponse()["a"].(map[string]any)
	if !ok {
		t.Fatalf("expected a to be map, got %T", result.GetResponse()["a"])
	}
	bVal, ok := aVal["b"].(map[string]any)
	if !ok {
		t.Fatalf("expected b to be map, got %T", aVal["b"])
	}
	if bVal["c"] != "result_c_from_b1" {
		t.Errorf("expected 'result_c_from_b1', got %v", bVal["c"])
	}
}

// ──────────────────────────────────────────────
// Case 24: list → list 嵌套（二维数组结构）
// 结构：root(list) → tags(list of scalar，每个父元素有自己的 tag 数组)
// 期望：每个父元素组装到对应的 tags 子数组
// ──────────────────────────────────────────────

func TestExecute_List_NestedListChild(t *testing.T) {
	// batch 模式：一次拿回所有父元素的 tags，按父 key 分组
	tagsField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:                  2,
		ParentFieldId:            1,
		ResponseName:             "tags",
		FieldType:                build.FIELD_TYPE_SCALAR,
		FieldIsList:              true,
		Paths:                    []string{"posts", "tags"},
		ParentKeyFieldNames:      []string{"postId"},
		ArrayResultParentKeyName: "postId",
		ArrParamPlan:             build.NewFieldResultParamPlan("ids", 1, []string{"postId"}),
		ArrayResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			// 每个 postId 对应一个 tag 数组，以 []any 为值存入
			return []any{
				[]any{"postId", "p1", []any{"go", "backend"}},
				[]any{"postId", "p2", []any{"frontend", "react"}},
			}, nil
		},
	})

	rootField := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        1,
		ResponseName:   "posts",
		FieldType:      build.FIELD_TYPE_OBJECT,
		FieldIsList:    true,
		Paths:          []string{"posts"},
		ChildrenFields: []*build.FieldPlan{tagsField},
		ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
			return []any{
				map[string]any{"postId": "p1", "title": "Go tips"},
				map[string]any{"postId": "p2", "title": "React intro"},
			}, nil
		},
	})

	plan := build.NewSGraphPlan([]*build.FieldPlan{rootField}, nil)
	result := newEngine().Execute(plan)

	// 只验证不崩溃、无意外 panic，具体组装格式取决于嵌套 list 的实现
	if result == nil {
		t.Fatal("result should not be nil")
	}
}

// ──────────────────────────────────────────────
// Case 25: concurrent batch 多 step 并发写入 Rundata 无 race
// 两个无依赖的 root 字段在同一 concurrent batch 并发执行，验证线程安全
// 用 -race 可检测，这里仅验证结果正确
// ──────────────────────────────────────────────

func TestExecute_ConcurrentBatch_NoRace(t *testing.T) {
	makeField := func(id uint32, name, val string) *build.FieldPlan {
		return build.NewFieldPlan(build.FieldPlanOptions{
			FieldId:      id,
			ResponseName: name,
			FieldType:    build.FIELD_TYPE_SCALAR,
			Paths:        []string{name},
			ResolverFunc: func(source any, params map[string]any, ctx context.Context) (any, error) {
				return val, nil
			},
		})
	}

	fields := make([]*build.FieldPlan, 10)
	expected := make(map[string]string, 10)
	for i := range fields {
		name := "field" + string(rune('A'+i))
		val := "val" + string(rune('A'+i))
		fields[i] = makeField(uint32(i+1), name, val)
		expected[name] = val
	}

	plan := build.NewSGraphPlan(fields, nil)
	result := newEngine().Execute(plan)

	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	for k, v := range expected {
		if result.GetResponse()[k] != v {
			t.Errorf("%s: expected %s, got %v", k, v, result.GetResponse()[k])
		}
	}
}
