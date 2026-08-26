package graphql

import (
	"errors"
	"fmt"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/printer"
)

type directiveCompileScope struct {
	location       string
	fragmentName   string
	responsePath   []string
	parentTypeName string
	fieldName      string
}

type unresolvedParamDependency struct {
	paramPlan *ParamPlan
	source    FieldResponseParamSource
	target    string
}

func (compiler *PlanCompiler) initializeParamRegistry(document *ast.Document, operation *ast.OperationDefinition, registry *ParamRegistry) {
	compiler.fieldOwners = make(map[uint32]fieldOwnerKey)
	compiler.directiveBindingMatches = make(map[*ast.Directive][]int)
	compiler.externalBindingsByDirectivePlan = make(map[*DirectivePlan][]int)

	if registry == nil || operation == nil {
		return
	}

	queryKey := buildDocumentOperationKey(documentIdentityBody(document), operationDefinitionName(operation))
	compiler.paramBindings = registry.bindings(queryKey)
	if compiler.paramBindings == nil {
		return
	}

	compiler.fieldBindingFieldIDs = make([]uint32, len(compiler.paramBindings.fieldBindings))
	compiler.directiveBindingASTs = make([]*ast.Directive, len(compiler.paramBindings.directiveBindings))
	compiler.variableDefinitions = make(map[string]*ast.VariableDefinition)

	for _, definition := range operation.VariableDefinitions {
		if definition == nil || definition.Variable == nil || definition.Variable.Name == nil {
			continue
		}
		compiler.variableDefinitions[definition.Variable.Name.Value] = definition
	}

	compiler.usedVariables = make(map[string]struct{})
	validationContext := NewValidationContext(compiler.schema, document, nil)
	for _, usage := range validationContext.RecursiveVariableUsages(operation) {
		if usage == nil || usage.Node == nil || usage.Node.Name == nil {
			continue
		}

		compiler.usedVariables[usage.Node.Name.Value] = struct{}{}
	}
}

func (compiler *PlanCompiler) applyFieldParamBindings(owner fieldOwnerKey, fieldId uint32, argDefs []*Argument, basePlans []*ParamPlan) ([]*ParamPlan, error) {
	if compiler.paramBindings == nil {
		return basePlans, nil
	}

	bindingIndexes := compiler.paramBindings.fieldIndex[owner]
	if len(bindingIndexes) == 0 {
		return basePlans, nil
	}

	argDefByName, err := indexArgumentDefinitions(argDefs)
	if err != nil {
		return nil, err
	}
	planByName := indexParamPlans(basePlans)
	for _, bindingIndex := range bindingIndexes {
		binding := compiler.paramBindings.fieldBindings[bindingIndex]
		argDef := argDefByName[binding.Target.ParamName]
		if argDef == nil {
			return nil, fmt.Errorf("param registry target %s.%s contains unknown argument %q", binding.Target.ParentTypeName, binding.Target.FieldName, binding.Target.ParamName)
		}

		previousFieldID := compiler.fieldBindingFieldIDs[bindingIndex]
		if previousFieldID != 0 && previousFieldID != fieldId {
			return nil, fmt.Errorf("field parameter target %s.%s(%s:) is ambiguous", binding.Target.ParentTypeName, binding.Target.FieldName, binding.Target.ParamName)
		}

		paramPlan, compileErr := compiler.compileRegistryParamPlan(argDef, &binding.Source, fmt.Sprintf("field %s.%s(%s:)", binding.Target.ParentTypeName, binding.Target.FieldName, binding.Target.ParamName))
		if compileErr != nil {
			return nil, compileErr
		}

		planByName[argDef.Name()] = paramPlan
		compiler.fieldBindingFieldIDs[bindingIndex] = fieldId
	}
	return orderParamPlans(argDefs, planByName), nil
}

