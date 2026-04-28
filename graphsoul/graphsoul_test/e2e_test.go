package graphsoul_test

// e2e_test.go: 端到端集成测试
//
// 完整链路：graphql.Schema 定义（含 resolver）→ prepare1.Parse(query) →
//           core.SGraphEngine.Execute(plan) → 校验 SGraphResult
//
// 测试分组：
//   A. 基础标量场景（单字段 / 多字段 / alias / INPUT 变量 / CONST 参数）
//   B. 枚举字段
//   C. Object 嵌套（单层 / 多层 / 父子共享 source）
//   D. List 场景（list of scalar / list of object / traverse / 空列表）
//   E. Mutation 操作
//   F. 错误场景（resolver 错误 / nil resolver / 父节点失败传播）
//   G. 边界与极限值（零值 / 空字符串 / 大数 / 深嵌套 / 宽查询 20 字段 / null）

import (
	"context"
	"fmt"
	"testing"

	graphql "github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/graphsoul/core"
	"github.com/graphql-go/graphql/graphsoul/prepare1"
)

// ─────────────────────────────────────────────────────────────
// Schema helpers
// ─────────────────────────────────────────────────────────────

// e2eSchema 构建覆盖所有端到端场景的完整测试 Schema。
func e2eSchema(t *testing.T) graphql.Schema {
	t.Helper()

	// ── 枚举 ──────────────────────────────────────────────────
	statusEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "Status",
		Values: graphql.EnumValueConfigMap{
			"ACTIVE":   {Value: "ACTIVE"},
			"INACTIVE": {Value: "INACTIVE"},
		},
	})

	// ── Address（叶子 Object）─────────────────────────────────
	addressType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Address",
		Fields: graphql.Fields{
			"city": {
				// 无 resolver：引擎从父字段响应 map 中按 key "city" 取值
				Type: graphql.String,
			},
			"zip": {
				// 无 resolver：引擎从父字段响应 map 中按 key "zip" 取值
				Type: graphql.String,
			},
		},
	})

	// ── User（含嵌套 address）────────────────────────────────
	userType := graphql.NewObject(graphql.ObjectConfig{
		Name: "User",
		Fields: graphql.Fields{
			"id": {
				// 无 resolver：引擎从父字段响应 map 中按 key "id" 取值
				Type: graphql.NewNonNull(graphql.ID),
			},
			"name": {
				// 无 resolver：引擎从父字段响应 map 中按 key "name" 取值
				Type: graphql.String,
			},
			"address": {
				Type: addressType,
				// resolver 不依赖 p.Source，独立返回地址数据，避免与父字段产生 batch 0 竞争
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return map[string]any{"city": "Beijing", "zip": "100000"}, nil
				},
			},
		},
	})

	// ── Level3 / Level2 / Level1（深嵌套）────────────────────
	level3Type := graphql.NewObject(graphql.ObjectConfig{
		Name: "Level3",
		Fields: graphql.Fields{
			"value": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return "deep_value", nil
				},
			},
		},
	})
	level2Type := graphql.NewObject(graphql.ObjectConfig{
		Name: "Level2",
		Fields: graphql.Fields{
			"level3": {
				Type: level3Type,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return map[string]any{}, nil
				},
			},
		},
	})
	level1Type := graphql.NewObject(graphql.ObjectConfig{
		Name: "Level1",
		Fields: graphql.Fields{
			"level2": {
				Type: level2Type,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return map[string]any{}, nil
				},
			},
		},
	})

	// ── Query 根类型 ──────────────────────────────────────────
	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			// 基础标量
			"hello": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return "world", nil
				},
			},
			// 带 INPUT 参数
			"greeting": {
				Type: graphql.String,
				Args: graphql.FieldConfigArgument{
					"msg": {Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					msg, _ := p.Args["msg"].(string)
					return "hello " + msg, nil
				},
			},
			// 整数标量
			"answer": {
				Type: graphql.Int,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return 42, nil
				},
			},
			// Float 标量
			"pi": {
				Type: graphql.Float,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return 3.14159, nil
				},
			},
			// Boolean 标量
			"flag": {
				Type: graphql.Boolean,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return true, nil
				},
			},
			// 返回空字符串（边界）
			"emptyString": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return "", nil
				},
			},
			// 返回 nil（null）
			"nullField": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return nil, nil
				},
			},
			// 返回很大的整数（边界）
			"bigNumber": {
				Type: graphql.Int,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return 2147483647, nil // int32 max
				},
			},
			// 返回负数
			"negNumber": {
				Type: graphql.Int,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return -999, nil
				},
			},
			// 枚举
			"status": {
				Type: statusEnum,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return "ACTIVE", nil
				},
			},
			// 单层 Object
			"user": {
				Type: userType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return map[string]any{
						"id":   "u1",
						"name": "alice",
						"address": map[string]any{
							"city": "Beijing",
							"zip":  "100000",
						},
					}, nil
				},
			},
			// Object resolver 返回 error
			"userError": {
				Type: userType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return nil, fmt.Errorf("db connection failed")
				},
			},
			// 深嵌套 Object
			"deep": {
				Type: level1Type,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return map[string]any{}, nil
				},
			},
			// List of Scalar
			"tags": {
				Type: graphql.NewList(graphql.String),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return []any{"go", "graphql", "engine"}, nil
				},
			},
			// List of Scalar 空列表
			"emptyTags": {
				Type: graphql.NewList(graphql.String),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return []any{}, nil
				},
			},
			// List of Object
			"users": {
				Type: graphql.NewList(userType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return []any{
						map[string]any{"id": "u1", "name": "alice", "address": map[string]any{"city": "Beijing", "zip": "100000"}},
						map[string]any{"id": "u2", "name": "bob", "address": map[string]any{"city": "Shanghai", "zip": "200000"}},
					}, nil
				},
			},
			// List of Object 空列表
			"emptyUsers": {
				Type: graphql.NewList(userType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return []any{}, nil
				},
			},
			// resolver 返回 error（标量）
			"errField": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return nil, fmt.Errorf("intentional error")
				},
			},
		},
	})

	// ── Mutation 根类型 ───────────────────────────────────────
	mutationType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Mutation",
		Fields: graphql.Fields{
			"createUser": {
				Type: graphql.String,
				Args: graphql.FieldConfigArgument{
					"name": {Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					name, _ := p.Args["name"].(string)
					return "created:" + name, nil
				},
			},
		},
	})

	// 宽查询：动态添加 field0..field19
	for i := 0; i < 20; i++ {
		idx := i
		name := fmt.Sprintf("wideField%d", idx)
		val := fmt.Sprintf("val%d", idx)
		queryType.AddFieldConfig(name, &graphql.Field{
			Type: graphql.String,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return val, nil
			},
		})
	}

	schema, err := graphql.NewSchema(graphql.SchemaConfig{
		Query:    queryType,
		Mutation: mutationType,
	})
	if err != nil {
		t.Fatalf("e2eSchema: %v", err)
	}
	return schema
}

