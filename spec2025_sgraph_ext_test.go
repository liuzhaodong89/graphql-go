package graphql

// spec2025_sgraph_ext_test.go
//
// SGraph 专项：评估新引擎自身的设计目标（查询折叠、依赖编排、参数物化、池化隔离）。
// 这些用例不属于两条链路的对比基线，命名前缀 TestSGraphExt_ 使其不被 ^TestSpec2025 的
// 对比运行选中。它们只有在 sgraph 路由下才有意义。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

// ---------------------------------------------------------------------------
// 公共构件
// ---------------------------------------------------------------------------

// s25sgChainSchema 构造 root -> next -> next -> value 的自引用链，
// 每个 resolver 都把调用转发给回调，回调第一个参数是响应路径。
func s25sgChainSchema(t testing.TB, resolve func(path string, p ResolveParams) (any, error)) Schema {
	t.Helper()
	pathOf := func(p ResolveParams) string {
		parts := make([]string, 0, 4)
		for current := p.Info.Path; current != nil; current = current.Prev {
			parts = append([]string{fmt.Sprintf("%v", current.Key)}, parts...)
		}
		return strings.Join(parts, ".")
	}
	var node *Object
	node = NewObject(ObjectConfig{
		Name: "S25SgNode",
		Fields: FieldsThunk(func() Fields {
			return Fields{
				"id": &Field{Type: NewNonNull(ID)},
				"next": &Field{
					Type: node,
					Args: FieldConfigArgument{"id": &ArgumentConfig{Type: NewNonNull(ID)}},
					Resolve: func(p ResolveParams) (any, error) {
						return resolve(pathOf(p), p)
					},
				},
				"value": &Field{
					Type: String,
					Args: FieldConfigArgument{"id": &ArgumentConfig{Type: NewNonNull(ID)}},
					Resolve: func(p ResolveParams) (any, error) {
						return resolve(pathOf(p), p)
					},
				},
			}
		}),
	})
	schema, err := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
			"root": &Field{
				Type: node,
				Args: FieldConfigArgument{"id": &ArgumentConfig{Type: NewNonNull(ID)}},
				Resolve: func(p ResolveParams) (any, error) {
					return resolve(pathOf(p), p)
				},
			},
		}}),
		Types: []Type{node},
	})
	if err != nil {
		t.Fatalf("build sgraph chain schema: %v", err)
	}
	return schema
}

const s25sgChainQuery = `query Chain($id: ID!) {
	root(id: $id) {
		id
		next(id: $id) {
			id
			value(id: $id)
		}
	}
}`

func s25sgParse(t testing.TB, schema Schema, body string) *ast.Document {
	t.Helper()
	document, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{
		Body: []byte(body), Name: "SGraph ext test",
	})})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if validation := ValidateDocument(&schema, document, nil); !validation.IsValid {
		t.Fatalf("validate: %v", validation.Errors)
	}
	return document
}

// s25sgBatchPaths 把编排结果转成每批的字段名列表，用于断言批次拓扑。
func s25sgBatchPaths(batches []*BatchPlan) [][]string {
	result := make([][]string, 0, len(batches))
	for _, batch := range batches {
		names := make([]string, 0, len(batch.steps))
		for _, step := range batch.steps {
			switch typed := step.(type) {
			case *SingleCallStep:
				names = append(names, strings.Join(typed.fieldPlan.paths, "."))
			case *IterationCallStep:
				names = append(names, strings.Join(typed.fieldPlan.paths, "."))
			default:
				names = append(names, "unknown")
			}
		}
		result = append(result, names)
	}
	return result
}

func s25sgCompile(t testing.TB, schema Schema, body, operationName string, registry *ParamRegistry) ([]*BatchPlan, error) {
	t.Helper()
	document := s25sgParse(t, schema, body)
	var name *string
	if operationName != "" {
		name = &operationName
	}
	plan, err := compileExecutionPlan(document, &schema, name, NewDirectiveRegistry(), registry)
	if err != nil {
		return nil, err
	}
	return coordinateBatches(plan)
}

// ---------------------------------------------------------------------------
// S-01 批次拓扑
// ---------------------------------------------------------------------------