func (compiler *PlanCompiler) applyDirectiveParamBindings(directiveAST *ast.Directive, scope directiveCompileScope, argDefs []*Argument, basePlans []*ParamPlan) ([]*ParamPlan, map[string]struct{}, error) {
	if compiler.paramBindings == nil || directiveAST == nil {
		return basePlans, nil, nil
	}

	owner := directiveOwnerKey{
		location:       scope.location,
		directiveName:  directiveAST.Name.Value,
		fragmentName:   scope.fragmentName,
		responsePath:   encodeResponsePath(scope.responsePath),
		parentTypeName: scope.parentTypeName,
		fieldName:      scope.fieldName,
	}
	bindingIndexes := compiler.paramBindings.directiveIndex[owner]
	if len(bindingIndexes) == 0 {
		return basePlans, nil, nil
	}

	argDefByName, err := indexArgumentDefinitions(argDefs)
	if err != nil {
		return nil, nil, err
	}
	planByName := indexParamPlans(basePlans)
	overriden := make(map[string]struct{})

	for _, bindingIndex := range bindingIndexes {
		binding := compiler.paramBindings.directiveBindings[bindingIndex]
		if binding.Target.SourceOffset != nil {
			if directiveAST.Loc == nil || directiveAST.Loc.Start != *binding.Target.SourceOffset {
				continue
			}
		}

		previousAST := compiler.directiveBindingASTs[bindingIndex]
		if previousAST != nil && previousAST != directiveAST {
			return nil, nil, fmt.Errorf("directive parameter target @%s(%s:) at %s is ambiguous; SourceOffset is required", binding.Target.DirectiveName, binding.Target.ParamName, binding.Target.Location)
		}

		argDef := argDefByName[binding.Target.ParamName]
		if argDef == nil {
			return nil, nil, fmt.Errorf("param registry target @%s contains unknown argument %q", binding.Target.DirectiveName, binding.Target.ParamName)
		}

		paramPlan, compileErr := compiler.compileRegistryParamPlan(argDef, &binding.Source, fmt.Sprintf("directive @%s(%s:) at %s", binding.Target.DirectiveName, binding.Target.ParamName, binding.Target.Location))
		if compileErr != nil {
			return nil, nil, compileErr
		}

		planByName[argDef.Name()] = paramPlan
		overriden[argDef.Name()] = struct{}{}
		compiler.directiveBindingASTs[bindingIndex] = directiveAST
		compiler.directiveBindingMatches[directiveAST] = append(compiler.directiveBindingMatches[directiveAST], bindingIndex)
	}
	if len(overriden) == 0 {
		return basePlans, nil, nil
	}
	return orderParamPlans(argDefs, planByName), overriden, nil
}

func (compiler *PlanCompiler) compileRegistryParamPlan(argDef *Argument, source *ParamSource, target string) (*ParamPlan, error) {
	if argDef == nil {
		return nil, errors.New("registry parameter argument definition is nil")
	}

	switch source.Kind {
	case ParamSourceConst:
		value, err := compiler.compileInputValue(argDef.Type, cloneParamValue(source.ConstValue))
		if err != nil {
			return nil, fmt.Errorf("%s has invalid CONST value: %w", target, err)
		}
		return newConstParamPlan(argDef.Name(), value), nil
	case ParamSourceInput:
		if err := compiler.validateRegistryInputSource(source.InputName, argDef, target); err != nil {
			return nil, err
		}
		result := newInputParamPlan(argDef.Name(), source.InputName)
		if argDef.DefaultValue != nil {
			result.inputDefaultValue = argDef.DefaultValue
		}
		return result, nil
	case ParamSourceFieldResponse:
		result := &ParamPlan{
			paramKey:           argDef.Name(),
			paramType:          PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE,
			fieldResponsePaths: append([]string(nil), source.FieldResponse.ResultPath...),
		}
		compiler.unresolvedParamDependencies = append(compiler.unresolvedParamDependencies, unresolvedParamDependency{paramPlan: result, source: *source.FieldResponse, target: target})
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported registry parameter type %s", source.Kind)
	}
}

func (compiler *PlanCompiler) validateRegistryInputSource(inputName string, argDef *Argument, target string) error {
	if _, used := compiler.usedVariables[inputName]; !used {
		return fmt.Errorf("%s references variable $%s which is not used by the validated operation", target, inputName)
	}

	variableDefinition := compiler.variableDefinitions[inputName]
	if variableDefinition == nil {
		return fmt.Errorf("%s references undefined variable $%s", target, inputName)
	}

	variableType, err := typeFromAST(*compiler.schema, variableDefinition.Type)
	if err != nil {
		return fmt.Errorf("%s has invalid variable $%s: %w", target, inputName, err)
	}
	if !isTypeSubTypeOf(compiler.schema, effectiveType(variableType, variableDefinition), argDef.Type) {
		return fmt.Errorf("%s uses variable $%s of type %s where %s is required", target, inputName, variableType, argDef.Type)
	}
	return nil
}