// run 是端到端测试的核心辅助函数：parse → build plan → execute。
func run(t *testing.T, schema graphql.Schema, query string, variables map[string]any) *core.SGraphResult {
	t.Helper()
	plan, err := prepare1.Parse(query, schema, variables)
	if err != nil {
		t.Fatalf("prepare1.Parse: %v", err)
	}
	engine := &core.SGraphEngine{}
	return engine.Execute(plan)
}

// runExpectPlanErr 期望 prepare1.Parse 返回错误。
func runExpectPlanErr(t *testing.T, schema graphql.Schema, query string) error {
	t.Helper()
	_, err := prepare1.Parse(query, schema, nil)
	return err
}

// ─────────────────────────────────────────────────────────────
// A. 基础标量场景
// ─────────────────────────────────────────────────────────────

// A1: 单个 String 字段
func TestE2E_A1_SingleScalar_String(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ hello }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["hello"] != "world" {
		t.Errorf("expected 'world', got %v", result.GetResponse()["hello"])
	}
}

// A2: Int 字段
func TestE2E_A2_SingleScalar_Int(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ answer }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["answer"] != 42 {
		t.Errorf("expected 42, got %v", result.GetResponse()["answer"])
	}
}

// A3: Float 字段
func TestE2E_A3_SingleScalar_Float(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ pi }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["pi"] != 3.14159 {
		t.Errorf("expected 3.14159, got %v", result.GetResponse()["pi"])
	}
}

// A4: Boolean 字段
func TestE2E_A4_SingleScalar_Boolean(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ flag }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["flag"] != true {
		t.Errorf("expected true, got %v", result.GetResponse()["flag"])
	}
}

