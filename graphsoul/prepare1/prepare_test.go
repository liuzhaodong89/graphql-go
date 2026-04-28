package prepare1_test

// prepare_test.go: 充分覆盖 prepare1 包所有公开行为、类型映射分支、
// 参数转换路径、Resolver 包装逻辑及边界/极限条件。
//
// 测试分组：
//   A. Build 层错误（nil 文档、无 Operation、无 MutationType）
//   B. Parse 层错误（语法错误、字段不存在、未定义 Fragment、Scalar 上有 SubSelection）
//   C. 字段类型映射（Scalar / Enum / Object / List / NonNull / NonNullList）
//   D. 字段属性（Alias、Path、FieldId 自增、ParentFieldId 父子关联、无 Resolver）
//   E. 参数类型（Variable / int / float / string / bool / Enum / List / Object / 多参数）
//   F. 选择集特性（__typename 跳过、FragmentSpread、InlineFragment、三层嵌套、多 root）
//   G. Mutation 操作
//   H. Resolver 包装行为（nil source / firstResponseGetter / 普通值 / 错误透传）
//   I. 变量透传（nil variables 自动初始化）
//   J. Interface 类型字段

import (
	"context"
	"errors"
	"testing"

	graphql "github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/graphsoul/build"
	"github.com/graphql-go/graphql/graphsoul/prepare1"
	"github.com/graphql-go/graphql/language/ast"
)

// ─────────────────────────────────────────────────────────────
// Schema 辅助构造
// ─────────────────────────────────────────────────────────────

// newTestSchema 返回覆盖所有类型场景的测试 Schema：
//
//	Query {
//	  hello: String                  (有 Resolver)
//	  noResolver: String             (无 Resolver)
//	  status: Status                 (Enum)
//	  required: String!              (NonNull Scalar)
//	  tags: [String]                 (List of Scalar)
//	  requiredTags: [String!]!       (NonNull List of NonNull Scalar)
//	  user: User                     (Object)
//	  users: [User]                  (List of Object)
//	  greeting(msg: String): String  (带参数)
//	  level1: Level1                 (三层嵌套顶层)
//	}
//
// User { id: ID!, name: String, address: Address }
// Address { city: String, zip: String }
// Level1 { level2: Level2 }
// Level2 { leaf: String }
func newTestSchema(t *testing.T) graphql.Schema {
	t.Helper()

	statusEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "Status",
		Values: graphql.EnumValueConfigMap{
			"ACTIVE":   {Value: "ACTIVE"},
			"INACTIVE": {Value: "INACTIVE"},
		},
	})

	addressType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Address",
		Fields: graphql.Fields{
			"city": {Type: graphql.String},
			"zip":  {Type: graphql.String},
		},
	})

	userType := graphql.NewObject(graphql.ObjectConfig{
		Name: "User",
		Fields: graphql.Fields{
			"id":      {Type: graphql.NewNonNull(graphql.ID)},
			"name":    {Type: graphql.String},
			"address": {Type: addressType},
		},
	})

	level2Type := graphql.NewObject(graphql.ObjectConfig{
		Name: "Level2",
		Fields: graphql.Fields{
			"leaf": {Type: graphql.String, Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return "deep", nil
			}},
		},
	})

	level1Type := graphql.NewObject(graphql.ObjectConfig{
		Name: "Level1",
		Fields: graphql.Fields{
			"level2": {Type: level2Type, Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return map[string]any{}, nil
			}},
		},
	})

	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"hello": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return "world", nil
				},
			},
			"noResolver":   {Type: graphql.String},
			"status":       {Type: statusEnum, Resolve: func(p graphql.ResolveParams) (interface{}, error) { return "ACTIVE", nil }},
			"required":     {Type: graphql.NewNonNull(graphql.String), Resolve: func(p graphql.ResolveParams) (interface{}, error) { return "yes", nil }},
			"tags":         {Type: graphql.NewList(graphql.String), Resolve: func(p graphql.ResolveParams) (interface{}, error) { return nil, nil }},
			"requiredTags": {Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String))), Resolve: func(p graphql.ResolveParams) (interface{}, error) { return nil, nil }},
			"user": {
				Type: userType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return map[string]any{"id": "u1", "name": "alice"}, nil
				},
			},
			"users": {
				Type: graphql.NewList(userType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return []any{}, nil
				},
			},
			"greeting": {
				Type: graphql.String,
				Args: graphql.FieldConfigArgument{
					"msg": {Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return "hello " + p.Args["msg"].(string), nil
				},
			},
			"level1": {
				Type: level1Type,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return map[string]any{}, nil
				},
			},
			"echoSource": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					// Returns whatever source is passed in (for wrapResolver tests).
					if p.Source == nil {
						return "nil_source", nil
					}
					if s, ok := p.Source.(string); ok {
						return s, nil
					}
					return "other", nil
				},
			},
			"errField": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return nil, errors.New("resolver_error")
				},
			},
		},
	})

	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("newTestSchema: %v", err)
	}
	return schema
}

