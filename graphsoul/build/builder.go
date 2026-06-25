package build

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
)

type DynamicTypeResolverFunction func(value any, context *context.Context) string

// 类型上下文结构体，在build阶段生成，在execute阶段运行，用于类型推断
type TypeRuntimeScope struct {
	declaredType        any
	allowedDynamicTypes map[string]*graphql.Object
	dynamicTypeResolver DynamicTypeResolverFunction
	staticTypeName      string
}

const DefaultParamKeyTypename = "typeName"

var TypeNameResolverFunc = func(source any, params map[string]any, ctx context.Context) (any, error) {
	if params != nil {
		if typeName, ok := params[DefaultParamKeyTypename].(string); ok {
			return typeName, nil
		}
	}
	return nil, nil
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

func (tr *TypeRuntimeScope) GetAllowedTypeNamesForField() map[string]bool {
	if tr == nil || len(tr.allowedDynamicTypes) == 0 {
		return nil
	}
	result := make(map[string]bool, len(tr.allowedDynamicTypes))
	for name := range tr.allowedDynamicTypes {
		result[name] = true
	}
	return result
}

func (tr *TypeRuntimeScope) GetAllowedDynamicTypes() map[string]*graphql.Object {
	return tr.allowedDynamicTypes
}

func (tr *TypeRuntimeScope) GetDynamicTypeResolverFunction() DynamicTypeResolverFunction {
	return tr.dynamicTypeResolver
}

func (tr *TypeRuntimeScope) GetStaticTypeName() string {
	return tr.staticTypeName
}

func (tr *TypeRuntimeScope) GetDeclaredType() any {
	return tr.declaredType
}

type PlanBuilder struct {
	schema            *graphql.Schema
	fragments         map[string]*ast.FragmentDefinition
	fieldIdCounter    atomic.Uint32
	originalInputs    map[string]any
	directiveRegistry *DirectiveRegistry
}

type FieldEntry struct {
	field            ast.Field
	typeScope        *TypeRuntimeScope
	directives       []*DirectivePlan
	dependencyParams []*ParamPlan
}

func BuildGraphPlan(document *ast.Document, schema *graphql.Schema, args map[string]any, operationName *string, registry *DirectiveRegistry) (*SGraphPlan, error) {
	//参数检查
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
	planBuilder := &PlanBuilder{
		schema:         schema,
		fragments:      make(map[string]*ast.FragmentDefinition),
		fieldIdCounter: atomic.Uint32{},
		originalInputs: args,
	}

	//选择Operation和Fragments
	fragments := make(map[string]*ast.FragmentDefinition)
	var operationDefinition *ast.OperationDefinition
	for _, def := range document.Definitions {
		switch defType := def.(type) {
		case *ast.FragmentDefinition:
			fragments[defType.Name.Value] = defType
		case *ast.OperationDefinition:
			if operationName != nil {
				if *operationName == defType.Name.Value {
					operationDefinition = defType
					break
				}
			} else {
				if operationDefinition == nil {
					operationDefinition = defType
				} else {
					return nil, errors.New("operation definition already exists")
				}
			}
		}
	}
	planBuilder.fragments = fragments

	//组装original_params
	if operationDefinition == nil {
		return nil, errors.New("no operation definition provided")
	}
	inputs, inputsErr := planBuilder.parseOperationVariables(schema, operationDefinition.VariableDefinitions, args)
	if inputsErr != nil {
		return nil, inputsErr
	}
	planBuilder.originalInputs = inputs
	result.originalInputs = inputs

	//确定RootType
	var rootNodeType *graphql.Object
	switch operationDefinition.Operation {
	case ast.OperationTypeMutation:
		rootNodeType = schema.MutationType()
		result.operationType = SGraphOperationTypeMutation
	case ast.OperationTypeSubscription:
		rootNodeType = schema.SubscriptionType()
		result.operationType = SGraphOperationTypeSubscription
	case ast.OperationTypeQuery:
		rootNodeType = schema.QueryType()
		result.operationType = SGraphOperationTypeQuery
	}
	//directives
	var directiveRegistry *DirectiveRegistry
	if registry != nil {
		directiveRegistry = registry
	} else {
		directiveRegistry = NewDirectiveRegistry()
	}
	planBuilder.directiveRegistry = directiveRegistry

	//query级别的directive
	operationLocation := operationDirectiveLocation(operationDefinition.Operation)
	queryCompiled, queryErr := planBuilder.compileDirectives(operationDefinition.Directives, operationLocation, nil)
	if queryErr != nil {
		return nil, queryErr
	}

	//提取根节点，并组装对应的TypeScope
	rootTypeScope, scopeErr := planBuilder.WrapStaticTypeScope(rootNodeType)
	if scopeErr != nil {
		return nil, scopeErr
	}
	//递归解析SelectionSet
	fieldPlans, fieldPlanErr := planBuilder.buildSelectionSetWithFlattenEntries(operationDefinition.SelectionSet, rootTypeScope, 0, false, nil, queryCompiled.RuntimePlans, false)
	if fieldPlanErr != nil {
		return nil, fieldPlanErr
	}
	//生成SGraphPlan
	result.roots = fieldPlans

	return result, nil
}

// 遍历一个selectionset下的所有selection来生成对应的fieldPlan
func (builder *PlanBuilder) buildSelectionSet(current *ast.SelectionSet, typeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, inheritedDirectives []*DirectivePlan) ([]*FieldPlan, error) {
	//检查入参
	if typeScope == nil {
		return nil, errors.New("no type scope provided while building selection set")
	}
	if current != nil {
		result := make([]*FieldPlan, 0)
		//TODO 循环遍历selection
		for _, selection := range current.Selections {
			selectionFieldPlans, selectionErr := builder.buildSelection(selection, typeScope, parentFieldId, parentFieldIsList, parentPaths, inheritedDirectives)
			if selectionErr != nil {
				return nil, selectionErr
			}
			result = append(result, selectionFieldPlans...)
		}
		return result, nil
	}
	return nil, nil
}

func (builder *PlanBuilder) buildSelectionSetWithFlattenEntries(current *ast.SelectionSet, typeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, inheritedDirectives []*DirectivePlan, isIntrospection bool) ([]*FieldPlan, error) {
	if typeScope == nil {
		return nil, errors.New("no type scope provided while building selection set")
	}
	if current == nil {
		return nil, nil
	}

	fieldEntries, fieldEntriesErr := builder.flattenSelections(current, typeScope, inheritedDirectives)
	if fieldEntriesErr != nil {
		return nil, fieldEntriesErr
	}

	type groupKey struct {
		responseName    string
		allowedTypeHash string
	}
	type group struct {
		fields           []ast.Field
		typeScope        *TypeRuntimeScope
		directives       []*DirectivePlan
		dependencyParams []*ParamPlan
	}

	var orderedKeys []groupKey
	groups := make(map[groupKey]*group)

	for _, entry := range fieldEntries {
		key := groupKey{
			responseName:    GetAstResponseName(&entry.field),
			allowedTypeHash: allowedTypeHash(entry.typeScope),
		}
		if g, exists := groups[key]; !exists {
			orderedKeys = append(orderedKeys, key)
			groups[key] = &group{
				fields:           []ast.Field{entry.field},
				typeScope:        entry.typeScope,
				directives:       entry.directives,
				dependencyParams: entry.dependencyParams,
			}
		} else {
			g.fields = append(g.fields, entry.field)
			//dependencyParams 合并所有出现
			g.dependencyParams = append(g.dependencyParams, entry.dependencyParams...)
		}
	}

	result := make([]*FieldPlan, 0)
	for _, key := range orderedKeys {
		g := groups[key]

		rep := g.fields[0]
		if len(g.fields) > 1 {
			merged := &ast.SelectionSet{
				Selections: []ast.Selection{},
			}
			for _, f := range g.fields {
				if f.SelectionSet != nil {
					merged.Selections = append(merged.Selections, f.SelectionSet.Selections...)
				}
			}
			if len(merged.Selections) > 0 {
				rep.SelectionSet = merged
			} else {
				rep.SelectionSet = nil
			}
		}

		fieldName := ""
		if rep.Name != nil {
			fieldName = rep.Name.Value
		}

		var fieldPlans []*FieldPlan
		var fieldErr error

		switch fieldName {
		case IntrospectionFieldNameTypename:
			fieldPlans, fieldErr = builder.parseIntrospectionTypenameField(rep, g.typeScope, parentFieldId, parentFieldIsList, parentPaths, g.directives, g.dependencyParams)
		case IntrospectionFieldNameMetaType:
			fieldPlans, fieldErr = builder.parseIntrospectionMetaTypeField(rep, g.typeScope, parentFieldId, parentFieldIsList, parentPaths, g.directives, g.dependencyParams)
		case IntrospectionFieldNameMetaSchema:
			fieldPlans, fieldErr = builder.parseIntrospectionMetaSchemaField(rep, g.typeScope, parentFieldId, parentFieldIsList, parentPaths, g.directives, g.dependencyParams)
		default:
			if isIntrospection {
				var fieldPlan *FieldPlan
				fieldPlan, fieldErr = builder.buildIntrospectionFieldPlan(&rep, g.typeScope, parentFieldId, parentFieldIsList, parentPaths, g.directives, g.dependencyParams)
				if fieldErr != nil {
					fieldPlans = []*FieldPlan{fieldPlan}
				}
			} else {
				fieldPlans, fieldErr = builder.parseField(rep, g.typeScope, parentFieldId, parentFieldIsList, parentPaths, g.directives, g.dependencyParams)
			}
		}
		if fieldErr != nil {
			return nil, fieldErr
		}
		result = append(result, fieldPlans...)
	}
	return result, nil
}

func (builder *PlanBuilder) flattenSelections(selectionSet *ast.SelectionSet, typeScope *TypeRuntimeScope, inheritedDirectives []*DirectivePlan) ([]FieldEntry, error) {
	if selectionSet == nil {
		return nil, nil
	}
	var result []FieldEntry
	for _, selection := range selectionSet.Selections {
		fieldEntries, fieldEntriesErr := builder.flattenOneSelection(selection, typeScope, inheritedDirectives)
		if fieldEntriesErr != nil {
			return nil, fieldEntriesErr
		}
		result = append(result, fieldEntries...)
	}
	return result, nil
}

func (builder *PlanBuilder) flattenOneSelection(selection ast.Selection, parentTypeScope *TypeRuntimeScope, inheritedDirectives []*DirectivePlan) ([]FieldEntry, error) {
	switch sel := selection.(type) {
	case *ast.Field:
		compiled, err := builder.compileDirectives(
			sel.Directives,
			graphql.DirectiveLocationField,
			inheritedDirectives,
		)
		if err != nil {
			return nil, err
		}
		if compiled.IncludeDecision != nil && !*compiled.IncludeDecision {
			return nil, nil
		}
		return []FieldEntry{{
			field:            *sel,
			typeScope:        parentTypeScope,
			directives:       compiled.RuntimePlans,
			dependencyParams: compiled.DependencyParamPlans,
		}}, nil
	case *ast.FragmentSpread:
		spreadCompiled, err := builder.compileDirectives(sel.Directives, graphql.DirectiveLocationFragmentSpread, inheritedDirectives)
		if err != nil {
			return nil, err
		}
		if spreadCompiled.IncludeDecision != nil && !*spreadCompiled.IncludeDecision {
			return nil, nil
		}
		//TODO 片段模式下先检查片段是否存在
		if sel.Name == nil {
			return nil, fmt.Errorf("no fragment spread found")
		}
		frag, ok := builder.fragments[sel.Name.Value]
		if !ok {
			return nil, fmt.Errorf("no fragment spread found for %s", sel.Name.Value)
		}
		//TODO 片段的场景下，继续递归
		croppedScope, scopeErr := builder.CropTypeRuntimeScope(parentTypeScope, frag.TypeCondition)
		if scopeErr != nil {
			return nil, scopeErr
		}
		fragmentCompiled, err := builder.compileDirectives(frag.Directives, graphql.DirectiveLocationFragmentDefinition, spreadCompiled.RuntimePlans)
		if err != nil {
			return nil, err
		}
		return builder.flattenSelections(frag.SelectionSet, croppedScope, fragmentCompiled.RuntimePlans)
	case *ast.InlineFragment:
		compiled, err := builder.compileDirectives(sel.Directives, graphql.DirectiveLocationInlineFragment, inheritedDirectives)
		if err != nil {
			return nil, err
		}
		if compiled.IncludeDecision != nil && !*compiled.IncludeDecision {
			return nil, nil
		}
		//TODO 内联的场景下，继续递归
		croppedScope, scopeErr := builder.CropTypeRuntimeScope(parentTypeScope, sel.TypeCondition)
		if scopeErr != nil {
			return nil, scopeErr
		}
		return builder.flattenSelections(sel.SelectionSet, croppedScope, compiled.RuntimePlans)
	}
	return nil, nil
}

// 根据一个具体的selection来生成对应的fieldPlan
func (builder *PlanBuilder) buildSelection(current ast.Selection, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, inheritedDirectives []*DirectivePlan) ([]*FieldPlan, error) {
	//检查入参
	if parentTypeScope == nil {
		return nil, errors.New("no type scope provided while building selection")
	}

	//TODO判断selection类型，根据不同类型做不同处理
	switch selectionType := current.(type) {
	case *ast.Field:
		compiled, err := builder.compileDirectives(
			selectionType.Directives,
			graphql.DirectiveLocationField,
			inheritedDirectives,
		)
		if err != nil {
			return nil, err
		}

		//__typename
		if selectionType.Name != nil && selectionType.Name.Value == IntrospectionFieldNameTypename {
			typenameFieldPlans, typenameFieldPlanErr := builder.parseIntrospectionTypenameField(*selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, compiled.RuntimePlans, compiled.DependencyParamPlans)
			if typenameFieldPlanErr != nil {
				return nil, typenameFieldPlanErr
			}
			return typenameFieldPlans, nil
		}
		//__type
		if selectionType.Name != nil && selectionType.Name.Value == IntrospectionFieldNameMetaType {
			typeFieldPlans, typeFieldPlansErr := builder.parseIntrospectionMetaTypeField(*selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, compiled.RuntimePlans, compiled.DependencyParamPlans)
			if typeFieldPlansErr != nil {
				return nil, typeFieldPlansErr
			}
			return typeFieldPlans, nil
		}
		//__schema
		if selectionType.Name != nil && selectionType.Name.Value == IntrospectionFieldNameMetaSchema {
			schemaFieldPlans, schemaFieldPlansErr := builder.parseIntrospectionMetaSchemaField(*selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, compiled.RuntimePlans, compiled.DependencyParamPlans)
			if schemaFieldPlansErr != nil {
				return nil, schemaFieldPlansErr
			}
			return schemaFieldPlans, nil
		}
		//default
		fieldPlans, fieldPlansErr := builder.parseField(*selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, compiled.RuntimePlans, compiled.DependencyParamPlans)
		if fieldPlansErr != nil {
			return nil, fieldPlansErr
		}
		return fieldPlans, nil
	case *ast.FragmentSpread:
		spreadCompiled, err := builder.compileDirectives(selectionType.Directives, graphql.DirectiveLocationFragmentSpread, inheritedDirectives)
		if err != nil {
			return nil, err
		}
		if spreadCompiled.IncludeDecision != nil && !*spreadCompiled.IncludeDecision {
			return nil, nil
		}
		//TODO 片段模式下先检查片段是否存在
		if selectionType.Name == nil {
			return nil, fmt.Errorf("no fragment spread found")
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
		fragmentCompiled, err := builder.compileDirectives(frag.Directives, graphql.DirectiveLocationFragmentDefinition, spreadCompiled.RuntimePlans)
		if err != nil {
			return nil, err
		}
		return builder.buildSelectionSet(frag.SelectionSet, croppedScope, parentFieldId, parentFieldIsList, parentPaths, fragmentCompiled.RuntimePlans)
	case *ast.InlineFragment:
		compiled, err := builder.compileDirectives(selectionType.Directives, graphql.DirectiveLocationInlineFragment, inheritedDirectives)
		if err != nil {
			return nil, err
		}
		if compiled.IncludeDecision != nil && !*compiled.IncludeDecision {
			return nil, nil
		}
		//TODO 内联的场景下，继续递归
		croppedScope, scopeErr := builder.CropTypeRuntimeScope(parentTypeScope, selectionType.TypeCondition)
		if scopeErr != nil {
			return nil, scopeErr
		}
		return builder.buildSelectionSet(current.GetSelectionSet(), croppedScope, parentFieldId, parentFieldIsList, parentPaths, compiled.RuntimePlans)
	}
	return nil, nil
}

func (builder *PlanBuilder) buildIntrospectionSelectionSet(current *ast.SelectionSet, typeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, inheritedDirectives []*DirectivePlan, directiveDependencyParams []*ParamPlan) ([]*FieldPlan, error) {
	//if current == nil {
	//	return nil, errors.New("no type scope provided while building introspection selection set")
	//}
	//if typeScope == nil || typeScope.dynamicTypeResolver == nil {
	//	return nil, errors.New("no type scope provided while building introspection selection set")
	//}
	//result := make([]*FieldPlan, 0)
	//for _, selection := range current.Selections {
	//	fieldPlans, fieldPlansErr := builder.buildIntrospectionSelection(selection, typeScope, parentFieldId, parentFieldIsList, parentPaths, inheritedDirectives, directiveDependencyParams)
	//	if fieldPlansErr != nil {
	//		return nil, fieldPlansErr
	//	}
	//	result = append(result, fieldPlans...)
	//}
	//return result, nil
	return builder.buildSelectionSetWithFlattenEntries(current, typeScope, parentFieldId, parentFieldIsList, parentPaths, inheritedDirectives, true)
}

func (builder *PlanBuilder) buildIntrospectionSelection(current ast.Selection, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, inheritedDirectives []*DirectivePlan, directiveDependencyParams []*ParamPlan) ([]*FieldPlan, error) {
	switch selectionType := current.(type) {
	case *ast.Field:
		if selectionType.Name == nil {
			return nil, errors.New("no type scope provided while building introspection selection set")
		}
		if selectionType.Name.Value == IntrospectionFieldNameTypename {
			return builder.parseIntrospectionTypenameField(*selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, inheritedDirectives, directiveDependencyParams)
		}
		if strings.HasPrefix(selectionType.Name.Value, "__") {
			return nil, fmt.Errorf("introspection selection found for %s", selectionType.Name.Value)
		}
		fieldPlan, fieldPlanErr := builder.buildIntrospectionFieldPlan(selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, inheritedDirectives, directiveDependencyParams)
		if fieldPlanErr != nil {
			return nil, fieldPlanErr
		}
		return []*FieldPlan{fieldPlan}, nil
	case *ast.FragmentSpread:
		if selectionType.Name == nil {
			return nil, errors.New("no type scope provided while building introspection selection set")
		}
		frag, ok := builder.fragments[selectionType.Name.Value]
		if !ok {
			return nil, fmt.Errorf("no fragment spread found for %s", selectionType.Name.Value)
		}
		scope, scopeErr := builder.CropTypeRuntimeScope(parentTypeScope, frag.TypeCondition)
		if scopeErr != nil {
			return nil, scopeErr
		}
		return builder.buildIntrospectionSelectionSet(frag.GetSelectionSet(), scope, parentFieldId, parentFieldIsList, parentPaths, inheritedDirectives, directiveDependencyParams)
	case *ast.InlineFragment:
		scope, scopeErr := builder.CropTypeRuntimeScope(parentTypeScope, selectionType.TypeCondition)
		if scopeErr != nil {
			return nil, scopeErr
		}
		return builder.buildIntrospectionSelectionSet(current.GetSelectionSet(), scope, parentFieldId, parentFieldIsList, parentPaths, inheritedDirectives, directiveDependencyParams)
	}
	return nil, nil
}

func (builder *PlanBuilder) buildIntrospectionFieldPlan(current *ast.Field, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, directives []*DirectivePlan, directiveDependencyParams []*ParamPlan) (*FieldPlan, error) {
	//responseName
	responseName := GetAstResponseName(current)
	//paths
	paths := append(parentPaths, responseName)

	fieldDef, fieldDefErr := builder.getFieldDefinition(parentTypeScope.declaredType, current.Name.Value)
	if fieldDefErr != nil {
		return nil, fieldDefErr
	}
	//fieldValueMetaInfo
	fieldValueMetaInfo, fieldValueMetaInfoErr := builder.parseFieldValueMetaInfo(fieldDef)
	if fieldValueMetaInfoErr != nil {
		return nil, fieldValueMetaInfoErr
	}
	//ParamPlans
	//paramPlans, paramPlansErr := builder.parseParamPlans(current.Arguments)
	paramPlans, paramPlansErr := builder.parseParamPlansByArgDefs(fieldDef.Args, current.Arguments)
	if paramPlansErr != nil {
		return nil, paramPlansErr
	}
	//fieldId
	fieldId := builder.generateFieldId()
	//typeRuntimeScope
	currentScope, currentScopeErr := builder.WrapFieldType2Scope(fieldValueMetaInfo.GetBaseElementOriginalType())
	if currentScopeErr != nil {
		return nil, currentScopeErr
	}
	//children
	var children []*FieldPlan
	var childrenErr error
	if current.SelectionSet != nil && currentScope != nil && currentScope.declaredType != nil {
		children, childrenErr = builder.buildIntrospectionSelectionSet(current.GetSelectionSet(), currentScope, fieldId, fieldValueMetaInfo.IsList, paths, directives, directiveDependencyParams)
		if childrenErr != nil {
			return nil, childrenErr
		}
	}

	return &FieldPlan{
		fieldId:                 fieldId,
		parentFieldId:           parentFieldId,
		fieldName:               current.Name.Value,
		responseName:            responseName,
		paths:                   paths,
		fieldValueMetaInfo:      *fieldValueMetaInfo,
		childrenFields:          children,
		paramPlans:              paramPlans,
		allowedRuntimeTypeNames: parentTypeScope.GetAllowedTypeNamesForField(),
		runtimeTypeResolverFunc: parentTypeScope.GetDynamicTypeResolverFunction(),
		compiledTypeName:        parentTypeScope.GetStaticTypeName(),
	}, nil
}

func (builder *PlanBuilder) getFieldDefinition(parentType any, fieldName string) (*graphql.FieldDefinition, error) {
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

// 例子：[[scalar!]!]!返回scalar。获取一个字段的基础类型，返回值只能是Object/Scalar/Enum/Interface/Union这几个，封装类型NotNull/List不算
// 返回值分别是:基础类型，是否为isList，是否为NotNull，错误
func (builder *PlanBuilder) parseFieldValueMetaInfo(fd *graphql.FieldDefinition) (*FieldValueMetaInfo, error) {
	if fd == nil {
		return nil, errors.New("no type definition provided while building selection set")
	}
	result := &FieldValueMetaInfo{}
	var currentType graphql.Output
	currentType = fd.Type

	currentValueMetaInfo := result

	for {
		switch t := currentType.(type) {
		case *graphql.NonNull:
			currentType = t.OfType

			currentValueMetaInfo.NotNil = true
			currentValueMetaInfo.OriginalType = t
		case *graphql.List:
			currentType = t.OfType

			currentValueMetaInfo.IsList = true
			currentValueMetaInfo.ValueType = FieldValueTypeList
			currentValueMetaInfo.OriginalType = t
			elementType := &FieldValueMetaInfo{}
			currentValueMetaInfo.ElementType = elementType
			currentValueMetaInfo = elementType
		case *graphql.Scalar:
			currentValueMetaInfo.ValueType = FieldValueTypeScalar
			currentValueMetaInfo.OriginalType = t
			return result, nil
		case *graphql.Object:
			currentValueMetaInfo.ValueType = FieldValueTypeObject
			currentValueMetaInfo.OriginalType = t
			return result, nil
		case *graphql.Enum:
			currentValueMetaInfo.ValueType = FieldValueTypeEnum
			currentValueMetaInfo.OriginalType = t
			return result, nil
		case *graphql.Interface:
			currentValueMetaInfo.ValueType = FieldValueTypeObject
			currentValueMetaInfo.OriginalType = t
			return result, nil
		case *graphql.Union:
			currentValueMetaInfo.ValueType = FieldValueTypeObject
			currentValueMetaInfo.OriginalType = t
			return result, nil
		default:
			return nil, fmt.Errorf("no type found for %s", currentType.Name())
		}
	}
}

// 组装编译期就能确定类型的节点类型上下文
func (builder *PlanBuilder) WrapStaticTypeScope(nodeType *graphql.Object) (*TypeRuntimeScope, error) {
	if nodeType == nil {
		return nil, errors.New("no node provided")
	}
	//创建编译期判定类型的类型上下文
	return &TypeRuntimeScope{
		declaredType: nodeType,
		allowedDynamicTypes: map[string]*graphql.Object{
			nodeType.Name(): nodeType,
		},
		staticTypeName: nodeType.Name(),
		//编译期和运行期的直接区别就是dynamicTypeResolver为nil，因为不需要动态解析
		dynamicTypeResolver: nil,
	}, nil
}

// 组装运行时动态判定类型的节点类型上下文。uniontype和interface类型的节点都需要走这个方法
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

		switch t := abs.(type) {
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
		if _, ok := possibleTypeMap[name]; ok {
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

func (builder *PlanBuilder) generateFieldId() uint32 {
	fid := builder.fieldIdCounter.Add(1)
	return uint32(fid)
}

func (builder *PlanBuilder) wrapResolverFunc(fieldResolveFn graphql.FieldResolveFn) ResolverFunc {
	if fieldResolveFn == nil {
		return nil
	}
	type firstResponseGetter interface {
		GetFirstResponse() any
	}
	return func(source any, params map[string]any, ctx context.Context) (any, error) {
		actualSource := source
		if wrappedSource, ok := source.(firstResponseGetter); ok {
			actualSource = wrappedSource.GetFirstResponse()
		}
		return fieldResolveFn(graphql.ResolveParams{
			Source:  actualSource,
			Args:    params,
			Context: ctx,
		})
	}
}

const ParentKeyFieldNameAsID string = "id"

// 寻找父节点中类型为scalar ID的字段，默认取id，否则返回第一个类型为scalar ID的字段名。如果没找到则返回空字符串。
func (builder *PlanBuilder) checkAndParseParentKeyFieldNames(parentFieldIsList bool, parentTypeScope *TypeRuntimeScope) string {
	//入参检查
	if !parentFieldIsList || parentTypeScope == nil || parentTypeScope.declaredType == nil {
		return ""
	}
	//获取约定的父节点KeyField的名称，即id
	fieldDefinition, fieldDefinitionErr := builder.getFieldDefinition(parentTypeScope.declaredType, ParentKeyFieldNameAsID)
	if fieldDefinitionErr == nil && fieldDefinition != nil {
		//检查id字段的类型是否为scalar
		t, te := builder.parseBaseType(fieldDefinition.Type)
		if te != nil || t == nil {
			return ""
		}
		if scalarType, ok := t.(*graphql.Scalar); ok && scalarType == graphql.ID {
			return ParentKeyFieldNameAsID
		}
	}
	//如果不存在id字段，则遍历全部字段寻找ID类型的字段
	switch t := parentTypeScope.declaredType.(type) {
	case *graphql.Object:
		if len(t.Fields()) > 0 {
			for _, field := range t.Fields() {
				fieldValueType, fieldValueTypeErr := builder.parseBaseType(field.Type)
				if fieldValueTypeErr != nil || fieldValueType == nil {
					continue
				}
				if scalarType, ok := fieldValueType.(*graphql.Scalar); ok && scalarType == graphql.ID {
					return field.Name
				}
			}
		}
	case *graphql.Interface:
		if len(t.Fields()) > 0 {
			for _, field := range t.Fields() {
				fieldValueType, fieldValueTypeErr := builder.parseBaseType(field.Type)
				if fieldValueTypeErr != nil || fieldValueType == nil {
					continue
				}
				if scalarType, ok := fieldValueType.(*graphql.Scalar); ok && scalarType == graphql.ID {
					return field.Name
				}
			}
		}
	default:
		return ""
	}

	return ""
}

func (builder *PlanBuilder) parseBaseType(t graphql.Type) (graphql.Type, error) {
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

func (builder *PlanBuilder) parseParamPlansByArgDefs(argDefs []*graphql.Argument, argASTs []*ast.Argument) ([]*ParamPlan, error) {
	defMap := map[string]*graphql.Argument{}
	for _, argDef := range argDefs {
		defMap[argDef.PrivateName] = argDef
	}

	astMap := map[string]*ast.Argument{}
	for _, arg := range argASTs {
		if arg == nil || arg.Name == nil {
			return nil, fmt.Errorf("invalid argument")
		}
		name := arg.Name.Value
		if _, ok := defMap[name]; !ok {
			return nil, fmt.Errorf("invalid argument definition")
		}
		if _, duplicated := astMap[name]; duplicated {
			return nil, fmt.Errorf("duplicate argument definition")
		}
		astMap[arg.Name.Value] = arg
	}

	var result []*ParamPlan
	for _, argDef := range argDefs {
		argAST, provided := astMap[argDef.PrivateName]

		var value any
		var valueErr error
		if provided {
			value, valueErr = ValueFromAST(argAST.Value, argDef.Type, builder.originalInputs)
			if valueErr != nil {
				return nil, valueErr
			}
		} else if argDef.DefaultValue != nil {
			value = argDef.DefaultValue
		} else {
			if IsNonNullInput(argDef.Type) {
				return nil, fmt.Errorf("required argument %s is missing", argDef.Name())
			}
			continue
		}

		parsedValue, parsedValueErr := builder.parseInputValue(argDef.Type, value)
		if parsedValueErr != nil {
			return nil, parsedValueErr
		}
		result = append(result, NewConstParamPlan(argDef.PrivateName, parsedValue))
	}
	return result, nil
}

func (builder *PlanBuilder) parseParamPlans(args []*ast.Argument) ([]*ParamPlan, error) {
	result := make([]*ParamPlan, len(args))
	for _, arg := range args {
		if arg == nil || arg.Name == nil {
			return nil, fmt.Errorf("invalid argument")
		}
		paramPlan, paramPlanErr := builder.parseArgumentToParamPlan(arg.Name.Value, arg.Value)
		if paramPlanErr != nil {
			return nil, paramPlanErr
		}
		result = append(result, paramPlan)
	}
	return result, nil
}

func (builder *PlanBuilder) parseArgumentToParamPlan(key string, value ast.Value) (*ParamPlan, error) {
	if value == nil {
		return builder.newConstParamPlan(key, nil), nil
	}
	switch typedValue := value.(type) {
	case *ast.Variable:
		if typedValue.Name == nil {
			return nil, fmt.Errorf("invalid variable name")
		}
		return builder.newInputParamPlan(key, typedValue.Name.Value), nil
	case *ast.IntValue:
		parsedInt, parsedIntErr := strconv.ParseInt(typedValue.Value, 10, 64)
		if parsedIntErr != nil {
			return nil, parsedIntErr
		}
		return builder.newConstParamPlan(key, parsedInt), nil
	case *ast.FloatValue:
		parsedFloat, parsedFloatErr := strconv.ParseFloat(typedValue.Value, 64)
		if parsedFloatErr != nil {
			return nil, parsedFloatErr
		}
		return builder.newConstParamPlan(key, parsedFloat), nil
	case *ast.StringValue:
		return builder.newConstParamPlan(key, typedValue.Value), nil
	case *ast.BooleanValue:
		return builder.newConstParamPlan(key, typedValue.Value), nil
	case *ast.EnumValue:
		return builder.newConstParamPlan(key, typedValue.Value), nil
	case *ast.ListValue:
		listValue := make([]any, 0, len(typedValue.Values))
		for i, listValueItem := range typedValue.Values {
			itemPlan, itemPlanErr := builder.parseArgumentToParamPlan(strconv.Itoa(i), listValueItem)
			if itemPlanErr != nil {
				return nil, itemPlanErr
			}
			listValue = append(listValue, itemPlan.GetConstValue())
		}
		return NewConstParamPlan(key, listValue), nil
	case *ast.ObjectValue:
		objectValue := make(map[string]any, len(typedValue.Fields))
		for _, field := range typedValue.Fields {
			if field == nil || field.Name == nil {
				continue
			}
			fieldPlan, fieldPlanErr := builder.parseArgumentToParamPlan(field.Name.Value, field.Value)
			if fieldPlanErr != nil {
				return nil, fieldPlanErr
			}
			objectValue[field.Name.Value] = fieldPlan.GetConstValue()
		}
		return NewConstParamPlan(key, objectValue), nil
	default:
		return nil, fmt.Errorf("unknown type of argument: %v", value)
	}
}

const IntrospectionFieldNameTypename string = "__typename"
const IntrospectionFieldNameMetaType string = "__type"
const IntrospectionFieldNameMetaSchema string = "__schema"

func (builder *PlanBuilder) parseIntrospectionTypenameField(current ast.Field, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, directivePlans []*DirectivePlan, directiveDependencyParams []*ParamPlan) ([]*FieldPlan, error) {
	var fieldId uint32
	responseName := IntrospectionFieldNameTypename
	if current.Alias != nil && current.Alias.Value != "" {
		responseName = current.Alias.Value
	}
	//fieldId
	fieldId = builder.generateFieldId()
	//paths
	paths := append(parentPaths, responseName)
	//fieldValueMetaInfo
	fieldValueMetaInfo := FieldValueMetaInfo{
		IsList:       false,
		NotNil:       true,
		ValueType:    FieldValueTypeScalar,
		OriginalType: graphql.String,
		ElementType:  nil,
	}
	//childrenFields
	var childrenFields []*FieldPlan
	//paramPlans
	needParentFieldFullResult := false
	paramPlans := make([]*ParamPlan, 0)
	//如果父节点是List，默认增加全结果依赖，要根据父节点的遍历次数添加信息
	if parentFieldIsList && parentFieldId > 0 {
		needParentFieldFullResult = true
	}
	//如果父节点类型要动态判定，默认增加全结果依赖
	if parentTypeScope != nil && parentTypeScope.dynamicTypeResolver != nil {
		needParentFieldFullResult = true
	}
	if needParentFieldFullResult {
		paramPlans = append(paramPlans, &ParamPlan{
			paramType:        ParamTypeFieldFullResult,
			dependentFieldId: parentFieldId,
		})
	}
	//如果父节点类型是静态类型，参数中增加常量参数并直接将结果写入
	if parentTypeScope != nil && parentTypeScope.dynamicTypeResolver == nil && parentTypeScope.staticTypeName != "" {
		typeNameParamPlan := builder.newConstParamPlan(DefaultParamKeyTypename, parentTypeScope.staticTypeName)
		paramPlans = append(paramPlans, typeNameParamPlan)
	}
	//parentKeyFieldName
	parentKeyFieldName := builder.checkAndParseParentKeyFieldNames(parentFieldIsList, parentTypeScope)

	fieldPlan := &FieldPlan{
		fieldId:                 fieldId,
		parentFieldId:           parentFieldId,
		fieldName:               IntrospectionFieldNameTypename,
		responseName:            responseName,
		paths:                   paths,
		fieldValueMetaInfo:      fieldValueMetaInfo,
		resultParentKeyName:     "",
		parentKeyFieldName:      parentKeyFieldName,
		childrenFields:          childrenFields,
		paramPlans:              paramPlans,
		resolverFunc:            TypeNameResolverFunc,
		arrParamPlans:           nil,
		arrayResolverFunc:       nil,
		allowedRuntimeTypeNames: parentTypeScope.GetAllowedTypeNamesForField(),
		runtimeTypeResolverFunc: parentTypeScope.GetDynamicTypeResolverFunction(),
		compiledTypeName:        parentTypeScope.GetStaticTypeName(),
		directivePlans:          directivePlans,
		directiveParamPlans:     directiveDependencyParams,
	}
	return []*FieldPlan{fieldPlan}, nil
}

func (builder *PlanBuilder) parseIntrospectionMetaTypeField(current ast.Field, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, directivePlans []*DirectivePlan, directiveDependencyPlans []*ParamPlan) ([]*FieldPlan, error) {
	//检查参数
	if parentFieldId != 0 || parentTypeScope == nil || parentTypeScope.declaredType != builder.schema.QueryType() {
		return nil, fmt.Errorf("invalid __type field")
	}
	//responseName
	responseName := IntrospectionFieldNameMetaType
	if current.Alias != nil && current.Alias.Value != "" {
		responseName = current.Alias.Value
	}
	//fieldId
	fieldId := builder.generateFieldId()
	//path
	paths := append(parentPaths, responseName)
	fieldDef, fdErr := builder.getFieldDefinition(parentTypeScope.declaredType, current.Name.Value)
	if fdErr != nil {
		return nil, fdErr
	}
	//fieldValueMetaInfo
	fieldValueMetaInfo, fieldValueMetaInfoErr := builder.parseFieldValueMetaInfo(fieldDef)
	if fieldValueMetaInfoErr != nil {
		return nil, fieldValueMetaInfoErr
	}
	if fieldValueMetaInfo == nil {
		return nil, fmt.Errorf("parse field value meta info error")
	}
	//typeRuntimeScope
	currentTypeRuntimeScope, currentTypeRuntimeScopeErr := builder.WrapStaticTypeScope(graphql.TypeType)
	if currentTypeRuntimeScopeErr != nil {
		return nil, currentTypeRuntimeScopeErr
	}
	//childrenFields
	childrenFields, childrenFieldsErr := builder.buildIntrospectionSelectionSet(current.GetSelectionSet(), currentTypeRuntimeScope, fieldId, fieldValueMetaInfo.IsList, paths, directivePlans, directiveDependencyPlans)
	if childrenFieldsErr != nil {
		return nil, childrenFieldsErr
	}
	//resolverFunc
	resolverFunc := func(source any, params map[string]any, ctx context.Context) (any, error) {
		name, _ := params["name"].(string)
		if name == "" {
			return nil, nil
		}
		t := builder.schema.Type(name)
		if t == nil {
			return nil, nil
		}
		return GenerateTypeMetaResult(builder.schema, t, childrenFields, builder.originalInputs), nil
	}
	fieldPlan := &FieldPlan{
		fieldId:                 fieldId,
		parentFieldId:           0,
		fieldName:               IntrospectionFieldNameMetaType,
		responseName:            responseName,
		paths:                   paths,
		fieldValueMetaInfo:      *fieldValueMetaInfo,
		childrenFields:          childrenFields,
		resolverFunc:            resolverFunc,
		allowedRuntimeTypeNames: parentTypeScope.GetAllowedTypeNamesForField(),
		runtimeTypeResolverFunc: parentTypeScope.GetDynamicTypeResolverFunction(),
		compiledTypeName:        parentTypeScope.GetStaticTypeName(),
		directivePlans:          directivePlans,
		directiveParamPlans:     directiveDependencyPlans,
	}
	return []*FieldPlan{
		fieldPlan,
	}, nil
}

func (builder *PlanBuilder) parseIntrospectionMetaSchemaField(current ast.Field, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, directives []*DirectivePlan, directiveDependencyParams []*ParamPlan) ([]*FieldPlan, error) {
	//检查入参
	if parentFieldId != 0 || parentTypeScope == nil || parentTypeScope.declaredType != builder.schema.QueryType() {
		return nil, fmt.Errorf("invalid schema field")
	}
	responseName := IntrospectionFieldNameMetaSchema
	if current.Alias != nil && current.Alias.Value != "" {
		responseName = current.Alias.Value
	}
	//fieldId
	fieldId := builder.generateFieldId()
	//paths
	paths := append(parentPaths, responseName)
	fieldDefinition, fdErr := builder.getFieldDefinition(parentTypeScope.declaredType, current.Name.Value)
	if fdErr != nil {
		return nil, fdErr
	}
	//fieldValueMetaInfo
	fieldValueMetaInfo, fieldValueMetaInfoErr := builder.parseFieldValueMetaInfo(fieldDefinition)
	if fieldValueMetaInfoErr != nil {
		return nil, fieldValueMetaInfoErr
	}
	if fieldValueMetaInfo == nil {
		return nil, fmt.Errorf("parse field value meta info error")
	}
	//typeRuntimeScope
	currentTypeRuntimeScope, currentTypeRuntimeScopeErr := builder.WrapStaticTypeScope(graphql.SchemaType)
	if currentTypeRuntimeScopeErr != nil {
		return nil, currentTypeRuntimeScopeErr
	}
	//childrenFields
	childrenFields, childrenFieldsErr := builder.buildIntrospectionSelectionSet(current.GetSelectionSet(), currentTypeRuntimeScope, fieldId, fieldValueMetaInfo.IsList, paths, directives, directiveDependencyParams)
	if childrenFieldsErr != nil {
		return nil, childrenFieldsErr
	}
	//resolverFunc
	resolverFunc := func(source any, params map[string]any, ctx context.Context) (any, error) {
		return GenerateSchemaMetaResult(builder.schema, childrenFields, builder.originalInputs), nil
	}

	fieldPlan := &FieldPlan{
		fieldId:                 fieldId,
		parentFieldId:           0,
		fieldName:               IntrospectionFieldNameMetaSchema,
		responseName:            responseName,
		paths:                   paths,
		fieldValueMetaInfo:      *fieldValueMetaInfo,
		childrenFields:          childrenFields,
		resolverFunc:            resolverFunc,
		allowedRuntimeTypeNames: parentTypeScope.GetAllowedTypeNamesForField(),
		runtimeTypeResolverFunc: parentTypeScope.GetDynamicTypeResolverFunction(),
		compiledTypeName:        parentTypeScope.GetStaticTypeName(),
		directivePlans:          directives,
		directiveParamPlans:     directiveDependencyParams,
	}
	return []*FieldPlan{fieldPlan}, nil
}

func (builder *PlanBuilder) parseField(current ast.Field, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, directivePlans []*DirectivePlan, directiveDependencyParams []*ParamPlan) ([]*FieldPlan, error) {
	var fieldId uint32
	var responseName string
	var paths []string
	var parentKeyFieldName string
	var paramPlans []*ParamPlan
	var resultParentKeyName string

	//responseName
	if current.Alias != nil {
		responseName = current.Alias.Value
	} else {
		responseName = current.Name.Value
	}
	//fieldValueMetaInfo
	fieldDefinition, fdErr := builder.getFieldDefinition(parentTypeScope.declaredType, current.Name.Value)
	if fdErr != nil {
		return nil, fdErr
	}
	fieldValueMetaInfo, fieldValueMetaInfoErr := builder.parseFieldValueMetaInfo(fieldDefinition)
	if fieldValueMetaInfoErr != nil {
		return nil, fieldValueMetaInfoErr
	}
	if fieldValueMetaInfo == nil {
		return nil, fmt.Errorf("parse field value meta info error")
	}
	//paths
	paths = append(parentPaths, responseName)
	//parentKeyFieldNames
	parentKeyFieldName = builder.checkAndParseParentKeyFieldNames(parentFieldIsList, parentTypeScope)
	//父节点是List，当前节点的parentKeyFieldName不能为空
	if parentFieldIsList && parentKeyFieldName == "" {
		return nil, fmt.Errorf("parent key field names for result binding is empty")
	}

	//paramPlans
	//paramPlans, paramPlansErr := builder.parseParamPlans(current.Arguments)
	paramPlans, paramPlansErr := builder.parseParamPlansByArgDefs(fieldDefinition.Args, current.Arguments)
	if paramPlansErr != nil {
		return nil, paramPlansErr
	}
	//arrParamPlans
	//arrayParamPlans, arrayParamPlansErr := builder.parseParamPlans(current.Arguments)
	arrayParamPlans, arrayParamPlansErr := builder.parseParamPlansByArgDefs(fieldDefinition.Args, current.Arguments)
	if arrayParamPlansErr != nil {
		return nil, arrayParamPlansErr
	}

	//resultParentKeyName
	resultParentKeyName = fieldDefinition.BatchResultMappedFieldName
	if fieldDefinition.BatchResolve != nil && fieldDefinition.BatchResultMappedFieldName == "" {
		return nil, fmt.Errorf("result parent key field name for result binding is empty")
	}

	//TypeRuntimeScope
	var fieldTypeRuntimeScope *TypeRuntimeScope
	var typeRuntimeScopeErr error
	originalType := fieldValueMetaInfo.GetBaseElementOriginalType()
	switch fieldType := originalType.(type) {
	case *graphql.Object:
		fieldTypeRuntimeScope, typeRuntimeScopeErr = builder.WrapStaticTypeScope(fieldType)
	case graphql.Abstract:
		fieldTypeRuntimeScope, typeRuntimeScopeErr = builder.WrapDynamicTypeScope(fieldType)
	default:
		fieldTypeRuntimeScope = &TypeRuntimeScope{}
	}
	if typeRuntimeScopeErr != nil {
		return nil, typeRuntimeScopeErr
	}
	needParentFieldFullResult := false
	//如果是抽象类型字段，需要动态类型判定，默认添加对父节点全部数据的依赖
	if fieldTypeRuntimeScope.dynamicTypeResolver != nil {
		needParentFieldFullResult = true
	}
	//当父字段是List，默认当前参数列表中增加对父节点结果默认当前参数列表中增加对父节点结果的依赖的依赖
	if !needParentFieldFullResult && parentFieldIsList {
		needParentFieldFullResult = true
	}
	if needParentFieldFullResult {
		if len(paramPlans) > 0 {
			hasFullResultParam := false
			for _, paramPlan := range paramPlans {
				if paramPlan.paramType == ParamTypeFieldFullResult {
					hasFullResultParam = true
					break
				}
			}
			if !hasFullResultParam {
				fullResultParam := &ParamPlan{
					paramType:        ParamTypeFieldFullResult,
					dependentFieldId: parentFieldId,
				}
				paramPlans = append(paramPlans, fullResultParam)
			}
		}
		if len(arrayParamPlans) > 0 {
			hasFullResultParam := false
			for _, arrParamPlan := range arrayParamPlans {
				if arrParamPlan.paramType == ParamTypeFieldFullResult {
					hasFullResultParam = true
					break
				}
			}
			if !hasFullResultParam {
				fullResultParam := &ParamPlan{
					paramType:        ParamTypeFieldFullResult,
					dependentFieldId: parentFieldId,
				}
				arrayParamPlans = append(arrayParamPlans, fullResultParam)
			}
		}
	}
	//fieldId
	fieldId = builder.generateFieldId()
	//childrenFields
	childrenFields, childrenFieldErr := builder.buildSelectionSetWithFlattenEntries(current.GetSelectionSet(), fieldTypeRuntimeScope, fieldId, fieldValueMetaInfo.IsList, paths, directivePlans, false)
	if childrenFieldErr != nil {
		return nil, childrenFieldErr
	}

	fieldPlan := &FieldPlan{
		fieldId:                 fieldId,
		fieldName:               current.Name.Value,
		responseName:            responseName,
		paths:                   paths,
		fieldValueMetaInfo:      *fieldValueMetaInfo,
		parentFieldId:           parentFieldId,
		resultParentKeyName:     resultParentKeyName,
		parentKeyFieldName:      parentKeyFieldName,
		childrenFields:          childrenFields,
		paramPlans:              paramPlans,
		resolverFunc:            builder.wrapResolverFunc(fieldDefinition.Resolve),
		arrParamPlans:           arrayParamPlans,
		arrayResolverFunc:       builder.wrapResolverFunc(fieldDefinition.BatchResolve),
		allowedRuntimeTypeNames: parentTypeScope.GetAllowedTypeNamesForField(),
		runtimeTypeResolverFunc: parentTypeScope.GetDynamicTypeResolverFunction(),
		compiledTypeName:        parentTypeScope.GetStaticTypeName(),
		directivePlans:          directivePlans,
		directiveParamPlans:     directiveDependencyParams,
	}

	return []*FieldPlan{fieldPlan}, nil
}

func (builder *PlanBuilder) newConstParamPlan(paramName string, constValue any) *ParamPlan {
	return &ParamPlan{
		paramKey:   paramName,
		paramType:  ParamTypeConst,
		constValue: constValue,
	}
}

func (builder *PlanBuilder) newInputParamPlan(paramName string, inputName string) *ParamPlan {
	return &ParamPlan{
		paramKey:  paramName,
		paramType: ParamTypeInput,
		inputName: inputName,
	}
}

func (builder *PlanBuilder) newFieldResultParamPlan(paramKey string, dependentFieldId uint32, paths []string) *ParamPlan {
	return &ParamPlan{
		paramKey:         paramKey,
		paramType:        ParamTypeFieldResult,
		dependentFieldId: dependentFieldId,
		fieldResultPaths: paths,
	}
}

func (builder *PlanBuilder) newFieldFullResultParamPlan(dependentFieldId uint32) *ParamPlan {
	return &ParamPlan{
		paramType:        ParamTypeFieldFullResult,
		dependentFieldId: dependentFieldId,
	}
}

func (builder *PlanBuilder) WrapFieldType2Scope(fieldType graphql.Type) (*TypeRuntimeScope, error) {
	if fieldType == nil {
		return &TypeRuntimeScope{}, nil
	}
	switch typedField := fieldType.(type) {
	case *graphql.Object:
		return builder.WrapStaticTypeScope(typedField)
	case *graphql.Interface:
		return builder.WrapDynamicTypeScope(typedField)
	case *graphql.Union:
		return builder.WrapDynamicTypeScope(typedField)
	default:
		return &TypeRuntimeScope{}, nil
	}
}

func GetAstResponseName(field *ast.Field) string {
	if field != nil && field.Alias != nil && field.Alias.Value != "" {
		return field.Alias.Value
	}
	if field != nil && field.Name != nil {
		return field.Name.Value
	}
	return ""
}

func (builder *PlanBuilder) parseOperationVariables(schema *graphql.Schema, defs []*ast.VariableDefinition, originalInputs map[string]any) (map[string]any, error) {
	if originalInputs == nil {
		originalInputs = make(map[string]any)
	}

	result := make(map[string]any)

	for _, def := range defs {
		name := def.Variable.Name.Value
		inputType, inputTypeErr := InputTypeFromAST(schema, def.Type)
		if inputTypeErr != nil {
			return nil, inputTypeErr
		}

		variableValue, provided := originalInputs[name]
		if !provided {
			if def.DefaultValue != nil {
				defaultValue, defaultValueErr := ValueFromAST(def.DefaultValue, inputType, nil)
				if defaultValueErr != nil {
					return nil, defaultValueErr
				}
				result[name] = defaultValue
				continue
			}

			if IsNonNullInput(inputType) {
				return nil, fmt.Errorf("non-null input variable %s is required", name)
			}

			continue
		}

		if variableValue == nil {
			if IsNonNullInput(inputType) {
				return nil, fmt.Errorf("non-null input variable %s is required", name)
			}

			result[name] = nil
			continue
		}

		parsed, err := builder.parseInputValue(inputType, variableValue)
		if err != nil {
			return nil, err
		}
		result[name] = parsed
	}
	return result, nil
}

func (builder *PlanBuilder) parseInputValue(inputType graphql.Input, source any) (any, error) {
	if nonNullType, ok := inputType.(*graphql.NonNull); ok {
		if source == nil {
			return nil, fmt.Errorf("non null input value is required")
		}

		inner, innerOk := nonNullType.OfType.(graphql.Input)
		if !innerOk {
			return nil, fmt.Errorf("non null input value is required")
		}

		return builder.parseInputValue(inner, source)
	}

	if source == nil {
		return nil, nil
	}

	switch t := inputType.(type) {
	case *graphql.List:
		inner := t.OfType.(graphql.Input)

		if IsSlice(source) {
			sourceItems := toAnySlice(source)
			result := make([]any, 0, len(sourceItems))

			for _, sourceItem := range sourceItems {
				item, err := builder.parseInputValue(inner, sourceItem)
				if err != nil {
					return nil, err
				}

				result = append(result, item)
			}
			return result, nil
		}

		single, err := builder.parseInputValue(inner, source)
		if err != nil {
			return nil, err
		}

		return []any{single}, nil

	case *graphql.InputObject:
		sourceMap, ok := source.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("input value is required")
		}

		fieldDefs := t.Fields()
		result := map[string]any{}

		for sourceFieldName := range sourceMap {
			if _, ok := fieldDefs[sourceFieldName]; !ok {
				return nil, fmt.Errorf("unknown field %s", sourceFieldName)
			}
		}

		for fieldName, fieldDef := range fieldDefs {
			sourceFieldValue, provided := sourceMap[fieldName]
			if !provided {
				if fieldDef.DefaultValue != nil {
					result[fieldName] = fieldDef.DefaultValue
					continue
				}

				if IsNonNullInput(fieldDef.Type) {
					return nil, fmt.Errorf("non-null input variable %s is required", fieldName)
				}

				continue
			}

			parsedFieldValue, parsedFieldValueErr := builder.parseInputValue(fieldDef.Type, sourceFieldValue)
			if parsedFieldValueErr != nil {
				return nil, parsedFieldValueErr
			}
			result[fieldName] = parsedFieldValue
		}
		return result, nil
	case *graphql.Scalar:
		parsed := t.ParseValue(source)
		if parsed == nil {
			return nil, fmt.Errorf("scalar is required")
		}
		return parsed, nil
	case *graphql.Enum:
		parsed := t.ParseValue(source)
		if parsed == nil {
			return nil, fmt.Errorf("enum is required")
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("unsupported input type %T", t)
	}
}

func (builder *PlanBuilder) directiveLocationAllowed(def *graphql.Directive, location string) bool {
	for _, allowed := range def.Locations {
		if allowed == location {
			return true
		}
	}
	return false
}

func (builder *PlanBuilder) validateDirectiveUsages(directiveASTs []*ast.Directive, location string) error {
	counting := map[string]int{}

	for _, directiveAST := range directiveASTs {
		name := directiveAST.Name.Value

		def := builder.schema.Directive(name)
		if def == nil {
			return fmt.Errorf("unknown directive %s", name)
		}

		if !builder.directiveLocationAllowed(def, location) {
			return fmt.Errorf("unknown location %s", location)
		}

		counting[name]++

		if counting[name] > 1 {
			return fmt.Errorf("too many counting directives for %s", name)
		}
	}
	return nil
}

func (builder *PlanBuilder) compileDirectives(directiveASTs []*ast.Directive, location string, inherited []*DirectivePlan) (*DirectiveCompileResult, error) {
	result := &DirectiveCompileResult{
		RuntimePlans: append([]*DirectivePlan{}, inherited...),
	}

	if err := builder.validateDirectiveUsages(directiveASTs, location); err != nil {
		return nil, err
	}

	for _, directiveAST := range directiveASTs {
		name := directiveAST.Name.Value
		def := builder.schema.Directive(name)

		args, err := builder.buildDirectiveArgs(def.Args, directiveAST.Arguments)
		if err != nil {
			return nil, err
		}

		compiler := builder.directiveRegistry.Compiler(name)

		if compiler == nil {
			if builder.directiveRegistry.MetadataOnly(name) {
				result.RuntimePlans = append(result.RuntimePlans, &DirectivePlan{
					Name:           name,
					Args:           args,
					Location:       location,
					Stage:          DirectiveStageMetadataOnly,
					RuntimeHandler: DefaultEmptyDirectiveRuntimeHandler{},
				})
				continue
			}
			return nil, fmt.Errorf("unknown compiler for %s", name)
		}

		compiled, compiledErr := compiler.Compile(name, location, args, builder.originalInputs, builder.schema)
		if compiledErr != nil {
			return nil, compiledErr
		}

		builder.bindRuntimeHandlers(compiled)
		builder.mergeDirectiveCompileResult(result, compiled)
	}
	return result, nil
}

func (builder *PlanBuilder) buildDirectiveArgs(argDefs []*graphql.Argument, argASTs []*ast.Argument) (map[string]any, error) {
	defMap := map[string]*graphql.Argument{}
	for _, argDef := range argDefs {
		defMap[argDef.PrivateName] = argDef
	}

	astMap := map[string]*ast.Argument{}
	for _, argAST := range argASTs {
		name := argAST.Name.Value

		if _, ok := defMap[name]; !ok {
			return nil, fmt.Errorf("unknown argument %s", name)
		}

		astMap[name] = argAST
	}

	result := make(map[string]any)

	for name, def := range defMap {
		argAST, provided := astMap[name]

		if provided {
			value, err := ValueFromAST(argAST.Value, def.Type, builder.originalInputs)
			if err != nil {
				return nil, err
			}

			parsed, parsedErr := builder.parseInputValue(def.Type, value)
			if parsedErr != nil {
				return nil, parsedErr
			}

			result[name] = parsed
			continue
		}

		if def.DefaultValue != nil {
			result[name] = def.DefaultValue
			continue
		}

		if IsNonNullInput(def.Type) {
			return nil, fmt.Errorf("non-null input variable %s is required", name)
		}
	}
	return result, nil
}

func (builder *PlanBuilder) bindRuntimeHandlers(compiled *DirectiveCompileResult) {
	if compiled == nil {
		return
	}

	for _, plan := range compiled.RuntimePlans {
		if plan.RuntimeHandler != nil {
			continue
		}

		handler := builder.directiveRegistry.RuntimeHandler(plan.Name)
		if handler != nil {
			plan.RuntimeHandler = handler
			continue
		}

		if plan.Stage == DirectiveStageMetadataOnly {
			plan.RuntimeHandler = &DefaultEmptyDirectiveRuntimeHandler{}
			continue
		}
	}
}

func (builder *PlanBuilder) mergeDirectiveCompileResult(dst *DirectiveCompileResult, src *DirectiveCompileResult) {
	if src.IncludeDecision != nil && !*src.IncludeDecision {
		include := false
		dst.IncludeDecision = &include
	}

	dst.RuntimePlans = append(dst.RuntimePlans, src.RuntimePlans...)
	dst.DependencyParamPlans = append(dst.DependencyParamPlans, src.DependencyParamPlans...)
}

func InputTypeFromAST(schema *graphql.Schema, typeAST ast.Type) (graphql.Input, error) {
	if schema == nil {
		return nil, fmt.Errorf("schema is nil")
	}
	if typeAST == nil {
		return nil, fmt.Errorf("type is nil")
	}

	switch tt := typeAST.(type) {
	case *ast.Named:
		if tt.Name == nil || tt.Name.Value == "" {
			return nil, fmt.Errorf("name is nil")
		}

		typeFound := schema.Type(tt.Name.Value)
		if typeFound == nil {
			return nil, fmt.Errorf("type is nil")
		}

		inputType, ok := typeFound.(graphql.Input)
		if !ok {
			return nil, fmt.Errorf("type is not an input type")
		}
		return inputType, nil
	case *ast.List:
		inner, err := InputTypeFromAST(schema, tt.Type)
		if err != nil {
			return nil, err
		}
		return graphql.NewList(inner), nil
	case *ast.NonNull:
		inner, err := InputTypeFromAST(schema, tt.Type)
		if err != nil {
			return nil, err
		}

		//object!!这种没意义的要过滤掉
		_, ok := inner.(graphql.Nullable)
		if !ok {
			return nil, fmt.Errorf("non-nullable type's element type is not an non-nullable type")
		}
		return graphql.NewNonNull(inner), nil
	default:
		return nil, fmt.Errorf("unknown type %T", typeAST)
	}
}

func ValueFromAST(valueAST ast.Value, inputType graphql.Input, originalInputs map[string]any) (any, error) {
	if valueAST == nil {
		return nil, fmt.Errorf("value is nil")
	}

	switch t := inputType.(type) {
	case *graphql.NonNull:
		value, err := ValueFromAST(valueAST, t.OfType.(graphql.Input), originalInputs)
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
				item, err := ValueFromAST(value, t.OfType, originalInputs)
				if err != nil {
					return nil, err
				}
				values = append(values, item)
			}
			return values, nil
		}

		single, err := ValueFromAST(valueAST, t.OfType.(graphql.Input), originalInputs)
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

				if IsNonNullInput(fieldDef.Type) {
					return nil, fmt.Errorf("field %s is not a non-nullable field", fieldName)
				}

				continue
			}

			fieldValue, fieldValueErr := ValueFromAST(fieldAST, fieldDef.Type, originalInputs)
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

func IsNonNullInput(t graphql.Input) bool {
	_, ok := t.(*graphql.NonNull)
	return ok
}

func IsSlice(v any) bool {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return false
	}
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	return rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array
}

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

func operationDirectiveLocation(operation string) string {
	switch operation {
	case ast.OperationTypeMutation:
		return graphql.DirectiveLocationMutation
	case ast.OperationTypeSubscription:
		return graphql.DirectiveLocationSubscription
	default:
		return graphql.DirectiveLocationQuery
	}
}

func allowedTypeHash(scope *TypeRuntimeScope) string {
	if scope == nil {
		return ""
	}
	names := make([]string, 0, len(scope.allowedDynamicTypes))
	for name := range scope.allowedDynamicTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

type DirectiveRegistry struct {
	compilers    map[string]DirectiveCompiler
	handlers     map[string]DirectiveRuntimeHandler
	metadataOnly map[string]bool
}

func NewDirectiveRegistry() *DirectiveRegistry {
	result := &DirectiveRegistry{
		compilers:    make(map[string]DirectiveCompiler),
		handlers:     make(map[string]DirectiveRuntimeHandler),
		metadataOnly: make(map[string]bool),
	}
	//注册默认的skip和include两个directive
	result.Register("skip", SkipDirectiveCompiler{}, nil)
	result.Register("include", IncludeDirectiveCompiler{}, nil)
	return result
}

func (r *DirectiveRegistry) Register(name string, compiler DirectiveCompiler, handler DirectiveRuntimeHandler) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}

	if compiler != nil {
		r.compilers[name] = compiler
	}
	if handler != nil {
		r.handlers[name] = handler
	}

	return nil
}

func (r *DirectiveRegistry) Compiler(name string) DirectiveCompiler {
	if r == nil || r.compilers == nil {
		return nil
	}
	return r.compilers[name]
}

func (r *DirectiveRegistry) RuntimeHandler(name string) DirectiveRuntimeHandler {
	if r == nil || r.handlers == nil {
		return nil
	}
	return r.handlers[name]
}

func (r *DirectiveRegistry) SetMetadataOnly(name string, metadataOnly bool) {
	r.metadataOnly[name] = metadataOnly
}

func (r *DirectiveRegistry) MetadataOnly(name string) bool {
	return r != nil && r.metadataOnly != nil && r.metadataOnly[name]
}
