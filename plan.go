package graphql

import (
	"sync"

	"github.com/graphql-go/graphql/language/ast"
)

type SGraphPlanOperationType uint8

const SGraphOperationTypeQuery SGraphPlanOperationType = 0
const SGraphOperationTypeMutation SGraphPlanOperationType = 1
const SGraphOperationTypeSubscription SGraphPlanOperationType = 2

type SGraphPlan struct {
	operationType SGraphPlanOperationType
	roots         []*FieldPlan
	operation     ast.Definition            //请求无关的 AST 元数据，运行期填充 ResolveInfo.Operation
	fragments     map[string]ast.Definition //请求无关的 fragment 定义，运行期填充 ResolveInfo.Fragments
	maxFieldId    uint32
	batchesOnce   sync.Once
	batches       []*Batch
}

func (s *SGraphPlan) GetOperationType() SGraphPlanOperationType {
	return s.operationType
}

func (s *SGraphPlan) GetRoots() []*FieldPlan {
	return s.roots
}

// MaxFieldId 返回 build 阶段缓存的最大 fieldId。
// 用于 NewRundata 预分配 slice 容量（下标 = fieldId）。
func (s *SGraphPlan) MaxFieldId() uint32 {
	if s == nil {
		return 0
	}
	if s.maxFieldId != 0 || len(s.roots) == 0 {
		return s.maxFieldId
	}
	// 兼容外部手动构造 SGraphPlan 但没有走 NewSGraphPlan 的场景。
	return calculateMaxFieldId(s.roots)
}

func calculateMaxFieldId(roots []*FieldPlan) uint32 {
	var max uint32
	for _, root := range roots {
		walkMaxFieldId(root, &max)
	}
	return max
}

func walkMaxFieldId(fp *FieldPlan, max *uint32) {
	if fp == nil {
		return
	}
	if fp.fieldId > *max {
		*max = fp.fieldId
	}
	for _, child := range fp.childrenFields {
		walkMaxFieldId(child, max)
	}
}

// NewConstParamPlan 构造一个常量类型的 ParamPlan。
func NewConstParamPlan(paramKey string, constValue any) *ParamPlan {
	return &ParamPlan{
		paramKey:   paramKey,
		paramType:  ParamTypeConst,
		constValue: constValue,
	}
}

// NewInputParamPlan 构造一个来自原始输入的 ParamPlan。
func NewInputParamPlan(paramKey, inputName string) *ParamPlan {
	return &ParamPlan{
		paramKey:  paramKey,
		paramType: ParamTypeInput,
		inputName: inputName,
	}
}

// NewFieldResultParamPlan 构造一个依赖上游字段结果的 ParamPlan。
func NewFieldResultParamPlan(paramKey string, dependentFieldId uint32, paths []string) *ParamPlan {
	return &ParamPlan{
		paramKey:         paramKey,
		paramType:        ParamTypeFieldResult,
		dependentFieldId: dependentFieldId,
		fieldResultPaths: paths,
	}
}

// NewSGraphPlan 构造一个顶层执行计划。计划只保存请求无关的结构信息。
func NewSGraphPlan(roots []*FieldPlan) *SGraphPlan {
	return &SGraphPlan{
		roots:      roots,
		maxFieldId: calculateMaxFieldId(roots),
	}
}