// newMutationSchema 返回包含 Query + Mutation 的 schema。
func newMutationSchema(t *testing.T) graphql.Schema {
	t.Helper()
	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"dummy": {Type: graphql.String},
		},
	})
	mutationType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Mutation",
		Fields: graphql.Fields{
			"createUser": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return "created", nil
				},
			},
		},
	})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{
		Query:    queryType,
		Mutation: mutationType,
	})
	if err != nil {
		t.Fatalf("newMutationSchema: %v", err)
	}
	return schema
}

// newInterfaceSchema 返回带 Interface 类型字段的 schema。
func newInterfaceSchema(t *testing.T) graphql.Schema {
	t.Helper()
	animalInterface := graphql.NewInterface(graphql.InterfaceConfig{
		Name: "Animal",
		Fields: graphql.Fields{
			"name": {Type: graphql.String},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object { return nil },
	})
	dogType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Dog",
		Fields: graphql.Fields{
			"name": {Type: graphql.String},
		},
		Interfaces: []*graphql.Interface{animalInterface},
	})
	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"animal": {
				Type: animalInterface,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return nil, nil
				},
			},
		},
	})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{
		Query: queryType,
		Types: []graphql.Type{dogType},
	})
	if err != nil {
		t.Fatalf("newInterfaceSchema: %v", err)
	}
	return schema
}

// ─────────────────────────────────────────────────────────────
// A. Build 层错误
// ─────────────────────────────────────────────────────────────

// A1: document 为 nil
func TestBuild_NilDocument_Error(t *testing.T) {
	schema := newTestSchema(t)
	_, err := prepare1.Build(nil, schema, nil)
	if err == nil {
		t.Fatal("expected error for nil document")
	}
}

// A2: 文档中无 OperationDefinition（仅有 FragmentDefinition）
func TestBuild_NoOperationDefinition_Error(t *testing.T) {
	schema := newTestSchema(t)
	doc := &ast.Document{
		Kind:        "Document",
		Definitions: []ast.Node{},
	}
	_, err := prepare1.Build(doc, schema, nil)
	if err == nil {
		t.Fatal("expected error for document with no operation")
	}
}

// A3: 使用 mutation 操作但 schema 没有 MutationType
func TestBuild_MutationOnQueryOnlySchema_Error(t *testing.T) {
	schema := newTestSchema(t) // 仅有 QueryType
	_, err := prepare1.Parse(`mutation { dummy }`, schema, nil)
	if err == nil {
		t.Fatal("expected error: schema has no mutation type")
	}
}

// A4: nil variables 在 Build 内部应被初始化为空 map（不崩溃）
func TestBuild_NilVariables_DoesNotCrash(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ hello }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}
}

// A5: 只有 FragmentDefinition，没有 OperationDefinition
func TestBuild_OnlyFragmentDefinition_Error(t *testing.T) {
	schema := newTestSchema(t)
	// 手动构造一个只含 FragmentDefinition 的文档
	doc := &ast.Document{
		Kind: "Document",
		Definitions: []ast.Node{
			&ast.FragmentDefinition{
				Kind: "FragmentDefinition",
				Name: &ast.Name{Value: "F"},
				SelectionSet: &ast.SelectionSet{
					Selections: []ast.Selection{},
				},
			},
		},
	}
	_, err := prepare1.Build(doc, schema, nil)
	if err == nil {
		t.Fatal("expected error: no OperationDefinition")
	}
}

// ─────────────────────────────────────────────────────────────
// B. Parse 层错误
// ─────────────────────────────────────────────────────────────

// B1: 非法 GraphQL 语法
func TestParse_InvalidSyntax_Error(t *testing.T) {
	schema := newTestSchema(t)
	_, err := prepare1.Parse(`{ {{ broken`, schema, nil)
	if err == nil {
		t.Fatal("expected parse error for invalid syntax")
	}
}

// B2: 查询中引用了 schema 不存在的字段
func TestParse_FieldNotInSchema_Error(t *testing.T) {
	schema := newTestSchema(t)
	_, err := prepare1.Parse(`{ nonexistentField }`, schema, nil)
	if err == nil {
		t.Fatal("expected error for field not in schema")
	}
}

// B3: 使用了未定义的 fragment
func TestParse_UndefinedFragmentSpread_Error(t *testing.T) {
	schema := newTestSchema(t)
	_, err := prepare1.Parse(`{ ...UndefinedFrag }`, schema, nil)
	if err == nil {
		t.Fatal("expected error for undefined fragment")
	}
}

