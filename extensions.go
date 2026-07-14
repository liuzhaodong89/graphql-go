package graphql

import (
	"context"
	"fmt"

	"github.com/graphql-go/graphql/gqlerrors"
)

type (
	// ParseFinishFunc is called when the parse of the query is done
	ParseFinishFunc func(error)
	// parseFinishFuncHandler handles the call of all the ParseFinishFuncs from the extenisons
	parseFinishFuncHandler func(error) []gqlerrors.FormattedError

	// ValidationFinishFunc is called when the Validation of the query is finished
	ValidationFinishFunc func([]gqlerrors.FormattedError)
	// validationFinishFuncHandler responsible for the call of all the ValidationFinishFuncs
	validationFinishFuncHandler func([]gqlerrors.FormattedError) []gqlerrors.FormattedError

	// ExecutionFinishFunc is called when the execution is done
	ExecutionFinishFunc func(*Result)
	// executionFinishFuncHandler calls all the ExecutionFinishFuncs from each extension
	executionFinishFuncHandler func(*Result) []gqlerrors.FormattedError

	// ResolveFieldFinishFunc is called with the result of the ResolveFn and the error it returned
	ResolveFieldFinishFunc func(interface{}, error)
	// resolveFieldFinishFuncHandler calls the resolveFieldFinishFns for all the extensions
	resolveFieldFinishFuncHandler func(interface{}, error) []gqlerrors.FormattedError
)

// Extension is an interface for extensions in graphql
type Extension interface {
	// Init is used to help you initialize the extension
	Init(context.Context, *Params) context.Context

	// Name returns the name of the extension (make sure it's custom)
	Name() string

	// ParseDidStart is being called before starting the parse
	ParseDidStart(context.Context) (context.Context, ParseFinishFunc)

	// ValidationDidStart is called just before the validation begins
	ValidationDidStart(context.Context) (context.Context, ValidationFinishFunc)

	// ExecutionDidStart notifies about the start of the execution
	ExecutionDidStart(context.Context) (context.Context, ExecutionFinishFunc)

	// ResolveFieldDidStart notifies about the start of the resolving of a field
	ResolveFieldDidStart(context.Context, *ResolveInfo) (context.Context, ResolveFieldFinishFunc)

	// HasResult returns if the extension wants to add data to the result
	HasResult() bool

	// GetResult returns the data that the extension wants to add to the result
	GetResult(context.Context) interface{}
}

// handleExtensionsInits handles all the init functions for all the extensions in the schema
func handleExtensionsInits(p *Params) gqlerrors.FormattedErrors {
	errs := gqlerrors.FormattedErrors{}
	for _, ext := range p.Schema.extensions {
		func() {
			// catch panic from an extension init fn
			defer func() {
				if r := recover(); r != nil {
					errs = append(errs, gqlerrors.FormatError(fmt.Errorf("%s.Init: %v", ext.Name(), r.(error))))
				}
			}()
			// update context
			p.Context = ext.Init(p.Context, p)
		}()
	}
	return errs
}

// handleExtensionsParseDidStart runs the ParseDidStart functions for each extension
func handleExtensionsParseDidStart(p *Params) ([]gqlerrors.FormattedError, parseFinishFuncHandler) {
	fs := map[string]ParseFinishFunc{}
	errs := gqlerrors.FormattedErrors{}
	for _, ext := range p.Schema.extensions {
		var (
			ctx      context.Context
			finishFn ParseFinishFunc
		)
		// catch panic from an extension's parseDidStart functions
		func() {
			defer func() {
				if r := recover(); r != nil {
					errs = append(errs, gqlerrors.FormatError(fmt.Errorf("%s.ParseDidStart: %v", ext.Name(), r.(error))))
				}
			}()
			ctx, finishFn = ext.ParseDidStart(p.Context)
			// update context
			p.Context = ctx
			fs[ext.Name()] = finishFn
		}()
	}
	return errs, func(err error) []gqlerrors.FormattedError {
		errs := gqlerrors.FormattedErrors{}
		for name, fn := range fs {
			func() {
				// catch panic from a finishFn
				defer func() {
					if r := recover(); r != nil {
						errs = append(errs, gqlerrors.FormatError(fmt.Errorf("%s.ParseFinishFunc: %v", name, r.(error))))
					}
				}()
				fn(err)
			}()
		}
		return errs
	}
}

