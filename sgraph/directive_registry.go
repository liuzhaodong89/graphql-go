package sgraph

import (
	"context"
	"fmt"

	"github.com/graphql-go/graphql"
)

// DirectiveCompiler 指令编译方法接口
type DirectiveCompiler interface {
	// Compile name指令名称，Location指令实际出现的位置例如Field，Args参数键值对，schema当前engine绑定的schema
	Compile(name string, location string, args map[string]any, schema *graphql.Schema) (*DirectiveCompileResult, error)
}

// RuntimeDirectivePlanCompiler 用于参数含变量的自定义指令。
// 这类指令不能在 compile 阶段读取变量值，只能生成运行期计划。
type RuntimeDirectivePlanCompiler interface {
	RuntimeCompile(name string, location string, argPlans []*ParamPlan, schema *graphql.Schema) (*DirectiveCompileResult, error)
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
	compilers    map[string]DirectiveCompiler       //指令名-Compiler，该指令在构建时如何编译
	handlers     map[string]DirectiveRuntimeHandler //指令名-RuntimeHandler，该指令在运行时是否执行step、执行前后怎么做切面修改
	metadataOnly map[string]bool                    //白名单，允许该指令没有compiler
}

func newDirectiveRegistry() *DirectiveRegistry {
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

func (r *DirectiveRegistry) MetadataOnly(name string) bool {
	if r == nil || r.metadataOnly == nil {
		return false
	}
	return r.metadataOnly[name]
}

type DirectiveCompileResult struct {
	IncludeDecision      bool             //当前selection是否要在静态编译期保留，true保留，false删除
	RuntimePlans         []*DirectivePlan //动态运行期的指令计划
	DependencyParamPlans []*ParamPlan     //动态运行期所依赖的参数计划
}

type SkipDirectiveCompiler struct{}

func (SkipDirectiveCompiler) Compile(name string, location string, args map[string]any, schema *graphql.Schema) (*DirectiveCompileResult, error) {
	skipIf, _ := args["if"].(bool)

	if skipIf {
		include := false
		return &DirectiveCompileResult{IncludeDecision: include}, nil
	}

	return &DirectiveCompileResult{}, nil
}

type IncludeDirectiveCompiler struct{}

func (IncludeDirectiveCompiler) Compile(name string, location string, args map[string]any, schema *graphql.Schema) (*DirectiveCompileResult, error) {
	includeIf, _ := args["if"].(bool)

	if !includeIf {
		include := false
		return &DirectiveCompileResult{IncludeDecision: include}, nil
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
