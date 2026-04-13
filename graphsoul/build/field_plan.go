package build

import "context"

type FieldType uint8

const FIELD_TYPE_SCALAR FieldType = 0
const FIELD_TYPE_OBJECT FieldType = 1
const FIELD_TYPE_ARRAY FieldType = 2

type ParamType uint8

const PARAM_TYPE_CONST ParamType = 0
const PARAM_TYPE_INPUT ParamType = 1
const PARAM_TYPE_FIELD_RESULT ParamType = 2

type ResolverFunc func(source any, params map[string]any, ctx context.Context) (any, error)

type ParamPlan struct {
	paramKey         string
	paramType        ParamType
	constValue       any
	inputName        string
	dependentFieldId uint32
	fieldResultPaths []string
}
type FieldPlan struct {
	fieldName         string
	responseName      string
	fieldType         FieldType
	paramPlans        []*ParamPlan
	arrParamPlans     []*ParamPlan
	resolverFunc      ResolverFunc
	arrayResolverFunc ResolverFunc
	fieldId           uint32
	parentFieldId     uint32
	parentFieldNotNil bool
	paths             []string
	childrenFields    []*FieldPlan
}

func (fp *FieldPlan) GetFieldId() uint32 {
	return fp.fieldId
}

func (fp *FieldPlan) GetParentFieldId() uint32 {
	return fp.parentFieldId
}

func (fp *FieldPlan) IsParentFieldNotNil() bool {
	return fp.parentFieldNotNil
}

func (fp *FieldPlan) GetPaths() []string {
	return fp.paths
}

func (fp *FieldPlan) GetParamPlans() []*ParamPlan {
	return fp.paramPlans
}

func (fp *FieldPlan) GetArrParamPlans() []*ParamPlan {
	return fp.arrParamPlans
}

func (fp *FieldPlan) GetResolverFunc() ResolverFunc {
	return fp.resolverFunc
}

func (fp *FieldPlan) GetArrayResolverFunc() ResolverFunc {
	return fp.arrayResolverFunc
}