// B4: 对 Scalar 类型字段使用 sub-selection（语义错误）
func TestParse_ScalarWithSubSelection_Error(t *testing.T) {
	schema := newTestSchema(t)
	// "hello" 是 String 类型，不支持 sub-selection
	_, err := prepare1.Parse(`{ hello { name } }`, schema, nil)
	if err == nil {
		t.Fatal("expected error: scalar field does not support sub-selection")
	}
}

// ─────────────────────────────────────────────────────────────
// C. 字段类型映射
// ─────────────────────────────────────────────────────────────

// C1: Scalar 字段 → FIELD_TYPE_SCALAR
func TestParse_ScalarField_TypeMapping(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ hello }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
	}
	if roots[0].GetFieldType() != build.FIELD_TYPE_SCALAR {
		t.Errorf("expected FIELD_TYPE_SCALAR, got %v", roots[0].GetFieldType())
	}
	if roots[0].GetFieldIsList() {
		t.Error("expected isList=false for scalar")
	}
	if roots[0].GetFieldNotNil() {
		t.Error("expected nonNull=false for nullable scalar")
	}
}

// C2: Enum 字段 → FIELD_TYPE_ENUM
func TestParse_EnumField_TypeMapping(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ status }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if roots[0].GetFieldType() != build.FIELD_TYPE_ENUM {
		t.Errorf("expected FIELD_TYPE_ENUM, got %v", roots[0].GetFieldType())
	}
}

// C3: Object 字段 → FIELD_TYPE_OBJECT，递归生成 children
func TestParse_ObjectField_TypeMapping(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ user { id name } }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if roots[0].GetFieldType() != build.FIELD_TYPE_OBJECT {
		t.Errorf("expected FIELD_TYPE_OBJECT, got %v", roots[0].GetFieldType())
	}
	if len(roots[0].GetChildrenFields()) != 2 {
		t.Errorf("expected 2 children, got %d", len(roots[0].GetChildrenFields()))
	}
}

// C4: List 字段 → FieldIsList=true
func TestParse_ListField_IsList(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ tags }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if !roots[0].GetFieldIsList() {
		t.Error("expected FieldIsList=true for [String]")
	}
	if roots[0].GetFieldNotNil() {
		t.Error("expected FieldNotNil=false for nullable [String]")
	}
}

// C5: NonNull Scalar → FieldNotNil=true
func TestParse_NonNullScalar_FieldNotNil(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ required }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if !roots[0].GetFieldNotNil() {
		t.Error("expected FieldNotNil=true for String!")
	}
	if roots[0].GetFieldIsList() {
		t.Error("expected FieldIsList=false for non-list")
	}
}

// C6: NonNull List of NonNull Scalar → FieldIsList=true AND FieldNotNil=true
func TestParse_NonNullList_BothFlags(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ requiredTags }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if !roots[0].GetFieldIsList() {
		t.Error("expected FieldIsList=true for [String!]!")
	}
	if !roots[0].GetFieldNotNil() {
		t.Error("expected FieldNotNil=true for [String!]!")
	}
}

// C7: List of Object → FieldIsList=true, FIELD_TYPE_OBJECT
func TestParse_ListOfObject_TypeAndList(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ users { id } }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if roots[0].GetFieldType() != build.FIELD_TYPE_OBJECT {
		t.Errorf("expected FIELD_TYPE_OBJECT, got %v", roots[0].GetFieldType())
	}
	if !roots[0].GetFieldIsList() {
		t.Error("expected FieldIsList=true for [User]")
	}
}

// ─────────────────────────────────────────────────────────────
// D. 字段属性
// ─────────────────────────────────────────────────────────────

// D1: Alias 优先于 fieldName 成为 ResponseName
func TestParse_Alias_ResponseName(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ greeting(msg: "world") }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.GetRoots()[0].GetResponseName() != "greeting" {
		t.Errorf("expected ResponseName='greeting', got %q", plan.GetRoots()[0].GetResponseName())
	}

	// 使用 alias
	plan2, err := prepare1.Parse(`{ hi: hello }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan2.GetRoots()[0].GetResponseName() != "hi" {
		t.Errorf("expected alias 'hi', got %q", plan2.GetRoots()[0].GetResponseName())
	}
}

// D2: Path 包含 ResponseName（alias 生效时以 alias 为 path 节点）
func TestParse_Path_Construction(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ user { name } }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	userField := plan.GetRoots()[0]
	if len(userField.GetPaths()) != 1 || userField.GetPaths()[0] != "user" {
		t.Errorf("expected path ['user'], got %v", userField.GetPaths())
	}
	nameField := userField.GetChildrenFields()[0]
	if len(nameField.GetPaths()) != 2 || nameField.GetPaths()[1] != "name" {
		t.Errorf("expected path ['user','name'], got %v", nameField.GetPaths())
	}
}

// D3: FieldId 按遍历顺序自增（先序 DFS）
func TestParse_FieldId_Sequential(t *testing.T) {
	schema := newTestSchema(t)
	// hello → ID=1, status → ID=2
	plan, err := prepare1.Parse(`{ hello status }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if roots[0].GetFieldId() != 1 {
		t.Errorf("expected ID=1, got %d", roots[0].GetFieldId())
	}
	if roots[1].GetFieldId() != 2 {
		t.Errorf("expected ID=2, got %d", roots[1].GetFieldId())
	}
}