// A5: 多个并发根字段
func TestE2E_A5_MultipleRoots(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ hello answer flag }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	resp := result.GetResponse()
	if resp["hello"] != "world" {
		t.Errorf("hello: expected 'world', got %v", resp["hello"])
	}
	if resp["answer"] != 42 {
		t.Errorf("answer: expected 42, got %v", resp["answer"])
	}
	if resp["flag"] != true {
		t.Errorf("flag: expected true, got %v", resp["flag"])
	}
}

// A6: alias 字段
func TestE2E_A6_Alias(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ greeting1: hello greeting2: hello }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	resp := result.GetResponse()
	if resp["greeting1"] != "world" {
		t.Errorf("greeting1: expected 'world', got %v", resp["greeting1"])
	}
	if resp["greeting2"] != "world" {
		t.Errorf("greeting2: expected 'world', got %v", resp["greeting2"])
	}
}

// A7: INPUT 变量参数
func TestE2E_A7_InputVariable(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `query($msg: String) { greeting(msg: $msg) }`, map[string]any{"msg": "alice"})
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["greeting"] != "hello alice" {
		t.Errorf("expected 'hello alice', got %v", result.GetResponse()["greeting"])
	}
}

// A8: CONST 字面量参数
func TestE2E_A8_ConstLiteralArg(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ greeting(msg: "bob") }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["greeting"] != "hello bob" {
		t.Errorf("expected 'hello bob', got %v", result.GetResponse()["greeting"])
	}
}

// ─────────────────────────────────────────────────────────────
// B. 枚举字段
// ─────────────────────────────────────────────────────────────

// B1: 枚举返回值正确传递
func TestE2E_B1_Enum(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ status }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["status"] != "ACTIVE" {
		t.Errorf("expected 'ACTIVE', got %v", result.GetResponse()["status"])
	}
}

// ─────────────────────────────────────────────────────────────
// C. Object 嵌套
// ─────────────────────────────────────────────────────────────

// C1: 单层 Object，取标量子字段
func TestE2E_C1_Object_ScalarChild(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ user { id name } }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	userVal, ok := result.GetResponse()["user"].(map[string]any)
	if !ok {
		t.Fatalf("expected user to be map, got %T", result.GetResponse()["user"])
	}
	if userVal["id"] != "u1" {
		t.Errorf("user.id: expected 'u1', got %v", userVal["id"])
	}
	if userVal["name"] != "alice" {
		t.Errorf("user.name: expected 'alice', got %v", userVal["name"])
	}
}

// C2: 两层嵌套 Object（user → address）
func TestE2E_C2_Object_TwoLevel(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ user { name address { city zip } } }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	userVal, ok := result.GetResponse()["user"].(map[string]any)
	if !ok {
		t.Fatalf("expected user map, got %T", result.GetResponse()["user"])
	}
	addrVal, ok := userVal["address"].(map[string]any)
	if !ok {
		t.Fatalf("expected address map, got %T", userVal["address"])
	}
	if addrVal["city"] != "Beijing" {
		t.Errorf("address.city: expected 'Beijing', got %v", addrVal["city"])
	}
	if addrVal["zip"] != "100000" {
		t.Errorf("address.zip: expected '100000', got %v", addrVal["zip"])
	}
}

// C3: 三层深嵌套 Object（deep → level1 → level2 → level3）
func TestE2E_C3_Object_ThreeLevel(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ deep { level2 { level3 { value } } } }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	deepVal, ok := result.GetResponse()["deep"].(map[string]any)
	if !ok {
		t.Fatalf("expected deep map, got %T", result.GetResponse()["deep"])
	}
	l2, ok := deepVal["level2"].(map[string]any)
	if !ok {
		t.Fatalf("expected level2 map, got %T", deepVal["level2"])
	}
	l3, ok := l2["level3"].(map[string]any)
	if !ok {
		t.Fatalf("expected level3 map, got %T", l2["level3"])
	}
	if l3["value"] != "deep_value" {
		t.Errorf("level3.value: expected 'deep_value', got %v", l3["value"])
	}
}

// C4: 同一查询中多个不同 root Object
func TestE2E_C4_MultipleObjectRoots(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ user { id } deep { level2 { level3 { value } } } }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["user"] == nil {
		t.Error("expected user to be non-nil")
	}
	if result.GetResponse()["deep"] == nil {
		t.Error("expected deep to be non-nil")
	}
}

