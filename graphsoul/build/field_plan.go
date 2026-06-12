package build

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/graphql-go/graphql"
)

type ParamType uint8

const ParamTypeConst ParamType = 0
const ParamTypeInput ParamType = 1
const ParamTypeFieldResult ParamType = 2
const ParamTypeFieldFullResult ParamType = 3

type ResolverFunc func(source any, params map[string]any, ctx context.Context) (any, error)

type ParamPlan struct {
	paramKey         string
	paramType        ParamType
	constValue       any
	inputName        string
	dependentFieldId uint32 //如果是root节点，依赖的field节点ID默认为0。root节点的fieldId从1开始
	fieldResultPaths []string
}

func (pp *ParamPlan) GetParamKey() string {
	return pp.paramKey
}

func (pp *ParamPlan) GetParamType() ParamType {
	return pp.paramType
}

func (pp *ParamPlan) GetDependentFieldId() uint32 {
	return pp.dependentFieldId
}

func (pp *ParamPlan) GetInputName() string {
	return pp.inputName
}

func (pp *ParamPlan) GetFieldResultPaths() []string {
	return pp.fieldResultPaths
}

func (pp *ParamPlan) GetConstValue() any {
	return pp.constValue
}

type DirectiveCompileResult struct {
	IncludeDecision      *bool
	RuntimePlans         []*DirectivePlan
	DependencyParamPlans []*ParamPlan
}

type DirectiveCompiler interface {
	Compile(Name string, Location string, Args map[string]any, Variables map[string]any, schema *graphql.Schema) (*DirectiveCompileResult, error)
}

type SkipDirectiveCompiler struct{}

func (SkipDirectiveCompiler) Compile(name string, location string, args map[string]any, variables map[string]any, schema *graphql.Schema) (*DirectiveCompileResult, error) {
	skipIf, _ := args["if"].(bool)

	if skipIf {
		include := false
		return &DirectiveCompileResult{IncludeDecision: &include}, nil
	}

	return &DirectiveCompileResult{}, nil
}

type IncludeDirectiveCompiler struct{}

func (IncludeDirectiveCompiler) Compile(name string, location string, args map[string]any, variables map[string]any, schema *graphql.Schema) (*DirectiveCompileResult, error) {
	includeIf, _ := args["if"].(bool)

	if !includeIf {
		include := false
		return &DirectiveCompileResult{IncludeDecision: &include}, nil
	}

	return &DirectiveCompileResult{}, nil
}

