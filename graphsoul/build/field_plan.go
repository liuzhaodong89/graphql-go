package build

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type FieldType uint8

const FIELD_TYPE_SCALAR FieldType = 0
const FIELD_TYPE_OBJECT FieldType = 1

// const FIELD_TYPE_ARRAY FieldType = 2
const FIELD_TYPE_ENUM FieldType = 3

type ParamType uint8

const PARAM_TYPE_CONST ParamType = 0
const PARAM_TYPE_INPUT ParamType = 1
const PARAM_TYPE_FIELD_RESULT ParamType = 2

type ResolverFunc func(source any, params map[string]any, ctx context.Context) (any, error)

type ParamPlan struct {
	paramKey   string
	paramType  ParamType
	constValue any
	inputName  string
	//如果是root节点，依赖的field节点ID默认为0。root节点的fieldId从1开始
	dependentFieldId uint32
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

type FieldPlan struct {
	fieldName         string
	responseName      string
	fieldType         FieldType
	paramPlans        []*ParamPlan
	arrParamPlan      *ParamPlan
	resolverFunc      ResolverFunc
	arrayResolverFunc ResolverFunc
	//从1开始自增
	fieldId           uint32
	parentFieldId     uint32
	parentFieldNotNil bool
	// 单次/遍历调用时用于标识父节点关联关系的字段名列表，支持多字段组合 key
	parentKeyFieldNames []string
	//批量调用时返回值中代表父节点映射key的字段name，获得返回结果后要根据这个字段name获取value并作为父子映射map的key
	arrayResultParentKeyName string
	paths                    []string
	childrenFields           []*FieldPlan
	fieldNotNil              bool
	fieldIsList              bool
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

func (fp *FieldPlan) GetArrParamPlan() *ParamPlan {
	return fp.arrParamPlan
}

func (fp *FieldPlan) GetResolverFunc() ResolverFunc {
	return fp.resolverFunc
}

func (fp *FieldPlan) GetArrayResolverFunc() ResolverFunc {
	return fp.arrayResolverFunc
}

func (fp *FieldPlan) GetFieldType() FieldType {
	return fp.fieldType
}

func (fp *FieldPlan) GetChildrenFields() []*FieldPlan {
	return fp.childrenFields
}

func (fp *FieldPlan) GetResponseName() string {
	return fp.responseName
}

func (fp *FieldPlan) GetFieldIsList() bool {
	return fp.fieldIsList
}

func (fp *FieldPlan) GetFieldNotNil() bool {
	return fp.fieldNotNil
}

func (fp *FieldPlan) GetParentKeyFieldNames() []string {
	return fp.parentKeyFieldNames
}

func (fp *FieldPlan) GetArrayResultParentKeyName() string {
	return fp.arrayResultParentKeyName
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

// BuildCompositeKey 将 source 中多个字段的值拼接为复合 key，字段顺序决定唯一性。
// 例如 fieldNames=["orderId","itemId"], source={"orderId":1,"itemId":5} → "1:5"
func BuildCompositeKey(fieldNames []string, source map[string]any) string {
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
