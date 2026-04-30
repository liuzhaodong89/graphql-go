package build

// FieldPlanOptions 用于构造 FieldPlan 的所有可选参数。
type FieldPlanOptions struct {
	FieldId                  uint32
	ParentFieldId            uint32
	FieldName                string
	ResponseName             string
	FieldType                FieldType
	FieldIsList              bool
	FieldNotNil              bool
	FieldListNotNil          bool
	ParentFieldNotNil        bool
	Paths                    []string
	ParentKeyFieldNames      []string
	ArrayResultParentKeyName string
	ResolverFunc             ResolverFunc
	ArrayResolverFunc        ResolverFunc
	ParamPlans               []*ParamPlan
	ArrParamPlan             *ParamPlan
	ChildrenFields           []*FieldPlan
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
