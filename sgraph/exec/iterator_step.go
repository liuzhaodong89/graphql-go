package exec

import (
	"context"
	"fmt"

	"github.com/graphql-go/graphql/sgraph/plan"
)

// IteratorStep 处理列表字段（KindList）的执行。
//
// 执行流程：
//  1. 执行列表字段本身的 resolver，获取 []interface{} 类型的原始列表；
//  2. 将列表写入 ResultSlot（供后续层读取，若有依赖的话）；
//  3. 遍历列表每个元素：
//     a. 创建独立子帧（元素间结果隔离）；
//     b. 将元素写入 ListItemSlot 作为子字段的 source；
//     c. 递归执行 ListItemChildren（该元素的所有子字段）；
//     d. 从子帧组装该元素的响应对象；
//  4. 将组装好的完整列表写入 ResultSlot（覆盖步骤 2 中写入的原始列表）。
type IteratorStep struct {
	// fp 是该列表字段对应的字段计划
	fp *plan.FieldPlan
}

// NewIteratorStep 创建一个 IteratorStep
func NewIteratorStep(fp *plan.FieldPlan) *IteratorStep {
	return &IteratorStep{fp: fp}
}

// FieldPlan 返回字段计划（实现 Step 接口）
func (s *IteratorStep) FieldPlan() *plan.FieldPlan {
	return s.fp
}

// Execute 执行列表字段，对每个元素执行子字段后，将组装好的列表写入 frame。
func (s *IteratorStep) Execute(frame *RunFrame, ctx context.Context) error {
	fp := s.fp

	// 1. 读取父字段结果作为 source
	var source interface{}
	if fp.SourceSlot != plan.NoSlot {
		source = frame.ReadSlot(fp.SourceSlot)
	}

	// 2. 组装 arguments
	args := resolveArgs(fp.ArgPlans, frame)

	// 3. 调用列表字段 resolver，期望返回 []interface{}
	raw, err := fp.Resolver(source, args, ctx)
	if err != nil {
		frame.AddError(fp.Path, err)
		_ = frame.WriteSlot(fp.ResultSlot, nil)
		return nil
	}

	if raw == nil {
		if fp.NonNull {
			frame.AddError(fp.Path, newNonNullError(fp.Path))
		}
		_ = frame.WriteSlot(fp.ResultSlot, nil)
		return nil
	}

	// 类型断言：resolver 须返回 []interface{}
	items, ok := raw.([]interface{})
	if !ok {
		ferr := fmt.Errorf("列表字段 %q 的 resolver 须返回 []interface{}，实际 %T", fp.FieldName, raw)
		frame.AddError(fp.Path, ferr)
		_ = frame.WriteSlot(fp.ResultSlot, nil)
		return nil
	}

	// 4. 遍历列表元素，逐一创建子帧并执行子字段
	resultList := make([]interface{}, 0, len(items))
	for idx, item := range items {
		// 为每个元素创建独立子帧（元素间结果互不干扰）
		itemFrame := NewRunFrame(frame.totalSlots(), frame.variables)
		// 将父帧中已写入的 slot 复制到子帧（保留上层上下文）
		frame.copyTo(itemFrame)

		// 将当前元素写入 ListItemSlot，作为子字段的 source
		if writeErr := itemFrame.WriteSlot(fp.ListItemSlot, item); writeErr != nil {
			panic(fmt.Sprintf("IteratorStep: 写入 ListItemSlot 失败: %v", writeErr))
		}

		// 执行该元素的所有子字段
		childSteps := buildSteps(fp.ListItemChildren)
		for _, cs := range childSteps {
			if execErr := cs.Execute(itemFrame, ctx); execErr != nil {
				return execErr
			}
		}

		// 将子帧错误合并到主帧（在 path 中插入列表下标）
		for _, fe := range itemFrame.Errors() {
			pathWithIdx := insertIndex(fe.Path, fp.Path, idx)
			frame.AddError(pathWithIdx, fe.Err)
		}

		// 从子帧读取所有子字段结果，组装成该元素的响应对象
		itemObj := collectObjectResult(fp.ListItemChildren, itemFrame)
		resultList = append(resultList, itemObj)
	}

	// 5. 将最终列表写入 ResultSlot
	if writeErr := frame.WriteSlot(fp.ResultSlot, resultList); writeErr != nil {
		panic(writeErr)
	}

	return nil
}

// insertIndex 在 errPath 中，于 basePath 后插入列表下标字符串。
// 例如 basePath=["users"], errPath=["users","name"], idx=1
// 结果为 ["users","1","name"]
func insertIndex(errPath []string, basePath []string, idx int) []string {
	baseLen := len(basePath)
	result := make([]string, 0, len(errPath)+1)
	result = append(result, errPath[:baseLen]...)
	result = append(result, fmt.Sprintf("%d", idx))
	if len(errPath) > baseLen {
		result = append(result, errPath[baseLen:]...)
	}
	return result
}

// collectObjectResult 从 frame 中读取一组子字段计划的结果，组装成 map。
// 用于组装列表元素对象和对象类型字段的响应数据。
func collectObjectResult(fields []*plan.FieldPlan, frame *RunFrame) map[string]interface{} {
	obj := make(map[string]interface{}, len(fields))
	for _, fp := range fields {
		val := frame.ReadSlot(fp.ResultSlot)
		if fp.TypeKind == plan.KindObject && len(fp.Children) > 0 {
			// 对象子字段：递归从子帧组装
			obj[fp.ResponseKey] = collectObjectResult(fp.Children, frame)
		} else {
			obj[fp.ResponseKey] = val
		}
	}
	return obj
}
