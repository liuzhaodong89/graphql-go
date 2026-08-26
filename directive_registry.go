package graphql

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// DirectiveCompiler 指令编译方法接口
type DirectiveCompiler interface {
	// Compile name指令名称，Location指令实际出现的位置例如Field，Args参数键值对，schema当前engine绑定的schema
	Compile(name string, location string, args map[string]any, schema *Schema) (*DirectiveCompileResult, error)
}

// RuntimeDirectivePlanCompiler 用于参数含变量的自定义指令。
// 这类指令不能在 compile 阶段读取变量值，只能生成运行期计划。
type RuntimeDirectivePlanCompiler interface {
	RuntimeCompile(name string, location string, argPlans []*ParamPlan, schema *Schema) (*DirectiveCompileResult, error)
}

// DirectiveRuntimeHandler 指令运行时方法接口
type DirectiveRuntimeHandler interface {
	// ShouldExecute fieldPlan当前指令所属的field，directiveArgs当前指令的已经完成物化的参数表，params当前字段传给resolver的完成物化的参数表，parentResponse依赖的父节点执行结果，originalInputs本次请求的原始参数表，ctx上下文
	ShouldExecute(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (bool, error)
	// BeforeResolve fieldPlan当前指令所属的field，directiveArgs当前指令的已经完成物化的参数表，params当前字段传给resolver的完成物化的参数表，parentResponse依赖的父节点执行结果，originalInputs本次请求的原始参数表，ctx上下文
	BeforeResolve(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (map[string]any, error)
	// AfterResolve fieldPlan当前指令所属的field，directiveArgs当前指令的已经完成物化的参数表，params当前字段传给resolver的完成物化的参数表，parentResponse依赖的父节点执行结果，currentResponse当前节点执行结果，originalInputs本次请求的原始参数表，ctx上下文
	AfterResolve(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, currentResponse any, originalInputs map[string]any, ctx context.Context) (any, error)
}

// DirectiveRegistry Directive注册表
type DirectiveRegistry struct {
	mu           sync.RWMutex
	frozen       bool
	compilers    map[string]DirectiveCompiler       //指令名-Compiler，该指令在构建时如何编译
	handlers     map[string]DirectiveRuntimeHandler //指令名-RuntimeHandler，该指令在运行时是否执行step、执行前后怎么做切面修改
	metadataOnly map[string]bool                    //白名单，允许该指令没有compiler
}

func NewDirectiveRegistry() *DirectiveRegistry {
	result := &DirectiveRegistry{
		compilers:    make(map[string]DirectiveCompiler),
		handlers:     make(map[string]DirectiveRuntimeHandler),
		metadataOnly: make(map[string]bool),
	}
	// 注册默认的 skip/include：literal 参数可 compile 阶段裁剪，变量参数走运行期判断。
	result.Register("skip", SkipDirectiveCompiler{}, SkipDirectiveRuntimeHandler{})
	result.Register("include", IncludeDirectiveCompiler{}, IncludeDirectiveRuntimeHandler{})
	return result
}

func (r *DirectiveRegistry) Register(name string, compiler DirectiveCompiler, handler any) error {
	if r == nil {
		return errors.New("directive registry is nil")
	}
	if name == "" {
		return fmt.Errorf("name is empty")
	}

	var runtimeHandler DirectiveRuntimeHandler
	switch h := handler.(type) {
	case nil:
	case DirectiveRuntimeHandler:
		runtimeHandler = h
	default:
		return fmt.Errorf("unsupported directive runtime handler for %s", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return fmt.Errorf("directive registry is frozen")
	}
	if r.compilers == nil {
		r.compilers = make(map[string]DirectiveCompiler)
	}
	if r.handlers == nil {
		r.handlers = make(map[string]DirectiveRuntimeHandler)
	}
	if r.metadataOnly == nil {
		r.metadataOnly = make(map[string]bool)
	}
	if r.metadataOnly[name] {
		return fmt.Errorf("directive %s is already registered as metadata-only", name)
	}
	if compiler != nil {
		r.compilers[name] = compiler
	}
	if runtimeHandler != nil {
		r.handlers[name] = runtimeHandler
	}
	return nil
}

// RegisterMetadataOnly 注册只保留名称、位置和参数计划，但不参与运行时执行的指令。
func (r *DirectiveRegistry) RegisterMetadataOnly(name string) error {
	if r == nil {
		return errors.New("directive registry is nil")
	}
	if name == "" {
		return errors.New("directive name is empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return errors.New("directive registry is frozen")
	}
	if r.compilers[name] != nil || r.handlers[name] != nil {
		return fmt.Errorf("directive %s already has a compiler or runtime handler", name)
	}
	if r.metadataOnly == nil {
		r.metadataOnly = make(map[string]bool)
	}
	if r.metadataOnly[name] {
		return fmt.Errorf("metadata-only directive %s is already registered", name)
	}
	r.metadataOnly[name] = true
	return nil
}

func (r *DirectiveRegistry) Compiler(name string) DirectiveCompiler {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.compilers == nil {
		return nil
	}
	compiler := r.compilers[name]
	return compiler
}

func (r *DirectiveRegistry) RuntimeHandler(name string) DirectiveRuntimeHandler {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.handlers == nil {
		return nil
	}
	handler := r.handlers[name]
	return handler
}

func (r *DirectiveRegistry) MetadataOnly(name string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.metadataOnly == nil {
		return false
	}
	metadataOnly := r.metadataOnly[name]
	return metadataOnly
}

func (r *DirectiveRegistry) cloneAndFreeze() *DirectiveRegistry {
	if r == nil {
		result := NewDirectiveRegistry()
		result.frozen = true
		return result
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	result := &DirectiveRegistry{
		frozen:       true,
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

type DirectiveCompileResult struct {
	IncludeDecision      *bool            //当前selection是否要在静态编译期保留，true保留，false删除，nil表示不做静态裁剪
	RuntimePlans         []*DirectivePlan //动态运行期的指令计划
	DependencyParamPlans []*ParamPlan     //动态运行期所依赖的参数计划
}

type SkipDirectiveCompiler struct{}

func (SkipDirectiveCompiler) Compile(name string, location string, args map[string]any, schema *Schema) (*DirectiveCompileResult, error) {
	skipIf, ok := args["if"].(bool)
	if !ok {
		return nil, fmt.Errorf("if directive argument must be a boolean")
	}

	if skipIf {
		include := false
		return &DirectiveCompileResult{IncludeDecision: &include}, nil
	}

	return &DirectiveCompileResult{}, nil
}

type IncludeDirectiveCompiler struct{}

func (IncludeDirectiveCompiler) Compile(name string, location string, args map[string]any, schema *Schema) (*DirectiveCompileResult, error) {
	includeIf, ok := args["if"].(bool)

	if !ok {
		return nil, fmt.Errorf("if directive argument must be a boolean")
	}

	if !includeIf {
		include := false
		return &DirectiveCompileResult{IncludeDecision: &include}, nil
	}

	return &DirectiveCompileResult{}, nil
}

type SkipDirectiveRuntimeHandler struct{}

func (SkipDirectiveRuntimeHandler) ShouldExecute(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (bool, error) {
	skipIf, _ := directiveArgs["if"].(bool)
	return !skipIf, nil
}

func (SkipDirectiveRuntimeHandler) BeforeResolve(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (map[string]any, error) {
	return params, nil
}

func (SkipDirectiveRuntimeHandler) AfterResolve(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, currentResponse any, originalInputs map[string]any, ctx context.Context) (any, error) {
	return currentResponse, nil
}

type IncludeDirectiveRuntimeHandler struct{}

func (IncludeDirectiveRuntimeHandler) ShouldExecute(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (bool, error) {
	includeIf, _ := directiveArgs["if"].(bool)
	return includeIf, nil
}

func (IncludeDirectiveRuntimeHandler) BeforeResolve(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (map[string]any, error) {
	return params, nil
}

func (IncludeDirectiveRuntimeHandler) AfterResolve(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, currentResponse any, originalInputs map[string]any, ctx context.Context) (any, error) {
	return currentResponse, nil
}

type DefaultEmptyDirectiveRuntimeHandler struct {
}

func (DefaultEmptyDirectiveRuntimeHandler) ShouldExecute(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (bool, error) {
	return true, nil
}

func (DefaultEmptyDirectiveRuntimeHandler) BeforeResolve(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, originalInputs map[string]any, ctx context.Context) (map[string]any, error) {
	return params, nil
}

func (DefaultEmptyDirectiveRuntimeHandler) AfterResolve(fieldPlan *FieldPlan, directiveArgs map[string]any, params map[string]any, parentResponse any, currentResponse any, originalInputs map[string]any, ctx context.Context) (any, error) {
	return currentResponse, nil
}
