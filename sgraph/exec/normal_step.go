package exec

import (
	"context"
	"fmt"
	"strings"

	"github.com/graphql-go/graphql/sgraph/plan"
)

// NormalStep 处理标量字段和对象字段的执行。
//
// 执行流程：
//  1. 从 RunFrame 读取父结果（source）；
//  2. 调用 resolveArgs 组装 arguments；
//  3. 调用 Resolver 获取字段原始值；
//  4. 将结果写入 ResultSlot；
//  5. 若 TypeKind == KindObject，递归执行子字段（Children）。
type NormalStep struct {
	// fp 是该 Step 对应的字段计划（包含 slot、resolver、args 等所有编译期信息）
	fp *plan.FieldPlan
}

// NewNormalStep 创建一个 NormalStep
func NewNormalStep(fp *plan.FieldPlan) *NormalStep {
	return &NormalStep{fp: fp}
}

// FieldPlan 返回该 Step 对应的字段计划（实现 Step 接口）
func (s *NormalStep) FieldPlan() *plan.FieldPlan {
	return s.fp
}

// Execute 执行该字段的 resolve 逻辑并将结果写入 frame。
//
// 错误处理：resolver 返回错误时，错误写入 frame，slot 写入 nil，
// 子字段跳过（source 为 nil），同级字段继续执行（partial data 语义）。
func (s *NormalStep) Execute(frame *RunFrame, ctx context.Context) error {
	fp := s.fp

	// 1. 读取父字段结果作为 resolver 的 source 参数
	var source interface{}
	if fp.SourceSlot != plan.NoSlot {
		source = frame.ReadSlot(fp.SourceSlot)
	}

	// 2. 组装 arguments
	args := resolveArgs(fp.ArgPlans, frame)

	// 3. 调用 resolver 获取字段值
	val, err := fp.Resolver(source, args, ctx)
	if err != nil {
		// 将字段错误记录到 frame，允许 partial data 继续
		frame.AddError(fp.Path, err)
		// 写入 nil 确保 slot 已标记（子字段读到 nil source 会跳过）
		_ = frame.WriteSlot(fp.ResultSlot, nil)
		return nil
	}

	// 4. NonNull 检查：非空字段返回 nil 时记录错误
	if val == nil && fp.NonNull {
		frame.AddError(fp.Path, newNonNullError(fp.Path))
		_ = frame.WriteSlot(fp.ResultSlot, nil)
		return nil
	}

	// 5. 将 resolver 结果写入 slot
	if writeErr := frame.WriteSlot(fp.ResultSlot, val); writeErr != nil {
		// slot 重复写入是 planner bug，panic 暴露问题
		panic(writeErr)
	}

	// 6. KindObject：立即递归执行子字段
	// 子字段依赖当前 resultSlot，必须在当前 step 写入 slot 后才能执行
	if fp.TypeKind == plan.KindObject && len(fp.Children) > 0 {
		childSteps := buildSteps(fp.Children)
		for _, cs := range childSteps {
			if execErr := cs.Execute(frame, ctx); execErr != nil {
				return execErr
			}
		}
	}

	return nil
}

// newNonNullError 构造一个非空字段返回 null 的标准错误
func newNonNullError(path []string) error {
	return fmt.Errorf("non-null 字段 %q 返回了 null", strings.Join(path, "."))
}