// handleExtensionsValidationDidStart notifies the extensions about the start of the validation process
func handleExtensionsValidationDidStart(p *Params) ([]gqlerrors.FormattedError, validationFinishFuncHandler) {
	fs := map[string]ValidationFinishFunc{}
	errs := gqlerrors.FormattedErrors{}
	for _, ext := range p.Schema.extensions {
		var (
			ctx      context.Context
			finishFn ValidationFinishFunc
		)
		// catch panic from an extension's validationDidStart function
		func() {
			defer func() {
				if r := recover(); r != nil {
					errs = append(errs, gqlerrors.FormatError(fmt.Errorf("%s.ValidationDidStart: %v", ext.Name(), r.(error))))
				}
			}()
			ctx, finishFn = ext.ValidationDidStart(p.Context)
			// update context
			p.Context = ctx
			fs[ext.Name()] = finishFn
		}()
	}
	return errs, func(errs []gqlerrors.FormattedError) []gqlerrors.FormattedError {
		extErrs := gqlerrors.FormattedErrors{}
		for name, finishFn := range fs {
			func() {
				// catch panic from a finishFn
				defer func() {
					if r := recover(); r != nil {
						extErrs = append(extErrs, gqlerrors.FormatError(fmt.Errorf("%s.ValidationFinishFunc: %v", name, r.(error))))
					}
				}()
				finishFn(errs)
			}()
		}
		return extErrs
	}
}

// handleExecutionDidStart handles the ExecutionDidStart functions
func handleExtensionsExecutionDidStart(p *ExecuteParams) ([]gqlerrors.FormattedError, executionFinishFuncHandler) {
	fs := map[string]ExecutionFinishFunc{}
	errs := gqlerrors.FormattedErrors{}
	for _, ext := range p.Schema.extensions {
		var (
			ctx      context.Context
			finishFn ExecutionFinishFunc
		)
		// catch panic from an extension's executionDidStart function
		func() {
			defer func() {
				if r := recover(); r != nil {
					errs = append(errs, gqlerrors.FormatError(fmt.Errorf("%s.ExecutionDidStart: %v", ext.Name(), r.(error))))
				}
			}()
			ctx, finishFn = ext.ExecutionDidStart(p.Context)
			// update context
			p.Context = ctx
			fs[ext.Name()] = finishFn
		}()
	}
	return errs, func(result *Result) []gqlerrors.FormattedError {
		extErrs := gqlerrors.FormattedErrors{}
		for name, finishFn := range fs {
			func() {
				// catch panic from a finishFn
				defer func() {
					if r := recover(); r != nil {
						extErrs = append(extErrs, gqlerrors.FormatError(fmt.Errorf("%s.ExecutionFinishFunc: %v", name, r.(error))))
					}
				}()
				finishFn(result)
			}()
		}
		return extErrs
	}
}

var sGraphExtensionsContextKey = &struct{}{}
var resolveInfoContextKey = &struct{}{}

func contextWithSGraphExtensions(ctx context.Context, extensions []Extension) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	// 只有 public Execute 会写入 extensions；底层 SGraphEngine.Execute 不主动启用半套 extension 生命周期。
	return context.WithValue(ctx, sGraphExtensionsContextKey, extensions)
}

func sGraphExtensionsFromContext(ctx context.Context) []Extension {
	if ctx == nil {
		return nil
	}
	// 没有从 public Execute 进入时返回 nil，GraphSoul 不会只触发 field hook 而跳过 execution hook。
	extensions, _ := ctx.Value(sGraphExtensionsContextKey).([]Extension)
	return extensions
}

func contextWithResolveInfo(ctx context.Context, info *ResolveInfo) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	// GraphSoul 内部 ResolverFunc 签名不带 ResolveInfo；通过 ctx 传给 wrapResolverFunc 填充 ResolveParams.Info。
	return context.WithValue(ctx, resolveInfoContextKey, info)
}

func resolveInfoFromContext(ctx context.Context) *ResolveInfo {
	if ctx == nil {
		return nil
	}
	info, _ := ctx.Value(resolveInfoContextKey).(*ResolveInfo)
	return info
}

// handleResolveFieldDidStart handles the notification of the extensions about the start of a resolve function
func handleExtensionsResolveFieldDidStart(exts []Extension, p *executionContext, i *ResolveInfo) ([]gqlerrors.FormattedError, resolveFieldFinishFuncHandler) {
	errs, ctx, finish := handleExtensionsResolveFieldDidStartWithContext(exts, p.Context, i)
	p.Context = ctx
	return errs, finish
}

