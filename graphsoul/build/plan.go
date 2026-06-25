package build

import (
	"strconv"
	"strings"
)

type SGraphPlanOperationType uint8

const SGraphOperationTypeQuery SGraphPlanOperationType = 0
const SGraphOperationTypeMutation SGraphPlanOperationType = 1
const SGraphOperationTypeSubscription SGraphPlanOperationType = 2

type SGraphPlan struct {
	operationType  SGraphPlanOperationType
	originalInputs map[string]any
	roots          []*FieldPlan
}

func (s *SGraphPlan) GetOperationType() SGraphPlanOperationType {
	return s.operationType
}

func (s *SGraphPlan) GetRoots() []*FieldPlan {
	return s.roots
}

func (s *SGraphPlan) GetOriginalInputs() map[string]any {
	return s.originalInputs
}

func (s *SGraphPlan) GetCacheKey() string {
	var sb strings.Builder
	sb.WriteString(strconv.FormatUint(uint64(s.GetOperationType()), 10))
	sb.WriteString("_")
	for _, root := range s.roots {
		s.loopCreateCacheKey(root, &sb)
	}
	return sb.String()
}

func (s *SGraphPlan) loopCreateCacheKey(fp *FieldPlan, keyBuilder *strings.Builder) {
	if fp == nil {
		return
	}
	//keyBuilder.WriteString(strconv.FormatUint(uint64(fp.GetFieldId()), 10))
	keyBuilder.WriteString(fp.GetFieldName())
	keyBuilder.WriteString("-")
	keyBuilder.WriteString(fp.GetResponseName())
	keyBuilder.WriteString("-")
	keyBuilder.WriteString(strconv.FormatUint(uint64(fp.GetFieldId()), 10))
	keyBuilder.WriteString("-")
	keyBuilder.WriteString(strconv.FormatUint(uint64(fp.GetParentFieldId()), 10))
	keyBuilder.WriteString("-")
	keyBuilder.WriteString(strconv.FormatUint(uint64(fp.GetFieldType()), 10))
	keyBuilder.WriteString("-")
	keyBuilder.WriteString(strconv.FormatBool(fp.GetFieldValueMetaInfo().NotNil))
	keyBuilder.WriteString("-")
	keyBuilder.WriteString(strconv.FormatBool(fp.GetFieldValueMetaInfo().IsList))
	keyBuilder.WriteString("-")
	keyBuilder.WriteString(strconv.FormatBool(fp.GetResolverFunc() != nil))
	keyBuilder.WriteString("-")
	keyBuilder.WriteString(strconv.FormatBool(fp.GetArrayResolverFunc() != nil))
	keyBuilder.WriteString("-")
	if len(fp.GetDirectivePlans()) > 0 {
		for _, directivePlan := range fp.GetDirectivePlans() {
			keyBuilder.WriteString(directivePlan.Name)
			keyBuilder.WriteString(";")
		}
	}
	for _, child := range fp.GetChildrenFields() {
		s.loopCreateCacheKey(child, keyBuilder)
	}
}

// MaxFieldId 遍历整棵字段树，返回最大的 fieldId。
// 用于 NewRundata 预分配 slice 容量（下标 = fieldId）。
func (s *SGraphPlan) MaxFieldId() uint32 {
	var max uint32
	for _, root := range s.roots {
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

// NewSGraphPlan 构造一个顶层执行计划。
func NewSGraphPlan(roots []*FieldPlan, originalInputs map[string]any) *SGraphPlan {
	if originalInputs == nil {
		originalInputs = make(map[string]any)
	}
	return &SGraphPlan{
		roots:          roots,
		originalInputs: originalInputs,
	}
}