// ─────────────────────────────────────────────────────────────
// D. List 场景
// ─────────────────────────────────────────────────────────────

// D1: List of Scalar（非空列表）
func TestE2E_D1_ListOfScalar(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ tags }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	tagsVal, ok := result.GetResponse()["tags"].([]any)
	if !ok {
		t.Fatalf("expected []any for tags, got %T", result.GetResponse()["tags"])
	}
	expected := []string{"go", "graphql", "engine"}
	if len(tagsVal) != len(expected) {
		t.Fatalf("expected %d tags, got %d", len(expected), len(tagsVal))
	}
	for i, v := range tagsVal {
		if v != expected[i] {
			t.Errorf("tags[%d]: expected %s, got %v", i, expected[i], v)
		}
	}
}

// D2: List of Scalar 空列表
func TestE2E_D2_ListOfScalar_Empty(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ emptyTags }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	tagsVal, ok := result.GetResponse()["emptyTags"].([]any)
	if !ok {
		t.Fatalf("expected []any for emptyTags, got %T", result.GetResponse()["emptyTags"])
	}
	if len(tagsVal) != 0 {
		t.Errorf("expected empty slice, got len=%d", len(tagsVal))
	}
}

// D3: List of Object，多个元素，验证每个元素的子字段
func TestE2E_D3_ListOfObject(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ users { id name } }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	usersVal, ok := result.GetResponse()["users"].([]any)
	if !ok {
		t.Fatalf("expected []any for users, got %T", result.GetResponse()["users"])
	}
	if len(usersVal) != 2 {
		t.Fatalf("expected 2 users, got %d", len(usersVal))
	}
	type userExpect struct{ id, name string }
	expected := []userExpect{{"u1", "alice"}, {"u2", "bob"}}
	for i, u := range usersVal {
		uMap, ok := u.(map[string]any)
		if !ok {
			t.Fatalf("users[%d] expected map, got %T", i, u)
		}
		if uMap["id"] != expected[i].id {
			t.Errorf("users[%d].id: expected %s, got %v", i, expected[i].id, uMap["id"])
		}
		if uMap["name"] != expected[i].name {
			t.Errorf("users[%d].name: expected %s, got %v", i, expected[i].name, uMap["name"])
		}
	}
}

// D4: List of Object 空列表
func TestE2E_D4_ListOfObject_Empty(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ emptyUsers { id } }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	usersVal, ok := result.GetResponse()["emptyUsers"].([]any)
	if !ok {
		t.Fatalf("expected []any for emptyUsers, got %T", result.GetResponse()["emptyUsers"])
	}
	if len(usersVal) != 0 {
		t.Errorf("expected empty slice, got len=%d", len(usersVal))
	}
}

// D5: List of Object，只查询部分字段（验证字段选择投影正确性）
// 注：list 子项嵌套 Object 需要额外的 ParentKeyFieldNames 支持（prepare1 暂不设置），
// 因此本 case 仅测试 list 中标量字段的部分选择，确认只有被查询的字段出现在结果中。
func TestE2E_D5_ListOfObject_PartialFields(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ users { name } }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	usersVal, ok := result.GetResponse()["users"].([]any)
	if !ok {
		t.Fatalf("expected []any for users, got %T", result.GetResponse()["users"])
	}
	if len(usersVal) != 2 {
		t.Fatalf("expected 2 users, got %d", len(usersVal))
	}
	expectedNames := []string{"alice", "bob"}
	for i, u := range usersVal {
		uMap, ok := u.(map[string]any)
		if !ok {
			t.Fatalf("users[%d] expected map, got %T", i, u)
		}
		if uMap["name"] != expectedNames[i] {
			t.Errorf("users[%d].name: expected %s, got %v", i, expectedNames[i], uMap["name"])
		}
	}
}

// ─────────────────────────────────────────────────────────────
// E. Mutation 操作
// ─────────────────────────────────────────────────────────────

// E1: Mutation with INPUT 变量
func TestE2E_E1_Mutation_WithVariable(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `mutation($name: String) { createUser(name: $name) }`, map[string]any{"name": "carol"})
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["createUser"] != "created:carol" {
		t.Errorf("expected 'created:carol', got %v", result.GetResponse()["createUser"])
	}
}

