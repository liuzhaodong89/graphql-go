package sgraph

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
)

const DefaultFieldKeyTypename string = "typeName"
const ParentKeyFieldNameAsID string = "id"
const IntrospectionFieldNameTypename string = "__typename"
const IntrospectionFieldNameMetaType string = "__type"
const IntrospectionFieldNameMetaSchema string = "__schema"
const sGraphRundataPoolMaxFieldSlots = 8192

// 小batch直接串行执行，避免goroutine调度成本超过并发收益。
const sGraphConcurrentStepMin = 8

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

func isNonNullInput(t graphql.Input) bool {
	_, ok := t.(*graphql.NonNull)
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

func valueFromAST(valueAST ast.Value, inputType graphql.Input, originalInputs map[string]any) (any, error) {
	if valueAST == nil {
		return nil, fmt.Errorf("value is nil")
	}

	//变量引用：值已在 parseOperationVariables 按声明类型协变，直接取用，不烤、缓存安全
	if variable, ok := valueAST.(*ast.Variable); ok {
		if variable.Name == nil {
			return nil, fmt.Errorf("variable name is nil")
		}
		val, provided := originalInputs[variable.Name.Value]
		if !provided || val == nil {
			if _, isNonNull := inputType.(*graphql.NonNull); isNonNull {
				return nil, fmt.Errorf("variable %q is required for a non-null input", variable.Name.Value)
			}
			return nil, nil
		}
		return val, nil
	}

	switch t := inputType.(type) {
	case *graphql.NonNull:
		value, err := valueFromAST(valueAST, t.OfType.(graphql.Input), originalInputs)
		if err != nil {
			return nil, err
		}
		if value == nil {
			return nil, fmt.Errorf("value is nil")
		}
		return value, nil
	case *graphql.List:
		if listAST, ok := valueAST.(*ast.ListValue); ok {
			values := make([]any, 0)

			for _, value := range listAST.Values {
				item, err := valueFromAST(value, t.OfType, originalInputs)
				if err != nil {
					return nil, err
				}
				values = append(values, item)
			}
			return values, nil
		}

		single, err := valueFromAST(valueAST, t.OfType.(graphql.Input), originalInputs)
		if err != nil {
			return nil, err
		}

		return []any{single}, nil
	case *graphql.InputObject:
		objectAST, ok := valueAST.(*ast.ObjectValue)
		if !ok {
			return nil, fmt.Errorf("value is not an object")
		}

		astFields := map[string]*ast.ObjectField{}
		for _, field := range objectAST.Fields {
			astFields[field.Name.Value] = field
		}

		fieldDefs := t.Fields()
		result := map[string]any{}

		for astFieldName := range astFields {
			if _, ok := fieldDefs[astFieldName]; !ok {
				return nil, fmt.Errorf("unknown field %s", astFieldName)
			}
		}

		for fieldName, fieldDef := range fieldDefs {
			fieldAST, provided := astFields[fieldName]

			if !provided {
				if fieldDef.DefaultValue != nil {
					result[fieldName] = fieldDef.DefaultValue
					continue
				}

				if isNonNullInput(fieldDef.Type) {
					return nil, fmt.Errorf("field %s is not a non-nullable field", fieldName)
				}

				continue
			}

			//修正：原先误传 fieldAST(*ast.ObjectField)，应传其内部值 fieldAST.Value
			fieldValue, fieldValueErr := valueFromAST(fieldAST.Value, fieldDef.Type, originalInputs)
			if fieldValueErr != nil {
				return nil, fieldValueErr
			}
			result[fieldName] = fieldValue
		}
		return result, nil
	case *graphql.Scalar:
		parsed := t.ParseLiteral(valueAST)
		if parsed == nil {
			return nil, fmt.Errorf("value is nil")
		}
		return parsed, nil
	case *graphql.Enum:
		parsed := t.ParseLiteral(valueAST)
		if parsed == nil {
			return nil, fmt.Errorf("value is nil")
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("unknown type %T", t)
	}
}

func paramsContainsRuntimeTypeVariable(params []*ParamPlan) bool {
	for _, p := range params {
		if p.paramType == PARAM_TYPE_ENUM_INPUT || p.paramType == PARAM_TYPE_ENUM_VAR_TEMPLATE {
			return true
		}
	}
	return false
}

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

func getFieldDefinition(parentType any, fieldName string) (*graphql.FieldDefinition, error) {
	if parentType == nil {
		return nil, errors.New("no type scope provided while building selection set")
	}
	switch t := parentType.(type) {
	case *graphql.Object:
		fieldDefinition := t.Fields()[fieldName]
		if fieldDefinition == nil {
			return nil, errors.New("no field definition found in Object for " + fieldName)
		}
		return fieldDefinition, nil
	case *graphql.Interface:
		fieldDefinition := t.Fields()[fieldName]
		if fieldDefinition == nil {
			return nil, errors.New("no field definition found in Interface for " + fieldName)
		}
		return fieldDefinition, nil
	case *graphql.Union:
		return nil, fmt.Errorf("no type definition found in Union for %s", fieldName)
	default:
		return nil, fmt.Errorf("no type definition found for %s", fieldName)
	}
	return nil, nil
}

func getBaseType(t graphql.Type) (graphql.Type, error) {
	for {
		switch tt := t.(type) {
		case *graphql.List:
			t = tt.OfType
		case *graphql.NonNull:
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

func getParentCompositeFromScope(scope *FieldTypeScope) graphql.Composite {
	if scope == nil {
		return nil
	}
	parentComposite, _ := scope.declaredType.(graphql.Composite)
	return parentComposite
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

func introspectionKind(t graphql.Type) string {
	switch t.(type) {
	case *graphql.Scalar:
		return graphql.TypeKindScalar
	case *graphql.Object:
		return graphql.TypeKindObject
	case *graphql.Enum:
		return graphql.TypeKindEnum
	case *graphql.List:
		return graphql.TypeKindList
	case *graphql.NonNull:
		return graphql.TypeKindNonNull
	case *graphql.Interface:
		return graphql.TypeKindInterface
	case *graphql.Union:
		return graphql.TypeKindUnion
	case *graphql.InputObject:
		return graphql.TypeKindInputObject
	default:
		return ""
	}
}

func typeNameOrNil(t graphql.Type) any {
	switch tt := t.(type) {
	case *graphql.List:
		return nil
	case *graphql.NonNull:
		return nil
	default:
		if tt == nil {
			return nil
		}
		return tt.Name()
	}
}

func typeDescriptionOrNil(t graphql.Type) any {
	switch tt := t.(type) {
	case *graphql.List:
		return nil
	case *graphql.NonNull:
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
	case *graphql.Argument:
		return tt.Name()
	case *graphql.InputObjectField:
		return tt.Name()
	default:
		return ""
	}
}

func inputValueDescription(v any) any {
	switch tt := v.(type) {
	case *graphql.Argument:
		if tt.Description() == "" {
			return nil
		}
		return tt.Description()
	case *graphql.InputObjectField:
		if tt.Description() == "" {
			return nil
		}
		return tt.Description()
	default:
		return nil
	}
}

func inputValueType(v any) graphql.Type {
	switch tt := v.(type) {
	case *graphql.Argument:
		return tt.Type
	case *graphql.InputObjectField:
		return tt.Type
	default:
		return nil
	}
}

func inputValueDefaultValue(v any) any {
	var defaultValue any
	var valueType graphql.Type

	switch tt := v.(type) {
	case *graphql.Argument:
		valueType = tt.Type
		defaultValue = tt.DefaultValue
	case *graphql.InputObjectField:
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
func defaultValueLiteral(value any, t graphql.Type) any {
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

func insideWrappedType(t graphql.Type) graphql.Type {
	switch tt := t.(type) {
	case *graphql.List:
		return tt.OfType
	case *graphql.NonNull:
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

// 将any类型的value转换成对应的类型切片并返回
func asListValue(value any) ([]any, bool) {
	if isNilInterfaceValue(value) {
		return nil, true
	}
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

	// resolver 可能返回 []string、[]map[string]any 等 typed slice；
	// 常见类型先走上面的 type switch 快路径，未知 typed slice 再用反射兜底。
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		result := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			result[i] = rv.Index(i).Interface()
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
