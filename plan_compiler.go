package graphql

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/graphql-go/graphql/language/ast"
)

type PlanCompiler struct {
	schema            *Schema                            //原始schema
	fragments         map[string]*ast.FragmentDefinition //片段信息
	directiveRegistry *DirectiveRegistry                 //directive注册表
	fieldIdCounter    atomic.Uint32                      //fieldId生成器

	paramBindings                   *queryParamBindings      //当前query命中的只读外部参数配置
	fieldBindingFieldIDs            []uint32                 //外部字段参数配置实际命中的fieldId
	directiveBindingASTs            []*ast.Directive         //外部指令参数配置实际命中的AST occurrence
	directiveBindingMatches         map[*ast.Directive][]int //用于拒绝覆盖被合并丢弃的普通指令occurrence
	externalBindingsByDirectivePlan map[*DirectivePlan][]int //把继承的运行期指令追溯到外部配置
	variableDefinitions             map[string]*ast.VariableDefinition
	usedVariables                   map[string]struct{}
	unresolvedParamDependencies     []unresolvedParamDependency
	fieldOwners                     map[uint32]fieldOwnerKey
}

// compile阶段Field对象封装
type FieldFlattenEntry struct {
	field            ast.Field
	fieldTypeScope   *FieldTypeScope
	directives       []*DirectivePlan
	dependencyParams []*ParamPlan
}

func compileExecutionPlan(document *ast.Document, schema *Schema, operationName *string, directiveRegistry *DirectiveRegistry, paramRegistry *ParamRegistry) (*SGraphExecutionPlan, error) {
	//校验参数
	if document == nil {
		return nil, errors.New("no document provided")
	}
	if schema == nil {
		return nil, errors.New("no schema provided")
	}
	result := &SGraphExecutionPlan{
		schemaResolveInfo: SchemaResolveInfo{},
	}
	compiler := &PlanCompiler{
		schema:         schema,
		fieldIdCounter: atomic.Uint32{},
	}

	//解析operation和fragments
	operationDefinition, operationErr := selectOperationDefinition(document, operationName)
	if operationErr != nil {
		return nil, operationErr
	}
	fragments := make(map[string]*ast.FragmentDefinition)
	for _, def := range document.Definitions {
		switch definition := def.(type) {
		case *ast.FragmentDefinition:
			fragments[definition.Name.Value] = definition
		}
	}
	//校验operationDefinition是否解析成功
	if operationDefinition == nil {
		if operationName != nil {
			return nil, fmt.Errorf("no operation definition found for %s", *operationName)
		}
		return nil, errors.New("operation definition is nil because operation name is nil")
	}
	compiler.fragments = fragments
	compiler.initializeParamRegistry(document, operationDefinition, paramRegistry)

	result.schemaResolveInfo.operation = operationDefinition
	result.schemaResolveInfo.fragments = make(map[string]ast.Definition, len(fragments))
	for name, fragment := range fragments {
		result.schemaResolveInfo.fragments[name] = fragment
	}

	var rootNodeType *Object
	switch operationDefinition.Operation {
	case "query":
		rootNodeType = schema.QueryType()
	case "mutation":
		rootNodeType = schema.MutationType()
	case "subscription":
		rootNodeType = schema.SubscriptionType()
	}
	//directive注册表
	var dr *DirectiveRegistry
	if directiveRegistry != nil {
		dr = directiveRegistry
	} else {
		dr = NewDirectiveRegistry()
	}
	compiler.directiveRegistry = dr

	//先compile directives，fields tree的编译依赖directives
	operationLocation := operationDirectiveLocation(operationDefinition.Operation)
	queryCompiled, queryErr := compiler.compileDirectives(operationDefinition.Directives, operationLocation, nil, directiveCompileScope{location: operationLocation})
	if queryErr != nil {
		return nil, queryErr
	}
	//提取根节点，并组装对应的TypeScope
	rootTypeScope, scopeErr := wrapStaticFieldTypeScope(rootNodeType)
	if scopeErr != nil {
		return nil, scopeErr
	}
	fieldPlans, fieldPlansErr := compiler.compileSelectionSetWithFlattenEntries(operationDefinition.SelectionSet, rootTypeScope, 0, false, nil, queryCompiled.RuntimePlans, false)
	if fieldPlansErr != nil {
		return nil, fieldPlansErr
	}
	result.roots = fieldPlans
	if finalizeErr := compiler.finalizeParamRegistry(fieldPlans); finalizeErr != nil {
		return nil, finalizeErr
	}
	result.maxFieldId = compiler.GetMaxFieldId(fieldPlans)
	result.fieldPlansById = make(map[uint32]*FieldPlan, int(result.maxFieldId))
	maxListPathDepth, responsePathErr := prepareResponsePathMetadata(fieldPlans, nil, 0, result.fieldPlansById)
	if responsePathErr != nil {
		return nil, responsePathErr
	}
	result.maxListPathDepth = maxListPathDepth
	return result, nil
}

// prepareResponsePathMetadata预计算动态错误路径所需的静态信息，并同步生成fieldId只读索引。
func prepareResponsePathMetadata(fields []*FieldPlan, parentPathListDepths []int, ancestorListDepth int, fieldPlansById map[uint32]*FieldPlan) (int, error) {
	if fieldPlansById == nil {
		return 0, errors.New("field plan index is nil")
	}
	maxListDepth := ancestorListDepth
	for _, field := range fields {
		if field == nil {
			continue
		}
		if field.fieldId == 0 {
			return 0, errors.New("field id must be greater than 0")
		}
		if _, exists := fieldPlansById[field.fieldId]; exists {
			return 0, fmt.Errorf("field id already defined: %d", field.fieldId)
		}
		fieldPlansById[field.fieldId] = field
		if len(field.paths) != len(parentPathListDepths)+1 {
			return 0, fmt.Errorf("field %s response path depth %d does not match plan tree depth %d", field.responseName, len(field.paths), len(parentPathListDepths)+1)
		}

		listDepth := 0
		for wrapper := &field.fieldWrapperTypeInfo; wrapper != nil && wrapper.isList; wrapper = wrapper.elementWrapperTypeInfo {
			listDepth++
		}
		field.pathListDepths = make([]int, len(parentPathListDepths)+1)
		copy(field.pathListDepths, parentPathListDepths)
		field.pathListDepths[len(parentPathListDepths)] = listDepth

		currentListDepth := ancestorListDepth + listDepth
		if currentListDepth > maxListDepth {
			maxListDepth = currentListDepth
		}

		childDepth, childErr := prepareResponsePathMetadata(field.childrenFields, field.pathListDepths, currentListDepth, fieldPlansById)
		if childErr != nil {
			return 0, childErr
		}
		if childDepth > maxListDepth {
			maxListDepth = childDepth
		}

		if field.fieldWrapperTypeInfo.isList {
			for _, child := range field.childrenFields {
				if child == nil {
					continue
				}
				usesIterationResolver := child.resolverFunc != nil && child.bulkResolverFunc == nil
				bulkNeedsParentPaths := child.bulkResolverFunc != nil && child.needsOccurrencePath
				if usesIterationResolver || bulkNeedsParentPaths {
					field.needsOccurrencePath = true
					break
				}
			}
		}
	}
	return maxListDepth, nil
}

