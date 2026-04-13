// Package main 展示三层架构 GraphQL 引擎的完整使用方式。
//
// 演示场景：查询用户列表（KindList），每个用户有 id、name 字段，
// 以及嵌套的 address 对象（KindObject，包含 city、country 子字段）。
// 同时演示同批次并发的根字段 version（KindScalar）。
//
// 三层架构回顾：
//  1. Plan 层 (sgraph/build)：FieldDef -> SelectionPlan（静态编译、slot 分配）
//  2. Exec 层 (sgraph/exec)：SelectionPlan + RunFrame -> 并发/串行执行
//  3. Response 层 (sgraph/response)：slot 数组 + errors -> GraphQL { data, errors }
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/graphql-go/graphql/sgraph/exec"
	"github.com/graphql-go/graphql/sgraph/plan"
)

func main() {
	// ======================================================================
	// 第一步：定义字段树（真实场景中由 Schema + AST 解析产生，此处用 API 直接注册）
	// ======================================================================

	// --- 列表字段 users（KindList）---
	usersDef := &plan.FieldDef{
		ResponseKey: "users",
		FieldName:   "users",
		TypeKind:    plan.KindList,
		NonNull:     false,
		// resolver：模拟从数据库查询用户列表，返回 []interface{}
		Resolver: func(source interface{}, args map[string]interface{}, ctx context.Context) (interface{}, error) {
			fmt.Println("[resolver] users: 查询用户列表")
			return []interface{}{
				map[string]interface{}{"id": "1", "name": "张三"},
				map[string]interface{}{"id": "2", "name": "李四"},
			}, nil
		},
		// 列表元素的子字段定义
		Children: []*plan.FieldDef{
			{
				ResponseKey: "id",
				FieldName:   "id",
				TypeKind:    plan.KindScalar,
				NonNull:     true,
				// resolver 的 source 是当前列表元素（单个用户 map）
				Resolver: func(source interface{}, args map[string]interface{}, ctx context.Context) (interface{}, error) {
					if m, ok := source.(map[string]interface{}); ok {
						return m["id"], nil
					}
					return nil, nil
				},
			},
			{
				ResponseKey: "name",
				FieldName:   "name",
				TypeKind:    plan.KindScalar,
				NonNull:     true,
				Resolver: func(source interface{}, args map[string]interface{}, ctx context.Context) (interface{}, error) {
					if m, ok := source.(map[string]interface{}); ok {
						return m["name"], nil
					}
					return nil, nil
				},
			},
			{
				// 嵌套对象字段 address（KindObject）
				ResponseKey: "address",
				FieldName:   "address",
				TypeKind:    plan.KindObject,
				NonNull:     false,
				// resolver 的 source 是当前用户 map，按 id 返回对应地址
				Resolver: func(source interface{}, args map[string]interface{}, ctx context.Context) (interface{}, error) {
					m, ok := source.(map[string]interface{})
					if !ok {
						return nil, nil
					}
					userID := m["id"]
					fmt.Printf("[resolver] address: 查询用户 %v 的地址\n", userID)
					addresses := map[string]map[string]interface{}{
						"1": {"city": "北京", "country": "中国"},
						"2": {"city": "上海", "country": "中国"},
					}
					if id, ok := userID.(string); ok {
						if addr, exists := addresses[id]; exists {
							return addr, nil
						}
					}
					return nil, nil
				},
				Children: []*plan.FieldDef{
					{
						ResponseKey: "city",
						FieldName:   "city",
						TypeKind:    plan.KindScalar,
						// resolver 的 source 是 address map
						Resolver: func(source interface{}, args map[string]interface{}, ctx context.Context) (interface{}, error) {
							if m, ok := source.(map[string]interface{}); ok {
								return m["city"], nil
							}
							return nil, nil
						},
					},
					{
						ResponseKey: "country",
						FieldName:   "country",
						TypeKind:    plan.KindScalar,
						Resolver: func(source interface{}, args map[string]interface{}, ctx context.Context) (interface{}, error) {
							if m, ok := source.(map[string]interface{}); ok {
								return m["country"], nil
							}
							return nil, nil
						},
					},
				},
			},
		},
	}

	// --- 根字段 version（KindScalar），与 users 同批次并发执行 ---
	versionDef := &plan.FieldDef{
		ResponseKey: "version",
		FieldName:   "version",
		TypeKind:    plan.KindScalar,
		NonNull:     true,
		Resolver: func(source interface{}, args map[string]interface{}, ctx context.Context) (interface{}, error) {
			fmt.Println("[resolver] version: 返回 API 版本号")
			return "v1.0.0", nil
		},
	}

	// ======================================================================
	// 第二步：Plan 层 —— 编译执行计划
	// ======================================================================

	planner := plan.NewPlanner()
	sel, err := planner.Build(plan.OperationQuery, []*plan.FieldDef{usersDef, versionDef})
	if err != nil {
		log.Fatalf("编译执行计划失败: %v", err)
	}

	fmt.Printf("\n[build] 共分配 %d 个 slot\n", sel.TotalSlots)
	fmt.Printf("[build] 根字段数量: %d\n\n", len(sel.RootFields))

	// ======================================================================
	// 第三步：Exec 层 —— 执行计划（内部自动分 batch 并发执行）
	// ======================================================================

	executor := exec.NewExecutor()
	result := executor.Execute(sel, nil, context.Background())

	// ======================================================================
	// 第四步：输出响应（response 层已在 Execute 内部完成组装）
	// ======================================================================

	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		log.Fatalf("JSON 序列化失败: %v", err)
	}
	fmt.Printf("\n[响应]\n%s\n", string(out))
}