func (compiler *PlanCompiler) recordFieldOwner(fieldID uint32, owner fieldOwnerKey) {
	if compiler.fieldOwners == nil {
		compiler.fieldOwners = make(map[uint32]fieldOwnerKey)
	}
	compiler.fieldOwners[fieldID] = owner
}

func (compiler *PlanCompiler) validateDiscardedDirectiveOverrides(fieldEntries []FieldFlattenEntry) error {
	if len(fieldEntries) < 2 || len(compiler.directiveBindingMatches) == 0 {
		return nil
	}

	for occurrence := 1; occurrence < len(fieldEntries); occurrence++ {
		for _, directivePlan := range fieldEntries[occurrence].directives {
			if directivePlan == nil || directivePlan.name == "skip" || directivePlan.name == "include" {
				continue
			}
			if len(compiler.externalBindingsByDirectivePlan[directivePlan]) != 0 {
				return fmt.Errorf("param registry cannot override inherited @%s on a discarded field occurrence", directivePlan.name)
			}
		}
		for _, directiveAST := range fieldEntries[occurrence].field.Directives {
			if directiveAST == nil || directiveAST.Name == nil {
				continue
			}
			name := directiveAST.Name.Value
			if name == "skip" || name == "include" {
				continue
			}
			if len(compiler.directiveBindingMatches[directiveAST]) != 0 {
				return fmt.Errorf("param registry cannot override @%s on discarded field occurrence at offset %d", name, directiveSourceOffset(directiveAST))
			}
		}
	}
	return nil
}

func (compiler *PlanCompiler) recordExternalDirectivePlans(directiveAST *ast.Directive, scope directiveCompileScope, plans []*DirectivePlan) {
	if compiler == nil || compiler.paramBindings == nil || directiveAST == nil || directiveAST.Name == nil {
		return
	}
	owner := directiveOwnerKey{
		location:       scope.location,
		directiveName:  directiveAST.Name.Value,
		fragmentName:   scope.fragmentName,
		responsePath:   encodeResponsePath(scope.responsePath),
		parentTypeName: scope.parentTypeName,
		fieldName:      scope.fieldName,
	}
	bindingIndexes := make([]int, 0)
	for _, bindingIndex := range compiler.paramBindings.directiveIndex[owner] {
		binding := compiler.paramBindings.directiveBindings[bindingIndex]
		if binding.Target.SourceOffset != nil && (directiveAST.Loc == nil || directiveAST.Loc.Start != *binding.Target.SourceOffset) {
			continue
		}
		if compiler.directiveBindingASTs[bindingIndex] == directiveAST {
			bindingIndexes = append(bindingIndexes, bindingIndex)
		}
	}
	if len(bindingIndexes) == 0 {
		return
	}
	for _, plan := range plans {
		if plan == nil {
			continue
		}
		compiler.externalBindingsByDirectivePlan[plan] = append(compiler.externalBindingsByDirectivePlan[plan], bindingIndexes...)
	}
}