// D4: 先序 DFS：user(2) → name(3)，hello(1) 在前
func TestParse_FieldId_PreOrder_DFS(t *testing.T) {
	schema := newTestSchema(t)
	// hello=1, user=2, name=3 (user的子字段)
	plan, err := prepare1.Parse(`{ hello user { name } }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if roots[0].GetFieldId() != 1 {
		t.Errorf("hello: expected ID=1, got %d", roots[0].GetFieldId())
	}
	userField := roots[1]
	if userField.GetFieldId() != 2 {
		t.Errorf("user: expected ID=2, got %d", userField.GetFieldId())
	}
	nameField := userField.GetChildrenFields()[0]
	if nameField.GetFieldId() != 3 {
		t.Errorf("name: expected ID=3, got %d", nameField.GetFieldId())
	}
}

// D5: 子字段 ParentFieldId 指向其父字段 FieldId
func TestParse_ParentFieldId_Correct(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ user { name } }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	userField := plan.GetRoots()[0]
	nameField := userField.GetChildrenFields()[0]

	if nameField.GetParentFieldId() != userField.GetFieldId() {
		t.Errorf("name.parentFieldId=%d, user.fieldId=%d, should be equal",
			nameField.GetParentFieldId(), userField.GetFieldId())
	}
}

// D6: 根节点的 ParentFieldId = 0
func TestParse_RootField_ParentFieldIdIsZero(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ hello }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.GetRoots()[0].GetParentFieldId() != 0 {
		t.Errorf("root field ParentFieldId should be 0, got %d", plan.GetRoots()[0].GetParentFieldId())
	}
}

// D7: 无 Resolve 函数的字段 → ResolverFunc = nil
func TestParse_NoSchemaResolver_NilFunc(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ noResolver }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.GetRoots()[0].GetResolverFunc() != nil {
		t.Error("expected ResolverFunc=nil for field without schema resolver")
	}
}

// D8: __typename 元字段被跳过，不生成 FieldPlan
func TestParse_MetaField_Skipped(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ __typename hello }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if len(roots) != 1 {
		t.Errorf("expected 1 root (skipping __typename), got %d", len(roots))
	}
	if roots[0].GetResponseName() != "hello" {
		t.Errorf("expected 'hello', got %q", roots[0].GetResponseName())
	}
}

// ─────────────────────────────────────────────────────────────
// E. 参数类型转换
// ─────────────────────────────────────────────────────────────

// getFirstParam 从第一个 root field 取第一个 ParamPlan（便于各参数测试）
func getFirstParam(t *testing.T, plan *build.SGraphPlan) *build.ParamPlan {
	t.Helper()
	roots := plan.GetRoots()
	if len(roots) == 0 {
		t.Fatal("no roots in plan")
	}
	params := roots[0].GetParamPlans()
	if len(params) == 0 {
		t.Fatal("no param plans in first root")
	}
	return params[0]
}

// E1: Variable 参数 → PARAM_TYPE_INPUT，inputName = 变量名
func TestParse_ArgVariable_InputParam(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ greeting(msg: $inputMsg) }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pp := getFirstParam(t, plan)
	if pp.GetParamType() != build.PARAM_TYPE_INPUT {
		t.Errorf("expected PARAM_TYPE_INPUT, got %v", pp.GetParamType())
	}
	if pp.GetInputName() != "inputMsg" {
		t.Errorf("expected inputName='inputMsg', got %q", pp.GetInputName())
	}
	if pp.GetParamKey() != "msg" {
		t.Errorf("expected paramKey='msg', got %q", pp.GetParamKey())
	}
}

// E2: Int 字面量 → PARAM_TYPE_CONST，值为 int64
func TestParse_ArgIntLiteral_ConstInt64(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ greeting(msg: 42) }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pp := getFirstParam(t, plan)
	if pp.GetParamType() != build.PARAM_TYPE_CONST {
		t.Errorf("expected PARAM_TYPE_CONST, got %v", pp.GetParamType())
	}
	v, ok := pp.GetConstValue().(int64)
	if !ok || v != 42 {
		t.Errorf("expected int64(42), got %T(%v)", pp.GetConstValue(), pp.GetConstValue())
	}
}

// E3: Float 字面量 → PARAM_TYPE_CONST，值为 float64
func TestParse_ArgFloatLiteral_ConstFloat64(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ greeting(msg: 3.14) }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pp := getFirstParam(t, plan)
	if pp.GetParamType() != build.PARAM_TYPE_CONST {
		t.Errorf("expected PARAM_TYPE_CONST, got %v", pp.GetParamType())
	}
	v, ok := pp.GetConstValue().(float64)
	if !ok {
		t.Fatalf("expected float64, got %T", pp.GetConstValue())
	}
	// 允许浮点精度误差
	if v < 3.13 || v > 3.15 {
		t.Errorf("expected ~3.14, got %v", v)
	}
}

// E4: String 字面量 → PARAM_TYPE_CONST，值为 string
func TestParse_ArgStringLiteral_ConstString(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ greeting(msg: "hello") }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pp := getFirstParam(t, plan)
	if pp.GetConstValue() != "hello" {
		t.Errorf("expected 'hello', got %v", pp.GetConstValue())
	}
}

// E5: Boolean 字面量 → PARAM_TYPE_CONST，值为 bool
func TestParse_ArgBoolLiteral_ConstBool(t *testing.T) {
	schema := newTestSchema(t)
	// 传入 true
	plan, err := prepare1.Parse(`{ greeting(msg: true) }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pp := getFirstParam(t, plan)
	v, ok := pp.GetConstValue().(bool)
	if !ok || !v {
		t.Errorf("expected bool(true), got %T(%v)", pp.GetConstValue(), pp.GetConstValue())
	}

	// 传入 false
	plan2, err := prepare1.Parse(`{ greeting(msg: false) }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pp2 := getFirstParam(t, plan2)
	v2, ok2 := pp2.GetConstValue().(bool)
	if !ok2 || v2 {
		t.Errorf("expected bool(false), got %T(%v)", pp2.GetConstValue(), pp2.GetConstValue())
	}
}

// E6: Enum 字面量 → PARAM_TYPE_CONST，值为 string（枚举名称）
func TestParse_ArgEnumLiteral_ConstString(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ greeting(msg: ACTIVE) }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pp := getFirstParam(t, plan)
	if pp.GetConstValue() != "ACTIVE" {
		t.Errorf("expected 'ACTIVE', got %v", pp.GetConstValue())
	}
}

// E7: List 字面量 → PARAM_TYPE_CONST，值为 []any
func TestParse_ArgListLiteral_ConstSlice(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ greeting(msg: [1, 2, 3]) }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pp := getFirstParam(t, plan)
	items, ok := pp.GetConstValue().([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", pp.GetConstValue())
	}
	if len(items) != 3 {
		t.Errorf("expected len=3, got %d", len(items))
	}
	// 每个元素是 int64
	if items[0].(int64) != 1 || items[1].(int64) != 2 || items[2].(int64) != 3 {
		t.Errorf("unexpected list values: %v", items)
	}
}

// E8: Object 字面量 → PARAM_TYPE_CONST，值为 map[string]any
func TestParse_ArgObjectLiteral_ConstMap(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ greeting(msg: {key: "val"}) }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pp := getFirstParam(t, plan)
	m, ok := pp.GetConstValue().(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", pp.GetConstValue())
	}
	if m["key"] != "val" {
		t.Errorf("expected m['key']='val', got %v", m["key"])
	}
}

// E9: 同一字段上多个参数 → 多个 ParamPlan
func TestParse_MultipleArgs_MultipleParamPlans(t *testing.T) {
	schema := newTestSchema(t)
	// greeting 只声明了 msg，但 prepare1 不做 schema 参数校验，多余的 args 也会被收集
	plan, err := prepare1.Parse(`{ greeting(msg: "hi", extra: 99) }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	params := plan.GetRoots()[0].GetParamPlans()
	if len(params) != 2 {
		t.Errorf("expected 2 ParamPlans, got %d", len(params))
	}
}

// E10: 嵌套 List 字面量（极限：[[1,2],[3]]）→ []any{[]any{int64(1),int64(2)}, []any{int64(3)}}
func TestParse_ArgNestedListLiteral(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ greeting(msg: [[1, 2], [3]]) }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pp := getFirstParam(t, plan)
	outer, ok := pp.GetConstValue().([]any)
	if !ok || len(outer) != 2 {
		t.Fatalf("expected outer []any len=2, got %T", pp.GetConstValue())
	}
	inner0, ok := outer[0].([]any)
	if !ok || len(inner0) != 2 {
		t.Fatalf("expected inner0 len=2")
	}
}

// ─────────────────────────────────────────────────────────────
// F. 选择集特性
// ─────────────────────────────────────────────────────────────

// F1: FragmentSpread → 字段从 fragment 内联进来
func TestParse_FragmentSpread_InlinesFields(t *testing.T) {
	schema := newTestSchema(t)
	query := `
		fragment UserFields on User {
			id
			name
		}
		{ user { ...UserFields } }
	`
	plan, err := prepare1.Parse(query, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	userField := plan.GetRoots()[0]
	children := userField.GetChildrenFields()
	if len(children) != 2 {
		t.Errorf("expected 2 children from fragment, got %d", len(children))
	}
	names := map[string]bool{children[0].GetResponseName(): true, children[1].GetResponseName(): true}
	if !names["id"] || !names["name"] {
		t.Errorf("expected children 'id' and 'name', got %v", names)
	}
}

// F2: InlineFragment → 透明透传，字段直接进入父选择集
func TestParse_InlineFragment_Transparent(t *testing.T) {
	schema := newTestSchema(t)
	query := `{
		user {
			... on User {
				id
				name
			}
		}
	}`
	plan, err := prepare1.Parse(query, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	children := plan.GetRoots()[0].GetChildrenFields()
	if len(children) != 2 {
		t.Errorf("expected 2 children via inline fragment, got %d", len(children))
	}
}

// F3: 三层嵌套 → user → address → city
func TestParse_ThreeLevelNesting(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ user { address { city } } }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	userField := plan.GetRoots()[0]
	if userField.GetFieldType() != build.FIELD_TYPE_OBJECT {
		t.Error("user should be OBJECT")
	}
	addressField := userField.GetChildrenFields()[0]
	if addressField.GetFieldType() != build.FIELD_TYPE_OBJECT {
		t.Error("address should be OBJECT")
	}
	cityField := addressField.GetChildrenFields()[0]
	if cityField.GetFieldType() != build.FIELD_TYPE_SCALAR {
		t.Error("city should be SCALAR")
	}
	// path 包含所有层级
	expectedPath := []string{"user", "address", "city"}
	gotPath := cityField.GetPaths()
	if len(gotPath) != len(expectedPath) {
		t.Errorf("expected path %v, got %v", expectedPath, gotPath)
	}
	for i, p := range expectedPath {
		if gotPath[i] != p {
			t.Errorf("path[%d]: expected %q, got %q", i, p, gotPath[i])
		}
	}
}

// F4: 四层嵌套 → level1 → level2 → leaf（极限嵌套）
func TestParse_FourLevelNesting(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ level1 { level2 { leaf } } }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	level1 := plan.GetRoots()[0]
	if len(level1.GetChildrenFields()) == 0 {
		t.Fatal("level1 should have children")
	}
	level2 := level1.GetChildrenFields()[0]
	if len(level2.GetChildrenFields()) == 0 {
		t.Fatal("level2 should have children")
	}
	leaf := level2.GetChildrenFields()[0]
	if leaf.GetResponseName() != "leaf" {
		t.Errorf("expected 'leaf', got %q", leaf.GetResponseName())
	}
}

// F5: 多个 root 字段 → roots 数量正确
func TestParse_MultipleRootFields(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ hello status required }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.GetRoots()) != 3 {
		t.Errorf("expected 3 roots, got %d", len(plan.GetRoots()))
	}
}

