package graphql

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/graphql-go/graphql/language/ast"
)

const DefaultFieldKeyTypename string = "typeName"
const ParentKeyFieldNameAsID string = "id"
const IntrospectionFieldNameTypename string = "__typename"
const IntrospectionFieldNameMetaType string = "__type"
const IntrospectionFieldNameMetaSchema string = "__schema"
const sGraphRundataPoolMaxFieldSlots = 8192

func toAnySlice(v any) []any {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}

	result := make([]any, 0)

	for i := 0; i < rv.Len(); i++ {
		result = append(result, rv.Index(i).Interface())
	}
	return result
}

func isSlice(v any) bool {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return false
	}
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	return rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array
}

func isNonNullInput(t Input) bool {
	_, ok := t.(*NonNull)
	return ok
}

// astContainsVariable 递归判断实参 AST 内是否出现变量引用。
func astContainsVariable(v ast.Value) bool {
	switch node := v.(type) {
	case *ast.Variable:
		return true
	case *ast.ListValue:
		for _, item := range node.Values {
			if astContainsVariable(item) {
				return true
			}
		}
	case *ast.ObjectValue:
		for _, f := range node.Fields {
			if f != nil && astContainsVariable(f.Value) {
				return true
			}
		}
	}
	return false
}

//func paramsContainsRuntimeTypeVariable(params []*ParamPlan) bool {
//	for _, p := range params {
//		if p.paramType == PARAM_TYPE_ENUM_INPUT || p.paramType == PARAM_TYPE_ENUM_VAR_TEMPLATE {
//			return true
//		}
//	}
//	return false
//}

func getASTResponseName(field *ast.Field) string {
	if field != nil && field.Alias != nil && field.Alias.Value != "" {
		return field.Alias.Value
	}
	if field != nil && field.Name != nil && field.Name.Value != "" {
		return field.Name.Value
	}
	return ""
}

