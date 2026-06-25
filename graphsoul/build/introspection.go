package build

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/graphql-go/graphql"
)

func GenerateSchemaMetaResult(schema *graphql.Schema, children []*FieldPlan, inputs map[string]any) map[string]any {
	if schema == nil {
		return nil
	}

	result := map[string]any{}
	for _, child := range children {
		switch child.GetFieldName() {
		case "types":
			var types []any
			for _, t := range schema.TypeMap() {
				types = append(types, GenerateTypeMetaResult(schema, t, child.childrenFields, inputs))
			}
			result[child.GetResponseName()] = types
		case "queryType":
			result[child.GetResponseName()] = GenerateTypeMetaResult(schema, schema.QueryType(), child.childrenFields, inputs)
		case "mutationType":
			result[child.GetResponseName()] = GenerateTypeMetaResult(schema, schema.MutationType(), child.childrenFields, inputs)
		case "subscriptionType":
			result[child.GetResponseName()] = GenerateTypeMetaResult(schema, schema.SubscriptionType(), child.childrenFields, inputs)
		case "directives":
			var directives []any
			for _, d := range schema.Directives() {
				directives = append(directives, GenerateDirectiveMetaResult(schema, d, child.childrenFields))
			}
			result[child.GetResponseName()] = directives
		case "__typename":
			result[child.GetResponseName()] = graphql.SchemaType.Name()
		}
	}
	return result
}

func GenerateTypeMetaResult(schema *graphql.Schema, t graphql.Type, children []*FieldPlan, inputs map[string]any) map[string]any {
	if t == nil {
		return nil
	}

	result := make(map[string]any)
	for _, child := range children {
		switch child.GetFieldName() {
		case "kind":
			result[child.GetResponseName()] = introspectionKind(t)
		case "name":
			result[child.GetResponseName()] = typeNameOrNil(t)
		case "description":
			result[child.GetResponseName()] = typeDescriptionOrNil(t)
		case "fields":
			includeDeprecated := argAsBool(child, "includeDeprecated", inputs, false)
			result[child.GetResponseName()] = GenerateFieldsMetaResult(schema, child.GetChildrenFields(), t, includeDeprecated, inputs)
		case "interfaces":
			result[child.GetResponseName()] = GenerateInterfacesMetaResult(schema, t, child.childrenFields, inputs)
		case "possibleTypes":
			result[child.GetResponseName()] = GeneratePossibleTypesMetaResult(schema, t, child.childrenFields, inputs)
		case "enumValues":
			includeDeprecated := argAsBool(child, "includeDeprecated", inputs, false)
			result[child.GetResponseName()] = GenerateEnumValuesMetaResult(schema, t, child.GetChildrenFields(), includeDeprecated, inputs)
		case "inputFields":
			result[child.GetResponseName()] = GenerateInputFieldsMetaResult(schema, t, child.childrenFields, inputs)
		case "ofType":
			result[child.GetResponseName()] = GenerateTypeMetaResult(schema, insideWrappedType(t), child.GetChildrenFields(), inputs)
		case "__typename":
			result[child.GetResponseName()] = graphql.TypeType.Name()
		}
	}
	return result
}

func GenerateDirectiveMetaResult(schema *graphql.Schema, d *graphql.Directive, children []*FieldPlan) map[string]any {
	if d == nil {
		return nil
	}

	result := map[string]any{}
	for _, child := range children {
		switch child.GetFieldName() {
		case "name":
			result[child.GetResponseName()] = d.Name
		case "description":
			result[child.GetResponseName()] = d.Description
		case "locations":
			result[child.GetResponseName()] = d.Locations
		case "args":
			result[child.GetResponseName()] = d.Args
		}
	}
	return result
}

func GenerateInputFieldsMetaResult(schema *graphql.Schema, t graphql.Type, children []*FieldPlan, inputs map[string]any) any {
	inputObj, ok := t.(*graphql.InputObject)
	if !ok || inputObj == nil {
		return nil
	}

	fields := inputObj.Fields()
	names := make([]string, 0, len(fields))

	for name, _ := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]any, 0, len(names))
	for _, name := range names {
		result = append(result, GenerateSingleInputValueMetaResult(schema, fields[name], children, inputs))
	}
	return result
}

func GenerateInputValuesMetaResult(schema *graphql.Schema, args []*graphql.Argument, children []*FieldPlan, inputs map[string]any) []any {
	result := make([]any, 0)
	for _, arg := range args {
		result = append(result, GenerateSingleInputValueMetaResult(schema, arg, children, inputs))
	}
	return result
}

func GenerateSingleInputValueMetaResult(schema *graphql.Schema, v any, children []*FieldPlan, inputs map[string]any) map[string]any {
	result := make(map[string]any)
	for _, child := range children {
		switch child.GetFieldName() {
		case "name":
			result[child.GetResponseName()] = inputValueName(v)
		case "description":
			result[child.GetResponseName()] = inputValueDescription(v)
		case "type":
			result[child.GetResponseName()] = GenerateTypeMetaResult(schema, inputValueType(v), child.GetChildrenFields(), inputs)
		case "defaultValue":
			result[child.GetResponseName()] = inputValueDefaultValue(v)
		}
	}
	return result
}

