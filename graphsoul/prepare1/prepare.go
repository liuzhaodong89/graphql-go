package prepare1

// Package prepare1 converts a parsed GraphQL AST (ast.Document) together
// with a graphql.Schema and raw input variables into a graphsoul SGraphPlan
// that can be executed by graphsoul/core.SGraphEngine.
//
// # Mapping rules
//
//   - ast.Field.Alias (or Name) → FieldPlan.ResponseName / FieldName
//   - graphql.Scalar / graphql.Enum resolved from schema → FIELD_TYPE_SCALAR / ENUM
//   - graphql.Object / Interface resolved from schema   → FIELD_TYPE_OBJECT
//   - graphql.List wrapper                              → FieldIsList = true
//   - graphql.NonNull wrapper                           → FieldNotNil = true
//   - ast.Argument with ast.Variable value              → PARAM_TYPE_INPUT
//   - ast.Argument with literal value                   → PARAM_TYPE_CONST
//   - graphql.FieldResolveFn from schema                → wrapped as build.ResolverFunc
//
// # Resolver wrapping
//
// The graphsoul engine passes the parent field's *core.FieldResponse as the
// first argument ("source") of every child resolver.  The wrapper extracts
// the actual parent object via the firstResponseGetter interface so that the
// original graphql.FieldResolveFn receives the correct p.Source value.
//
// # Usage
//
//	plan, err := prepare1.Build(doc, schema, variables)
//	result := engine.Execute(plan, variables)