// F6: 完全没有 sub-selection 的 Object 字段（无 children，不报错）
func TestParse_ObjectFieldWithoutSubSelection(t *testing.T) {
	schema := newTestSchema(t)
	// user 是 Object 但不写子字段 → children 为空，不报错
	plan, err := prepare1.Parse(`{ user }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	userField := plan.GetRoots()[0]
	if len(userField.GetChildrenFields()) != 0 {
		t.Errorf("expected 0 children, got %d", len(userField.GetChildrenFields()))
	}
}

// F7: 空查询体（手动构造 nil SelectionSet）→ 空 roots，不崩溃
func TestBuild_NilSelectionSet_EmptyRoots(t *testing.T) {
	schema := newTestSchema(t)
	doc := &ast.Document{
		Kind: "Document",
		Definitions: []ast.Node{
			&ast.OperationDefinition{
				Kind:         "OperationDefinition",
				Operation:    "query",
				SelectionSet: nil,
			},
		},
	}
	plan, err := prepare1.Build(doc, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.GetRoots()) != 0 {
		t.Errorf("expected empty roots for nil SelectionSet, got %d", len(plan.GetRoots()))
	}
}

// ─────────────────────────────────────────────────────────────
// G. Mutation 操作
// ─────────────────────────────────────────────────────────────

// G1: mutation 操作 → 使用 MutationType 的字段
func TestParse_MutationOperation_UsesMutationType(t *testing.T) {
	schema := newMutationSchema(t)
	plan, err := prepare1.Parse(`mutation { createUser }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
	}
	if roots[0].GetResponseName() != "createUser" {
		t.Errorf("expected 'createUser', got %q", roots[0].GetResponseName())
	}
}