func (compiler *PlanCompiler) finalizeParamRegistry(roots []*FieldPlan) error {
	if compiler.paramBindings == nil {
		return nil
	}

	fieldByID := make(map[uint32]*FieldPlan)
	paramConsumers := make(map[*ParamPlan][]*FieldPlan)
	collectPlanRegistryMetadata(roots, fieldByID, paramConsumers)

	fieldIDsByOwner := make(map[fieldOwnerKey][]uint32)
	for fieldID, owner := range compiler.fieldOwners {
		fieldIDsByOwner[owner] = append(fieldIDsByOwner[owner], fieldID)
	}

	for _, dependency := range compiler.unresolvedParamDependencies {
		sourceOwner := fieldOwnerKeyFromSource(dependency.source)
		sourceFieldIDs := fieldIDsByOwner[sourceOwner]
		if len(sourceFieldIDs) == 0 {
			return fmt.Errorf("%s references unknown FIELD_RESPONSE source %s.%s at %v", dependency.target, dependency.source.ParentTypeName, dependency.source.FieldName, dependency.source.ResponsePath)
		}
		if len(sourceFieldIDs) != 1 {
			return fmt.Errorf("%s FIELD_RESPONSE source %s.%s at %v is ambiguous", dependency.target, dependency.source.ParentTypeName, dependency.source.FieldName, dependency.source.ResponsePath)
		}
		producer := fieldByID[sourceFieldIDs[0]]
		if producer == nil || (producer.resolverFunc == nil && producer.bulkResolverFunc == nil) {
			return fmt.Errorf("%s FIELD_RESPONSE source field %d has no resolver", dependency.target, sourceFieldIDs[0])
		}

		consumers := paramConsumers[dependency.paramPlan]
		if len(consumers) == 0 {
			return fmt.Errorf("%s is not attached to a FieldPlan", dependency.target)
		}
		for _, consumer := range consumers {
			if isSingleCallFieldPlan(consumer, fieldByID) && fieldPlanCanProduceMultipleResponses(producer, fieldByID) {
				return fmt.Errorf("%s cannot consume multi-result FIELD_RESPONSE source field %d in SingleCallStep field %d", dependency.target, producer.fieldId, consumer.fieldId)
			}
		}
		dependency.paramPlan.dependentFieldId = producer.fieldId
	}

	for index, fieldID := range compiler.fieldBindingFieldIDs {
		if fieldID == 0 {
			binding := compiler.paramBindings.fieldBindings[index]
			return fmt.Errorf("field parameter target %s.%s(%s:) at %v did not match the operation", binding.Target.ParentTypeName, binding.Target.FieldName, binding.Target.ParamName, binding.Target.ResponsePath)
		}
	}
	for index, directiveAST := range compiler.directiveBindingASTs {
		if directiveAST == nil {
			binding := compiler.paramBindings.directiveBindings[index]
			return fmt.Errorf("directive parameter target @%s(%s:) at %s did not match the operation", binding.Target.DirectiveName, binding.Target.ParamName, binding.Target.Location)
		}
	}
	return nil
}

func compileArgsRawFromParamPlans(paramPlans []*ParamPlan) (map[string]any, error) {
	result := make(map[string]any, len(paramPlans))
	for _, paramPlan := range paramPlans {
		if paramPlan == nil {
			return nil, errors.New("directive parameter plan is nil")
		}
		if paramPlan.paramType != PARAM_TYPE_ENUM_CONST {
			return nil, fmt.Errorf("directive parameter %q requires runtime materialization", paramPlan.paramKey)
		}
		result[paramPlan.paramKey] = paramPlan.constValue
	}
	return result, nil
}

func appendExternalDirectiveDependencies(result *DirectiveCompileResult, paramPlans []*ParamPlan) {
	if result == nil {
		return
	}
	for _, paramPlan := range paramPlans {
		if paramPlan == nil || paramPlan.paramType != PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE {
			continue
		}
		found := false
		for _, current := range result.DependencyParamPlans {
			if current == paramPlan {
				found = true
				break
			}
		}
		if !found {
			result.DependencyParamPlans = append(result.DependencyParamPlans, paramPlan)
		}
	}
}

func directiveRuntimePlansExecute(plans []*DirectivePlan) bool {
	for _, plan := range plans {
		if plan != nil && plan.stage != DIRECTIVE_STAGE_METADATA_ONLY {
			return true
		}
	}
	return false
}

func mergeExternalDirectiveArgPlans(runtimePlans []*DirectivePlan, effectivePlans []*ParamPlan, overridden map[string]struct{}) {
	for _, runtimePlan := range runtimePlans {
		if runtimePlan == nil {
			continue
		}
		if runtimePlan.argsPlans == nil {
			runtimePlan.argsPlans = effectivePlans
			continue
		}
		if len(overridden) == 0 {
			continue
		}

		planByName := indexParamPlans(runtimePlan.argsPlans)
		for _, effectivePlan := range effectivePlans {
			if effectivePlan == nil {
				continue
			}
			if _, ok := overridden[effectivePlan.paramKey]; ok {
				planByName[effectivePlan.paramKey] = effectivePlan
			}
		}

		merged := make([]*ParamPlan, 0, len(planByName))
		seen := make(map[string]struct{}, len(planByName))
		for _, current := range runtimePlan.argsPlans {
			if current == nil {
				continue
			}
			if _, exists := seen[current.paramKey]; exists {
				continue
			}
			if replacement := planByName[current.paramKey]; replacement != nil {
				merged = append(merged, replacement)
				seen[current.paramKey] = struct{}{}
			}
		}
		for _, effectivePlan := range effectivePlans {
			if effectivePlan == nil {
				continue
			}
			if _, external := overridden[effectivePlan.paramKey]; !external {
				continue
			}
			if _, exists := seen[effectivePlan.paramKey]; exists {
				continue
			}
			merged = append(merged, effectivePlan)
		}
		runtimePlan.argsPlans = merged
	}
}