// 编译指令
func (compiler *PlanCompiler) compileDirectives(directiveASTs []*ast.Directive, location string, inherited []*DirectivePlan, scope directiveCompileScope) (*DirectiveCompileResult, error) {
	result := &DirectiveCompileResult{
		RuntimePlans: append([]*DirectivePlan{}, inherited...),
	}

	for _, inheritedPlan := range inherited {
		if inheritedPlan != nil {
			appendExternalDirectiveDependencies(result, inheritedPlan.argsPlans)
		}
	}
	scope.location = location

	//校验directives名称、位置、重复使用
	if directiveValidationErr := compiler.validateDirectiveUsages(directiveASTs, location); directiveValidationErr != nil {
		return nil, directiveValidationErr
	}
	for _, directiveAST := range directiveASTs {
		directiveName := directiveAST.Name.Value
		directiveDef := compiler.schema.Directive(directiveName)

		//解析指令上的参数并生成ParamPlan
		paramPlans, paramPlansErr := compiler.compileParamPlansByArgDefs(directiveDef.Args, directiveAST.Arguments)
		if paramPlansErr != nil {
			return nil, paramPlansErr
		}

		paramPlans, overriddenParams, bindingErr := compiler.applyDirectiveParamBindings(directiveAST, scope, directiveDef.Args, paramPlans)
		if bindingErr != nil {
			return nil, bindingErr
		}

		directiveCompiler := compiler.directiveRegistry.Compiler(directiveName)
		requiresRuntime := paramsRequireRuntimeEvaluation(paramPlans)

		//metadata-only指令没有编译行为，但仍保留参数plan
		if directiveCompiler == nil {
			if !compiler.directiveRegistry.MetadataOnly(directiveName) {
				return nil, fmt.Errorf("no directive compiler found for %s", directiveName)
			}
			if paramsContainsFieldResponse(paramPlans) {
				return nil, fmt.Errorf("metadata-only directive %s cannot consume FIELD_RESPONSE", directiveName)
			}

			var argsRaw map[string]any
			var argsRawErr error
			if !requiresRuntime {
				argsRaw, argsRawErr = compileArgsRawFromParamPlans(paramPlans)
				if argsRawErr != nil {
					return nil, argsRawErr
				}
			}

			metadataPlan := &DirectivePlan{
				name:           directiveName,
				location:       location,
				argsRaw:        argsRaw,
				argsPlans:      paramPlans,
				stage:          DIRECTIVE_STAGE_METADATA_ONLY,
				runtimeHandler: DefaultEmptyDirectiveRuntimeHandler{},
			}
			result.RuntimePlans = append(result.RuntimePlans, metadataPlan)
			compiler.recordExternalDirectivePlans(directiveAST, scope, []*DirectivePlan{
				metadataPlan,
			})
			continue
		}

		if requiresRuntime {
			//skip/include的变量参数直接交给运行时handler判断
			if directiveName == "skip" || directiveName == "include" {
				if paramsContainsFieldResponse(paramPlans) {
					return nil, fmt.Errorf("directive %s cannot consume FIELD_RESPONSE", directiveName)
				}
				handler := compiler.directiveRegistry.RuntimeHandler(directiveName)
				if handler == nil {
					return nil, fmt.Errorf("no runtime handler found for %s", directiveName)
				}

				runtimePlan := &DirectivePlan{
					name:           directiveName,
					location:       location,
					argsPlans:      paramPlans,
					stage:          DIRECTIVE_STAGE_SHOULD_EXECUTE,
					runtimeHandler: handler,
				}
				result.RuntimePlans = append(result.RuntimePlans, runtimePlan)
				compiler.recordExternalDirectivePlans(directiveAST, scope, []*DirectivePlan{
					runtimePlan,
				})
				continue
			}

			runtimeCompiler, ok := directiveCompiler.(RuntimeDirectivePlanCompiler)
			if !ok {
				return nil, fmt.Errorf("no runtime compiler found for %s", directiveName)
			}

			compiled, compileErr := runtimeCompiler.RuntimeCompile(directiveName, location, paramPlans, compiler.schema)
			if compileErr != nil {
				return nil, compileErr
			}
			if compiled == nil {
				return nil, fmt.Errorf("runtime compiler for directive %s returned nil", directiveName)
			}

			if paramsContainsFieldResponse(paramPlans) && !directiveRuntimePlansExecute(compiled.RuntimePlans) {
				return nil, fmt.Errorf("directive %s FIELD_RESPONSE parameter has no executable runtime plan", directiveName)
			}

			for _, plan := range compiled.RuntimePlans {
				if plan == nil {
					return nil, fmt.Errorf("runtime compiler for directive %s returned a nil runtime plan", directiveName)
				}

				if plan.name == "" {
					plan.name = directiveName
				}
				if plan.location == "" {
					plan.location = location
				}
			}

			mergeExternalDirectiveArgPlans(compiled.RuntimePlans, paramPlans, overriddenParams)
			appendExternalDirectiveDependencies(compiled, paramPlans)
			compiler.recordExternalDirectivePlans(directiveAST, scope, compiled.RuntimePlans)

			compiler.bindRuntimeHandler2Directive(compiled.RuntimePlans)
			compiler.mergeCompiledResults(result, compiled)
			continue
		}

		argsRaw, err := compileArgsRawFromParamPlans(paramPlans)
		if err != nil {
			return nil, err
		}

		compiled, compileErr := directiveCompiler.Compile(directiveName, location, argsRaw, compiler.schema)
		if compileErr != nil {
			return nil, compileErr
		}
		if compiled == nil {
			return nil, fmt.Errorf("runtime compiler for directive %s returned nil", directiveName)
		}

		for _, plan := range compiled.RuntimePlans {
			if plan == nil {
				return nil, fmt.Errorf("compiler for directive %s returned a nil runtime plan", directiveName)
			}
			if plan.name == "" {
				plan.name = directiveName
			}
			if plan.location == "" {
				plan.location = location
			}
		}
		mergeExternalDirectiveArgPlans(compiled.RuntimePlans, paramPlans, overriddenParams)
		appendExternalDirectiveDependencies(compiled, paramPlans)
		compiler.recordExternalDirectivePlans(directiveAST, scope, compiled.RuntimePlans)
		compiler.bindRuntimeHandler2Directive(compiled.RuntimePlans)
		compiler.mergeCompiledResults(result, compiled)
	}
	return result, nil
}

func (compiler *PlanCompiler) validateDirectiveUsages(directiveASTs []*ast.Directive, location string) error {
	counting := map[string]int{}

	for _, directiveAST := range directiveASTs {
		//检查directive是否注册
		directiveName := directiveAST.Name.Value
		directiveDefinition := compiler.schema.Directive(directiveName)
		if directiveDefinition == nil {
			return fmt.Errorf("directive %s is not defined", directiveName)
		}

		//检查directive出现的位置是否合法
		locationAllowed := compiler.validateDirectiveLocation(directiveDefinition, location)
		if !locationAllowed {
			return fmt.Errorf("location for directive %s is not allowed", directiveName)
		}

		//检查directive是否在本处重复出现，默认不允许重复出现。graphql-go框架中没有字段存储repeatable信息，从可靠性角度拒绝全部重复的directive
		counting[directiveName]++
		if counting[directiveName] > 1 {
			return fmt.Errorf("directive %s is repeated", directiveName)
		}
	}

	return nil
}

func (compiler *PlanCompiler) validateDirectiveLocation(directiveDefinition *Directive, location string) bool {
	for _, allowedLocation := range directiveDefinition.Locations {
		if allowedLocation == location {
			return true
		}
	}
	return false
}

// 根据argDefinition解析出ParamPlan
func (compiler *PlanCompiler) compileParamPlansByArgDefs(argDefs []*Argument, argASTs []*ast.Argument) ([]*ParamPlan, error) {
	//校验argASTs使用是否合法
	defMap := make(map[string]*Argument)
	for _, argDef := range argDefs {
		defMap[argDef.Name()] = argDef
	}

	astMap := map[string]*ast.Argument{}
	for _, astArg := range argASTs {
		if astArg == nil || astArg.Name == nil {
			return nil, fmt.Errorf("invalid argument:argument or its name is nil")
		}
		astName := astArg.Name.Value
		if _, ok := defMap[astName]; !ok {
			return nil, fmt.Errorf("cannot find argument definition: %s", astName)
		}
		if _, ok := astMap[astName]; ok {
			return nil, fmt.Errorf("duplicate argument: %s", astName)
		}
		astMap[astName] = astArg
	}

	var result []*ParamPlan
	//遍历生成ParamPlan
	for _, argDef := range argDefs {
		argAST, provided := astMap[argDef.Name()]

		if !provided {
			if argDef.DefaultValue != nil {
				result = append(result, newConstParamPlan(argDef.PrivateName, argDef.DefaultValue))
				continue
			}

			if isNonNullInput(argDef.Type) {
				return nil, fmt.Errorf("non-null input argument definition: %s", argDef.Name())
			}
			continue
		}

		//输入型参数
		if variable, vok := argAST.Value.(*ast.Variable); vok {
			//校验实参名是否为空
			if variable.Name == nil {
				return nil, fmt.Errorf("invalid variable definition")
			}
			inputParamPlan := newInputParamPlan(argDef.Name(), variable.Name.Value)
			if argDef.DefaultValue != nil {
				inputParamPlan.inputDefaultValue = argDef.DefaultValue
			}
			result = append(result, inputParamPlan)
			continue
		}

		//复合变量模板
		if astContainsVariable(argAST.Value) {
			result = append(result, newVariableTemplateParamPlan(argDef.PrivateName, argAST.Value, argDef.Type))
			continue
		}

		//纯字面量参数，烤制成常量
		value := valueFromAST(argAST.Value, argDef.Type, nil)
		result = append(result, newConstParamPlan(argDef.PrivateName, value))
	}
	return result, nil
}