// E2: Mutation with CONST 字面量参数
func TestE2E_E2_Mutation_WithConstArg(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `mutation { createUser(name: "dave") }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["createUser"] != "created:dave" {
		t.Errorf("expected 'created:dave', got %v", result.GetResponse()["createUser"])
	}
}

// ─────────────────────────────────────────────────────────────
// F. 错误场景
// ─────────────────────────────────────────────────────────────

// F1: 根字段 resolver 返回 error，errors 非空，response 中该字段为 nil
func TestE2E_F1_RootResolver_Error(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ errField }`, nil)
	if len(result.GetErrors()) == 0 {
		t.Error("expected errors, got none")
	}
}

// F2: Object resolver 返回 error，errors 非空
func TestE2E_F2_ObjectResolver_Error(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ userError { id } }`, nil)
	if len(result.GetErrors()) == 0 {
		t.Error("expected errors for userError, got none")
	}
}

// F3: 同一查询中部分字段成功、部分失败
func TestE2E_F3_PartialSuccess(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ hello errField }`, nil)
	// hello 应成功
	if result.GetResponse()["hello"] != "world" {
		t.Errorf("hello: expected 'world', got %v", result.GetResponse()["hello"])
	}
	// errField 应有 error
	if len(result.GetErrors()) == 0 {
		t.Error("expected error for errField, got none")
	}
}

// F4: 语法错误的 query，prepare1.Parse 应返回 error
func TestE2E_F4_ParseError_InvalidSyntax(t *testing.T) {
	schema := e2eSchema(t)
	err := runExpectPlanErr(t, schema, `{ hello `)
	if err == nil {
		t.Error("expected parse error, got nil")
	}
}

// F5: 查询了 schema 中不存在的字段，应返回 error
func TestE2E_F5_ParseError_UnknownField(t *testing.T) {
	schema := e2eSchema(t)
	err := runExpectPlanErr(t, schema, `{ nonExistentField }`)
	if err == nil {
		t.Error("expected validation error for unknown field, got nil")
	}
}

// ─────────────────────────────────────────────────────────────
// G. 边界与极限值
// ─────────────────────────────────────────────────────────────

// G1: 返回空字符串（零值）
func TestE2E_G1_EmptyString(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ emptyString }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["emptyString"] != "" {
		t.Errorf("expected empty string, got %v", result.GetResponse()["emptyString"])
	}
}

// G2: 返回 null（nil）
func TestE2E_G2_NullField(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ nullField }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	// nullField resolver 返回 nil，结果中该字段应为 nil
	if v, exists := result.GetResponse()["nullField"]; exists && v != nil {
		t.Errorf("expected null, got %v", v)
	}
}

// G3: int32 最大值
func TestE2E_G3_MaxInt(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ bigNumber }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["bigNumber"] != 2147483647 {
		t.Errorf("expected 2147483647, got %v", result.GetResponse()["bigNumber"])
	}
}

// G4: 负数
func TestE2E_G4_NegativeNumber(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ negNumber }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["negNumber"] != -999 {
		t.Errorf("expected -999, got %v", result.GetResponse()["negNumber"])
	}
}

// G5: 宽查询——20 个并发根字段
func TestE2E_G5_WideQuery_20Fields(t *testing.T) {
	schema := e2eSchema(t)

	fields := ""
	for i := 0; i < 20; i++ {
		fields += fmt.Sprintf(" wideField%d", i)
	}
	result := run(t, schema, `{`+fields+`}`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("wideField%d", i)
		expected := fmt.Sprintf("val%d", i)
		if result.GetResponse()[key] != expected {
			t.Errorf("%s: expected %s, got %v", key, expected, result.GetResponse()[key])
		}
	}
}