func fieldOwnerFromPlanScope(responsePath []string, parentScope *FieldTypeScope, fieldName string) fieldOwnerKey {
	return fieldOwnerKey{
		responsePath:   encodeResponsePath(responsePath),
		parentTypeName: fieldTypeScopeName(parentScope),
		fieldName:      fieldName,
	}
}

func fieldTypeScopeName(scope *FieldTypeScope) string {
	if scope == nil {
		return ""
	}
	if declared, ok := scope.declaredType.(Type); ok && declared != nil {
		return declared.Name()
	}
	return ""
}

func collectPlanRegistryMetadata(fields []*FieldPlan, fieldByID map[uint32]*FieldPlan, paramConsumers map[*ParamPlan][]*FieldPlan) {
	for _, field := range fields {
		if field == nil {
			continue
		}
		fieldByID[field.fieldId] = field
		for _, plans := range [3][]*ParamPlan{
			field.paramPlans, field.directiveParamPlans, field.bulkParamPlans,
		} {
			for _, plan := range plans {
				if plan != nil {
					consumers := paramConsumers[plan]
					alreadyAdded := false
					for _, consumer := range consumers {
						if consumer == field {
							alreadyAdded = true
							break
						}
					}
					if !alreadyAdded {
						paramConsumers[plan] = append(consumers, field)
					}
				}
			}
		}
		collectPlanRegistryMetadata(field.childrenFields, fieldByID, paramConsumers)
	}
}

func indexArgumentDefinitions(argDefs []*Argument) (map[string]*Argument, error) {
	result := make(map[string]*Argument, len(argDefs))
	for _, argDef := range argDefs {
		if argDef == nil {
			return nil, errors.New("nil argument definition")
		}
		result[argDef.Name()] = argDef
	}
	return result, nil
}

func indexParamPlans(plans []*ParamPlan) map[string]*ParamPlan {
	result := make(map[string]*ParamPlan, len(plans))
	for _, plan := range plans {
		if plan != nil && plan.paramKey != "" {
			result[plan.paramKey] = plan
		}
	}
	return result
}

func orderParamPlans(argDefs []*Argument, planByName map[string]*ParamPlan) []*ParamPlan {
	result := make([]*ParamPlan, 0, len(planByName))
	for _, argDef := range argDefs {
		if argDef == nil {
			continue
		}
		if plan := planByName[argDef.Name()]; plan != nil {
			result = append(result, plan)
		}
	}
	return result
}

func operationDefinitionName(operation *ast.OperationDefinition) string {
	if operation == nil || operation.Name == nil {
		return ""
	}
	return operation.Name.Value
}

func documentIdentityBody(document *ast.Document) []byte {
	if document != nil && document.Loc != nil && document.Loc.Source != nil && len(document.Loc.Source.Body) > 0 {
		return document.Loc.Source.Body
	}
	return []byte(fmt.Sprintf("%v", printer.Print(document)))
}

func directiveSourceOffset(directive *ast.Directive) int {
	if directive == nil || directive.Loc == nil {
		return -1
	}
	return directive.Loc.Start
}

func isSingleCallFieldPlan(field *FieldPlan, fieldByID map[uint32]*FieldPlan) bool {
	if field == nil || field.parentFieldId == 0 {
		return true
	}
	parent := fieldByID[field.parentFieldId]
	return parent == nil || !parent.fieldWrapperTypeInfo.isList
}

func fieldPlanCanProduceMultipleResponses(field *FieldPlan, fieldByID map[uint32]*FieldPlan) bool {
	if field == nil {
		return false
	}
	if field.fieldWrapperTypeInfo.isList {
		return true
	}
	parent := fieldByID[field.parentFieldId]
	return parent != nil && parent.fieldWrapperTypeInfo.isList
}