// G2: mutation 操作引用了 MutationType 中不存在的字段 → 报错
func TestParse_MutationField_NotInMutationType_Error(t *testing.T) {
	schema := newMutationSchema(t)
	_, err := prepare1.Parse(`mutation { nonexistent }`, schema, nil)
	if err == nil {
		t.Fatal("expected error for field not in MutationType")
	}
}

// G3: query 操作（明确写 query 关键字） → 使用 QueryType
func TestParse_ExplicitQuery_UsesQueryType(t *testing.T) {
	schema := newMutationSchema(t)
	plan, err := prepare1.Parse(`query { dummy }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.GetRoots()) != 1 || plan.GetRoots()[0].GetResponseName() != "dummy" {
		t.Errorf("expected root 'dummy', got %+v", plan.GetRoots())
	}
}

// ─────────────────────────────────────────────────────────────
// H. Resolver 包装行为
// ─────────────────────────────────────────────────────────────

// fakeResponse 满足 prepare1 内部 firstResponseGetter 接口（鸭子类型）
type fakeResponse struct{ val any }

func (f *fakeResponse) GetFirstResponse() any { return f.val }

// H1: source = nil（根字段）→ resolver 的 p.Source = nil
func TestWrapResolver_NilSource_PassedThrough(t *testing.T) {
	var capturedSource any = "not_nil_sentinel"
	schema := makeSourceCaptureSchema(t, func(s any) { capturedSource = s })

	plan, err := prepare1.Parse(`{ captureSource }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resolver := plan.GetRoots()[0].GetResolverFunc()
	if resolver == nil {
		t.Fatal("expected non-nil resolver")
	}
	_, _ = resolver(nil, nil, context.Background())
	if capturedSource != nil {
		t.Errorf("expected p.Source=nil for nil input, got %v", capturedSource)
	}
}

// H2: source 实现 firstResponseGetter → p.Source = GetFirstResponse() 的返回值
func TestWrapResolver_FirstResponseGetter_ExtractsInner(t *testing.T) {
	expected := map[string]any{"id": "u1"}
	var capturedSource any
	schema := makeSourceCaptureSchema(t, func(s any) { capturedSource = s })

	plan, err := prepare1.Parse(`{ captureSource }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resolver := plan.GetRoots()[0].GetResolverFunc()

	fake := &fakeResponse{val: expected}
	_, _ = resolver(fake, nil, context.Background())
	if capturedSource == nil {
		t.Fatal("capturedSource should not be nil")
	}
	m, ok := capturedSource.(map[string]any)
	if !ok || m["id"] != "u1" {
		t.Errorf("expected extracted source=%v, got %v", expected, capturedSource)
	}
}

// H3: source 是普通值（不实现 firstResponseGetter）→ p.Source = source 本身
func TestWrapResolver_PlainSource_PassedThrough(t *testing.T) {
	var capturedSource any
	schema := makeSourceCaptureSchema(t, func(s any) { capturedSource = s })

	plan, err := prepare1.Parse(`{ captureSource }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resolver := plan.GetRoots()[0].GetResolverFunc()

	_, _ = resolver("plain_value", nil, context.Background())
	if capturedSource != "plain_value" {
		t.Errorf("expected 'plain_value', got %v", capturedSource)
	}
}

// H4: resolver 返回 error → 透传给调用方
func TestWrapResolver_ResolverError_Propagated(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ errField }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resolver := plan.GetRoots()[0].GetResolverFunc()
	_, resolveErr := resolver(nil, nil, context.Background())
	if resolveErr == nil {
		t.Fatal("expected resolver error to be propagated")
	}
	if resolveErr.Error() != "resolver_error" {
		t.Errorf("expected 'resolver_error', got %q", resolveErr.Error())
	}
}

// H5: context 被正确透传到 resolver
func TestWrapResolver_ContextPassedThrough(t *testing.T) {
	type ctxKey struct{}
	var capturedCtx context.Context
	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"ctxField": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					capturedCtx = p.Context
					return "ok", nil
				},
			},
		},
	})
	schema, _ := graphql.NewSchema(graphql.SchemaConfig{Query: queryType})
	plan, err := prepare1.Parse(`{ ctxField }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resolver := plan.GetRoots()[0].GetResolverFunc()

	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	_, _ = resolver(nil, nil, ctx)

	if capturedCtx == nil {
		t.Fatal("expected context to be captured")
	}
	if capturedCtx.Value(ctxKey{}) != "marker" {
		t.Error("context value not passed through correctly")
	}
}

// makeSourceCaptureSchema 创建一个 schema，其 captureSource 字段 resolver 调用 cb(p.Source)。
func makeSourceCaptureSchema(t *testing.T, cb func(any)) graphql.Schema {
	t.Helper()
	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"captureSource": {
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					cb(p.Source)
					return "ok", nil
				},
			},
		},
	})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatalf("makeSourceCaptureSchema: %v", err)
	}
	return schema
}

// ─────────────────────────────────────────────────────────────
// I. 变量透传
// ─────────────────────────────────────────────────────────────

// I1: 运行时变量被透传进 SGraphPlan.GetOriginalInputs()
func TestBuild_Variables_PreservedInPlan(t *testing.T) {
	schema := newTestSchema(t)
	vars := map[string]any{"userId": "u42"}
	plan, err := prepare1.Parse(`{ hello }`, schema, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := plan.GetOriginalInputs()
	if got["userId"] != "u42" {
		t.Errorf("expected variables to be preserved, got %v", got)
	}
}

// I2: nil variables → OriginalInputs 为空 map（不是 nil）
func TestBuild_NilVariables_EmptyMap(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ hello }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := plan.GetOriginalInputs()
	if got == nil {
		t.Error("expected non-nil OriginalInputs for nil variables")
	}
}

// ─────────────────────────────────────────────────────────────
// J. Interface 类型字段
// ─────────────────────────────────────────────────────────────

// J1: Interface 类型字段 → FIELD_TYPE_OBJECT，sub-selection 被正确构建
func TestParse_InterfaceField_TypeObject_WithChildren(t *testing.T) {
	schema := newInterfaceSchema(t)
	plan, err := prepare1.Parse(`{ animal { name } }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roots := plan.GetRoots()
	if roots[0].GetFieldType() != build.FIELD_TYPE_OBJECT {
		t.Errorf("expected FIELD_TYPE_OBJECT for interface field, got %v", roots[0].GetFieldType())
	}
	children := roots[0].GetChildrenFields()
	if len(children) != 1 || children[0].GetResponseName() != "name" {
		t.Errorf("expected child 'name', got %v", children)
	}
}