func (compiler *PlanCompiler) compileInputValue(inputType Input, source any) (any, error) {
	//检查并拆开封装类型non-null
	if nonNullType, ok := inputType.(*NonNull); ok {
		if source == nil {
			return nil, fmt.Errorf("non null input value is required")
		}

		innerType, innerOk := nonNullType.OfType.(Input)
		if !innerOk {
			return nil, fmt.Errorf("non null input base type is required")
		}

		return compiler.compileInputValue(innerType, source)
	}

	//源数据为空直接返回
	if source == nil {
		return nil, nil
	}

	switch t := inputType.(type) {
	case *List:
		inner := t.OfType.(Input)

		if isSlice(source) {
			sourceItems := toAnySlice(source)
			result := make([]any, 0, len(sourceItems))

			for _, sourceItem := range sourceItems {
				item, err := compiler.compileInputValue(inner, sourceItem)
				if err != nil {
					return nil, err
				}

				result = append(result, item)
			}
			return result, nil
		}

		single, err := compiler.compileInputValue(inner, source)
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

				if isNonNullInput(fieldDef.Type) {
					return nil, fmt.Errorf("non-null input variable %s is required", fieldName)
				}

				continue
			}

			parsedFieldValue, parsedFieldValueErr := compiler.compileInputValue(fieldDef.Type, sourceFieldValue)
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

// 查找directive注册表中的handler，绑定到DirectivePlan上
func (compiler *PlanCompiler) bindRuntimeHandler2Directive(directivePlans []*DirectivePlan) {
	for _, plan := range directivePlans {
		if plan == nil {
			continue
		}
		//已经绑定runtimeHandler的跳过
		if plan.runtimeHandler != nil {
			continue
		}

		handler := compiler.directiveRegistry.RuntimeHandler(plan.name)
		if handler != nil {
			plan.runtimeHandler = handler
			continue
		}

		if plan.stage == DIRECTIVE_STAGE_METADATA_ONLY {
			plan.runtimeHandler = &DefaultEmptyDirectiveRuntimeHandler{}
		}
	}
}

// 合并指令编译结果。对于selection是否裁剪，以source为准。
func (compiler *PlanCompiler) mergeCompiledResults(dst *DirectiveCompileResult, src *DirectiveCompileResult) {
	if src == nil || dst == nil {
		return
	}
	if src.IncludeDecision != nil {
		if *src.IncludeDecision == false || dst.IncludeDecision == nil {
			include := *src.IncludeDecision
			dst.IncludeDecision = &include
		}
	}

	dst.RuntimePlans = append(dst.RuntimePlans, src.RuntimePlans...)
	dst.DependencyParamPlans = append(dst.DependencyParamPlans, src.DependencyParamPlans...)
}

func (compiler *PlanCompiler) compileArgsRawFromDefsAndASTs(argDefs []*Argument, argASTs []*ast.Argument) (map[string]any, error) {
	argDefMap := make(map[string]*Argument, len(argDefs))
	for _, argDef := range argDefs {
		if argDef == nil {
			return nil, errors.New("directive argument definition is required")
		}
		argDefMap[argDef.Name()] = argDef
	}

	argASTMap := make(map[string]*ast.Argument, len(argASTs))
	for _, argAST := range argASTs {
		if argAST == nil || argAST.Name == nil {
			return nil, errors.New("directive argument AST is invalid")
		}
		argName := argAST.Name.Value
		if _, exists := argDefMap[argName]; !exists {
			return nil, fmt.Errorf("unknown argument definition %s", argName)
		}
		if _, exists := argASTMap[argName]; exists {
			return nil, fmt.Errorf("duplicate argument AST %s", argName)
		}
		argASTMap[argName] = argAST
	}

	result := make(map[string]any, len(argDefs))

	for _, argDef := range argDefs {
		argName := argDef.Name()
		argAST, provided := argASTMap[argName]
		if provided {
			result[argName] = valueFromAST(argAST.Value, argDef.Type, nil)
			continue
		}

		if argDef.DefaultValue != nil {
			result[argName] = argDef.DefaultValue
			continue
		}

		if isNonNullInput(argDef.Type) {
			return nil, fmt.Errorf("non-null input variable %s is required", argName)
		}
	}
	return result, nil
}

// 提取指令位置
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

// 从graphql-go的SelectionSet编译FieldPlans
func (compiler *PlanCompiler) compileSelectionSetWithFlattenEntries(current *ast.SelectionSet, fieldTypeScope *FieldTypeScope, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, inheritedDirectives []*DirectivePlan, isIntrospection bool) ([]*FieldPlan, error) {
	if fieldTypeScope == nil {
		return nil, fmt.Errorf("field type scope is required")
	}
	if current == nil {
		return nil, nil
	}
	fieldEntries, fieldEntriesErr := compiler.flattenSelections(current, fieldTypeScope, inheritedDirectives, parentPaths)
	if fieldEntriesErr != nil {
		return nil, fieldEntriesErr
	}
	return compiler.compileFieldPlansFromEntries(fieldEntries, parentFieldId, parentFieldIsList, parentPaths, isIntrospection)
}

func (compiler *PlanCompiler) flattenSelections(selectionSet *ast.SelectionSet, fieldTypeScope *FieldTypeScope, inheritedDirectives []*DirectivePlan, parentPaths []string) ([]FieldFlattenEntry, error) {
	if selectionSet == nil {
		return nil, nil
	}
	var result []FieldFlattenEntry
	for _, selection := range selectionSet.Selections {
		fieldEntries, fieldEntriesErr := compiler.flattenOneSelection(selection, fieldTypeScope, inheritedDirectives, parentPaths)
		if fieldEntriesErr != nil {
			return nil, fieldEntriesErr
		}
		result = append(result, fieldEntries...)
	}
	return result, nil
}

func (compiler *PlanCompiler) flattenOneSelection(selection ast.Selection, parentFieldTypeScope *FieldTypeScope, inheritedDirectives []*DirectivePlan, parentPaths []string) ([]FieldFlattenEntry, error) {
	switch sel := selection.(type) {
	case *ast.Field:
		fieldName := ""
		if sel.Name != nil {
			fieldName = sel.Name.Value
		}

		responsePath := appendResponsePath(parentPaths, getASTResponseName(sel))
		//编译指令，根据指令编译期的结果判断是否返回FieldPlan
		compiledDrectives, compiledDirectivesErr := compiler.compileDirectives(sel.Directives, DirectiveLocationField, inheritedDirectives, directiveCompileScope{responsePath: responsePath, parentTypeName: fieldTypeScopeName(parentFieldTypeScope), fieldName: fieldName})
		if compiledDirectivesErr != nil {
			return nil, compiledDirectivesErr
		}
		if compiledDrectives != nil && compiledDrectives.IncludeDecision != nil && *compiledDrectives.IncludeDecision == false {
			return nil, nil
		}
		return []FieldFlattenEntry{{
			field:            *sel,
			fieldTypeScope:   parentFieldTypeScope,
			directives:       compiledDrectives.RuntimePlans,
			dependencyParams: compiledDrectives.DependencyParamPlans,
		}}, nil
	case *ast.InlineFragment:
		compiled, compiledErr := compiler.compileDirectives(sel.Directives, DirectiveLocationInlineFragment, inheritedDirectives, directiveCompileScope{responsePath: parentPaths})
		if compiledErr != nil {
			return nil, compiledErr
		}
		if compiled != nil && compiled.IncludeDecision != nil && *compiled.IncludeDecision == false {
			return nil, nil
		}
		//内联fragment允许没有类型判断，此时搭配directive使用，类型直接继承父字段
		inlineFieldTypeScope := parentFieldTypeScope
		if sel.TypeCondition != nil {
			croppedFieldTypeScope, croppedFieldTypeScopeErr := cropFieldTypeScope(compiler, parentFieldTypeScope, sel.TypeCondition)
			if croppedFieldTypeScopeErr != nil {
				return nil, croppedFieldTypeScopeErr
			}
			inlineFieldTypeScope = croppedFieldTypeScope
		}
		return compiler.flattenSelections(sel.SelectionSet, inlineFieldTypeScope, compiled.RuntimePlans, parentPaths)
	case *ast.FragmentSpread:
		//检查片段是否存在
		if sel.Name == nil {
			return nil, fmt.Errorf("no fragment spread found")
		}
		spreadCompiledDirectives, spreadCompiledDirectivesErr := compiler.compileDirectives(sel.Directives, DirectiveLocationFragmentSpread, inheritedDirectives, directiveCompileScope{responsePath: parentPaths, fragmentName: sel.Name.Value})
		if spreadCompiledDirectivesErr != nil {
			return nil, spreadCompiledDirectivesErr
		}
		if spreadCompiledDirectives != nil && spreadCompiledDirectives.IncludeDecision != nil && *spreadCompiledDirectives.IncludeDecision == false {
			return nil, nil
		}

		spreadFrag, spreadFragOk := compiler.fragments[sel.Name.Value]
		if !spreadFragOk {
			return nil, fmt.Errorf("no fragment spread found for %s", sel.Name.Value)
		}
		croppedTypeScope, croppedTypeScopeErr := cropFieldTypeScope(compiler, parentFieldTypeScope, spreadFrag.TypeCondition)
		if croppedTypeScopeErr != nil {
			return nil, croppedTypeScopeErr
		}
		fragCompiledDirectives, fragCompiledDirectivesErr := compiler.compileDirectives(spreadFrag.Directives, DirectiveLocationFragmentDefinition, spreadCompiledDirectives.RuntimePlans, directiveCompileScope{fragmentName: sel.Name.Value})
		if fragCompiledDirectivesErr != nil {
			return nil, fragCompiledDirectivesErr
		}
		if fragCompiledDirectives.IncludeDecision != nil && *fragCompiledDirectives.IncludeDecision == false {
			return nil, nil
		}
		return compiler.flattenSelections(spreadFrag.SelectionSet, croppedTypeScope, fragCompiledDirectives.RuntimePlans, parentPaths)
	}
	return nil, nil
}

// 把当前层已经展开的FieldEntries，按照最终Response字段合并后生成当前层的FieldPlan，再递归生成子字段FieldPlan
func (compiler *PlanCompiler) compileFieldPlansFromEntries(fieldEntries []FieldFlattenEntry, parentFieldId uint32, parentFieldIsList bool, parentPaths []string, isIntrospection bool) ([]*FieldPlan, error) {
	//对responseName及类型范围相同的FieldEntry合并
	type fieldBPGroupKey struct {
		responseName    string
		allowedTypeHash string
	}
	type fieldBPGroup struct {
		fieldEntries     []FieldFlattenEntry
		fieldTypeScope   *FieldTypeScope
		dependencyParams []*ParamPlan
	}

	var orderedKeys []fieldBPGroupKey
	groups := make(map[fieldBPGroupKey]*fieldBPGroup)

	for _, fieldEntry := range fieldEntries {
		key := fieldBPGroupKey{
			responseName:    getASTResponseName(&fieldEntry.field),
			allowedTypeHash: allowedFieldTypeHash(fieldEntry.fieldTypeScope),
		}

		if group, ok := groups[key]; !ok {
			orderedKeys = append(orderedKeys, key)
			groups[key] = &fieldBPGroup{
				fieldEntries:     []FieldFlattenEntry{fieldEntry},
				fieldTypeScope:   fieldEntry.fieldTypeScope,
				dependencyParams: fieldEntry.dependencyParams,
			}
		} else {
			group.fieldEntries = append(group.fieldEntries, fieldEntry)
			group.dependencyParams = append(group.dependencyParams, fieldEntry.dependencyParams...)
		}
	}
	result := make([]*FieldPlan, 0, len(orderedKeys))
	for _, key := range orderedKeys {
		group := groups[key]

		if err := compiler.validateDiscardedDirectiveOverrides(group.fieldEntries); err != nil {
			return nil, err
		}
		field := group.fieldEntries[0].field
		skipIncludeDirectiveGroups := make([][]*DirectivePlan, 0, len(group.fieldEntries))
		//每个重复字段都保留自己的 @skip/@include 条件，运行期按 OR 语义判断
		for _, entry := range group.fieldEntries {
			skipIncludeDirectives, _ := splitSkipIncludeDirectives(entry.directives)
			skipIncludeDirectiveGroups = append(skipIncludeDirectiveGroups, skipIncludeDirectives)
		}

		_, runtimeDirectives := splitSkipIncludeDirectives(group.fieldEntries[0].directives)
		fieldName := ""
		if field.Name != nil {
			fieldName = field.Name.Value
		}

		var fieldPlans []*FieldPlan
		var fieldErr error

		//根据不同类型的内省字段走不同的FieldPlan生成方法
		switch fieldName {
		case IntrospectionFieldNameMetaSchema:
			fieldPlans, fieldErr = compiler.compileIntrospectionMetaSchemaField(field, group.fieldTypeScope, parentFieldId, parentPaths, runtimeDirectives, group.dependencyParams, skipIncludeDirectiveGroups, group.fieldEntries)
		case IntrospectionFieldNameTypename:
			fieldPlans, fieldErr = compiler.compileIntrospectionTypenameField(field, group.fieldTypeScope, parentFieldId, parentFieldIsList, runtimeDirectives, group.dependencyParams, parentPaths, skipIncludeDirectiveGroups)
		case IntrospectionFieldNameMetaType:
			fieldPlans, fieldErr = compiler.compileIntrospectionMetaTypeField(field, group.fieldTypeScope, parentFieldId, runtimeDirectives, group.dependencyParams, parentPaths, skipIncludeDirectiveGroups, group.fieldEntries)
		default:
			if isIntrospection {
				var fieldPlan *FieldPlan
				fieldPlan, fieldErr = compiler.compileCommonIntrospectionField(field, parentFieldId, group.fieldTypeScope, parentPaths, runtimeDirectives, group.dependencyParams, skipIncludeDirectiveGroups, group.fieldEntries)
				if fieldErr == nil && fieldPlan != nil {
					fieldPlans = []*FieldPlan{fieldPlan}
				}
			} else {
				//默认字段走默认FieldPlan生成方法
				fieldPlans, fieldErr = compiler.compileFieldPlans(field, parentFieldId, group.fieldTypeScope, parentFieldIsList, parentPaths, runtimeDirectives, group.dependencyParams, skipIncludeDirectiveGroups, group.fieldEntries)
			}
		}
		if fieldErr != nil {
			return nil, fieldErr
		}
		result = append(result, fieldPlans...)
	}
	return result, nil
}

func (compiler *PlanCompiler) generateFieldId() uint32 {
	fid := compiler.fieldIdCounter.Add(1)
	return uint32(fid)
}

func (compiler *PlanCompiler) compileFieldPlans(current ast.Field, parentFieldId uint32, parentTypeScope *FieldTypeScope, parentFieldIsList bool, parentPaths []string, directivePlans []*DirectivePlan, directiveDependencyParams []*ParamPlan, skipIncludeDirectivePlans [][]*DirectivePlan, fieldEntries []FieldFlattenEntry) ([]*FieldPlan, error) {
	if current.Name == nil || current.Name.Value == "" {
		return nil, errors.New("field name is required")
	}
	//fieldName
	fieldName := current.Name.Value
	//ResponseName
	responseName := getASTResponseName(&current)
	//FieldId
	fieldId := compiler.generateFieldId()
	//paths
	paths := appendResponsePath(parentPaths, responseName)
	//FieldWrapperTypeInfo
	fieldDefinition, fieldDefinitionErr := getFieldDefinition(parentTypeScope.declaredType, fieldName)
	if fieldDefinitionErr != nil {
		return nil, fieldDefinitionErr
	}
	fieldWrapperTypeInfo, fieldWrapperTypeInfoErr := compiler.compileFieldWrapperTypeInfo(fieldDefinition)
	if fieldWrapperTypeInfoErr != nil {
		return nil, fieldWrapperTypeInfoErr
	}
	if fieldWrapperTypeInfo == nil {
		return nil, fmt.Errorf("compile field wrapper type info error")
	}
	if fieldDefinition.BulkResolve != nil {
		baseType, baseTypeErr := getBaseType(fieldDefinition.Type)
		if baseTypeErr != nil {
			return nil, baseTypeErr
		}
		switch baseType.(type) {
		case *Scalar, *Enum:
			return nil, fmt.Errorf("bulk resolver field %s must return object values carrying %s", responseName, fieldDefinition.BulkResultMappedFieldName)
		}
	}
	//ParentFieldKeyName
	parentKeyFieldName, parentKeyCandidateFieldNames := compiler.checkAndCompileParentKeyFieldNames(parentFieldIsList, parentTypeScope)
	// Bulk结果允许乱序和一对多，只能使用业务key关联父元素；普通逐元素resolver可回退到请求级父occurrence路径。
	if parentFieldIsList && fieldDefinition.BulkResolve != nil && parentKeyFieldName == "" {
		if len(parentKeyCandidateFieldNames) > 1 {
			// 父类型声明了多个ID字段时无法推断身份字段，此处报错而不是任选一个，
			// 否则父子映射会整体落空并静默返回空列表。
			return nil, fmt.Errorf("parent key field name for bulk resolver %s result binding is ambiguous: parent type %s declares multiple ID fields %v, declare an id: ID! field on the parent type to disambiguate", responseName, fieldTypeScopeName(parentTypeScope), parentKeyCandidateFieldNames)
		}
		return nil, fmt.Errorf("parent key field name for bulk resolver %s result binding is empty", responseName)
	}
	//ParamPlans
	paramPlans, paramPlansErr := compiler.compileParamPlansByArgDefs(fieldDefinition.Args, current.Arguments)
	if paramPlansErr != nil {
		return nil, paramPlansErr
	}

	fieldOwner := fieldOwnerFromPlanScope(paths, parentTypeScope, fieldName)
	compiler.recordFieldOwner(fieldId, fieldOwner)

	paramPlans, paramPlansErr = compiler.applyFieldParamBindings(fieldOwner, fieldId, fieldDefinition.Args, paramPlans)
	if paramPlansErr != nil {
		return nil, paramPlansErr
	}

	//当前Field是否必须在ParentField之后执行
	usesNormalResolver := fieldDefinition.Resolve != nil && (!parentFieldIsList || fieldDefinition.BulkResolve == nil)
	//父节点存在且当前节点是普通节点且父节点是List类型或者父节点类型需要运行时动态判定
	needParentFieldResponseRaw := parentFieldId > 0 && usesNormalResolver && (parentFieldIsList || (parentTypeScope != nil && parentTypeScope.dynamicTypeResolver != nil))
	if needParentFieldResponseRaw {
		hasParentFieldResponseRaw := false
		for _, paramPlan := range paramPlans {
			if paramPlan != nil && paramPlan.paramType == PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW {
				hasParentFieldResponseRaw = true
				break
			}
		}
		if !hasParentFieldResponseRaw {
			paramPlans = append(paramPlans, newFieldResponseRawParamPlan(parentFieldId))
		}
	}

	//ArrParamPlans
	arrParamPlans := append([]*ParamPlan(nil), paramPlans...)

	//ResultParentKeyName
	resultParentKeyName := fieldDefinition.BulkResultMappedFieldName
	if fieldDefinition.BulkResolve != nil && fieldDefinition.BulkResultMappedFieldName == "" {
		return nil, fmt.Errorf("result parent key field name for result binding is empty")
	}
	//FieldTypeScope
	fieldTypeScope, fieldTypeScopeErr := wrapTypeDefinition2Scope(fieldWrapperTypeInfo.baseType, compiler)
	if fieldTypeScopeErr != nil {
		return nil, fieldTypeScopeErr
	}
	//ChildrenFields
	var childrenFields []*FieldPlan
	var childrenFieldsErr error
	if len(fieldEntries) > 0 {
		var childrenEntries []FieldFlattenEntry
		childrenEntries, childrenFieldsErr = compiler.flattenChildrenFieldEntriesForField(fieldEntries, fieldTypeScope, paths)
		if childrenFieldsErr == nil {
			childrenFields, childrenFieldsErr = compiler.compileFieldPlansFromEntries(childrenEntries, fieldId, fieldWrapperTypeInfo.isList, paths, false)
		}
	} else {
		childrenFields, childrenFieldsErr = compiler.compileSelectionSetWithFlattenEntries(current.SelectionSet, fieldTypeScope, fieldId, fieldWrapperTypeInfo.isList, paths, directivePlans, false)
	}
	if childrenFieldsErr != nil {
		return nil, childrenFieldsErr
	}

	fieldPlan := &FieldPlan{
		fieldId:              fieldId,
		fieldName:            fieldName,
		responseName:         responseName,
		paths:                paths,
		fieldWrapperTypeInfo: *fieldWrapperTypeInfo,
		fieldASTs:            fieldASTsForFieldPlanAsLegacy(current, fieldEntries),
		returnType:           fieldDefinition.Type,
		parentType:           getParentCompositeFromScope(parentTypeScope),
		parentFieldId:        parentFieldId,
		resultParentKeyName:  resultParentKeyName,
		parentKeyFieldName:   parentKeyFieldName,
		childrenFields:       childrenFields,
		paramPlans:           paramPlans,
		resolverFunc:         wrapFieldResolverFunc(fieldDefinition.Resolve),
		bulkParamPlans:       arrParamPlans,
		bulkResolverFunc:     wrapFieldResolverFunc(fieldDefinition.BulkResolve),
		// FieldPlan保存字段所属父对象的类型范围；字段返回类型范围只用于递归编译childrenFields。
		fieldTypeScope:             parentTypeScope,
		directivePlans:             directivePlans,
		directiveParamPlans:        directiveDependencyParams,
		skipIncludeDirectiveGroups: skipIncludeDirectivePlans,
	}

	return []*FieldPlan{fieldPlan}, nil
}

func (compiler *PlanCompiler) compileIntrospectionTypenameField(current ast.Field, parentFieldTypeScope *FieldTypeScope, parentFieldId uint32, parentFieldIsList bool, directivePlans []*DirectivePlan, directiveDependencyParams []*ParamPlan, parentPaths []string, skipIncludeDirectiveGroups [][]*DirectivePlan) ([]*FieldPlan, error) {
	var fieldId uint32
	responseName := IntrospectionFieldNameTypename
	if current.Alias != nil && current.Alias.Value != "" {
		responseName = current.Alias.Value
	}

	fieldId = compiler.generateFieldId()
	paths := appendResponsePath(parentPaths, responseName)

	fieldWrapperTypeInfo := FieldWrapperTypeInfo{
		isList:                 false,
		notNil:                 true,
		fieldElementTypeEnum:   FIELD_ELEMENT_TYPE_SCALAR,
		baseType:               String,
		elementWrapperTypeInfo: nil,
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
	if parentFieldTypeScope != nil && parentFieldTypeScope.dynamicTypeResolver != nil {
		needParentFieldFullResult = true
	}
	if needParentFieldFullResult {
		paramPlans = append(paramPlans, &ParamPlan{
			paramType:        PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW,
			dependentFieldId: parentFieldId,
		})
	}
	//如果父节点类型是静态类型，参数中增加常量参数并直接将结果写入
	if parentFieldTypeScope != nil && parentFieldTypeScope.dynamicTypeResolver == nil && parentFieldTypeScope.staticTypeName != "" {
		typeNameParamPlan := newConstParamPlan(DefaultFieldKeyTypename, parentFieldTypeScope.staticTypeName)
		paramPlans = append(paramPlans, typeNameParamPlan)
	}
	//parentKeyFieldName
	// __typename由父元素实际类型直接完成，bindIterationResponse对它提前返回，
	// 该取值不参与父子绑定，因此候选歧义在这里无需处理。
	parentKeyFieldName, _ := compiler.checkAndCompileParentKeyFieldNames(parentFieldIsList, parentFieldTypeScope)
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
		fieldWrapperTypeInfo:       fieldWrapperTypeInfo,
		fieldASTs:                  fieldASTsForFieldPlanAsLegacy(current, nil),
		returnType:                 returnType,
		parentType:                 getParentCompositeFromScope(parentFieldTypeScope),
		resultParentKeyName:        "",
		parentKeyFieldName:         parentKeyFieldName,
		childrenFields:             childrenFields,
		paramPlans:                 paramPlans,
		resolverFunc:               TypeNameResolverFunc,
		bulkParamPlans:             nil,
		bulkResolverFunc:           nil,
		fieldTypeScope:             parentFieldTypeScope,
		directivePlans:             directivePlans,
		directiveParamPlans:        directiveDependencyParams,
		skipIncludeDirectiveGroups: skipIncludeDirectiveGroups,
	}
	return []*FieldPlan{fieldPlan}, nil
}

func (compiler *PlanCompiler) compileIntrospectionMetaTypeField(currentField ast.Field, parentTypeScope *FieldTypeScope, parentFieldId uint32, directivePlans []*DirectivePlan, directiveDependenctParams []*ParamPlan, parentPaths []string, skipIncludeDirectiveGroups [][]*DirectivePlan, fieldEntries []FieldFlattenEntry) ([]*FieldPlan, error) {
	//检查入参
	if parentFieldId != 0 || parentTypeScope == nil || parentTypeScope.declaredType != compiler.schema.QueryType() {
		return nil, fmt.Errorf("invalid __type field")
	}
	//responseName
	responseName := IntrospectionFieldNameMetaType
	if currentField.Alias != nil && currentField.Alias.Value != "" {
		responseName = currentField.Alias.Value
	}
	//fieldId
	fieldId := compiler.generateFieldId()
	//paths
	paths := appendResponsePath(parentPaths, responseName)
	//FieldWrapperTypeInfo
	fieldDef := TypeMetaFieldDef
	if fieldDef == nil {
		return nil, fmt.Errorf("__type meta field definition is nil")
	}
	fieldWrapperTypeInfo, fieldWrapperTypeInfoErr := compiler.compileFieldWrapperTypeInfo(fieldDef)
	if fieldWrapperTypeInfoErr != nil {
		return nil, fieldWrapperTypeInfoErr
	}
	if fieldWrapperTypeInfo == nil {
		return nil, fmt.Errorf("__type meta field type info is nil")
	}
	//ParamPlans
	paramPlans, paramPlansErr := compiler.compileParamPlansByArgDefs(fieldDef.Args, currentField.Arguments)
	if paramPlansErr != nil {
		return nil, paramPlansErr
	}
	//fieldTypeScope
	fieldTypeScope, fieldTypeScopeErr := wrapStaticFieldTypeScope(TypeType)
	if fieldTypeScopeErr != nil {
		return nil, fieldTypeScopeErr
	}
	//childrenFields
	var childrenFields []*FieldPlan
	var childrenFieldsErr error
	if len(fieldEntries) > 0 {
		var childFlattenEntries []FieldFlattenEntry
		childFlattenEntries, childrenFieldsErr = compiler.flattenChildrenFieldEntriesForField(fieldEntries, fieldTypeScope, paths)
		if childrenFieldsErr == nil {
			childrenFields, childrenFieldsErr = compiler.compileFieldPlansFromEntries(childFlattenEntries, fieldId, fieldWrapperTypeInfo.isList, paths, true)
		}
	} else {
		childrenFields, childrenFieldsErr = compiler.compileSelectionSetWithFlattenEntries(currentField.SelectionSet, fieldTypeScope, fieldId, fieldWrapperTypeInfo.isList, paths, directivePlans, true)
	}
	if childrenFieldsErr != nil {
		return nil, childrenFieldsErr
	}
	//resolverFunc
	resolverFunc := func(source any, params map[string]any, info ResolveInfo, ctx context.Context) (any, error) {
		name, _ := params["name"].(string)
		if name == "" {
			return nil, nil
		}
		t := compiler.schema.Type(name)
		if t == nil {
			return nil, nil
		}
		// 内省参数可能引用请求变量；变量通过 ResolveInfo 显式传入，不再隐藏在 context 中。
		return GenerateTypeMetaResult(compiler.schema, t, childrenFields, info.VariableValues), nil
	}
	fieldPlan := &FieldPlan{
		fieldId:                    fieldId,
		parentFieldId:              0,
		fieldName:                  IntrospectionFieldNameMetaType,
		responseName:               responseName,
		paths:                      paths,
		fieldWrapperTypeInfo:       *fieldWrapperTypeInfo,
		fieldASTs:                  fieldASTsForFieldPlanAsLegacy(currentField, fieldEntries),
		returnType:                 fieldDef.Type,
		parentType:                 getParentCompositeFromScope(parentTypeScope),
		childrenFields:             childrenFields,
		paramPlans:                 paramPlans,
		resolverFunc:               resolverFunc,
		fieldTypeScope:             fieldTypeScope,
		directivePlans:             directivePlans,
		directiveParamPlans:        directiveDependenctParams,
		skipIncludeDirectiveGroups: skipIncludeDirectiveGroups,
	}
	return []*FieldPlan{
		fieldPlan,
	}, nil
}

func (compiler *PlanCompiler) compileIntrospectionMetaSchemaField(current ast.Field, parentTypeScope *FieldTypeScope, parentFieldId uint32, parentPaths []string, inheritedDirectives []*DirectivePlan, directiveDependencyParams []*ParamPlan, skipIncludeDirectiveGroups [][]*DirectivePlan, fieldEntries []FieldFlattenEntry) ([]*FieldPlan, error) {
	//检查入参
	if parentTypeScope == nil || parentFieldId != 0 || parentTypeScope.declaredType != compiler.schema.QueryType() {
		return nil, fmt.Errorf("invalid schema field")
	}
	//ResponseName
	responseName := IntrospectionFieldNameMetaSchema
	if current.Alias != nil && current.Alias.Value != "" {
		responseName = current.Alias.Value
	}
	//FieldId
	fieldId := compiler.generateFieldId()
	//Paths
	paths := appendResponsePath(parentPaths, responseName)
	//FieldWrapperTypeInfo
	fieldDef := SchemaMetaFieldDef
	if fieldDef == nil {
		return nil, fmt.Errorf("__type meta field definition is nil")
	}
	fieldWrapperTypeInfo, fieldWrapperTypeInfoErr := compiler.compileFieldWrapperTypeInfo(fieldDef)
	if fieldWrapperTypeInfoErr != nil {
		return nil, fieldWrapperTypeInfoErr
	}
	if fieldWrapperTypeInfo == nil {
		return nil, fmt.Errorf("__type meta field type info is nil")
	}
	//ParamPlans
	paramPlans, paramPlansErr := compiler.compileParamPlansByArgDefs(fieldDef.Args, current.Arguments)
	if paramPlansErr != nil {
		return nil, paramPlansErr
	}
	//TypeScope
	fieldTypeScope, fieldTypeScopeErr := wrapStaticFieldTypeScope(SchemaType)
	if fieldTypeScopeErr != nil {
		return nil, fieldTypeScopeErr
	}
	//ChildrenFields
	var childrenFields []*FieldPlan
	var childrenFieldsErr error
	if len(fieldEntries) > 0 {
		var childFlattenEntries []FieldFlattenEntry
		childFlattenEntries, childrenFieldsErr = compiler.flattenChildrenFieldEntriesForField(fieldEntries, fieldTypeScope, paths)
		if childrenFieldsErr == nil {
			childrenFields, childrenFieldsErr = compiler.compileFieldPlansFromEntries(childFlattenEntries, fieldId, fieldWrapperTypeInfo.isList, paths, true)
		}
		if childrenFieldsErr != nil {
			return nil, childrenFieldsErr
		}
	} else {
		childrenFields, childrenFieldsErr = compiler.compileSelectionSetWithFlattenEntries(current.SelectionSet, fieldTypeScope, fieldId, fieldWrapperTypeInfo.isList, paths, inheritedDirectives, true)
	}
	if childrenFieldsErr != nil {
		return nil, childrenFieldsErr
	}
	//resolverFunc
	resolverFunc := func(source any, params map[string]any, info ResolveInfo, ctx context.Context) (any, error) {
		// 内省参数可能引用请求变量；变量通过 ResolveInfo 显式传入，不再隐藏在 context 中。
		return GenerateSchemaMetaResult(compiler.schema, childrenFields, info.VariableValues), nil
	}
	fieldPlan := &FieldPlan{
		fieldId:                    fieldId,
		parentFieldId:              0,
		fieldName:                  IntrospectionFieldNameMetaSchema,
		responseName:               responseName,
		paths:                      paths,
		fieldWrapperTypeInfo:       *fieldWrapperTypeInfo,
		fieldASTs:                  fieldASTsForFieldPlanAsLegacy(current, fieldEntries),
		returnType:                 fieldDef.Type,
		parentType:                 getParentCompositeFromScope(parentTypeScope),
		childrenFields:             childrenFields,
		paramPlans:                 paramPlans,
		resolverFunc:               resolverFunc,
		fieldTypeScope:             fieldTypeScope,
		directivePlans:             inheritedDirectives,
		directiveParamPlans:        directiveDependencyParams,
		skipIncludeDirectiveGroups: skipIncludeDirectiveGroups,
	}
	return []*FieldPlan{fieldPlan}, nil
}

func (compiler *PlanCompiler) compileCommonIntrospectionField(current ast.Field, parentFieldId uint32, parentTypeScope *FieldTypeScope, parentPaths []string, directives []*DirectivePlan, directiveDependencyParams []*ParamPlan, skipIncludeDirectivesGroup [][]*DirectivePlan, fieldEntries []FieldFlattenEntry) (*FieldPlan, error) {
	//fieldName
	fieldName := current.Name.Value
	//ResponseName
	responseName := getASTResponseName(&current)
	//FieldId
	fieldId := compiler.generateFieldId()
	//paths
	paths := appendResponsePath(parentPaths, responseName)
	//fieldWrapperTypeInfo
	fieldDef, fieldDefErr := getFieldDefinition(parentTypeScope.declaredType, fieldName)
	if fieldDefErr != nil {
		return nil, fieldDefErr
	}
	fieldWrapperTypeInfo, fieldWrapperTypeInfoErr := compiler.compileFieldWrapperTypeInfo(fieldDef)
	if fieldWrapperTypeInfoErr != nil {
		return nil, fieldWrapperTypeInfoErr
	}
	//ParamPlans
	paramPlans, paramPlansErr := compiler.compileParamPlansByArgDefs(fieldDef.Args, current.Arguments)
	if paramPlansErr != nil {
		return nil, paramPlansErr
	}
	//FieldTypeScope
	fieldTypeScope, fieldTypeScopeErr := wrapTypeDefinition2Scope(fieldWrapperTypeInfo.baseType, compiler)
	if fieldTypeScopeErr != nil {
		return nil, fieldTypeScopeErr
	}
	//children
	var childrenFields []*FieldPlan
	var childrenFieldsErr error
	if current.SelectionSet != nil && fieldTypeScope != nil && fieldTypeScope.declaredType != nil {
		if len(fieldEntries) > 0 {
			var childFlattenEntries []FieldFlattenEntry
			childFlattenEntries, childrenFieldsErr = compiler.flattenChildrenFieldEntriesForField(fieldEntries, fieldTypeScope, paths)
			if childrenFieldsErr == nil {
				childrenFields, childrenFieldsErr = compiler.compileFieldPlansFromEntries(childFlattenEntries, fieldId, fieldWrapperTypeInfo.isList, paths, true)
			}
		} else {
			childrenFields, childrenFieldsErr = compiler.compileSelectionSetWithFlattenEntries(current.SelectionSet, fieldTypeScope, fieldId, fieldWrapperTypeInfo.isList, paths, directives, true)
		}
	}
	if childrenFieldsErr != nil {
		return nil, childrenFieldsErr
	}
	return &FieldPlan{
		fieldId:                    fieldId,
		parentFieldId:              parentFieldId,
		fieldName:                  fieldName,
		responseName:               responseName,
		paths:                      paths,
		fieldWrapperTypeInfo:       *fieldWrapperTypeInfo,
		fieldASTs:                  fieldASTsForFieldPlanAsLegacy(current, fieldEntries),
		returnType:                 fieldDef.Type,
		parentType:                 getParentCompositeFromScope(parentTypeScope),
		childrenFields:             childrenFields,
		paramPlans:                 paramPlans,
		fieldTypeScope:             parentTypeScope,
		directivePlans:             directives,
		directiveParamPlans:        directiveDependencyParams,
		skipIncludeDirectiveGroups: skipIncludeDirectivesGroup,
	}, nil
}

// 当一个父字段已经由多个 occurrence 合并成一个 FieldPlan 后，把每个 occurrence 的子 SelectionSet 全部展开、继承各自的 directive 条件，生成统一的子 FieldEntry 列表，交给后续逻辑继续归并成子 FieldPlan
func (compiler *PlanCompiler) flattenChildrenFieldEntriesForField(fieldFlattenEntries []FieldFlattenEntry, childTypeScope *FieldTypeScope, parentPaths []string) ([]FieldFlattenEntry, error) {
	if len(fieldFlattenEntries) == 0 || childTypeScope == nil {
		return nil, nil
	}
	result := make([]FieldFlattenEntry, 0)
	for _, fieldFlattenEntry := range fieldFlattenEntries {
		if fieldFlattenEntry.field.SelectionSet == nil {
			continue
		}
		childrenFlattenEntries, err := compiler.flattenSelections(fieldFlattenEntry.field.SelectionSet, childTypeScope, fieldFlattenEntry.directives, parentPaths)
		if err != nil {
			return nil, err
		}
		result = append(result, childrenFlattenEntries...)
	}
	return result, nil
}

// 寻找父节点中用于父子结果关联的key字段。
// 返回值为(选中的字段名, 无法判定时的候选字段名列表)，二者不会同时非空。
// 判定顺序：
//  1. 父类型存在名为id且基础类型是内置ID scalar的字段，直接采用，这是约定的身份字段；
//  2. 否则收集父类型中全部基础类型为ID的字段作为候选，恰好1个时采用；
//  3. 候选为0个或>=2个时返回空字段名，由调用方决定报错还是回退到responsePath绑定。
//
// 候选>=2时不任选一个：Fields()是map，遍历顺序未定义且被运行时随机化，而这里的选择在
// 编译期一次性决定并随Plan写入缓存，任选会造成同一schema每次进程启动绑定到不同字段。
// 排序后取首位同样不可取：bulk场景下父key与BulkResultMappedFieldName的值必须语义对齐，
// 选错字段会让父子映射整体落空并静默返回空列表，稳定地选错比随机选错更难发现。
//
// 返回的候选列表只用于调用方拼装错误信息，不写入FieldPlan，因此不会进入可缓存的Plan。
func (compiler *PlanCompiler) checkAndCompileParentKeyFieldNames(parentFieldIsList bool, parentTypeScope *FieldTypeScope) (string, []string) {
	//入参检查
	if !parentFieldIsList || parentTypeScope == nil || parentTypeScope.declaredType == nil {
		return "", nil
	}
	//获取约定的父节点KeyField的名称，即id
	fieldDefinition, fieldDefinitionErr := getFieldDefinition(parentTypeScope.declaredType, ParentKeyFieldNameAsID)
	if fieldDefinitionErr == nil && fieldDefinition != nil {
		//检查id字段的类型是否为scalar
		t, te := getBaseType(fieldDefinition.Type)
		if te != nil || t == nil {
			return "", nil
		}
		if scalarType, ok := t.(*Scalar); ok && scalarType == ID {
			return ParentKeyFieldNameAsID, nil
		}
	}
	//如果不存在可用的id字段，则收集全部ID类型字段作为候选
	var parentFields FieldDefinitionMap
	switch t := parentTypeScope.declaredType.(type) {
	case *Object:
		parentFields = t.Fields()
	case *Interface:
		parentFields = t.Fields()
	default:
		return "", nil
	}

	candidateFieldNames := make([]string, 0, len(parentFields))
	for _, field := range parentFields {
		if field == nil {
			continue
		}
		fieldValueType, fieldValueTypeErr := getBaseType(field.Type)
		if fieldValueTypeErr != nil || fieldValueType == nil {
			continue
		}
		if scalarType, ok := fieldValueType.(*Scalar); ok && scalarType == ID {
			candidateFieldNames = append(candidateFieldNames, field.Name)
		}
	}
	//候选唯一时推断结果与遍历顺序无关，可以安全采用
	if len(candidateFieldNames) == 1 {
		return candidateFieldNames[0], nil
	}
	if len(candidateFieldNames) == 0 {
		return "", nil
	}
	//候选排序后回传，保证错误信息在多次运行之间稳定
	sort.Strings(candidateFieldNames)
	return "", candidateFieldNames
}

func (compiler *PlanCompiler) GetMaxFieldId(fieldPlans []*FieldPlan) uint32 {
	if compiler == nil {
		return 0
	}
	if compiler.fieldIdCounter.Load() != 0 || len(fieldPlans) == 0 {
		return compiler.fieldIdCounter.Load()
	}
	return calculateMaxFieldId(fieldPlans)
}

// 例子：[[scalar!]!]!返回scalar。获取一个字段的基础类型，返回值只能是Object/Scalar/Enum/Interface/Union这几个，封装类型NotNull/List不算
// 返回值分别是:基础类型，是否为isList，是否为NotNull，错误
func (compiler *PlanCompiler) compileFieldWrapperTypeInfo(fd *FieldDefinition) (*FieldWrapperTypeInfo, error) {
	if fd == nil {
		return nil, errors.New("no type definition provided while building field wrapper")
	}
	result := &FieldWrapperTypeInfo{}
	var currentType Output
	currentType = fd.Type

	currentValueMetaInfo := result

	for {
		switch t := currentType.(type) {
		case *NonNull:
			currentType = t.OfType

			currentValueMetaInfo.notNil = true
			currentValueMetaInfo.baseType = t
		case *List:
			currentType = t.OfType

			currentValueMetaInfo.isList = true
			currentValueMetaInfo.fieldElementTypeEnum = FIELD_ELEMENT_TYPE_LIST
			currentValueMetaInfo.baseType = t
			elementType := &FieldWrapperTypeInfo{}
			currentValueMetaInfo.elementWrapperTypeInfo = elementType
			currentValueMetaInfo = elementType
		case *Scalar:
			currentValueMetaInfo.fieldElementTypeEnum = FIELD_ELEMENT_TYPE_SCALAR
			currentValueMetaInfo.baseType = t
			return result, nil
		case *Object:
			currentValueMetaInfo.fieldElementTypeEnum = FIELD_ELEMENT_TYPE_OBJECT
			currentValueMetaInfo.baseType = t
			return result, nil
		case *Enum:
			currentValueMetaInfo.fieldElementTypeEnum = FIELD_ELEMENT_TYPE_ENUM
			currentValueMetaInfo.baseType = t
			return result, nil
		case *Interface:
			currentValueMetaInfo.fieldElementTypeEnum = FIELD_ELEMENT_TYPE_OBJECT
			currentValueMetaInfo.baseType = t
			return result, nil
		case *Union:
			currentValueMetaInfo.fieldElementTypeEnum = FIELD_ELEMENT_TYPE_OBJECT
			currentValueMetaInfo.baseType = t
			return result, nil
		default:
			return nil, fmt.Errorf("no type found for %s", currentType.Name())
		}
	}
}

func GenerateTypeMetaResult(schema *Schema, t Type, children []*FieldPlan, inputs map[string]any) map[string]any {
	if isNilInterfaceValue(t) {
		return nil
	}

	result := make(map[string]any)
	for _, child := range children {
		switch child.getFieldName() {
		case "kind":
			result[child.getResponseName()] = introspectionKind(t)
		case "name":
			result[child.getResponseName()] = typeNameOrNil(t)
		case "description":
			result[child.getResponseName()] = typeDescriptionOrNil(t)
		case "fields":
			includeDeprecated := argAsBool(child, "includeDeprecated", inputs, false)
			result[child.getResponseName()] = GenerateFieldsMetaResult(schema, child.getChildrenFields(), t, includeDeprecated, inputs)
		case "interfaces":
			result[child.getResponseName()] = GenerateInterfacesMetaResult(schema, t, child.childrenFields, inputs)
		case "possibleTypes":
			result[child.getResponseName()] = GeneratePossibleTypesMetaResult(schema, t, child.childrenFields, inputs)
		case "enumValues":
			includeDeprecated := argAsBool(child, "includeDeprecated", inputs, false)
			result[child.getResponseName()] = GenerateEnumValuesMetaResult(schema, t, child.getChildrenFields(), includeDeprecated, inputs)
		case "inputFields":
			result[child.getResponseName()] = GenerateInputFieldsMetaResult(schema, t, child.childrenFields, inputs)
		case "ofType":
			result[child.getResponseName()] = GenerateTypeMetaResult(schema, insideWrappedType(t), child.getChildrenFields(), inputs)
		case "__typename":
			result[child.getResponseName()] = TypeType.Name()
		}
	}
	return result
}

func GenerateFieldsMetaResult(schema *Schema, children []*FieldPlan, t Type, includeDeprecated bool, inputs map[string]any) any {
	var fields FieldDefinitionMap
	switch tt := t.(type) {
	case *Object:
		fields = tt.Fields()
	case *Interface:
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

func GenerateInputFieldsMetaResult(schema *Schema, t Type, children []*FieldPlan, inputs map[string]any) any {
	inputObj, ok := t.(*InputObject)
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

func GenerateSingleFieldMetaResult(schema *Schema, fd *FieldDefinition, children []*FieldPlan, inputs map[string]any) map[string]any {
	result := map[string]any{}

	for _, child := range children {
		switch child.getFieldName() {
		case "name":
			result[child.getResponseName()] = fd.Name
		case "description":
			if fd.Description == "" {
				result[child.getResponseName()] = nil
			} else {
				result[child.getResponseName()] = fd.Description
			}
		case "args":
			result[child.getResponseName()] = GenerateInputValuesMetaResult(schema, fd.Args, child.getChildrenFields(), inputs)
		case "type":
			result[child.getResponseName()] = GenerateTypeMetaResult(schema, fd.Type, child.getChildrenFields(), inputs)
		case "isDeprecated":
			result[child.getResponseName()] = fd.DeprecationReason != ""
		case "deprecationReason":
			if fd.DeprecationReason == "" {
				result[child.getResponseName()] = nil
			} else {
				result[child.getResponseName()] = fd.DeprecationReason
			}
		}
	}
	return result
}

func GeneratePossibleTypesMetaResult(schema *Schema, t Type, children []*FieldPlan, inputs map[string]any) any {
	var abs Abstract

	switch tt := t.(type) {
	case *Interface:
		abs = tt
	case *Union:
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

func GenerateInterfacesMetaResult(schema *Schema, t Type, children []*FieldPlan, inputs map[string]any) any {
	obj, ok := t.(*Object)
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

func GenerateEnumValuesMetaResult(schema *Schema, t Type, children []*FieldPlan, includeDeprecated bool, inputs map[string]any) any {
	enumType, ok := t.(*Enum)
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

func GenerateSingleEnumValueMetaResult(vd *EnumValueDefinition, t Type, children []*FieldPlan, includeDeprecated bool, inputs map[string]any) any {
	if vd == nil {
		return nil
	}

	result := map[string]any{}

	for _, child := range children {
		switch child.getFieldName() {
		case "name":
			result[child.getResponseName()] = vd.Name
		case "description":
			if vd.Description == "" {
				result[child.getResponseName()] = nil
			} else {
				result[child.getResponseName()] = vd.Description
			}
		case "isDeprecated":
			result[child.getResponseName()] = vd.DeprecationReason != ""
		case "deprecationReason":
			if vd.DeprecationReason == "" {
				result[child.getResponseName()] = nil
			} else {
				result[child.getResponseName()] = vd.DeprecationReason
			}
		case "__typename":
			result[child.getResponseName()] = EnumValueType.Name()
		}
	}
	return result
}

func GenerateInputValuesMetaResult(schema *Schema, args []*Argument, children []*FieldPlan, inputs map[string]any) []any {
	result := make([]any, 0)
	for _, arg := range args {
		result = append(result, GenerateSingleInputValueMetaResult(schema, arg, children, inputs))
	}
	return result
}

func GenerateSingleInputValueMetaResult(schema *Schema, v any, children []*FieldPlan, inputs map[string]any) map[string]any {
	result := make(map[string]any)
	for _, child := range children {
		switch child.getFieldName() {
		case "name":
			result[child.getResponseName()] = inputValueName(v)
		case "description":
			result[child.getResponseName()] = inputValueDescription(v)
		case "type":
			result[child.getResponseName()] = GenerateTypeMetaResult(schema, inputValueType(v), child.getChildrenFields(), inputs)
		case "defaultValue":
			result[child.getResponseName()] = inputValueDefaultValue(v)
		}
	}
	return result
}

// 内省结果生成规则：用 fieldName 判断标准内省字段语义，用 responseName 写结果，保证 alias 不丢失。
func GenerateSchemaMetaResult(schema *Schema, children []*FieldPlan, inputs map[string]any) map[string]any {
	if schema == nil {
		return nil
	}

	result := map[string]any{}
	for _, child := range children {
		switch child.getFieldName() {
		case "types":
			var types []any
			for _, t := range schema.TypeMap() {
				types = append(types, GenerateTypeMetaResult(schema, t, child.childrenFields, inputs))
			}
			result[child.getResponseName()] = types
		case "queryType":
			result[child.getResponseName()] = GenerateTypeMetaResult(schema, schema.QueryType(), child.childrenFields, inputs)
		case "mutationType":
			result[child.getResponseName()] = GenerateTypeMetaResult(schema, schema.MutationType(), child.childrenFields, inputs)
		case "subscriptionType":
			result[child.getResponseName()] = GenerateTypeMetaResult(schema, schema.SubscriptionType(), child.childrenFields, inputs)
		case "directives":
			var directives []any
			for _, d := range schema.Directives() {
				directives = append(directives, GenerateDirectiveMetaResult(schema, d, child.getChildrenFields(), inputs))
			}
			result[child.getResponseName()] = directives
		case "__typename":
			result[child.getResponseName()] = SchemaType.Name()
		}
	}
	return result
}

func GenerateDirectiveMetaResult(schema *Schema, d *Directive, children []*FieldPlan, inputs map[string]any) map[string]any {
	if d == nil {
		return nil
	}

	result := map[string]any{}
	for _, child := range children {
		switch child.getFieldName() {
		case "name":
			result[child.getResponseName()] = d.Name
		case "description":
			result[child.getResponseName()] = d.Description
		case "locations":
			result[child.getResponseName()] = d.Locations
		case "args":
			// __Directive.args 的元素类型是 __InputValue，必须生成 map 结构供后续嵌套字段组装。
			result[child.getResponseName()] = GenerateInputValuesMetaResult(schema, d.Args, child.getChildrenFields(), inputs)
		}
	}
	return result
}