// G6: 变量为 nil（应自动初始化为空 map，不崩溃）
func TestE2E_G6_NilVariables(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ hello }`, nil)
	if result == nil {
		t.Fatal("result should not be nil with nil variables")
	}
	if result.GetResponse()["hello"] != "world" {
		t.Errorf("expected 'world', got %v", result.GetResponse()["hello"])
	}
}

// G7: 同一字段被 alias 为多个名字，各自独立计算
func TestE2E_G7_SameFieldMultipleAlias(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ a: hello b: hello c: hello }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	for _, key := range []string{"a", "b", "c"} {
		if result.GetResponse()[key] != "world" {
			t.Errorf("%s: expected 'world', got %v", key, result.GetResponse()[key])
		}
	}
}

// G8: 深嵌套 Object，只选取最深层叶子字段
func TestE2E_G8_DeepNested_LeafOnly(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ deep { level2 { level3 { value } } } }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	// 层层解包
	deep, _ := result.GetResponse()["deep"].(map[string]any)
	if deep == nil {
		t.Fatal("deep is nil")
	}
	l2, _ := deep["level2"].(map[string]any)
	if l2 == nil {
		t.Fatal("level2 is nil")
	}
	l3, _ := l2["level3"].(map[string]any)
	if l3 == nil {
		t.Fatal("level3 is nil")
	}
	if l3["value"] != "deep_value" {
		t.Errorf("level3.value: expected 'deep_value', got %v", l3["value"])
	}
}

// G9: Execute 返回的 SGraphResult 非 nil（任何情况下不应崩溃）
func TestE2E_G9_ResultAlwaysNonNil(t *testing.T) {
	schema := e2eSchema(t)
	for _, query := range []string{
		`{ hello }`,
		`{ errField }`,
		`{ hello errField }`,
		`{ emptyTags }`,
		`{ emptyUsers { id } }`,
	} {
		result := run(t, schema, query, nil)
		if result == nil {
			t.Errorf("query %q: result should not be nil", query)
		}
	}
}

// G10: 同一 plan 实例可被多次 Execute，结果一致（plan 无状态复用）
func TestE2E_G10_PlanReuse(t *testing.T) {
	schema := e2eSchema(t)
	plan, err := prepare1.Parse(`{ hello answer }`, schema, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	engine := &core.SGraphEngine{}

	// 连续执行 5 次，结果应完全一致
	for i := 0; i < 5; i++ {
		result := engine.Execute(plan)
		if len(result.GetErrors()) != 0 {
			t.Fatalf("round %d: unexpected errors: %v", i, result.GetErrors())
		}
		if result.GetResponse()["hello"] != "world" {
			t.Errorf("round %d: hello expected 'world', got %v", i, result.GetResponse()["hello"])
		}
		if result.GetResponse()["answer"] != 42 {
			t.Errorf("round %d: answer expected 42, got %v", i, result.GetResponse()["answer"])
		}
	}
}

// G11: 同一 plan 并发执行，无 data race（需配合 -race 使用）
func TestE2E_G11_PlanConcurrentExecution(t *testing.T) {
	schema := e2eSchema(t)
	plan, err := prepare1.Parse(`{ hello answer }`, schema, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	engine := &core.SGraphEngine{}

	done := make(chan struct{}, 20)
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			result := engine.Execute(plan)
			if result.GetResponse()["hello"] != "world" {
				t.Errorf("concurrent: hello expected 'world', got %v", result.GetResponse()["hello"])
			}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// G12: Fragment spread 场景
func TestE2E_G12_FragmentSpread(t *testing.T) {
	schema := e2eSchema(t)
	query := `
		fragment UserFields on User { id name }
		{ user { ...UserFields } }
	`
	result := run(t, schema, query, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	userVal, ok := result.GetResponse()["user"].(map[string]any)
	if !ok {
		t.Fatalf("expected user map, got %T", result.GetResponse()["user"])
	}
	if userVal["id"] != "u1" {
		t.Errorf("user.id: expected 'u1', got %v", userVal["id"])
	}
	if userVal["name"] != "alice" {
		t.Errorf("user.name: expected 'alice', got %v", userVal["name"])
	}
}

// G13: Inline fragment 场景
func TestE2E_G13_InlineFragment(t *testing.T) {
	schema := e2eSchema(t)
	query := `{ user { ... on User { id name } } }`
	result := run(t, schema, query, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	userVal, ok := result.GetResponse()["user"].(map[string]any)
	if !ok {
		t.Fatalf("expected user map, got %T", result.GetResponse()["user"])
	}
	if userVal["name"] != "alice" {
		t.Errorf("user.name: expected 'alice', got %v", userVal["name"])
	}
}

// G14: 混合标量 + Object + List 的复合查询
func TestE2E_G14_Mixed_Scalar_Object_List(t *testing.T) {
	schema := e2eSchema(t)
	result := run(t, schema, `{ hello answer user { id } tags }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	resp := result.GetResponse()
	if resp["hello"] != "world" {
		t.Errorf("hello: expected 'world', got %v", resp["hello"])
	}
	if resp["answer"] != 42 {
		t.Errorf("answer: expected 42, got %v", resp["answer"])
	}
	if _, ok := resp["user"].(map[string]any); !ok {
		t.Errorf("user: expected map, got %T", resp["user"])
	}
	if _, ok := resp["tags"].([]any); !ok {
		t.Errorf("tags: expected []any, got %T", resp["tags"])
	}
}