import (
	"context"
	"fmt"
	"strconv"

	graphql "github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/graphsoul/build"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

// ──────────────────────────────────────────────────────────────────────────────
// Public API
// ──────────────────────────────────────────────────────────────────────────────

// Build converts a pre-parsed AST Document, a Schema and raw input variables
// into a SGraphPlan.  The first OperationDefinition found in the document is
// used; if none is found an error is returned.
func Build(doc *ast.Document, schema graphql.Schema, variables map[string]any) (*build.SGraphPlan, error) {
	if doc == nil {
		return nil, fmt.Errorf("prepare1.Build: document is nil")
	}
	if variables == nil {
		variables = make(map[string]any)
	}

	// Collect fragment definitions and the first operation.
	frags := make(map[string]*ast.FragmentDefinition)
	var opDef *ast.OperationDefinition
	for _, node := range doc.Definitions {
		switch n := node.(type) {
		case *ast.OperationDefinition:
			if opDef == nil {
				opDef = n
			}
		case *ast.FragmentDefinition:
			if n.Name != nil {
				frags[n.Name.Value] = n
			}
		}
	}
	if opDef == nil {
		return nil, fmt.Errorf("prepare1.Build: no OperationDefinition found in document")
	}

	// Select the root Object type from the schema.
	var rootType *graphql.Object
	switch opDef.Operation {
	case ast.OperationTypeMutation:
		rootType = schema.MutationType()
	default: // query (default) and subscription treated as query here
		rootType = schema.QueryType()
	}
	if rootType == nil {
		return nil, fmt.Errorf("prepare1.Build: schema has no root type for operation %q", opDef.Operation)
	}

	p := &planner{
		nextID:    0,
		fragments: frags,
	}

	roots, err := p.buildSelectionSet(opDef.SelectionSet, rootType, nil, 0)
	if err != nil {
		return nil, err
	}
	return build.NewSGraphPlan(roots, variables), nil
}

// Parse is a convenience wrapper that parses queryStr before calling Build.
func Parse(queryStr string, schema graphql.Schema, variables map[string]any) (*build.SGraphPlan, error) {
	src := source.NewSource(&source.Source{Body: []byte(queryStr)})
	doc, err := parser.Parse(parser.ParseParams{Source: src})
	if err != nil {
		return nil, fmt.Errorf("prepare1.Parse: %w", err)
	}
	return Build(doc, schema, variables)
}

// ──────────────────────────────────────────────────────────────────────────────
// planner – internal traversal state
// ──────────────────────────────────────────────────────────────────────────────

type planner struct {
	nextID    uint32
	fragments map[string]*ast.FragmentDefinition
}

func (p *planner) allocID() uint32 {
	p.nextID++
	return p.nextID
}

// ──────────────────────────────────────────────────────────────────────────────
// SelectionSet recursion
// ──────────────────────────────────────────────────────────────────────────────

// buildSelectionSet converts every selection in ss against parentType into
// a flat list of FieldPlans.  parentPath and parentFieldId are inherited from
// the enclosing field.
func (p *planner) buildSelectionSet(
	ss *ast.SelectionSet,
	parentType *graphql.Object,
	parentPath []string,
	parentFieldId uint32,
) ([]*build.FieldPlan, error) {
	if ss == nil {
		return nil, nil
	}
	plans := make([]*build.FieldPlan, 0, len(ss.Selections))
	for _, sel := range ss.Selections {
		fps, err := p.buildSelection(sel, parentType, parentPath, parentFieldId)
		if err != nil {
			return nil, err
		}
		plans = append(plans, fps...)
	}
	return plans, nil
}

func (p *planner) buildSelection(
	sel ast.Selection,
	parentType *graphql.Object,
	parentPath []string,
	parentFieldId uint32,
) ([]*build.FieldPlan, error) {
	switch s := sel.(type) {
	case *ast.Field:
		// Skip meta-fields (__typename etc.)
		if s.Name != nil && len(s.Name.Value) > 0 && s.Name.Value[0] == '_' && len(s.Name.Value) > 1 && s.Name.Value[1] == '_' {
			return nil, nil
		}
		fp, err := p.buildField(s, parentType, parentPath, parentFieldId)
		if err != nil {
			return nil, err
		}
		return []*build.FieldPlan{fp}, nil

	case *ast.FragmentSpread:
		if s.Name == nil {
			return nil, fmt.Errorf("prepare1: FragmentSpread has no name")
		}
		frag, ok := p.fragments[s.Name.Value]
		if !ok {
			return nil, fmt.Errorf("prepare1: fragment %q not defined", s.Name.Value)
		}
		return p.buildSelectionSet(frag.SelectionSet, parentType, parentPath, parentFieldId)

	case *ast.InlineFragment:
		// Ignore type condition for now; treat as transparent wrapper.
		return p.buildSelectionSet(s.SelectionSet, parentType, parentPath, parentFieldId)
	}
	return nil, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Single field
// ──────────────────────────────────────────────────────────────────────────────

func (p *planner) buildField(
	f *ast.Field,
	parentType *graphql.Object,
	parentPath []string,
	parentFieldId uint32,
) (*build.FieldPlan, error) {
	if f.Name == nil {
		return nil, fmt.Errorf("prepare1: Field has no name")
	}
	fieldName := f.Name.Value
	responseKey := fieldName
	if f.Alias != nil && f.Alias.Value != "" {
		responseKey = f.Alias.Value
	}

	// Resolve field definition from the schema's Object type.
	fieldDef, ok := parentType.Fields()[fieldName]
	if !ok {
		return nil, fmt.Errorf("prepare1: field %q not found on type %q", fieldName, parentType.Name())
	}

	// Determine field type characteristics by unwrapping the graphql type.
	fieldType, isList, nonNull, namedType, err := analyzeType(fieldDef.Type)
	if err != nil {
		return nil, fmt.Errorf("prepare1: field %q type analysis: %w", fieldName, err)
	}

	// Build the response path for this field.
	path := make([]string, len(parentPath)+1)
	copy(path, parentPath)
	path[len(parentPath)] = responseKey

	// Convert AST arguments to ParamPlans.
	paramPlans, err := buildParamPlans(f.Arguments)
	if err != nil {
		return nil, fmt.Errorf("prepare1: field %q arguments: %w", fieldName, err)
	}

	// Wrap the schema resolver.
	var resolverFunc build.ResolverFunc
	if fieldDef.Resolve != nil {
		resolverFunc = wrapResolver(fieldDef.Resolve)
	}

	fieldId := p.allocID()

	// Recurse into sub-selection if this is an Object/Interface field.
	var children []*build.FieldPlan
	if f.SelectionSet != nil && namedType != nil {
		objType, ok := namedType.(*graphql.Object)
		if !ok {
			// Try Interface: resolve children against a synthetic helper.
			iface, isIface := namedType.(*graphql.Interface)
			if !isIface {
				return nil, fmt.Errorf(
					"prepare1: field %q has sub-selections but type %T does not support them",
					fieldName, namedType,
				)
			}
			// Build a temporary Object from the interface's field map so we
			// can reuse the same recursive path.
			objType, err = interfaceToObject(iface)
			if err != nil {
				return nil, fmt.Errorf("prepare1: field %q (interface): %w", fieldName, err)
			}
		}
		children, err = p.buildSelectionSet(f.SelectionSet, objType, path, fieldId)
		if err != nil {
			return nil, err
		}
	}

	fp := build.NewFieldPlan(build.FieldPlanOptions{
		FieldId:        fieldId,
		ParentFieldId:  parentFieldId,
		FieldName:      fieldName,
		ResponseName:   responseKey,
		FieldType:      fieldType,
		FieldIsList:    isList,
		FieldNotNil:    nonNull,
		Paths:          path,
		ParamPlans:     paramPlans,
		ResolverFunc:   resolverFunc,
		ChildrenFields: children,
	})
	return fp, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Argument → ParamPlan conversion
// ──────────────────────────────────────────────────────────────────────────────

func buildParamPlans(args []*ast.Argument) ([]*build.ParamPlan, error) {
	plans := make([]*build.ParamPlan, 0, len(args))
	for _, arg := range args {
		if arg.Name == nil {
			return nil, fmt.Errorf("argument has no name")
		}
		pp, err := argValueToParamPlan(arg.Name.Value, arg.Value)
		if err != nil {
			return nil, fmt.Errorf("argument %q: %w", arg.Name.Value, err)
		}
		plans = append(plans, pp)
	}
	return plans, nil
}

// argValueToParamPlan converts a single AST value to a ParamPlan.
//
//   - Variable ($var)  → PARAM_TYPE_INPUT  (value comes from runtime variables)
//   - Literal          → PARAM_TYPE_CONST  (value is inlined)
func argValueToParamPlan(paramKey string, val ast.Value) (*build.ParamPlan, error) {
	if val == nil {
		return build.NewConstParamPlan(paramKey, nil), nil
	}
	switch v := val.(type) {
	case *ast.Variable:
		// $variable → resolved from input variables at runtime
		if v.Name == nil {
			return nil, fmt.Errorf("variable has no name")
		}
		return build.NewInputParamPlan(paramKey, v.Name.Value), nil

	case *ast.IntValue:
		n, err := strconv.ParseInt(v.Value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cannot parse int literal %q: %w", v.Value, err)
		}
		return build.NewConstParamPlan(paramKey, n), nil

	case *ast.FloatValue:
		f, err := strconv.ParseFloat(v.Value, 64)
		if err != nil {
			return nil, fmt.Errorf("cannot parse float literal %q: %w", v.Value, err)
		}
		return build.NewConstParamPlan(paramKey, f), nil

	case *ast.StringValue:
		return build.NewConstParamPlan(paramKey, v.Value), nil

	case *ast.BooleanValue:
		return build.NewConstParamPlan(paramKey, v.Value), nil

	case *ast.EnumValue:
		return build.NewConstParamPlan(paramKey, v.Value), nil

	case *ast.ListValue:
		// Recursively convert each element; collect const values into []any.
		items := make([]any, 0, len(v.Values))
		for i, item := range v.Values {
			pp, err := argValueToParamPlan(strconv.Itoa(i), item)
			if err != nil {
				return nil, fmt.Errorf("list element %d: %w", i, err)
			}
			items = append(items, pp.GetConstValue())
		}
		return build.NewConstParamPlan(paramKey, items), nil

	case *ast.ObjectValue:
		// Convert to map[string]any.
		m := make(map[string]any, len(v.Fields))
		for _, field := range v.Fields {
			if field.Name == nil {
				continue
			}
			pp, err := argValueToParamPlan(field.Name.Value, field.Value)
			if err != nil {
				return nil, fmt.Errorf("object field %q: %w", field.Name.Value, err)
			}
			m[field.Name.Value] = pp.GetConstValue()
		}
		return build.NewConstParamPlan(paramKey, m), nil
	}
	return nil, fmt.Errorf("unsupported argument value kind %T (kind=%q)", val, val.GetKind())
}

// ──────────────────────────────────────────────────────────────────────────────
// Type analysis
// ──────────────────────────────────────────────────────────────────────────────

// analyzeType unwraps NonNull and List wrappers and determines:
//
//   - The graphsoul FieldType (SCALAR / OBJECT / ENUM)
//   - Whether the field is a list (isList)
//   - Whether the field is non-null (nonNull)
//   - The innermost named graphql.Type for further child resolution
func analyzeType(t graphql.Output) (build.FieldType, bool, bool, graphql.Type, error) {
	isList := false
	nonNull := false

	// Use interface{} to allow seamless re-assignment across different
	// graphql type interfaces (Output / Type share the same method set but
	// are distinct in Go's type system).
	var current interface{} = t
	for {
		switch ct := current.(type) {
		case *graphql.NonNull:
			nonNull = true
			current = ct.OfType
		case *graphql.List:
			isList = true
			current = ct.OfType
		case *graphql.Scalar:
			return build.FIELD_TYPE_SCALAR, isList, nonNull, ct, nil
		case *graphql.Enum:
			return build.FIELD_TYPE_ENUM, isList, nonNull, ct, nil
		case *graphql.Object:
			return build.FIELD_TYPE_OBJECT, isList, nonNull, ct, nil
		case *graphql.Interface:
			return build.FIELD_TYPE_OBJECT, isList, nonNull, ct, nil
		case *graphql.Union:
			// Union resolution requires runtime type discrimination; return
			// OBJECT and let callers handle the sub-selection limitations.
			return build.FIELD_TYPE_OBJECT, isList, nonNull, ct, nil
		default:
			return build.FIELD_TYPE_SCALAR, isList, nonNull, nil,
				fmt.Errorf("unsupported graphql type %T", current)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Interface helper
// ──────────────────────────────────────────────────────────────────────────────

// interfaceToObject constructs a temporary *graphql.Object from an Interface's
// field definitions.  This is only used for child-selection traversal; the
// resolver is expected to return the concrete type at runtime.
func interfaceToObject(iface *graphql.Interface) (*graphql.Object, error) {
	ifaceFields := iface.Fields()
	fields := make(graphql.Fields, len(ifaceFields))
	for name, def := range ifaceFields {
		fields[name] = &graphql.Field{
			Name:    def.Name,
			Type:    def.Type,
			Resolve: def.Resolve,
		}
	}
	obj := graphql.NewObject(graphql.ObjectConfig{
		Name:   iface.Name(),
		Fields: fields,
	})
	if obj.Error() != nil {
		return nil, obj.Error()
	}
	return obj, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Resolver wrapping
// ──────────────────────────────────────────────────────────────────────────────

// firstResponseGetter is satisfied by *core.FieldResponse.  Using a local
// interface avoids a direct dependency on the core package.
type firstResponseGetter interface {
	GetFirstResponse() any
}

// wrapResolver converts a standard graphql.FieldResolveFn into a graphsoul
// build.ResolverFunc.
//
// The graphsoul engine passes a *core.FieldResponse as the "source" argument
// for non-root resolvers.  This wrapper extracts the actual parent object via
// GetFirstResponse() and forwards it as p.Source so that existing resolvers
// that read p.Source continue to work correctly.
//
// For root fields the engine passes nil, which is forwarded unchanged.
func wrapResolver(fn graphql.FieldResolveFn) build.ResolverFunc {
	return func(source any, params map[string]any, ctx context.Context) (any, error) {
		// Extract the real parent object when the engine has wrapped it inside
		// a *core.FieldResponse.
		var actualSource any
		if rg, ok := source.(firstResponseGetter); ok {
			actualSource = rg.GetFirstResponse()
		} else {
			actualSource = source // nil for root fields
		}
		return fn(graphql.ResolveParams{
			Source:  actualSource,
			Args:    params,
			Context: ctx,
		})
	}
}
