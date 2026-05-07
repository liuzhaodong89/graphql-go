package build

import (
	"strconv"
	"strings"
)

type SGraphPlanOperationType uint8

const SGRAPH_OPERATION_TYPE_QUERY SGraphPlanOperationType = 0
const SGRAPH_OPERATION_TYPE_MUTATION SGraphPlanOperationType = 1
const SGRAPH_OPERATION_TYPE_SUBSCRIPTION SGraphPlanOperationType = 2

type SGraphPlan struct {
	operationType  SGraphPlanOperationType
	originalInputs map[string]any
	roots          []*FieldPlan
}

func (s *SGraphPlan) GetRoots() []*FieldPlan {
	return s.roots
}

func (s *SGraphPlan) GetOriginalInputs() map[string]any {
	return s.originalInputs
}

func (s *SGraphPlan) GetCacheKey() string {
	var sb strings.Builder
	for _, root := range s.roots {
		s.loopCreateCacheKey(root, &sb)
	}
	return sb.String()
}

func (s *SGraphPlan) loopCreateCacheKey(fp *FieldPlan, keyBuilder *strings.Builder) {
	if fp == nil {
		return
	}
	keyBuilder.WriteString(strconv.FormatUint(uint64(fp.GetFieldId()), 10))
	keyBuilder.WriteString("-")
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

// NewFieldPlan 通过 FieldPlanOptions 构造一个 FieldPlan。
func NewFieldPlan(opts FieldPlanOptions) *FieldPlan {
	fp := &FieldPlan{
		fieldId:         opts.FieldId,
		parentFieldId:   opts.ParentFieldId,
		fieldName:       opts.FieldName,
		responseName:    opts.ResponseName,
		fieldType:       opts.FieldType,
		fieldIsList:     opts.FieldIsList,
		fieldNotNil:     opts.FieldNotNil,
		fieldListNotNil: opts.FieldListNotNil,
		//parentFieldNotNil:        opts.ParentFieldNotNil,
		paths:                    opts.Paths,
		parentKeyFieldNames:      opts.ParentKeyFieldNames,
		arrayResultParentKeyName: opts.ArrayResultParentKeyName,
		resolverFunc:             opts.ResolverFunc,
		arrayResolverFunc:        opts.ArrayResolverFunc,
		paramPlans:               opts.ParamPlans,
		arrParamPlan:             opts.ArrParamPlan,
		childrenFields:           opts.ChildrenFields,
	}
	if fp.paramPlans == nil {
		fp.paramPlans = make([]*ParamPlan, 0)
	}
	if fp.childrenFields == nil {
		fp.childrenFields = make([]*FieldPlan, 0)
	}
	if fp.paths == nil {
		fp.paths = make([]string, 0)
	}
	return fp
}

// NewConstParamPlan 构造一个常量类型的 ParamPlan。
func NewConstParamPlan(paramKey string, constValue any) *ParamPlan {
	return &ParamPlan{
		paramKey:   paramKey,
		paramType:  PARAM_TYPE_CONST,
		constValue: constValue,
	}
}

// NewInputParamPlan 构造一个来自原始输入的 ParamPlan。
func NewInputParamPlan(paramKey, inputName string) *ParamPlan {
	return &ParamPlan{
		paramKey:  paramKey,
		paramType: PARAM_TYPE_INPUT,
		inputName: inputName,
	}
}

// NewFieldResultParamPlan 构造一个依赖上游字段结果的 ParamPlan。
func NewFieldResultParamPlan(paramKey string, dependentFieldId uint32, paths []string) *ParamPlan {
	return &ParamPlan{
		paramKey:         paramKey,
		paramType:        PARAM_TYPE_FIELD_RESULT,
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