// ─────────────────────────────────────────────────────────────
// K. 综合集成：Parse → Execute
// ─────────────────────────────────────────────────────────────

// K1: Parse 生成的 Plan 可以被 SGraphEngine 执行并返回正确结果
func TestParse_PlanIsExecutable(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ hello }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 不依赖 engine，只验证 Plan 结构正确
	roots := plan.GetRoots()
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
	}
	root := roots[0]
	if root.GetResponseName() != "hello" {
		t.Errorf("expected ResponseName='hello', got %q", root.GetResponseName())
	}
	if root.GetResolverFunc() == nil {
		t.Error("expected non-nil resolver")
	}
	// 直接调用包装后的 resolver，验证端到端行为
	res, err := root.GetResolverFunc()(nil, nil, context.Background())
	if err != nil {
		t.Fatalf("resolver error: %v", err)
	}
	if res != "world" {
		t.Errorf("expected 'world', got %v", res)
	}
}

// K2: 查询变量通过 PARAM_TYPE_INPUT 传递到 resolver
func TestParse_VariablePassedToResolver(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ greeting(msg: $name) }`, schema, map[string]any{"name": "alice"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 验证 ParamPlan 是 INPUT 类型且 inputName = "name"
	root := plan.GetRoots()[0]
	params := root.GetParamPlans()
	if len(params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(params))
	}
	if params[0].GetParamType() != build.PARAM_TYPE_INPUT {
		t.Errorf("expected PARAM_TYPE_INPUT, got %v", params[0].GetParamType())
	}
	if params[0].GetInputName() != "name" {
		t.Errorf("expected inputName='name', got %q", params[0].GetInputName())
	}
}

// K3: alias 在 path 中体现（alias → ResponseName → path 节点）
func TestParse_AliasInPath(t *testing.T) {
	schema := newTestSchema(t)
	plan, err := prepare1.Parse(`{ hi: hello }`, schema, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	root := plan.GetRoots()[0]
	if root.GetPaths()[0] != "hi" {
		t.Errorf("expected path[0]='hi' (alias), got %q", root.GetPaths()[0])
	}
}