func GenerateFieldsMetaResult(schema *graphql.Schema, children []*FieldPlan, t graphql.Type, includeDeprecated bool, inputs map[string]any) any {
	var fields graphql.FieldDefinitionMap
	switch tt := t.(type) {
	case *graphql.Object:
		fields = tt.Fields()
	case *graphql.Interface:
		fields = tt.Fields()
	default:
		return nil
	}

	fieldNames := make([]string, 0, len(fields))
	for name, field := range fields {
		if !includeDeprecated && field.DeprecationReason != "" {
			continue
		}
		fieldNames = append(fieldNames, name)
	}

	sort.Strings(fieldNames)

	result := make([]any, 0)
	for _, name := range fieldNames {
		result = append(result, GenerateSingleFieldMetaResult(schema, fields[name], children, inputs))
	}
	return result
}

func GenerateSingleFieldMetaResult(schema *graphql.Schema, fd *graphql.FieldDefinition, children []*FieldPlan, inputs map[string]any) map[string]any {
	result := map[string]any{}

	for _, child := range children {
		switch child.GetFieldName() {
		case "name":
			result[child.GetResponseName()] = fd.Name
		case "description":
			if fd.Description != "" {
				result[child.GetResponseName()] = nil
			} else {
				result[child.GetResponseName()] = fd.Description
			}
		case "args":
			result[child.GetResponseName()] = GenerateInputValuesMetaResult(schema, fd.Args, child.GetChildrenFields(), inputs)
		case "type":
			result[child.GetResponseName()] = GenerateTypeMetaResult(schema, fd.Type, child.GetChildrenFields(), inputs)
		case "isDeprecated":
			result[child.GetResponseName()] = fd.DeprecationReason != ""
		case "deprecationReason":
			if fd.DeprecationReason == "" {
				result[child.GetResponseName()] = nil
			} else {
				result[child.GetResponseName()] = fd.DeprecationReason
			}
		}
	}
	return result
}

func GeneratePossibleTypesMetaResult(schema *graphql.Schema, t graphql.Type, children []*FieldPlan, inputs map[string]any) any {
	var abs graphql.Abstract

	switch tt := t.(type) {
	case *graphql.Interface:
		abs = tt
	case *graphql.Union:
		abs = tt
	default:
		return nil
	}

	possible := schema.PossibleTypes(abs)
	result := make([]any, 0, len(possible))
	for _, obj := range possible {
		result = append(result, GenerateTypeMetaResult(schema, obj, children, inputs))
	}
	return result
}

func GenerateInterfacesMetaResult(schema *graphql.Schema, t graphql.Type, children []*FieldPlan, inputs map[string]any) any {
	obj, ok := t.(*graphql.Object)
	if !ok || obj == nil {
		return nil
	}

	interfaces := obj.Interfaces()
	result := make([]any, 0, len(interfaces))
	for _, ifa := range interfaces {
		result = append(result, GenerateTypeMetaResult(schema, ifa, children, inputs))
	}
	return result
}

func GenerateEnumValuesMetaResult(schema *graphql.Schema, t graphql.Type, children []*FieldPlan, includeDeprecated bool, inputs map[string]any) any {
	enumType, ok := t.(*graphql.Enum)
	if !ok || enumType == nil {
		return nil
	}

	values := enumType.Values()
	result := make([]any, 0, len(values))
	for _, value := range values {
		if !includeDeprecated && value.DeprecationReason != "" {
			continue
		}
		result = append(result, GenerateSingleEnumValueMetaResult(value, t, children, includeDeprecated, inputs))
	}
	return result
}

func GenerateSingleEnumValueMetaResult(vd *graphql.EnumValueDefinition, t graphql.Type, children []*FieldPlan, includeDeprecated bool, inputs map[string]any) any {
	if vd == nil {
		return nil
	}

	result := map[string]any{}

	for _, child := range children {
		switch child.GetFieldName() {
		case "name":
			result[child.GetResponseName()] = vd.Name
		case "description":
			if vd.Description == "" {
				result[child.GetResponseName()] = nil
			} else {
				result[child.GetResponseName()] = vd.Description
			}
		case "isDeprecated":
			result[child.GetResponseName()] = vd.DeprecationReason != ""
		case "deprecationReason":
			if vd.DeprecationReason == "" {
				result[child.GetResponseName()] = nil
			} else {
				result[child.GetResponseName()] = vd.DeprecationReason
			}
		case "__typename":
			result[child.GetFieldName()] = graphql.EnumValueType.Name()
		}
	}
	return result
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

		switch p.paramType {
		case ParamTypeConst:
			return p.constValue
		case ParamTypeInput:
			return inputs[p.inputName]
		}

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
