package build

import (
	"context"
	"errors"
	"fmt"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
)

// FieldPlanOptions 用于构造 FieldPlan 的所有可选参数。
type FieldPlanOptions struct {
	FieldId                  uint32
	ParentFieldId            uint32
	FieldName                string
	ResponseName             string
	FieldType                FieldType
	FieldObjectName          string
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

type DynamicTypeResolverFunction func(value any, context *context.Context) string
type TypeRuntimeScope struct {
	declaredType        any
	allowedDynamicTypes map[string]*graphql.Object
	dynamicTypeResolver DynamicTypeResolverFunction
	staticTypeName      string
}

func (tr *TypeRuntimeScope) AllowedTypeNamesForField() map[string]bool {
	if len(tr.allowedDynamicTypes) == 0 {
		return nil
	}
	result := make(map[string]bool, len(tr.allowedDynamicTypes))
	for name := range tr.allowedDynamicTypes {
		result[name] = true
	}
	return result
}

type PlanBuilder struct {
	schema    *graphql.Schema
	fragments map[string]*ast.FragmentDefinition
}

func BuildGraphPlan(document *ast.Document, schema *graphql.Schema, args map[string]any) (*SGraphPlan, error) {
	//TODO 参数检查
	if document == nil {
		return nil, errors.New("no document provided")
	}
	if schema == nil {
		return nil, errors.New("no schema provided")
	}
	if args == nil {
		args = map[string]any{}
	}
	result := &SGraphPlan{}

	//TODO 构建fragments map
	fragments := make(map[string]*ast.FragmentDefinition)
	var operationDefinition *ast.OperationDefinition
	for _, def := range document.Definitions {
		switch defType := def.(type) {
		case *ast.FragmentDefinition:
			fragments[defType.Name.Value] = defType
		case *ast.OperationDefinition:
			operationDefinition = defType
		}
	}
	//TODO 获取根节点type
	if operationDefinition == nil {
		return nil, errors.New("no operation definition provided")
	}
	var rootNodeType *graphql.Object
	switch operationDefinition.Operation {
	case ast.OperationTypeMutation:
		rootNodeType = schema.MutationType()
	case ast.OperationTypeSubscription:
		rootNodeType = schema.SubscriptionType()
	case ast.OperationTypeQuery:
		rootNodeType = schema.QueryType()
	}
	//TODO 创建FieldPlanBuilder
	planBuilder := &PlanBuilder{
		schema:    schema,
		fragments: make(map[string]*ast.FragmentDefinition),
	}

	//TODO 提取根节点，并组装对应的type scope
	rootTypeScope, scopeErr := planBuilder.WrapStaticTypeScope(rootNodeType)
	if scopeErr != nil {
		return nil, scopeErr
	}
	//TODO 从根节点开始递归生成FieldPlans
	fieldPlans, fieldPlanErr := planBuilder.buildSelectionSet(operationDefinition.SelectionSet, rootTypeScope)
	if fieldPlanErr != nil {
		return nil, fieldPlanErr
	}
	result.roots = fieldPlans
	return result, nil
}

func (builder *PlanBuilder) buildSelectionSet(current *ast.SelectionSet, typeScope *TypeRuntimeScope) ([]*FieldPlan, error) {
	//TODO 检查入参
	if typeScope == nil {
		return nil, errors.New("no type scope provided while building selection set")
	}
	if current != nil {
		result := make([]*FieldPlan, 0)
		//TODO 循环遍历
		for _, selection := range current.Selections {
			selectionFieldPlans, selectionErr := builder.buildSelection(selection, typeScope)
			if selectionErr != nil {
				return nil, selectionErr
			}
			result = append(result, selectionFieldPlans...)
		}
		return result, nil
	}
	return nil, nil
}

func (builder *PlanBuilder) buildSelection(current ast.Selection, parentTypeScope *TypeRuntimeScope) ([]*FieldPlan, error) {
	//TODO检查入参
	if parentTypeScope == nil {
		return nil, errors.New("no type scope provided while building selection")
	}
	var planFieldType FieldType
	var planFieldIsList bool
	var planFieldListIsNotNil bool
	var planFieldIsNotNil bool

	//TODO判断selection类型，根据不同类型做不同处理
	switch selectionType := current.(type) {
	case *ast.Field:
		if parentTypeScope == nil || parentTypeScope.declaredType == nil {
			return nil, fmt.Errorf("no type scope provided while building selection for field %s", selectionType.Name)
		}
		//TODO 获取当前Field的元素类型，如果是List或者Non-null需要递归获取基本类型
		currentFieldType, fieldTypeErr := builder.GetFieldBaseType(parentTypeScope, selectionType.Name.Value)
		if fieldTypeErr != nil {
			return nil, fieldTypeErr
		}
		//TODO 根据元素类型生成TypeRuntimeScope
		var childTypeScope *TypeRuntimeScope
		var childTypeErr error
		if currentFieldType == nil {
			childTypeScope = &TypeRuntimeScope{}
		}
		switch tt := currentFieldType.(type) {
		case *graphql.Object:
			childTypeScope, childTypeErr = builder.WrapStaticTypeScope(tt)
		case graphql.Abstract:
			childTypeScope, childTypeErr = builder.WrapDynamicTypeScope(tt)
		}
		if childTypeErr != nil {
			return nil, childTypeErr
		}
		//TODO 处理当前类型的selectionSet
		if current.GetSelectionSet() != nil && childTypeScope.declaredType != nil {
			children, err := builder.buildSelectionSet(current.GetSelectionSet(), childTypeScope)
			if err != nil {
				return nil, err
			}
			return children, nil
		}
		//TODO 如果当前是叶子节点，直接返回
		fieldPlan := &FieldPlan{
			fieldName:               selectionType.Name.Value,
			responseName:            selectionType.Name.Value,
			fieldType:               planFieldType,
			fieldIsList:             planFieldIsList,
			fieldNotNil:             planFieldIsNotNil,
			fieldListNotNil:         planFieldListIsNotNil,
			allowedRuntimeTypeNames: parentTypeScope.AllowedTypeNamesForField(),
			runtimeTypeResolverFunc: parentTypeScope.dynamicTypeResolver,
			compiledTypeName:        parentTypeScope.staticTypeName,
			childrenFields:          nil,
		}
		return []*FieldPlan{fieldPlan}, nil
	case *ast.FragmentSpread:
		//TODO 片段模式下先检查片段是否存在
		if selectionType.Name == nil {
			return nil, fmt.Errorf("no fragment spread found for %s", selectionType.Name)
		}
		frag, ok := builder.fragments[selectionType.Name.Value]
		if !ok {
			return nil, fmt.Errorf("no fragment spread found for %s", selectionType.Name.Value)
		}
		//TODO 片段的场景下，继续递归
		croppedScope, scopeErr := builder.CropTypeRuntimeScope(parentTypeScope, frag.TypeCondition)
		if scopeErr != nil {
			return nil, scopeErr
		}
		return builder.buildSelectionSet(frag.SelectionSet, croppedScope)
	case *ast.InlineFragment:
		//TODO 内联的场景下，继续递归
		croppedScope, scopeErr := builder.CropTypeRuntimeScope(parentTypeScope, selectionType.TypeCondition)
		if scopeErr != nil {
			return nil, scopeErr
		}
		return builder.buildSelectionSet(current.GetSelectionSet(), croppedScope)
	}
	return nil, nil
}

func (builder *PlanBuilder) WrapStaticTypeScope(nodeType *graphql.Object) (*TypeRuntimeScope, error) {
	if nodeType == nil {
		return &TypeRuntimeScope{}, errors.New("no node provided")
	}
	return &TypeRuntimeScope{
		declaredType: nodeType,
		allowedDynamicTypes: map[string]*graphql.Object{
			nodeType.Name(): nodeType,
		},
		staticTypeName: nodeType.Name(),
	}, nil
}

func (builder *PlanBuilder) WrapDynamicTypeScope(nodeType graphql.Abstract) (*TypeRuntimeScope, error) {
	possibleTypes := builder.schema.PossibleTypes(nodeType)
	result := make(map[string]*graphql.Object, len(possibleTypes))
	for _, possibleType := range possibleTypes {
		result[possibleType.Name()] = possibleType
	}
	return &TypeRuntimeScope{
		declaredType:        nodeType,
		allowedDynamicTypes: result,
		dynamicTypeResolver: builder.NewDynamicTypeResolverFunction(nodeType),
		staticTypeName:      nodeType.Name(),
	}, nil
}

func (builder *PlanBuilder) NewDynamicTypeResolverFunction(abs graphql.Abstract) DynamicTypeResolverFunction {
	return func(value any, ctx *context.Context) string {
		if value == nil {
			return ""
		}

		switch t := value.(type) {
		case *graphql.Interface:
			if t.ResolveType != nil {
				if obj := t.ResolveType(graphql.ResolveTypeParams{Value: value, Context: *ctx}); obj != nil {
					return obj.Name()
				}
			}
		case *graphql.Union:
			if t.ResolveType != nil {
				if obj := t.ResolveType(graphql.ResolveTypeParams{Value: value, Context: *ctx}); obj != nil {
					return obj.Name()
				}
			}
		}

		for _, possible := range builder.schema.PossibleTypes(abs) {
			if possible.IsTypeOf == nil {
				continue
			}
			if possible.IsTypeOf(graphql.IsTypeOfParams{Value: value, Context: *ctx}) {
				return possible.Name()
			}
		}
		return ""
	}
}

func (builder *PlanBuilder) CropTypeRuntimeScope(parentScope *TypeRuntimeScope, typeCondition *ast.Named) (*TypeRuntimeScope, error) {
	//TODO 参数检查
	if typeCondition == nil || typeCondition.Name == nil || typeCondition.Name.Value == "" {
		return parentScope, nil
	}
	//TODO 查找typeCondition对应的Type
	t := builder.schema.Type(typeCondition.Name.Value)
	if t == nil {
		return nil, fmt.Errorf("no type condition found for %s", typeCondition.Name.Value)
	}
	//TODO 如果是抽象类Type检索可能的所有实现类Type，否则直接使用Type
	var possibleTypeMap map[string]*graphql.Object
	switch tt := t.(type) {
	case *graphql.Object:
		possibleTypeMap = map[string]*graphql.Object{t.Name(): tt}
	case graphql.Abstract:
		objects := builder.schema.PossibleTypes(tt)
		possibleTypeMap = map[string]*graphql.Object{}
		for _, obj := range objects {
			possibleTypeMap[obj.Name()] = obj
		}
	default:
		return nil, fmt.Errorf("unknown type condition found for %s", typeCondition.Name.Value)
	}

	//TODO 将scope里allowedRuntimeType和检索的实现类Type做一个交集，解决内敛嵌套问题
	croppedTypeMap := make(map[string]*graphql.Object)
	for name, obj := range parentScope.allowedDynamicTypes {
		if _, ok := possibleTypeMap[name]; !ok {
			croppedTypeMap[name] = obj
		}
	}

	if len(croppedTypeMap) == 0 {
		return nil, fmt.Errorf("no type condition matched for %s", typeCondition.Name.Value)
	}
	return &TypeRuntimeScope{
		declaredType:        t,
		allowedDynamicTypes: croppedTypeMap,
		staticTypeName:      parentScope.staticTypeName,
		dynamicTypeResolver: parentScope.dynamicTypeResolver,
	}, nil
}

func (builder *PlanBuilder) GetFieldBaseType(parentType any, fieldName string) (graphql.Type, error) {
	//TODO 找到FieldDefinition
	var fieldTypeDef *graphql.FieldDefinition
	switch t := parentType.(type) {
	case *graphql.Object:
		fieldTypeDef = t.Fields()[fieldName]
		if fieldTypeDef == nil {
			return nil, fmt.Errorf("no type definition found for %s in object", fieldName)
		}
	case *graphql.Interface:
		fieldTypeDef = t.Fields()[fieldName]
		if fieldTypeDef == nil {
			return nil, fmt.Errorf("no type definition found for %s in interface", fieldName)
		}
	case *graphql.Union:
		return nil, fmt.Errorf("no type definition found for %s in union", fieldName)
	default:
		return nil, fmt.Errorf("no type definition found for %s", fieldName)
	}

	//TODO 根据FieldDefinition找到对应的基本Type
	for {
		switch t := fieldTypeDef.Type.(type) {
		case *graphql.Object:
			return t, nil
		case *graphql.Scalar:
			return t, nil
		case *graphql.Enum:
			return t, nil
		case *graphql.Interface:
			return t, nil
		case *graphql.Union:
			return t, nil
		case *graphql.List:
			return builder.UnWrapType(t.OfType), nil
		case *graphql.NonNull:
			return builder.UnWrapType(t.OfType), nil
		}
	}

	return nil, fmt.Errorf("no type found for %s", fieldName)
}

func (builder *PlanBuilder) UnWrapType(t graphql.Type) graphql.Type {
	//TODO 递归处理类型嵌套
	for {
		switch tt := t.(type) {
		case *graphql.List:
			t = tt.OfType
		case *graphql.NonNull:
			t = tt.OfType
		default:
			return tt
		}
	}
	return nil
}
