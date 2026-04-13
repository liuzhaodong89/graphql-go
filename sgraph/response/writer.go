package response

import (
	"fmt"

	"github.com/graphql-go/graphql/sgraph/plan"
)

// Build 从 SelectionPlan + slot 数组 + 错误列表组装最终的 GraphQL 响应。
//
//   - sel:    已执行的 SelectionPlan（包含字段树结构和 slot 映射关系）
//   - slots:  RunFrame 中所有 slot 的值数组（索引即 SlotID）
//   - errors: 执行过程中收集的字段错误列表
func Build(sel *plan.SelectionPlan, slots []interface{}, errors []FieldErrorInfo) *Result {
	// 从根字段计划读取 slot 结果，递归组装 data 对象
	data := buildObject(sel.RootFields, slots)

	// 将字段错误列表转换为 GraphQL errors 格式
	var gqlErrors []GraphQLError
	for _, fe := range errors {
		gqlErrors = append(gqlErrors, toGraphQLError(fe))
	}

	return &Result{
		Data:   data,
		Errors: gqlErrors,
	}
}

// buildObject 从 slot 数组中读取一组字段计划的结果，组装成 map[string]interface{}。
// 递归处理 KindObject 嵌套（KindList 由 IteratorStep 已组装完毕，直接取 slot 值）。
func buildObject(fields []*plan.FieldPlan, slots []interface{}) map[string]interface{} {
	obj := make(map[string]interface{}, len(fields))
	for _, fp := range fields {
		obj[fp.ResponseKey] = buildValue(fp, slots)
	}
	return obj
}

// buildValue 从 slot 读取单个字段的值，按类型完成（complete）响应数据。
func buildValue(fp *plan.FieldPlan, slots []interface{}) interface{} {
	val := readSlot(slots, fp.ResultSlot)

	switch fp.TypeKind {
	case plan.KindScalar, plan.KindEnum:
		// 标量和枚举：直接返回，JSON 编码器负责序列化
		return val

	case plan.KindObject:
		// 对象类型：递归组装子字段（子字段结果已在各自 slot 中）
		if val == nil {
			return nil
		}
		if len(fp.Children) > 0 {
			return buildObject(fp.Children, slots)
		}
		return val

	case plan.KindList:
		// 列表类型：IteratorStep 已将完整列表写入 slot，直接返回
		return val
	}

	return val
}

// readSlot 安全地从 slot 数组中按 SlotID 读取值，越界返回 nil
func readSlot(slots []interface{}, id plan.SlotID) interface{} {
	idx := int(id)
	if idx < 0 || idx >= len(slots) {
		return nil
	}
	return slots[idx]
}

// toGraphQLError 将 FieldErrorInfo 转换为标准 GraphQLError 格式。
// path 中纯数字字符串会转换为 int（对应列表下标），符合 GraphQL 规范。
func toGraphQLError(fe FieldErrorInfo) GraphQLError {
	path := make([]interface{}, len(fe.Path))
	for i, p := range fe.Path {
		var idx int
		// 尝试解析为列表下标整数，失败则保留字符串
		if _, err := fmt.Sscanf(p, "%d", &idx); err == nil {
			path[i] = idx
		} else {
			path[i] = p
		}
	}
	return GraphQLError{
		Message: fe.Message,
		Path:    path,
	}
}