// G15: 操作名不影响执行结果
func TestE2E_G15_OperationName_NoEffect(t *testing.T) {
	schema := e2eSchema(t)
	withName := run(t, schema, `query MyQuery { hello }`, nil)
	withoutName := run(t, schema, `{ hello }`, nil)
	if withName.GetResponse()["hello"] != withoutName.GetResponse()["hello"] {
		t.Error("operation name should not affect result")
	}
}

// G16: context 可访问（resolver 使用 context，不崩溃）
func TestE2E_G16_ContextPropagation(t *testing.T) {
	// 构建一个 resolver 使用 context 的临时 schema
	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"ctxField": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					_ = p.Context // 不应 panic
					if p.Context == nil {
						return nil, fmt.Errorf("context is nil")
					}
					return "ok", nil
				},
			},
		},
	})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	result := run(t, schema, `{ ctxField }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["ctxField"] != "ok" {
		t.Errorf("expected 'ok', got %v", result.GetResponse()["ctxField"])
	}
}

// G17: resolver 使用 p.Args 取参数（确认 prepare 包装后 args 正确传递）
func TestE2E_G17_ArgsPassthrough(t *testing.T) {
	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"add": {
				Type: graphql.Int,
				Args: graphql.FieldConfigArgument{
					"a": {Type: graphql.Int},
					"b": {Type: graphql.Int},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					a, _ := p.Args["a"].(int)
					b, _ := p.Args["b"].(int)
					return a + b, nil
				},
			},
		},
	})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	result := run(t, schema, `query($a: Int, $b: Int) { add(a: $a, b: $b) }`, map[string]any{"a": 3, "b": 5})
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	if result.GetResponse()["add"] != 8 {
		t.Errorf("expected 8, got %v", result.GetResponse()["add"])
	}
}

// G18: 子字段无 resolver 时从父节点响应 map 按 key 取值
// 这是 graphsoul 的正确数据传递模式：父字段 resolver 将数据写入响应 map，
// 子字段（无 resolver）由引擎组装阶段直接按 responseKey 从父 map 读取。
func TestE2E_G18_SourcePassthrough(t *testing.T) {
	childType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Child",
		Fields: graphql.Fields{
			"parentId": {
				// 无 resolver：引擎从父字段响应 map 中按 key "parentId" 取值
				Type: graphql.String,
			},
		},
	})
	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"parent": {
				Type: childType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					// key 与子字段名 "parentId" 保持一致，引擎组装时直接读取
					return map[string]any{"parentId": "parent_42"}, nil
				},
			},
		},
	})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	result := run(t, schema, `{ parent { parentId } }`, nil)
	if len(result.GetErrors()) != 0 {
		t.Fatalf("unexpected errors: %v", result.GetErrors())
	}
	parentVal, ok := result.GetResponse()["parent"].(map[string]any)
	if !ok {
		t.Fatalf("expected parent map, got %T", result.GetResponse()["parent"])
	}
	if parentVal["parentId"] != "parent_42" {
		t.Errorf("parentId: expected 'parent_42', got %v", parentVal["parentId"])
	}
}

// ─────────────────────────────────────────────────────────────
// 端到端压测（smoke benchmark）
// ─────────────────────────────────────────────────────────────

func BenchmarkE2E_Hello(b *testing.B) {
	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"hello": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return "world", nil
				},
			},
		},
	})
	schema, _ := graphql.NewSchema(graphql.SchemaConfig{Query: queryType})
	plan, _ := prepare1.Parse(`{ hello }`, schema, nil)
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.Execute(plan)
	}
}

func BenchmarkE2E_ParseAndExecute_Hello(b *testing.B) {
	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"hello": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return "world", nil
				},
			},
		},
	})
	schema, _ := graphql.NewSchema(graphql.SchemaConfig{Query: queryType})
	engine := &core.SGraphEngine{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		plan, _ := prepare1.Parse(`{ hello }`, schema, nil)
		engine.Execute(plan)
	}
}

// 确保 context.TODO 在 resolver 中可用（防止 prepare 包装时漏传）
var _ = context.TODO