func allowedFieldTypeHash(fieldTypeScope *FieldTypeScope) string {
	if fieldTypeScope == nil {
		return ""
	}
	names := make([]string, len(fieldTypeScope.allowedDynamicTypes))
	for name := range fieldTypeScope.allowedDynamicTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func splitSkipIncludeDirectives(plans []*DirectivePlan) ([]*DirectivePlan, []*DirectivePlan) {
	conditionalPlans := make([]*DirectivePlan, 0)
	otherPlans := make([]*DirectivePlan, 0, len(plans))
	for _, plan := range plans {
		if plan == nil {
			continue
		}
		if plan.stage == DIRECTIVE_STAGE_SHOULD_EXECUTE && (plan.name == "skip" || plan.name == "include") {
			conditionalPlans = append(conditionalPlans, plan)
			continue
		} else {
			otherPlans = append(otherPlans, plan)
		}
	}
	return conditionalPlans, otherPlans
}

func getFieldDefinition(parentType any, fieldName string) (*FieldDefinition, error) {
	if parentType == nil {
		return nil, errors.New("no type scope provided while building selection set")
	}
	switch t := parentType.(type) {
	case *Object:
		fieldDefinition := t.Fields()[fieldName]
		if fieldDefinition == nil {
			return nil, errors.New("no field definition found in Object for " + fieldName)
		}
		return fieldDefinition, nil
	case *Interface:
		fieldDefinition := t.Fields()[fieldName]
		if fieldDefinition == nil {
			return nil, errors.New("no field definition found in Interface for " + fieldName)
		}
		return fieldDefinition, nil
	case *Union:
		return nil, fmt.Errorf("no type definition found in Union for %s", fieldName)
	default:
		return nil, fmt.Errorf("no type definition found for %s", fieldName)
	}
}

func getBaseType(t Type) (Type, error) {
	for {
		switch tt := t.(type) {
		case *List:
			t = tt.OfType
		case *NonNull:
			t = tt.OfType
		default:
			return tt, nil
		}
	}
}

// 兼容graphql-go链路上，resolver能够读取原始fieldAST的特性
func fieldASTsForFieldPlanAsLegacy(current ast.Field, fieldBluePrints []FieldFlattenEntry) []*ast.Field {
	if len(fieldBluePrints) == 0 {
		return []*ast.Field{&current}
	}
	result := make([]*ast.Field, 0, len(fieldBluePrints))
	for i, _ := range fieldBluePrints {
		result = append(result, &fieldBluePrints[i].field)
	}
	return result
}

func getParentCompositeFromScope(scope *FieldTypeScope) Composite {
	if scope == nil {
		return nil
	}
	parentComposite, _ := scope.declaredType.(Composite)
	return parentComposite
}

// fieldErrorCoordinate 生成错误文案中的字段坐标，与 graphql-go 原生一致地采用
// "ParentType.fieldName" 形式（见 executor.go 的 completeValue / completeListValue /
// completeAbstractValue）。使用 schema 字段名而非 responseName，因此 alias 不影响坐标。
// parentType 缺失时退化为仅字段名，避免产生 ".field" 这种残缺坐标。
func fieldErrorCoordinate(parentType Composite, fieldName string) string {
	if isNilInterfaceValue(parentType) {
		return fieldName
	}
	return parentType.Name() + "." + fieldName
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

func introspectionKind(t Type) string {
	switch t.(type) {
	case *Scalar:
		return TypeKindScalar
	case *Object:
		return TypeKindObject
	case *Enum:
		return TypeKindEnum
	case *List:
		return TypeKindList
	case *NonNull:
		return TypeKindNonNull
	case *Interface:
		return TypeKindInterface
	case *Union:
		return TypeKindUnion
	case *InputObject:
		return TypeKindInputObject
	default:
		return ""
	}
}

func typeNameOrNil(t Type) any {
	switch tt := t.(type) {
	case *List:
		return nil
	case *NonNull:
		return nil
	default:
		if tt == nil {
			return nil
		}
		return tt.Name()
	}
}

func typeDescriptionOrNil(t Type) any {
	switch tt := t.(type) {
	case *List:
		return nil
	case *NonNull:
		return nil
	default:
		if tt == nil {
			return nil
		}
		description := tt.Description()
		if description == "" {
			return nil
		}
		return description
	}
}

func inputValueName(v any) string {
	switch tt := v.(type) {
	case *Argument:
		return tt.Name()
	case *InputObjectField:
		return tt.Name()
	default:
		return ""
	}
}

func inputValueDescription(v any) any {
	switch tt := v.(type) {
	case *Argument:
		if tt.Description() == "" {
			return nil
		}
		return tt.Description()
	case *InputObjectField:
		if tt.Description() == "" {
			return nil
		}
		return tt.Description()
	default:
		return nil
	}
}

func inputValueType(v any) Type {
	switch tt := v.(type) {
	case *Argument:
		return tt.Type
	case *InputObjectField:
		return tt.Type
	default:
		return nil
	}
}

func inputValueDefaultValue(v any) any {
	var defaultValue any
	var valueType Type

	switch tt := v.(type) {
	case *Argument:
		valueType = tt.Type
		defaultValue = tt.DefaultValue
	case *InputObjectField:
		valueType = tt.Type
		defaultValue = tt.DefaultValue
	default:
		return defaultValueLiteral(defaultValue, valueType)
	}

	if defaultValue == nil {
		return nil
	}
	return defaultValueLiteral(defaultValue, valueType)
}

// TODO要补list,input object,enum,null
func defaultValueLiteral(value any, t Type) any {
	if value == nil {
		return nil
	}

	switch v := value.(type) {
	case string:
		return strconv.Quote(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int, int32, int64, float32, float64:
		return fmt.Sprintf("%v", value)
	default:
		return fmt.Sprintf("%v", value)
	}
}

func argAsBool(field *FieldPlan, name string, inputs map[string]any, defaultValue bool) bool {
	v := argValue(field, name, inputs)
	if v == nil {
		return defaultValue
	}

	b, ok := v.(bool)
	if !ok {
		return defaultValue
	}
	return b
}

func argValue(field *FieldPlan, name string, inputs map[string]any) any {
	for _, p := range field.paramPlans {
		if p.paramKey != name {
			continue
		}
		// 内省参数可能来自变量；ParamPlan 必须在请求期用 inputs 物化，不能在 plan 里缓存参数值。
		v, _, _ := p.resolveFromInputs(inputs)
		return v
	}
	return nil
}

func insideWrappedType(t Type) Type {
	switch tt := t.(type) {
	case *List:
		return tt.OfType
	case *NonNull:
		return tt.OfType
	default:
		return nil
	}
}

// 判断是否为nil
func isNilInterfaceValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

// recoveredValueAsError 将任意 panic 值转换为 error，避免对字符串等非 error panic 做类型断言时再次 panic。
func recoveredValueAsError(recovered any) error {
	if recoveredErr, ok := recovered.(error); ok {
		return recoveredErr
	}
	return fmt.Errorf("%v", recovered)
}

// 将any类型的value转换成对应的类型切片并返回。
func asListValue(value any) ([]any, bool) {
	// nil、typed-nil slice 和 typed-nil pointer 都保持当前的 GraphQL null 语义。
	if isNilInterfaceValue(value) {
		return nil, true
	}

	// 常用类型保留无反射快路径，避免普通 List resolver 增加反射开销。
	switch items := value.(type) {
	case []any:
		return items, true
	case []string:
		return copySliceToAny(items), true
	case []int:
		return copySliceToAny(items), true
	case []int8:
		return copySliceToAny(items), true
	case []int16:
		return copySliceToAny(items), true
	case []int32:
		return copySliceToAny(items), true
	case []int64:
		return copySliceToAny(items), true
	case []uint:
		return copySliceToAny(items), true
	case []uint8:
		return copySliceToAny(items), true
	case []uint16:
		return copySliceToAny(items), true
	case []uint32:
		return copySliceToAny(items), true
	case []uint64:
		return copySliceToAny(items), true
	case []float32:
		return copySliceToAny(items), true
	case []float64:
		return copySliceToAny(items), true
	case []bool:
		return copySliceToAny(items), true
	case []map[string]any:
		return copySliceToAny(items), true
	}

	rv := reflect.ValueOf(value)

	// 与 graphql-go 原生 completeListValue 保持一致：
	// resolver 返回 *[]T 或 *[N]T 时解引用一层，再按 List 结果处理。
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
		if !rv.IsValid() {
			return nil, true
		}
	}

	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		result := make([]any, rv.Len())
		for index := 0; index < rv.Len(); index++ {
			result[index] = rv.Index(index).Interface()
		}
		return result, true
	default:
		return nil, false
	}
}

// 将切片拷贝一份返回
func copySliceToAny[T any](items []T) []any {
	if items == nil {
		return nil
	}
	result := make([]any, len(items))
	for i := range items {
		result[i] = items[i]
	}
	return result
}

// generateCompositeKey 将 source 中多个字段的值拼接为复合 key，字段顺序决定唯一性。
// 例如 fieldNames=["orderId","itemId"], source={"orderId":1,"itemId":5} → "1:5"
func generateCompositeKey(fieldNames []string, source map[string]any) string {
	parts := make([]string, 0, len(fieldNames))
	for _, name := range fieldNames {
		parts = append(parts, valueToString(source[name]))
	}
	return strings.Join(parts, ":")
}

// responsePathBindingKey将请求级动态响应路径编码成稳定的父结果绑定key。
// 字段名携带长度且List下标使用独立类型标记，避免不同路径产生相同编码。
func responsePathBindingKey(path *ResponsePath) (string, error) {
	if path == nil {
		return "", errors.New("response path is nil")
	}

	depth := 0
	for current := path; current != nil; current = current.Prev {
		depth++
	}

	keys := make([]any, depth)
	index := depth - 1
	for current := path; current != nil; current = current.Prev {
		keys[index] = current.Key
		index--
	}

	var builder strings.Builder
	builder.Grow(depth * 8)
	builder.WriteString("path|")
	for _, key := range keys {
		switch value := key.(type) {
		case string:
			builder.WriteByte('s')
			builder.WriteString(strconv.Itoa(len(value)))
			builder.WriteByte(':')
			builder.WriteString(value)
			builder.WriteByte('|')
		case int:
			builder.WriteByte('i')
			builder.WriteString(strconv.Itoa(value))
			builder.WriteByte('|')
		default:
			return "", fmt.Errorf("unsupported response path key type %T", key)
		}
	}
	return builder.String(), nil
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

func paramsRequireRuntimeEvaluation(params []*ParamPlan) bool {
	for _, p := range params {
		if p == nil {
			continue
		}

		if p.paramType == PARAM_TYPE_ENUM_INPUT || p.paramType == PARAM_TYPE_ENUM_VAR_TEMPLATE || p.paramType == PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE {
			return true
		}
	}
	return false
}

func paramsContainsFieldResponse(params []*ParamPlan) bool {
	for _, p := range params {
		if p != nil && p.paramType == PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE {
			return true
		}
	}
	return false
}

func appendResponsePath(parent []string, responseName string) []string {
	result := make([]string, len(parent)+1)
	copy(result, parent)
	result[len(parent)] = responseName
	return result
}