func handleExtensionsResolveFieldDidStartWithContext(exts []Extension, ctx context.Context, i *ResolveInfo) ([]gqlerrors.FormattedError, context.Context, resolveFieldFinishFuncHandler) {
	fs := map[string]ResolveFieldFinishFunc{}
	errs := gqlerrors.FormattedErrors{}
	currentCtx := ctx
	if currentCtx == nil {
		currentCtx = context.Background()
	}
	for _, ext := range exts {
		var (
			nextCtx  context.Context
			finishFn ResolveFieldFinishFunc
		)
		// catch panic from an extension's resolveFieldDidStart function
		func() {
			defer func() {
				if r := recover(); r != nil {
					errs = append(errs, gqlerrors.FormatError(fmt.Errorf("%s.ResolveFieldDidStart: %v", ext.Name(), r)))
				}
			}()
			nextCtx, finishFn = ext.ResolveFieldDidStart(currentCtx, i)
			if nextCtx != nil {
				// extension 可以把 trace/span 等信息写回 ctx，后续 extension 和 resolver 都要看到最新 ctx。
				currentCtx = nextCtx
			}
			fs[ext.Name()] = finishFn
		}()
	}
	return errs, currentCtx, func(val interface{}, err error) []gqlerrors.FormattedError {
		extErrs := gqlerrors.FormattedErrors{}
		for name, finishFn := range fs {
			func() {
				// catch panic from a finishFn
				defer func() {
					if r := recover(); r != nil {
						extErrs = append(extErrs, gqlerrors.FormatError(fmt.Errorf("%s.ResolveFieldFinishFunc: %v", name, r)))
					}
				}()
				finishFn(val, err)
			}()
		}
		return extErrs
	}
}

func startSGraphResolveFieldHook(rundata *Rundata, fieldPlan *FieldPlan, ctx context.Context, listIndex int) (context.Context, resolveFieldFinishFuncHandler) {
	if ctx == nil {
		ctx = context.Background()
	}
	if rundata == nil || fieldPlan == nil {
		return ctx, nil
	}

	// ResolveInfo 需要同时包含 build 期字段元数据和请求期变量/root/fragments。
	info := buildSGraphResolveInfo(rundata, fieldPlan, responsePathForFieldPlan(fieldPlan, listIndex))
	fieldCtx := contextWithResolveInfo(ctx, &info)
	if len(rundata.extensions) == 0 {
		// 即使没有 extension，也要让 wrapResolverFunc 能从 ctx 取到 ResolveInfo。
		return fieldCtx, nil
	}

	extErrs, extCtx, finish := handleExtensionsResolveFieldDidStartWithContext(rundata.extensions, fieldCtx, &info)
	rundata.AddExtensionErrors(extErrs)
	return contextWithResolveInfo(extCtx, &info), finish
}

func finishSGraphResolveFieldHook(rundata *Rundata, finish resolveFieldFinishFuncHandler, val interface{}, err error) {
	if finish == nil {
		return
	}
	// extension 自身错误只进入 Result.Errors，不作为字段错误参与 null bubbling。
	extErrs := finish(val, err)
	if rundata != nil {
		rundata.AddExtensionErrors(extErrs)
	}
}

func buildSGraphResolveInfo(rundata *Rundata, fieldPlan *FieldPlan, path *ResponsePath) ResolveInfo {
	// plan 中只缓存 schema/AST 结构信息，请求相关的变量、root 和 extension 状态都来自 Rundata。
	info := ResolveInfo{
		FieldName:      fieldPlan.fieldName,
		FieldASTs:      fieldPlan.fieldASTs,
		Path:           path,
		ReturnType:     fieldPlan.returnType,
		ParentType:     fieldPlan.parentType,
		Fragments:      rundata.fragments,
		RootValue:      rundata.rootValue,
		Operation:      rundata.operation,
		VariableValues: rundata.originalParams,
	}
	if rundata.schema != nil {
		info.Schema = *rundata.schema
	}
	if info.ReturnType == nil {
		if returnType, ok := fieldPlan.fieldValueMetaInfo.OriginalType.(Output); ok {
			info.ReturnType = returnType
		}
	}
	return info
}

func responsePathForFieldPlan(fieldPlan *FieldPlan, listIndex int) *ResponsePath {
	if fieldPlan == nil {
		return nil
	}
	var path *ResponsePath
	paths := fieldPlan.GetPaths()
	for i, key := range paths {
		if listIndex >= 0 && i == len(paths)-1 {
			// list item 字段的 path 需要插入数组下标，例如 friends.0.name。
			path = path.WithKey(listIndex)
		}
		path = path.WithKey(key)
	}
	return path
}

func addExtensionResults(p *ExecuteParams, result *Result) {
	if len(p.Schema.extensions) != 0 {
		for _, ext := range p.Schema.extensions {
			func() {
				defer func() {
					if r := recover(); r != nil {
						result.Errors = append(result.Errors, gqlerrors.FormatError(fmt.Errorf("%s.GetResult: %v", ext.Name(), r.(error))))
					}
				}()
				if ext.HasResult() {
					if result.Extensions == nil {
						result.Extensions = make(map[string]interface{})
					}
					result.Extensions[ext.Name()] = ext.GetResult(p.Context)
				}
			}()
		}
	}
}
