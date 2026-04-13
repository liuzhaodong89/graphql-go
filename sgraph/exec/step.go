package exec

import (
	"context"

	"github.com/graphql-go/graphql/sgraph/plan"
)

// Step 是执行层的基本执行单元接口。
// 每个 Step 对应一个字段的完整执行流程（resolve + complete）。
// 不同类型的字段使用不同的 Step 实现：
//   - 标量/对象字段 -> NormalStep
//   - 列表字段      -> IteratorStep
type Step interface {
	// Execute 执行该字段，将结果写入 frame；ctx 传递请求上下文。
	// 发生错误时将错误记录到 frame，允许 partial data 继续执行同级字段。
	Execute(frame *RunFrame, ctx context.Context) error

	// FieldPlan 返回该 Step 对应的字段计划（调度器读取依赖关系时使用）
	FieldPlan() *plan.FieldPlan
}

// resolveArgs 根据 ArgPlan 列表和运行帧组装 argument map，供 resolver 调用。
// 支持三种取值方式：常量、变量、从父字段 slot 提取。
func resolveArgs(argPlans []*plan.ArgPlan, frame *RunFrame) map[string]interface{} {
	if len(argPlans) == 0 {
		return map[string]interface{}{}
	}
	args := make(map[string]interface{}, len(argPlans))
	for _, ap := range argPlans {
		switch ap.Kind {
		case plan.ArgKindConst:
			// 直接使用编译期确定的常量值
			args[ap.Name] = ap.ConstValue

		case plan.ArgKindVariable:
			// 从请求变量表中按名称取值
			args[ap.Name] = frame.GetVariable(ap.VariableName)

		case plan.ArgKindFromSlot:
			// 从父字段的结果 slot 中提取子字段值作为参数
			val := frame.ReadSlot(ap.SourceSlot)
			args[ap.Name] = extractByPath(val, ap.FieldPath)
		}
	}
	return args
}

// extractByPath 从 map 类型的值中按字段路径逐层取值。
// path 为空时直接返回 val；中途类型不匹配则返回 nil。
func extractByPath(val interface{}, path []string) interface{} {
	for _, key := range path {
		m, ok := val.(map[string]interface{})
		if !ok {
			return nil
		}
		val = m[key]
	}
	return val
}

// buildSteps 将一组 FieldPlan 转换为对应的 Step 列表（不分层，直接转换）。
// 用于 NormalStep 内部处理对象子字段的递归执行。
func buildSteps(fields []*plan.FieldPlan) []Step {
	steps := make([]Step, 0, len(fields))
	for _, fp := range fields {
		if fp.TypeKind == plan.KindList {
			steps = append(steps, NewIteratorStep(fp))
		} else {
			steps = append(steps, NewNormalStep(fp))
		}
	}
	return steps
}
