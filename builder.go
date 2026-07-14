package graphql

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/graphql-go/graphql/language/ast"
)

type DynamicTypeResolverFunction func(value any, context *context.Context) string

// 类型上下文结构体，在build阶段生成，在execute阶段运行，用于类型推断
type TypeRuntimeScope struct {
	declaredType        any
	allowedDynamicTypes map[string]*Object
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

func (tr *TypeRuntimeScope) GetAllowedDynamicTypes() map[string]*Object {
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
	schema            *Schema
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

func BuildGraphPlan(document *ast.Document, schema *Schema, operationName *string, registry *DirectiveRegistry) (*SGraphPlan, error) {
	//参数检查
	if document == nil {
		return nil, errors.New("no document provided")
	}
	if schema == nil {
		return nil, errors.New("no schema provided")
	}
	result := &SGraphPlan{}
	planBuilder := &PlanBuilder{
		schema:         schema,
		fragments:      make(map[string]*ast.FragmentDefinition),
		fieldIdCounter: atomic.Uint32{},
		originalInputs: nil,
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

	if operationDefinition == nil {
		return nil, errors.New("no operation definition provided")
	}
	result.operation = operationDefinition
	result.fragments = make(map[string]ast.Definition, len(fragments))
	for name, fragment := range fragments {
		result.fragments[name] = fragment
	}

	//确定RootType
	var rootNodeType *Object
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
	result.maxFieldId = calculateMaxFieldId(fieldPlans)

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

	return builder.buildFieldPlansFromEntries(fieldEntries, parentFieldId, parentFieldIsList, parentPaths, isIntrospection)
}

func (builder *PlanBuilder) buildFieldPlansFromEntries(fieldEntries []FieldEntry, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, isIntrospection bool) ([]*FieldPlan, error) {
	type groupKey struct {
		responseName    string
		allowedTypeHash string
	}
	type group struct {
		entries          []FieldEntry
		typeScope        *TypeRuntimeScope
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
				entries:          []FieldEntry{entry},
				typeScope:        entry.typeScope,
				dependencyParams: entry.dependencyParams,
			}
		} else {
			g.entries = append(g.entries, entry)
			//dependencyParams 合并所有出现
			g.dependencyParams = append(g.dependencyParams, entry.dependencyParams...)
		}
	}

	result := make([]*FieldPlan, 0)
	for _, key := range orderedKeys {
		g := groups[key]

		rep := g.entries[0].field
		conditionalDirectiveGroups := make([][]*DirectivePlan, 0, len(g.entries))
		for _, entry := range g.entries {
			//每个重复字段 occurrence 都保留自己的 @skip/@include 条件，运行期按 OR 语义判断。
			conditionalDirectives, _ := splitConditionalDirectives(entry.directives)
			conditionalDirectiveGroups = append(conditionalDirectiveGroups, conditionalDirectives)
		}
		_, runtimeDirectives := splitConditionalDirectives(g.entries[0].directives)

		fieldName := ""
		if rep.Name != nil {
			fieldName = rep.Name.Value
		}

		var fieldPlans []*FieldPlan
		var fieldErr error

		switch fieldName {
		case IntrospectionFieldNameTypename:
			fieldPlans, fieldErr = builder.parseIntrospectionTypenameField(rep, g.typeScope, parentFieldId, parentFieldIsList, parentPaths, runtimeDirectives, g.dependencyParams, conditionalDirectiveGroups)
		case IntrospectionFieldNameMetaType:
			fieldPlans, fieldErr = builder.parseIntrospectionMetaTypeField(rep, g.typeScope, parentFieldId, parentFieldIsList, parentPaths, runtimeDirectives, g.dependencyParams, conditionalDirectiveGroups, g.entries)
		case IntrospectionFieldNameMetaSchema:
			fieldPlans, fieldErr = builder.parseIntrospectionMetaSchemaField(rep, g.typeScope, parentFieldId, parentFieldIsList, parentPaths, runtimeDirectives, g.dependencyParams, conditionalDirectiveGroups, g.entries)
		default:
			if isIntrospection {
				var fieldPlan *FieldPlan
				fieldPlan, fieldErr = builder.buildIntrospectionFieldPlan(&rep, g.typeScope, parentFieldId, parentFieldIsList, parentPaths, runtimeDirectives, g.dependencyParams, conditionalDirectiveGroups, g.entries)
				if fieldErr == nil {
					fieldPlans = []*FieldPlan{fieldPlan}
				}
			} else {
				fieldPlans, fieldErr = builder.parseField(rep, g.typeScope, parentFieldId, parentFieldIsList, parentPaths, runtimeDirectives, g.dependencyParams, conditionalDirectiveGroups, g.entries)
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
			DirectiveLocationField,
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
		spreadCompiled, err := builder.compileDirectives(sel.Directives, DirectiveLocationFragmentSpread, inheritedDirectives)
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
		fragmentCompiled, err := builder.compileDirectives(frag.Directives, DirectiveLocationFragmentDefinition, spreadCompiled.RuntimePlans)
		if err != nil {
			return nil, err
		}
		return builder.flattenSelections(frag.SelectionSet, croppedScope, fragmentCompiled.RuntimePlans)
	case *ast.InlineFragment:
		compiled, err := builder.compileDirectives(sel.Directives, DirectiveLocationInlineFragment, inheritedDirectives)
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

func (builder *PlanBuilder) flattenChildEntriesForField(fieldEntries []FieldEntry, childTypeScope *TypeRuntimeScope) ([]FieldEntry, error) {
	if len(fieldEntries) == 0 || childTypeScope == nil {
		return nil, nil
	}

	var result []FieldEntry
	for _, entry := range fieldEntries {
		if entry.field.SelectionSet == nil {
			continue
		}
		// 子选择必须继承自己所属字段 occurrence 的指令条件，避免重复 responseName 合并后条件串线。
		children, err := builder.flattenSelections(entry.field.SelectionSet, childTypeScope, entry.directives)
		if err != nil {
			return nil, err
		}
		result = append(result, children...)
	}
	return result, nil
}

func splitConditionalDirectives(plans []*DirectivePlan) ([]*DirectivePlan, []*DirectivePlan) {
	conditionalPlans := make([]*DirectivePlan, 0)
	otherPlans := make([]*DirectivePlan, 0, len(plans))

	for _, plan := range plans {
		if plan == nil {
			continue
		}
		// 只有标准 @skip/@include 参与 field collection 的 occurrence 条件语义。
		if plan.Stage == DirectiveStageShouldExecute && (plan.Name == "skip" || plan.Name == "include") {
			conditionalPlans = append(conditionalPlans, plan)
			continue
		}
		otherPlans = append(otherPlans, plan)
	}

	return conditionalPlans, otherPlans
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
			DirectiveLocationField,
			inheritedDirectives,
		)
		if err != nil {
			return nil, err
		}
		conditionalDirectives, runtimeDirectives := splitConditionalDirectives(compiled.RuntimePlans)
		conditionalDirectiveGroups := [][]*DirectivePlan{conditionalDirectives}

		//__typename
		if selectionType.Name != nil && selectionType.Name.Value == IntrospectionFieldNameTypename {
			typenameFieldPlans, typenameFieldPlanErr := builder.parseIntrospectionTypenameField(*selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, runtimeDirectives, compiled.DependencyParamPlans, conditionalDirectiveGroups)
			if typenameFieldPlanErr != nil {
				return nil, typenameFieldPlanErr
			}
			return typenameFieldPlans, nil
		}
		//__type
		if selectionType.Name != nil && selectionType.Name.Value == IntrospectionFieldNameMetaType {
			typeFieldPlans, typeFieldPlansErr := builder.parseIntrospectionMetaTypeField(*selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, runtimeDirectives, compiled.DependencyParamPlans, conditionalDirectiveGroups, nil)
			if typeFieldPlansErr != nil {
				return nil, typeFieldPlansErr
			}
			return typeFieldPlans, nil
		}
		//__schema
		if selectionType.Name != nil && selectionType.Name.Value == IntrospectionFieldNameMetaSchema {
			schemaFieldPlans, schemaFieldPlansErr := builder.parseIntrospectionMetaSchemaField(*selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, runtimeDirectives, compiled.DependencyParamPlans, conditionalDirectiveGroups, nil)
			if schemaFieldPlansErr != nil {
				return nil, schemaFieldPlansErr
			}
			return schemaFieldPlans, nil
		}
		//default
		fieldPlans, fieldPlansErr := builder.parseField(*selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, runtimeDirectives, compiled.DependencyParamPlans, conditionalDirectiveGroups, nil)
		if fieldPlansErr != nil {
			return nil, fieldPlansErr
		}
		return fieldPlans, nil
	case *ast.FragmentSpread:
		spreadCompiled, err := builder.compileDirectives(selectionType.Directives, DirectiveLocationFragmentSpread, inheritedDirectives)
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
		fragmentCompiled, err := builder.compileDirectives(frag.Directives, DirectiveLocationFragmentDefinition, spreadCompiled.RuntimePlans)
		if err != nil {
			return nil, err
		}
		return builder.buildSelectionSet(frag.SelectionSet, croppedScope, parentFieldId, parentFieldIsList, parentPaths, fragmentCompiled.RuntimePlans)
	case *ast.InlineFragment:
		compiled, err := builder.compileDirectives(selectionType.Directives, DirectiveLocationInlineFragment, inheritedDirectives)
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
		conditionalDirectives, runtimeDirectives := splitConditionalDirectives(inheritedDirectives)
		conditionalDirectiveGroups := [][]*DirectivePlan{conditionalDirectives}
		if selectionType.Name.Value == IntrospectionFieldNameTypename {
			return builder.parseIntrospectionTypenameField(*selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, runtimeDirectives, directiveDependencyParams, conditionalDirectiveGroups)
		}
		if strings.HasPrefix(selectionType.Name.Value, "__") {
			return nil, fmt.Errorf("introspection selection found for %s", selectionType.Name.Value)
		}
		fieldPlan, fieldPlanErr := builder.buildIntrospectionFieldPlan(selectionType, parentTypeScope, parentFieldId, parentFieldIsList, parentPaths, runtimeDirectives, directiveDependencyParams, conditionalDirectiveGroups, nil)
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

func (builder *PlanBuilder) buildIntrospectionFieldPlan(current *ast.Field, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, directives []*DirectivePlan, directiveDependencyParams []*ParamPlan, conditionalDirectiveGroups [][]*DirectivePlan, fieldEntries []FieldEntry) (*FieldPlan, error) {
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
		if len(fieldEntries) > 0 {
			var childEntries []FieldEntry
			childEntries, childrenErr = builder.flattenChildEntriesForField(fieldEntries, currentScope)
			if childrenErr == nil {
				children, childrenErr = builder.buildFieldPlansFromEntries(childEntries, fieldId, fieldValueMetaInfo.IsList, paths, true)
			}
		} else {
			children, childrenErr = builder.buildIntrospectionSelectionSet(current.GetSelectionSet(), currentScope, fieldId, fieldValueMetaInfo.IsList, paths, directives, directiveDependencyParams)
		}
		if childrenErr != nil {
			return nil, childrenErr
		}
	}

	return &FieldPlan{
		fieldId:                    fieldId,
		parentFieldId:              parentFieldId,
		fieldName:                  current.Name.Value,
		responseName:               responseName,
		paths:                      paths,
		fieldValueMetaInfo:         *fieldValueMetaInfo,
		fieldASTs:                  fieldASTsForPlan(*current, fieldEntries),
		returnType:                 fieldDef.Type,
		parentType:                 parentCompositeFromScope(parentTypeScope),
		childrenFields:             children,
		paramPlans:                 paramPlans,
		allowedRuntimeTypeNames:    parentTypeScope.GetAllowedTypeNamesForField(),
		runtimeTypeResolverFunc:    parentTypeScope.GetDynamicTypeResolverFunction(),
		compiledTypeName:           parentTypeScope.GetStaticTypeName(),
		directivePlans:             directives,
		directiveParamPlans:        directiveDependencyParams,
		conditionalDirectiveGroups: conditionalDirectiveGroups,
	}, nil
}

func (builder *PlanBuilder) getFieldDefinition(parentType any, fieldName string) (*FieldDefinition, error) {
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
	return nil, nil
}

// 例子：[[scalar!]!]!返回scalar。获取一个字段的基础类型，返回值只能是Object/Scalar/Enum/Interface/Union这几个，封装类型NotNull/List不算
// 返回值分别是:基础类型，是否为isList，是否为NotNull，错误
func (builder *PlanBuilder) parseFieldValueMetaInfo(fd *FieldDefinition) (*FieldValueMetaInfo, error) {
	if fd == nil {
		return nil, errors.New("no type definition provided while building selection set")
	}
	result := &FieldValueMetaInfo{}
	var currentType Output
	currentType = fd.Type

	currentValueMetaInfo := result

	for {
		switch t := currentType.(type) {
		case *NonNull:
			currentType = t.OfType

			currentValueMetaInfo.NotNil = true
			currentValueMetaInfo.OriginalType = t
		case *List:
			currentType = t.OfType

			currentValueMetaInfo.IsList = true
			currentValueMetaInfo.ValueType = FieldValueTypeList
			currentValueMetaInfo.OriginalType = t
			elementType := &FieldValueMetaInfo{}
			currentValueMetaInfo.ElementType = elementType
			currentValueMetaInfo = elementType
		case *Scalar:
			currentValueMetaInfo.ValueType = FieldValueTypeScalar
			currentValueMetaInfo.OriginalType = t
			return result, nil
		case *Object:
			currentValueMetaInfo.ValueType = FieldValueTypeObject
			currentValueMetaInfo.OriginalType = t
			return result, nil
		case *Enum:
			currentValueMetaInfo.ValueType = FieldValueTypeEnum
			currentValueMetaInfo.OriginalType = t
			return result, nil
		case *Interface:
			currentValueMetaInfo.ValueType = FieldValueTypeObject
			currentValueMetaInfo.OriginalType = t
			return result, nil
		case *Union:
			currentValueMetaInfo.ValueType = FieldValueTypeObject
			currentValueMetaInfo.OriginalType = t
			return result, nil
		default:
			return nil, fmt.Errorf("no type found for %s", currentType.Name())
		}
	}
}

// 组装编译期就能确定类型的节点类型上下文
func (builder *PlanBuilder) WrapStaticTypeScope(nodeType *Object) (*TypeRuntimeScope, error) {
	if nodeType == nil {
		return nil, errors.New("no node provided")
	}
	//创建编译期判定类型的类型上下文
	return &TypeRuntimeScope{
		declaredType: nodeType,
		allowedDynamicTypes: map[string]*Object{
			nodeType.Name(): nodeType,
		},
		staticTypeName: nodeType.Name(),
		//编译期和运行期的直接区别就是dynamicTypeResolver为nil，因为不需要动态解析
		dynamicTypeResolver: nil,
	}, nil
}

// 组装运行时动态判定类型的节点类型上下文。uniontype和interface类型的节点都需要走这个方法
func (builder *PlanBuilder) WrapDynamicTypeScope(nodeType Abstract) (*TypeRuntimeScope, error) {
	possibleTypes := builder.schema.PossibleTypes(nodeType)
	result := make(map[string]*Object, len(possibleTypes))
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

func (builder *PlanBuilder) NewDynamicTypeResolverFunction(abs Abstract) DynamicTypeResolverFunction {
	return func(value any, ctx *context.Context) string {
		if value == nil {
			return ""
		}

		switch t := abs.(type) {
		case *Interface:
			if t.ResolveType != nil {
				if obj := t.ResolveType(ResolveTypeParams{Value: value, Context: *ctx}); obj != nil {
					return obj.Name()
				}
			}
		case *Union:
			if t.ResolveType != nil {
				if obj := t.ResolveType(ResolveTypeParams{Value: value, Context: *ctx}); obj != nil {
					return obj.Name()
				}
			}
		}

		for _, possible := range builder.schema.PossibleTypes(abs) {
			if possible.IsTypeOf == nil {
				continue
			}
			if possible.IsTypeOf(IsTypeOfParams{Value: value, Context: *ctx}) {
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
	var possibleTypeMap map[string]*Object
	switch tt := t.(type) {
	case *Object:
		possibleTypeMap = map[string]*Object{t.Name(): tt}
	case Abstract:
		objects := builder.schema.PossibleTypes(tt)
		possibleTypeMap = map[string]*Object{}
		for _, obj := range objects {
			possibleTypeMap[obj.Name()] = obj
		}
	default:
		return nil, fmt.Errorf("unknown type condition found for %s", typeCondition.Name.Value)
	}

	//TODO 将scope里allowedRuntimeType和检索的实现类Type做一个交集，解决内敛嵌套问题
	croppedTypeMap := make(map[string]*Object)
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

func (builder *PlanBuilder) wrapResolverFunc(fieldResolveFn FieldResolveFn) ResolverFunc {
	if fieldResolveFn == nil {
		return nil
	}
	type firstResponseGetter interface {
		GetFirstResponse() any
	}
	return func(source any, params map[string]any, ctx context.Context) (any, error) {
		actualSource := source
		if wrappedSource, ok := source.(firstResponseGetter); ok {
			// 兼容内部 FieldResponse 包装值；GraphSoul 的业务 resolver source 仍由调用点决定。
			actualSource = wrappedSource.GetFirstResponse()
		}
		info := ResolveInfo{}
		if currentInfo := resolveInfoFromContext(ctx); currentInfo != nil {
			// GraphSoul ResolverFunc 签名没有 ResolveInfo，执行前通过 ctx 注入，再还原成 graphql-go 的 ResolveParams。
			info = *currentInfo
		}
		return fieldResolveFn(ResolveParams{
			Source:  actualSource,
			Args:    params,
			Info:    info,
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
		if scalarType, ok := t.(*Scalar); ok && scalarType == ID {
			return ParentKeyFieldNameAsID
		}
	}
	//如果不存在id字段，则遍历全部字段寻找ID类型的字段
	switch t := parentTypeScope.declaredType.(type) {
	case *Object:
		if len(t.Fields()) > 0 {
			for _, field := range t.Fields() {
				fieldValueType, fieldValueTypeErr := builder.parseBaseType(field.Type)
				if fieldValueTypeErr != nil || fieldValueType == nil {
					continue
				}
				if scalarType, ok := fieldValueType.(*Scalar); ok && scalarType == ID {
					return field.Name
				}
			}
		}
	case *Interface:
		if len(t.Fields()) > 0 {
			for _, field := range t.Fields() {
				fieldValueType, fieldValueTypeErr := builder.parseBaseType(field.Type)
				if fieldValueTypeErr != nil || fieldValueType == nil {
					continue
				}
				if scalarType, ok := fieldValueType.(*Scalar); ok && scalarType == ID {
					return field.Name
				}
			}
		}
	default:
		return ""
	}

	return ""
}

func (builder *PlanBuilder) parseBaseType(t Type) (Type, error) {
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

func (builder *PlanBuilder) parseParamPlansByArgDefs(argDefs []*Argument, argASTs []*ast.Argument) ([]*ParamPlan, error) {
	defMap := map[string]*Argument{}
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

		//未提供：默认值烤常量；必填缺失报错；可选跳过（保持原行为）
		if !provided {
			if argDef.DefaultValue != nil {
				parsedDefault, parsedDefaultErr := builder.parseInputValue(argDef.Type, argDef.DefaultValue)
				if parsedDefaultErr != nil {
					return nil, parsedDefaultErr
				}
				result = append(result, NewConstParamPlan(argDef.PrivateName, parsedDefault))
				continue
			}
			if IsNonNullInput(argDef.Type) {
				return nil, fmt.Errorf("required argument %s is missing", argDef.Name())
			}
			continue
		}

		//1) 裸变量 $var → 运行期取（值已在 parseOperationVariables 协变），不烤、缓存安全
		if variable, ok := argAST.Value.(*ast.Variable); ok {
			if variable.Name == nil {
				return nil, fmt.Errorf("invalid variable name")
			}
			result = append(result, NewInputParamPlan(argDef.PrivateName, variable.Name.Value))
			continue
		}

		//2) 含嵌套变量的复合字面量 → 模板，运行期物化、缓存安全
		if astContainsVariable(argAST.Value) {
			result = append(result, NewVariableTemplateParamPlan(argDef.PrivateName, argAST.Value, argDef.Type))
			continue
		}

		//3) 纯字面量 → 编译期烤常量（原行为不变）
		value, valueErr := ValueFromAST(argAST.Value, argDef.Type, builder.originalInputs)
		if valueErr != nil {
			return nil, valueErr
		}
		parsedValue, parsedValueErr := builder.parseInputValue(argDef.Type, value)
		if parsedValueErr != nil {
			return nil, parsedValueErr
		}
		result = append(result, NewConstParamPlan(argDef.PrivateName, parsedValue))
	}
	return result, nil
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

type inputsContextKeyType struct{}

var inputsContextKey = inputsContextKeyType{}

// ContextWithInputs 把本请求（已协变）变量放进 ctx，供内省 resolver 运行期读取。
func ContextWithInputs(ctx context.Context, inputs map[string]any) context.Context {
	return context.WithValue(ctx, inputsContextKey, inputs)
}

// InputsFromContext 取出本请求变量；不存在返回 nil（按未提供处理）。
func InputsFromContext(ctx context.Context) map[string]any {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(inputsContextKey).(map[string]any); ok {
		return v
	}
	return nil
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

func (builder *PlanBuilder) parseIntrospectionTypenameField(current ast.Field, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, directivePlans []*DirectivePlan, directiveDependencyParams []*ParamPlan, conditionalDirectiveGroups [][]*DirectivePlan) ([]*FieldPlan, error) {
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
		OriginalType: String,
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
	returnType := Output(String)
	if TypeNameMetaFieldDef != nil && TypeNameMetaFieldDef.Type != nil {
		returnType = TypeNameMetaFieldDef.Type
	}

	fieldPlan := &FieldPlan{
		fieldId:                    fieldId,
		parentFieldId:              parentFieldId,
		fieldName:                  IntrospectionFieldNameTypename,
		responseName:               responseName,
		paths:                      paths,
		fieldValueMetaInfo:         fieldValueMetaInfo,
		fieldASTs:                  fieldASTsForPlan(current, nil),
		returnType:                 returnType,
		parentType:                 parentCompositeFromScope(parentTypeScope),
		resultParentKeyName:        "",
		parentKeyFieldName:         parentKeyFieldName,
		childrenFields:             childrenFields,
		paramPlans:                 paramPlans,
		resolverFunc:               TypeNameResolverFunc,
		arrParamPlans:              nil,
		arrayResolverFunc:          nil,
		allowedRuntimeTypeNames:    parentTypeScope.GetAllowedTypeNamesForField(),
		runtimeTypeResolverFunc:    parentTypeScope.GetDynamicTypeResolverFunction(),
		compiledTypeName:           parentTypeScope.GetStaticTypeName(),
		directivePlans:             directivePlans,
		directiveParamPlans:        directiveDependencyParams,
		conditionalDirectiveGroups: conditionalDirectiveGroups,
	}
	return []*FieldPlan{fieldPlan}, nil
}

func (builder *PlanBuilder) parseIntrospectionMetaTypeField(current ast.Field, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, directivePlans []*DirectivePlan, directiveDependencyPlans []*ParamPlan, conditionalDirectiveGroups [][]*DirectivePlan, fieldEntries []FieldEntry) ([]*FieldPlan, error) {
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
	fieldDef := TypeMetaFieldDef
	if fieldDef == nil {
		return nil, fmt.Errorf("__type meta field definition is nil")
	}
	//fieldValueMetaInfo
	fieldValueMetaInfo, fieldValueMetaInfoErr := builder.parseFieldValueMetaInfo(fieldDef)
	if fieldValueMetaInfoErr != nil {
		return nil, fieldValueMetaInfoErr
	}
	if fieldValueMetaInfo == nil {
		return nil, fmt.Errorf("parse field value meta info error")
	}
	// __type 是根内省字段，不存在于用户 Query Object 中；
	// 参数定义必须来自 TypeMetaFieldDef，保证 name: String! 能进入运行期 params。
	paramPlans, paramPlansErr := builder.parseParamPlansByArgDefs(fieldDef.Args, current.Arguments)
	if paramPlansErr != nil {
		return nil, paramPlansErr
	}
	//typeRuntimeScope
	currentTypeRuntimeScope, currentTypeRuntimeScopeErr := builder.WrapStaticTypeScope(TypeType)
	if currentTypeRuntimeScopeErr != nil {
		return nil, currentTypeRuntimeScopeErr
	}
	//childrenFields
	var childrenFields []*FieldPlan
	var childrenFieldsErr error
	if len(fieldEntries) > 0 {
		var childEntries []FieldEntry
		childEntries, childrenFieldsErr = builder.flattenChildEntriesForField(fieldEntries, currentTypeRuntimeScope)
		if childrenFieldsErr == nil {
			childrenFields, childrenFieldsErr = builder.buildFieldPlansFromEntries(childEntries, fieldId, fieldValueMetaInfo.IsList, paths, true)
		}
	} else {
		childrenFields, childrenFieldsErr = builder.buildIntrospectionSelectionSet(current.GetSelectionSet(), currentTypeRuntimeScope, fieldId, fieldValueMetaInfo.IsList, paths, directivePlans, directiveDependencyPlans)
	}
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
		return GenerateTypeMetaResult(builder.schema, t, childrenFields, InputsFromContext(ctx)), nil
	}
	fieldPlan := &FieldPlan{
		fieldId:                    fieldId,
		parentFieldId:              0,
		fieldName:                  IntrospectionFieldNameMetaType,
		responseName:               responseName,
		paths:                      paths,
		fieldValueMetaInfo:         *fieldValueMetaInfo,
		fieldASTs:                  fieldASTsForPlan(current, fieldEntries),
		returnType:                 fieldDef.Type,
		parentType:                 parentCompositeFromScope(parentTypeScope),
		childrenFields:             childrenFields,
		paramPlans:                 paramPlans,
		resolverFunc:               resolverFunc,
		allowedRuntimeTypeNames:    parentTypeScope.GetAllowedTypeNamesForField(),
		runtimeTypeResolverFunc:    parentTypeScope.GetDynamicTypeResolverFunction(),
		compiledTypeName:           parentTypeScope.GetStaticTypeName(),
		directivePlans:             directivePlans,
		directiveParamPlans:        directiveDependencyPlans,
		conditionalDirectiveGroups: conditionalDirectiveGroups,
	}
	return []*FieldPlan{
		fieldPlan,
	}, nil
}

func (builder *PlanBuilder) parseIntrospectionMetaSchemaField(current ast.Field, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, directives []*DirectivePlan, directiveDependencyParams []*ParamPlan, conditionalDirectiveGroups [][]*DirectivePlan, fieldEntries []FieldEntry) ([]*FieldPlan, error) {
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
	fieldDefinition := SchemaMetaFieldDef
	if fieldDefinition == nil {
		return nil, fmt.Errorf("__schema meta field definition is nil")
	}
	//fieldValueMetaInfo
	fieldValueMetaInfo, fieldValueMetaInfoErr := builder.parseFieldValueMetaInfo(fieldDefinition)
	if fieldValueMetaInfoErr != nil {
		return nil, fieldValueMetaInfoErr
	}
	if fieldValueMetaInfo == nil {
		return nil, fmt.Errorf("parse field value meta info error")
	}
	// __schema 是根内省字段，不存在于用户 Query Object 中；
	// 仍按 SchemaMetaFieldDef 解析参数，用来拒绝非法实参并保持构建逻辑一致。
	paramPlans, paramPlansErr := builder.parseParamPlansByArgDefs(fieldDefinition.Args, current.Arguments)
	if paramPlansErr != nil {
		return nil, paramPlansErr
	}
	//typeRuntimeScope
	currentTypeRuntimeScope, currentTypeRuntimeScopeErr := builder.WrapStaticTypeScope(SchemaType)
	if currentTypeRuntimeScopeErr != nil {
		return nil, currentTypeRuntimeScopeErr
	}
	//childrenFields
	var childrenFields []*FieldPlan
	var childrenFieldsErr error
	if len(fieldEntries) > 0 {
		var childEntries []FieldEntry
		childEntries, childrenFieldsErr = builder.flattenChildEntriesForField(fieldEntries, currentTypeRuntimeScope)
		if childrenFieldsErr == nil {
			childrenFields, childrenFieldsErr = builder.buildFieldPlansFromEntries(childEntries, fieldId, fieldValueMetaInfo.IsList, paths, true)
		}
	} else {
		childrenFields, childrenFieldsErr = builder.buildIntrospectionSelectionSet(current.GetSelectionSet(), currentTypeRuntimeScope, fieldId, fieldValueMetaInfo.IsList, paths, directives, directiveDependencyParams)
	}
	if childrenFieldsErr != nil {
		return nil, childrenFieldsErr
	}
	//resolverFunc
	resolverFunc := func(source any, params map[string]any, ctx context.Context) (any, error) {
		return GenerateSchemaMetaResult(builder.schema, childrenFields, InputsFromContext(ctx)), nil
	}

	fieldPlan := &FieldPlan{
		fieldId:                    fieldId,
		parentFieldId:              0,
		fieldName:                  IntrospectionFieldNameMetaSchema,
		responseName:               responseName,
		paths:                      paths,
		fieldValueMetaInfo:         *fieldValueMetaInfo,
		fieldASTs:                  fieldASTsForPlan(current, fieldEntries),
		returnType:                 fieldDefinition.Type,
		parentType:                 parentCompositeFromScope(parentTypeScope),
		childrenFields:             childrenFields,
		paramPlans:                 paramPlans,
		resolverFunc:               resolverFunc,
		allowedRuntimeTypeNames:    parentTypeScope.GetAllowedTypeNamesForField(),
		runtimeTypeResolverFunc:    parentTypeScope.GetDynamicTypeResolverFunction(),
		compiledTypeName:           parentTypeScope.GetStaticTypeName(),
		directivePlans:             directives,
		directiveParamPlans:        directiveDependencyParams,
		conditionalDirectiveGroups: conditionalDirectiveGroups,
	}
	return []*FieldPlan{fieldPlan}, nil
}

func (builder *PlanBuilder) parseField(current ast.Field, parentTypeScope *TypeRuntimeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, directivePlans []*DirectivePlan, directiveDependencyParams []*ParamPlan, conditionalDirectiveGroups [][]*DirectivePlan, fieldEntries []FieldEntry) ([]*FieldPlan, error) {
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
	// parentKeyFieldName 只用于父 list 下 resolver 结果回填；无 resolver 字段直接从父 item 取值。
	parentKeyFieldName = builder.checkAndParseParentKeyFieldNames(parentFieldIsList, parentTypeScope)
	fieldHasResolver := fieldDefinition.Resolve != nil || fieldDefinition.BatchResolve != nil
	if parentFieldIsList && fieldHasResolver && parentKeyFieldName == "" {
		return nil, fmt.Errorf("parent key field name for resolver result binding is empty")
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
	case *Object:
		fieldTypeRuntimeScope, typeRuntimeScopeErr = builder.WrapStaticTypeScope(fieldType)
	case Abstract:
		fieldTypeRuntimeScope, typeRuntimeScopeErr = builder.WrapDynamicTypeScope(fieldType)
	default:
		fieldTypeRuntimeScope = &TypeRuntimeScope{}
	}
	if typeRuntimeScopeErr != nil {
		return nil, typeRuntimeScopeErr
	}
	usesNormalResolver := fieldDefinition.Resolve != nil && (!parentFieldIsList || fieldDefinition.BatchResolve == nil)
	needParentFieldFullResult := parentFieldId > 0 &&
		usesNormalResolver &&
		(parentFieldIsList || (parentTypeScope != nil && parentTypeScope.GetDynamicTypeResolverFunction() != nil))
	if needParentFieldFullResult {
		hasFullResultParam := false
		for _, paramPlan := range paramPlans {
			if paramPlan != nil &&
				paramPlan.paramType == ParamTypeFieldFullResult &&
				paramPlan.dependentFieldId == parentFieldId {
				hasFullResultParam = true
				break
			}
		}
		if !hasFullResultParam {
			// 普通 resolver 在 list 父节点下要遍历父结果；在 interface/union 父节点下要用父结果判断运行时类型。
			// 这里把真实父结果消费显式建模为参数依赖，batch 调度不再通过父子层级兜底。
			paramPlans = append(paramPlans, builder.newFieldFullResultParamPlan(parentFieldId))
		}
	}
	//fieldId
	fieldId = builder.generateFieldId()
	//childrenFields
	var childrenFields []*FieldPlan
	var childrenFieldErr error
	if len(fieldEntries) > 0 {
		var childEntries []FieldEntry
		childEntries, childrenFieldErr = builder.flattenChildEntriesForField(fieldEntries, fieldTypeRuntimeScope)
		if childrenFieldErr == nil {
			childrenFields, childrenFieldErr = builder.buildFieldPlansFromEntries(childEntries, fieldId, fieldValueMetaInfo.IsList, paths, false)
		}
	} else {
		childrenFields, childrenFieldErr = builder.buildSelectionSetWithFlattenEntries(current.GetSelectionSet(), fieldTypeRuntimeScope, fieldId, fieldValueMetaInfo.IsList, paths, directivePlans, false)
	}
	if childrenFieldErr != nil {
		return nil, childrenFieldErr
	}

	fieldPlan := &FieldPlan{
		fieldId:                    fieldId,
		fieldName:                  current.Name.Value,
		responseName:               responseName,
		paths:                      paths,
		fieldValueMetaInfo:         *fieldValueMetaInfo,
		fieldASTs:                  fieldASTsForPlan(current, fieldEntries),
		returnType:                 fieldDefinition.Type,
		parentType:                 parentCompositeFromScope(parentTypeScope),
		parentFieldId:              parentFieldId,
		resultParentKeyName:        resultParentKeyName,
		parentKeyFieldName:         parentKeyFieldName,
		childrenFields:             childrenFields,
		paramPlans:                 paramPlans,
		resolverFunc:               builder.wrapResolverFunc(fieldDefinition.Resolve),
		arrParamPlans:              arrayParamPlans,
		arrayResolverFunc:          builder.wrapResolverFunc(fieldDefinition.BatchResolve),
		allowedRuntimeTypeNames:    parentTypeScope.GetAllowedTypeNamesForField(),
		runtimeTypeResolverFunc:    parentTypeScope.GetDynamicTypeResolverFunction(),
		compiledTypeName:           parentTypeScope.GetStaticTypeName(),
		directivePlans:             directivePlans,
		directiveParamPlans:        directiveDependencyParams,
		conditionalDirectiveGroups: conditionalDirectiveGroups,
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

func (builder *PlanBuilder) WrapFieldType2Scope(fieldType Type) (*TypeRuntimeScope, error) {
	if fieldType == nil {
		return &TypeRuntimeScope{}, nil
	}
	switch typedField := fieldType.(type) {
	case *Object:
		return builder.WrapStaticTypeScope(typedField)
	case *Interface:
		return builder.WrapDynamicTypeScope(typedField)
	case *Union:
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

func fieldASTsForPlan(current ast.Field, fieldEntries []FieldEntry) []*ast.Field {
	if len(fieldEntries) == 0 {
		return []*ast.Field{&current}
	}
	// 同一 responseName 可能由多个 occurrence 合并，ResolveInfo.FieldASTs 必须保留全部 occurrence。
	result := make([]*ast.Field, 0, len(fieldEntries))
	for i := range fieldEntries {
		result = append(result, &fieldEntries[i].field)
	}
	return result
}

func parentCompositeFromScope(scope *TypeRuntimeScope) Composite {
	if scope == nil {
		return nil
	}
	parentType, _ := scope.declaredType.(Composite)
	return parentType
}

func CoerceOperationVariables(document *ast.Document, schema *Schema, args map[string]any, operationName *string) (map[string]any, error) {
	if document == nil {
		return nil, errors.New("no document provided")
	}
	if schema == nil {
		return nil, errors.New("no schema provided")
	}
	if args == nil {
		args = map[string]any{}
	}

	var operationDefinition *ast.OperationDefinition
	for _, def := range document.Definitions {
		op, ok := def.(*ast.OperationDefinition)
		if !ok {
			continue
		}
		if operationName != nil {
			if op.Name != nil && op.Name.Value == *operationName {
				operationDefinition = op
				break
			}
			continue
		}
		if operationDefinition != nil {
			return nil, errors.New("operation definition already exists")
		}
		operationDefinition = op
	}

	if operationDefinition == nil {
		return nil, errors.New("no operation definition provided")
	}

	builder := &PlanBuilder{schema: schema}
	return builder.parseOperationVariables(schema, operationDefinition.VariableDefinitions, args)
}

func (builder *PlanBuilder) parseOperationVariables(schema *Schema, defs []*ast.VariableDefinition, originalInputs map[string]any) (map[string]any, error) {
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

func (builder *PlanBuilder) parseInputValue(inputType Input, source any) (any, error) {
	if nonNullType, ok := inputType.(*NonNull); ok {
		if source == nil {
			return nil, fmt.Errorf("non null input value is required")
		}

		inner, innerOk := nonNullType.OfType.(Input)
		if !innerOk {
			return nil, fmt.Errorf("non null input value is required")
		}

		return builder.parseInputValue(inner, source)
	}

	if source == nil {
		return nil, nil
	}

	switch t := inputType.(type) {
	case *List:
		inner := t.OfType.(Input)

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

	case *InputObject:
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
	case *Scalar:
		parsed := t.ParseValue(source)
		if parsed == nil {
			return nil, fmt.Errorf("scalar is required")
		}
		return parsed, nil
	case *Enum:
		parsed := t.ParseValue(source)
		if parsed == nil {
			return nil, fmt.Errorf("enum is required")
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("unsupported input type %T", t)
	}
}

func (builder *PlanBuilder) directiveLocationAllowed(def *Directive, location string) bool {
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

		argPlans, argPlansErr := builder.parseParamPlansByArgDefs(def.Args, directiveAST.Arguments)
		if argPlansErr != nil {
			return nil, argPlansErr
		}

		compiler := builder.directiveRegistry.Compiler(name)

		if paramPlansContainRuntimeInput(argPlans) {
			if name == "skip" || name == "include" {
				handler := builder.directiveRegistry.RuntimeHandler(name)
				if handler == nil {
					return nil, fmt.Errorf("directive runtime handler not defined for %s", name)
				}
				// 变量版 @skip/@include 必须运行期判断，不能在 build 阶段裁剪字段树。
				result.RuntimePlans = append(result.RuntimePlans, &DirectivePlan{
					Name:           name,
					Location:       location,
					argPlans:       argPlans,
					Stage:          DirectiveStageShouldExecute,
					RuntimeHandler: handler,
				})
				continue
			}

			runtimeCompiler, ok := compiler.(RuntimeDirectivePlanCompiler)
			if !ok {
				return nil, fmt.Errorf("directive %s contains variables but does not support runtime plan compilation", name)
			}

			compiled, compiledErr := runtimeCompiler.CompileRuntime(name, location, argPlans, builder.schema)
			if compiledErr != nil {
				return nil, compiledErr
			}
			for _, rp := range compiled.RuntimePlans {
				if rp != nil && rp.argPlans == nil {
					rp.argPlans = argPlans
				}
			}
			builder.bindRuntimeHandlers(compiled)
			builder.mergeDirectiveCompileResult(result, compiled)
			continue
		}

		// 不含变量的指令参数可以在 build 阶段物化，literal/default 属于 plan 结构的一部分。
		args, err := builder.buildDirectiveArgs(def.Args, directiveAST.Arguments)
		if err != nil {
			return nil, err
		}

		if compiler == nil {
			if builder.directiveRegistry.MetadataOnly(name) {
				result.RuntimePlans = append(result.RuntimePlans, &DirectivePlan{
					Name:           name,
					Args:           args,
					argPlans:       argPlans,
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

		//给本指令产出的每个运行期计划附加 argPlans（供运行期物化）
		for _, rp := range compiled.RuntimePlans {
			if rp != nil && rp.argPlans == nil {
				rp.argPlans = argPlans
			}
		}

		builder.bindRuntimeHandlers(compiled)
		builder.mergeDirectiveCompileResult(result, compiled)
	}
	return result, nil
}

func paramPlansContainRuntimeInput(plans []*ParamPlan) bool {
	for _, plan := range plans {
		switch plan.GetParamType() {
		case ParamTypeInput, ParamTypeVariableTemplate:
			return true
		}
	}
	return false
}

func (builder *PlanBuilder) buildDirectiveArgs(argDefs []*Argument, argASTs []*ast.Argument) (map[string]any, error) {
	defMap := map[string]*Argument{}
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

func InputTypeFromAST(schema *Schema, typeAST ast.Type) (Input, error) {
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

		inputType, ok := typeFound.(Input)
		if !ok {
			return nil, fmt.Errorf("type is not an input type")
		}
		return inputType, nil
	case *ast.List:
		inner, err := InputTypeFromAST(schema, tt.Type)
		if err != nil {
			return nil, err
		}
		return NewList(inner), nil
	case *ast.NonNull:
		inner, err := InputTypeFromAST(schema, tt.Type)
		if err != nil {
			return nil, err
		}

		//object!!这种没意义的要过滤掉
		_, ok := inner.(Nullable)
		if !ok {
			return nil, fmt.Errorf("non-nullable type's element type is not an non-nullable type")
		}
		return NewNonNull(inner), nil
	default:
		return nil, fmt.Errorf("unknown type %T", typeAST)
	}
}

func ValueFromAST(valueAST ast.Value, inputType Input, originalInputs map[string]any) (any, error) {
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
			if _, isNonNull := inputType.(*NonNull); isNonNull {
				return nil, fmt.Errorf("variable %q is required for a non-null input", variable.Name.Value)
			}
			return nil, nil
		}
		return val, nil
	}

	switch t := inputType.(type) {
	case *NonNull:
		value, err := ValueFromAST(valueAST, t.OfType.(Input), originalInputs)
		if err != nil {
			return nil, err
		}
		if value == nil {
			return nil, fmt.Errorf("value is nil")
		}
		return value, nil
	case *List:
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

		single, err := ValueFromAST(valueAST, t.OfType.(Input), originalInputs)
		if err != nil {
			return nil, err
		}

		return []any{single}, nil
	case *InputObject:
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

			//修正：原先误传 fieldAST(*ast.ObjectField)，应传其内部值 fieldAST.Value
			fieldValue, fieldValueErr := ValueFromAST(fieldAST.Value, fieldDef.Type, originalInputs)
			if fieldValueErr != nil {
				return nil, fieldValueErr
			}
			result[fieldName] = fieldValue
		}
		return result, nil
	case *Scalar:
		parsed := t.ParseLiteral(valueAST)
		if parsed == nil {
			return nil, fmt.Errorf("value is nil")
		}
		return parsed, nil
	case *Enum:
		parsed := t.ParseLiteral(valueAST)
		if parsed == nil {
			return nil, fmt.Errorf("value is nil")
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("unknown type %T", t)
	}
}

func IsNonNullInput(t Input) bool {
	_, ok := t.(*NonNull)
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
		return DirectiveLocationMutation
	case ast.OperationTypeSubscription:
		return DirectiveLocationSubscription
	default:
		return DirectiveLocationQuery
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
	// 注册默认的 skip/include：literal 参数可 build 阶段裁剪，变量参数走运行期判断。
	result.Register("skip", SkipDirectiveCompiler{}, SkipDirectiveRuntimeHandler{})
	result.Register("include", IncludeDirectiveCompiler{}, IncludeDirectiveRuntimeHandler{})
	return result
}

func (r *DirectiveRegistry) Clone() *DirectiveRegistry {
	if r == nil {
		return NewDirectiveRegistry()
	}

	result := &DirectiveRegistry{
		compilers:    make(map[string]DirectiveCompiler, len(r.compilers)),
		handlers:     make(map[string]DirectiveRuntimeHandler, len(r.handlers)),
		metadataOnly: make(map[string]bool, len(r.metadataOnly)),
	}

	for name, compiler := range r.compilers {
		result.compilers[name] = compiler
	}
	for name, handler := range r.handlers {
		result.handlers[name] = handler
	}
	for name, metadataOnly := range r.metadataOnly {
		result.metadataOnly[name] = metadataOnly
	}

	return result
}

func (r *DirectiveRegistry) Register(name string, compiler DirectiveCompiler, handler any) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}

	if compiler != nil {
		r.compilers[name] = compiler
	}
	switch h := handler.(type) {
	case nil:
	case DirectiveRuntimeHandler:
		r.handlers[name] = h
	case LegacyDirectiveRuntimeHandler:
		// 兼容旧版自定义指令 handler：旧签名没有 directiveArgs，注册时包装成新版运行接口。
		r.handlers[name] = legacyDirectiveRuntimeHandlerAdapter{handler: h}
	default:
		return fmt.Errorf("unsupported directive runtime handler for %s", name)
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