type DirectiveRuntimeHandler interface {
	ShouldExecute(fieldPlan *FieldPlan, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (bool, error)
	BeforeResolve(fieldPlan *FieldPlan, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (map[string]any, error)
	AfterResolve(fieldPlan *FieldPlan, params map[string]any, parentResponse any, currentResponse any, ctx context.Context) (any, error)
}

type DefaultEmptyDirectiveRuntimeHandler struct {
}

func (DefaultEmptyDirectiveRuntimeHandler) ShouldExecute(fieldPlan *FieldPlan, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (bool, error) {
	return true, nil
}

func (DefaultEmptyDirectiveRuntimeHandler) BeforeResolve(fieldPlan *FieldPlan, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (map[string]any, error) {
	return nil, nil
}

func (DefaultEmptyDirectiveRuntimeHandler) AfterResolve(fieldPlan *FieldPlan, params map[string]any, parentResponse any, currentResponse any, ctx context.Context) (any, error) {
	return nil, nil
}

type DirectiveStage uint8

const DirectiveStageMetadataOnly DirectiveStage = 1
const DirectiveStageShouldExecute DirectiveStage = 2
const DirectiveStageBeforeResolve DirectiveStage = 3
const DirectiveStageAfterResolve DirectiveStage = 4

type DirectivePlan struct {
	Name           string
	Location       string
	Args           map[string]any
	Stage          DirectiveStage
	Metadata       map[string]any
	RuntimeHandler DirectiveRuntimeHandler
}

type FieldValueType uint8

const FieldValueTypeScalar FieldValueType = 0
const FieldValueTypeObject FieldValueType = 1
const FieldValueTypeEnum FieldValueType = 2
const FieldValueTypeList FieldValueType = 3

// 字段返回值的封装类型元信息结构体
type FieldValueMetaInfo struct {
	NotNil       bool           //是否允许为null
	IsList       bool           //是否为List类型
	ValueType    FieldValueType //字段基础类型
	OriginalType graphql.Type
	ElementType  *FieldValueMetaInfo //如果是List类型，里面元素的封装类型元信息
}

func (fm *FieldValueMetaInfo) GetBaseElementOriginalType() graphql.Type {
	if fm.ElementType == nil {
		return fm.OriginalType
	}
	return fm.ElementType.GetBaseElementOriginalType()
}

type FieldPlan struct {
	fieldId      uint32 //字段自增ID，从1开始
	fieldName    string //schema中的字段名称
	responseName string //组装最后结果时的字段名称，如果有alias就使用alias
	//fieldNamedType           FieldNamedType //基础类型，Object/Scalar/Enum
	paramPlans              []*ParamPlan //单次参数计划List，NormalStep和IteratorStep中遍历模式执行时使用
	arrParamPlans           []*ParamPlan //批量参数计划，IteratorStep中批量模式执行时使用
	resolverFunc            ResolverFunc //单次执行Resolver方法，NormalStep和IteratorStep中遍历模式执行时使用
	arrayResolverFunc       ResolverFunc //批量执行Resolver方法，IteratorStep中批量模式执行时使用
	parentFieldId           uint32       //父字段ID，root节点该字段为0
	parentKeyFieldName      string       //单次/遍历调用时用于标识父节点关联关系的字段名，默认是父节点中的id
	resultParentKeyName     string       //批量调用时返回值中代表父节点映射key的字段name，获得返回结果后要根据这个字段name获取value并作为父子映射map的key
	paths                   []string
	childrenFields          []*FieldPlan
	allowedRuntimeTypeNames map[string]bool
	runtimeTypeResolverFunc DynamicTypeResolverFunction
	compiledTypeName        string
	fieldValueMetaInfo      FieldValueMetaInfo //字段返回值的封装类型元信息
	directivePlans          []*DirectivePlan
	directiveParamPlans     []*ParamPlan
}

func (fp *FieldPlan) GetFieldId() uint32 {
	return fp.fieldId
}

func (fp *FieldPlan) GetParentFieldId() uint32 {
	return fp.parentFieldId
}

func (fp *FieldPlan) GetFieldName() string {
	return fp.fieldName
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

func (fp *FieldPlan) GetFieldType() FieldValueType {
	return fp.fieldValueMetaInfo.ValueType
}

func (fp *FieldPlan) GetChildrenFields() []*FieldPlan {
	return fp.childrenFields
}

func (fp *FieldPlan) GetResponseName() string {
	return fp.responseName
}

func (fp *FieldPlan) GetFieldValueMetaInfo() FieldValueMetaInfo {
	return fp.fieldValueMetaInfo
}

func (fp *FieldPlan) GetParentKeyFieldName() string {
	return fp.parentKeyFieldName
}

func (fp *FieldPlan) GetResultParentKeyFieldName() string {
	return fp.resultParentKeyName
}

func (fp *FieldPlan) GetAllowedRuntimeTypeNames() map[string]bool {
	return fp.allowedRuntimeTypeNames
}

func (fp *FieldPlan) GetRuntimeTypeResolverFunc() DynamicTypeResolverFunction {
	return fp.runtimeTypeResolverFunc
}

func (fp *FieldPlan) GetCompiledTypeName() string {
	return fp.compiledTypeName
}

func (fp *FieldPlan) GetDirectivePlans() []*DirectivePlan {
	return fp.directivePlans
}

func (fp *FieldPlan) GetDirectiveParamPlans() []*ParamPlan {
	return fp.directiveParamPlans
}

func (fp *FieldPlan) IsIntrospectionTypeNameField() bool {
	return fp.fieldName == IntrospectionFieldNameTypename
}

// GetValueByPath 按 paths 路径从嵌套数据中逐层取值。
// 例如 paths=["order","user","name"] 等价于 data["order"]["user"]["name"]。
// 中途任一层不是 map[string]any 则返回 nil。
func GetValueByPath(data any, paths []string) any {
	current := data
	for _, key := range paths {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}

// GenerateCompositeKey 将 source 中多个字段的值拼接为复合 key，字段顺序决定唯一性。
// 例如 fieldNames=["orderId","itemId"], source={"orderId":1,"itemId":5} → "1:5"
func GenerateCompositeKey(fieldNames []string, source map[string]any) string {
	parts := make([]string, 0, len(fieldNames))
	for _, name := range fieldNames {
		parts = append(parts, valueToString(source[name]))
	}
	return strings.Join(parts, ":")
}

func valueToString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%v", value)
	}
}