func TestSGraphExt_BatchTopology(t *testing.T) {
	schema := s25sgChainSchema(t, func(path string, p ResolveParams) (any, error) {
		if strings.HasSuffix(path, "value") {
			return "leaf", nil
		}
		return map[string]any{"id": "n"}, nil
	})

	t.Run("no_declared_dependency_folds_everything_into_one_batch", func(t *testing.T) {
		batches, err := s25sgCompile(t, schema, s25sgChainQuery, "Chain", NewParamRegistry())
		if err != nil {
			t.Fatalf("coordinate: %v", err)
		}
		paths := s25sgBatchPaths(batches)
		if len(paths) != 1 {
			t.Fatalf("expected a single folded batch, got %d: %v", len(paths), paths)
		}
		if len(paths[0]) != 3 {
			t.Errorf("batch 0 = %v, want the three resolver steps folded together", paths[0])
		}
	})

	t.Run("declared_dependencies_produce_a_chain_of_batches", func(t *testing.T) {
		registry := NewParamRegistry()
		if err := registry.RegisterQuery(QueryParamConfig{
			DocumentBody:  s25sgChainQuery,
			OperationName: "Chain",
			FieldParams: []FieldParamBinding{
				s25NamedBinding([]string{"root", "next"}, "S25SgNode", "next", "id",
					[]string{"root"}, "Query", "root", "id"),
				s25NamedBinding([]string{"root", "next", "value"}, "S25SgNode", "value", "id",
					[]string{"root", "next"}, "S25SgNode", "next", "id"),
			},
		}); err != nil {
			t.Fatalf("register: %v", err)
		}
		batches, err := s25sgCompile(t, schema, s25sgChainQuery, "Chain", registry)
		if err != nil {
			t.Fatalf("coordinate: %v", err)
		}
		paths := s25sgBatchPaths(batches)
		if len(paths) != 3 {
			t.Fatalf("expected 3 chained batches, got %d: %v", len(paths), paths)
		}
		for index, batch := range paths {
			if len(batch) != 1 {
				t.Errorf("batch %d = %v, want exactly one step", index, batch)
			}
		}
	})

	t.Run("mixed_dependencies_split_into_two_batches", func(t *testing.T) {
		registry := NewParamRegistry()
		if err := registry.RegisterQuery(QueryParamConfig{
			DocumentBody:  s25sgChainQuery,
			OperationName: "Chain",
			FieldParams: []FieldParamBinding{
				s25NamedBinding([]string{"root", "next", "value"}, "S25SgNode", "value", "id",
					[]string{"root", "next"}, "S25SgNode", "next", "id"),
			},
		}); err != nil {
			t.Fatalf("register: %v", err)
		}
		batches, err := s25sgCompile(t, schema, s25sgChainQuery, "Chain", registry)
		if err != nil {
			t.Fatalf("coordinate: %v", err)
		}
		paths := s25sgBatchPaths(batches)
		if len(paths) != 2 {
			t.Fatalf("expected 2 batches, got %d: %v", len(paths), paths)
		}
		if len(paths[0]) != 2 || len(paths[1]) != 1 {
			t.Errorf("batch shape = %v, want two independent steps then the dependent one", paths)
		}
	})

	t.Run("every_query_batch_is_marked_concurrent", func(t *testing.T) {
		batches, err := s25sgCompile(t, schema, s25sgChainQuery, "Chain", NewParamRegistry())
		if err != nil {
			t.Fatalf("coordinate: %v", err)
		}
		for index, batch := range batches {
			if !batch.concurrent {
				t.Errorf("batch %d is not marked concurrent", index)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// S-02 同 batch 真并发
// ---------------------------------------------------------------------------

func TestSGraphExt_FoldedStepsRunConcurrently(t *testing.T) {
	// 父与子在同一 batch 时必须真正并发：双方互相等待，只有并发才能同时完成。
	parentEntered := make(chan struct{})
	childEntered := make(chan struct{})
	const wait = 3 * time.Second

	schema := s25sgChainSchema(t, func(path string, p ResolveParams) (any, error) {
		switch {
		case path == "root":
			close(parentEntered)
			select {
			case <-childEntered:
			case <-time.After(wait):
				return nil, fmt.Errorf("root timed out waiting for the child step")
			}
			return map[string]any{"id": "n"}, nil
		case strings.HasSuffix(path, "next"):
			close(childEntered)
			select {
			case <-parentEntered:
			case <-time.After(wait):
				return nil, fmt.Errorf("next timed out waiting for the parent step")
			}
			return map[string]any{"id": "n2"}, nil
		default:
			return "leaf", nil
		}
	})

	const query = `query Pair($id: ID!) { root(id: $id) { id next(id: $id) { id } } }`
	engine, err := NewSGraphEngine(&schema, nil, NewParamRegistry())
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	document := s25sgParse(t, schema, query)
	name := "Pair"
	result := engine.Execute(document, map[string]any{"id": "x"}, &name, nil, context.Background()).toGraphQLResult()
	if len(result.Errors) != 0 {
		t.Fatalf("folded steps did not execute concurrently: %v", s25ErrorMessages(result))
	}
}

// ---------------------------------------------------------------------------
// S-03 参数来源物化
// ---------------------------------------------------------------------------

func TestSGraphExt_ParamSourceMaterialization(t *testing.T) {
	const query = `query Src { root(id: "r") { id next(id: "n") { id value(id: "v") } } }`

	seen := map[string]string{}
	var mu sync.Mutex
	schema := s25sgChainSchema(t, func(path string, p ResolveParams) (any, error) {
		mu.Lock()
		seen[path] = fmt.Sprintf("%v", p.Args["id"])
		mu.Unlock()
		if strings.HasSuffix(path, "value") {
			return "leaf", nil
		}
		return map[string]any{"id": "obj-" + path}, nil
	})

	registry := NewParamRegistry()
	if err := registry.RegisterQuery(QueryParamConfig{
		DocumentBody:  query,
		OperationName: "Src",
		FieldParams: []FieldParamBinding{
			// CONST 覆盖
			{
				Target: FieldParamTarget{ResponsePath: []string{"root", "next"}, ParentTypeName: "S25SgNode", FieldName: "next", ParamName: "id"},
				Source: ParamSource{Kind: ParamSourceConst, ConstValue: "const-id"},
			},
			// FIELD_RESPONSE 深路径取值
			s25NamedBinding([]string{"root", "next", "value"}, "S25SgNode", "value", "id",
				[]string{"root", "next"}, "S25SgNode", "next", "id"),
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	engine, err := NewSGraphEngine(&schema, nil, registry)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	document := s25sgParse(t, schema, query)
	name := "Src"
	result := engine.Execute(document, nil, &name, nil, context.Background()).toGraphQLResult()
	if len(result.Errors) != 0 {
		t.Fatalf("execution errors: %v", s25ErrorMessages(result))
	}

	mu.Lock()
	defer mu.Unlock()
	if seen["root"] != "r" {
		t.Errorf("root received id=%q, want the query literal %q", seen["root"], "r")
	}
	if seen["root.next"] != "const-id" {
		t.Errorf("next received id=%q, want the registered CONST override %q", seen["root.next"], "const-id")
	}
	if seen["root.next.value"] != "obj-root.next" {
		t.Errorf("value received id=%q, want the parent field result %q", seen["root.next.value"], "obj-root.next")
	}
}

// ---------------------------------------------------------------------------
// S-04 非法依赖图
// ---------------------------------------------------------------------------

func TestSGraphExt_RejectsInvalidDependencyGraphs(t *testing.T) {
	schema := s25sgChainSchema(t, func(path string, p ResolveParams) (any, error) {
		if strings.HasSuffix(path, "value") {
			return "leaf", nil
		}
		return map[string]any{"id": "n"}, nil
	})

	cases := []struct {
		name     string
		bindings []FieldParamBinding
		want     string
	}{
		{
			name: "self_dependency",
			bindings: []FieldParamBinding{
				s25NamedBinding([]string{"root", "next"}, "S25SgNode", "next", "id",
					[]string{"root", "next"}, "S25SgNode", "next", "id"),
			},
			want: "cannot depend on itself",
		},
		{
			name: "cycle",
			bindings: []FieldParamBinding{
				s25NamedBinding([]string{"root", "next"}, "S25SgNode", "next", "id",
					[]string{"root", "next", "value"}, "S25SgNode", "value"),
				s25NamedBinding([]string{"root", "next", "value"}, "S25SgNode", "value", "id",
					[]string{"root", "next"}, "S25SgNode", "next", "id"),
			},
			want: "cycle",
		},
		{
			name: "unknown_source",
			bindings: []FieldParamBinding{
				s25NamedBinding([]string{"root", "next"}, "S25SgNode", "next", "id",
					[]string{"root", "missing"}, "S25SgNode", "missing", "id"),
			},
			want: "unknown FIELD_RESPONSE source",
		},
	}

	for _, current := range cases {
		t.Run(current.name, func(t *testing.T) {
			registry := NewParamRegistry()
			if err := registry.RegisterQuery(QueryParamConfig{
				DocumentBody:  s25sgChainQuery,
				OperationName: "Chain",
				FieldParams:   current.bindings,
			}); err != nil {
				t.Fatalf("register: %v", err)
			}
			_, err := s25sgCompile(t, schema, s25sgChainQuery, "Chain", registry)
			if err == nil {
				t.Fatalf("expected the dependency graph to be rejected")
			}
			if !strings.Contains(err.Error(), current.want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), current.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// S-07 Plan cache
// ---------------------------------------------------------------------------

func TestSGraphExt_PlanCacheIsolatesRequestValues(t *testing.T) {
	schema := s25sgChainSchema(t, func(path string, p ResolveParams) (any, error) {
		if strings.HasSuffix(path, "value") {
			return fmt.Sprintf("v:%v", p.Args["id"]), nil
		}
		return map[string]any{"id": fmt.Sprintf("%v", p.Args["id"])}, nil
	})
	engine, err := NewSGraphEngine(&schema, nil, NewParamRegistry())
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	document := s25sgParse(t, schema, s25sgChainQuery)
	name := "Chain"

	for _, id := range []string{"a", "b", "a", "c"} {
		result := engine.Execute(document, map[string]any{"id": id}, &name, nil, context.Background()).toGraphQLResult()
		if len(result.Errors) != 0 {
			t.Fatalf("id=%s errors: %v", id, s25ErrorMessages(result))
		}
		data, ok := s25Plain(result.Data).(map[string]any)
		if !ok {
			t.Fatalf("id=%s data is %T", id, result.Data)
		}
		root := data["root"].(map[string]any)
		if root["id"] != id {
			t.Errorf("id=%s: root.id = %v, want %v (plan cache leaked a request value)", id, root["id"], id)
		}
	}
}

func TestSGraphExt_ConcurrentRequestsOnOneEngine(t *testing.T) {
	schema := s25sgChainSchema(t, func(path string, p ResolveParams) (any, error) {
		if strings.HasSuffix(path, "value") {
			return fmt.Sprintf("v:%v", p.Args["id"]), nil
		}
		return map[string]any{"id": fmt.Sprintf("%v", p.Args["id"])}, nil
	})
	engine, err := NewSGraphEngine(&schema, nil, NewParamRegistry())
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	document := s25sgParse(t, schema, s25sgChainQuery)
	name := "Chain"

	const requests = 64
	failures := make([]string, requests)
	var wg sync.WaitGroup
	wg.Add(requests)
	for index := 0; index < requests; index++ {
		go func(index int) {
			defer wg.Done()
			id := fmt.Sprintf("id-%d", index)
			result := engine.Execute(document, map[string]any{"id": id}, &name, nil, context.Background()).toGraphQLResult()
			if len(result.Errors) != 0 {
				failures[index] = fmt.Sprintf("request %d errors: %v", index, s25ErrorMessages(result))
				return
			}
			data, ok := s25Plain(result.Data).(map[string]any)
			if !ok {
				failures[index] = fmt.Sprintf("request %d data is %T", index, result.Data)
				return
			}
			root := data["root"].(map[string]any)
			if root["id"] != id {
				failures[index] = fmt.Sprintf("request %d root.id = %v, want %v", index, root["id"], id)
			}
		}(index)
	}
	wg.Wait()
	for _, failure := range failures {
		if failure != "" {
			t.Errorf("%s", failure)
		}
	}
}

// ---------------------------------------------------------------------------
// S-08 池化后无请求间残留
// ---------------------------------------------------------------------------

func TestSGraphExt_PoolsClearRequestState(t *testing.T) {
	rundata := newRundata(map[string]any{"a": 1}, 4)
	rundata.addFieldErrorAtPath(1, FieldErrorTypeField, fmt.Errorf("first"), []any{"a", 0})
	response := acquireFieldResponse()
	response.responseRaws = append(response.responseRaws, "value")
	response.parentBindingMode = fieldResponseBindingCompositeKey
	rundata.setFieldResponse(1, response)
	releaseRundata(rundata)

	reused := newRundata(nil, 4)
	defer releaseRundata(reused)
	if got := reused.getAllFieldErrors(); len(got) != 0 {
		t.Errorf("recycled Rundata still holds %d field errors", len(got))
	}
	if reused.originalParams != nil {
		t.Errorf("recycled Rundata still holds request inputs: %v", reused.originalParams)
	}
	if reused.schema != nil || reused.executionPlan != nil || reused.operation != nil {
		t.Errorf("recycled Rundata still references request-scoped objects")
	}
	for index := range reused.fieldResponses {
		if reused.fieldResponses[index].Load() != nil {
			t.Errorf("recycled Rundata still holds a FieldResponse at slot %d", index)
		}
	}
}
