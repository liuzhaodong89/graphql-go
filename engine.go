package graphql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/printer"
	Lmap "github.com/liuzhaodong89/lockfree-collection/map"
)

type SGraphResult struct {
	response         map[string]any
	orderedResponses *SGraphResponseOrderedMap
	errors           []*FieldError
	extensionErrors  []gqlerrors.FormattedError //extension hook 错误不参与字段 null bubbling，只在最终 Result.Errors 中返回
}

func (r *SGraphResult) GetResponse() map[string]any {
	if r == nil || r.orderedResponses == nil {
		return nil // 顶层 null 传播时为 nil，对应 data: null
	}
	if plain, ok := toPlainValue(r.orderedResponses).(map[string]any); ok {
		return plain
	}
	return nil
}

// toPlainValue 把有序响应树（含嵌套 *SGraphResponseOrderedMap / []any）递归转成
// 纯 map[string]any / []any。map 无序，仅供需要普通 map 的调用方；
// 要按查询字段顺序输出 JSON 请用 SGraphResponseOrderedMap.MarshalJSON。
func toPlainValue(v any) any {
	switch val := v.(type) {
	case *SGraphResponseOrderedMap:
		if val == nil {
			return nil
		}
		m := make(map[string]any, len(val.fieldResponses))
		for _, f := range val.fieldResponses {
			m[f.key] = toPlainValue(f.value)
		}
		return m
	case []any:
		out := make([]any, len(val))
		for i, e := range val {
			out[i] = toPlainValue(e)
		}
		return out
	default:
		return v
	}
}

func (r *SGraphResult) GetOrderedResponses() *SGraphResponseOrderedMap {
	if r.orderedResponses == nil {
		return nil
	}
	return r.orderedResponses
}

func (r *SGraphResult) GetErrors() []*FieldError {
	return r.errors
}

// ToGraphQLResult 保留 SGraph 的有序响应结构，避免转换成普通 map 后丢失 query 字段顺序。
func (r *SGraphResult) ToGraphQLResult() *Result {
	result := &Result{}
	if r == nil {
		return result
	}

	result.Data = r.GetOrderedResponses()
	for _, fieldErr := range r.GetErrors() {
		if fieldErr == nil || fieldErr.err == nil {
			continue
		}
		formatted := gqlerrors.FormatError(fieldErr.err)
		formatted.Message = fieldErr.message
		if len(fieldErr.fieldPath) > 0 {
			formatted.Path = make([]any, 0, len(fieldErr.fieldPath))
			for _, path := range fieldErr.fieldPath {
				formatted.Path = append(formatted.Path, path)
			}
		}
		result.Errors = append(result.Errors, formatted)
	}
	// extension 错误已经是 gqlerrors.FormattedError，直接追加并保留其原始 message/path。
	result.Errors = append(result.Errors, r.extensionErrors...)
	return result
}

type OrderedFieldResponse struct {
	key   string
	value any
}

func (f OrderedFieldResponse) GetKey() string {
	return f.key
}

func (f OrderedFieldResponse) GetValue() any {
	return f.value
}

type SGraphResponseOrderedMap struct {
	fieldResponses []OrderedFieldResponse
	indexs         map[string]int
}

const sGraphOrderedMapIndexMin = 8

func NewSGraphResponseOrderedMap(capacity int) *SGraphResponseOrderedMap {
	result := &SGraphResponseOrderedMap{
		fieldResponses: make([]OrderedFieldResponse, 0, capacity),
	}
	if capacity >= sGraphOrderedMapIndexMin {
		// 已知字段数较多时直接按目标容量创建索引，避免懒创建后从小 map 扩容。
		result.indexs = make(map[string]int, capacity)
	}
	return result
}

func (r *SGraphResponseOrderedMap) Set(key string, value any) {
	if r == nil {
		return
	}

	if r.indexs != nil {
		if index, ok := r.indexs[key]; ok {
			r.fieldResponses[index].value = value
			return
		}
		r.indexs[key] = len(r.fieldResponses)
		r.fieldResponses = append(r.fieldResponses, OrderedFieldResponse{key, value})
		return
	}

	for i := range r.fieldResponses {
		if r.fieldResponses[i].key == key {
			r.fieldResponses[i].value = value
			return
		}
	}

	if len(r.fieldResponses)+1 >= sGraphOrderedMapIndexMin {
		// 小对象线性扫描更便宜；字段数变大后再创建索引，兼顾重复 responseName 覆盖和 Get 性能。
		r.indexs = make(map[string]int, len(r.fieldResponses)+1)
		for i, fieldResponse := range r.fieldResponses {
			r.indexs[fieldResponse.key] = i
		}
		r.indexs[key] = len(r.fieldResponses)
	}
	r.fieldResponses = append(r.fieldResponses, OrderedFieldResponse{key, value})
}

func (r *SGraphResponseOrderedMap) Get(key string) (any, bool) {
	if r == nil {
		return nil, false
	}

	if r.indexs != nil {
		index, ok := r.indexs[key]
		if !ok {
			return nil, false
		}
		return r.fieldResponses[index].value, true
	}

	for _, fieldResponse := range r.fieldResponses {
		if fieldResponse.key == key {
			return fieldResponse.value, true
		}
	}
	return nil, false
}

func (r *SGraphResponseOrderedMap) Fields() []OrderedFieldResponse {
	if r == nil {
		return nil
	}
	return r.fieldResponses
}

// MarshalJSON 按查询字段顺序序列化（spec §3.3 响应保序）；嵌套有序 map 递归生效，
// nil 指针输出 null。
func (r *SGraphResponseOrderedMap) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range r.fieldResponses {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(f.key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		val, err := json.Marshal(f.value)
		if err != nil {
			return nil, err
		}
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

type SGraphEngine struct {
	schema            Schema
	planCache         *Lmap.Lmap[string, *SGraphPlan]
	batchCache        *Lmap.Lmap[string, []*Batch]
	directiveRegistry *DirectiveRegistry
}

var sGraphEngineCache = Lmap.New[string, *SGraphEngine]()
var sGraphDirectiveRegistryCache = Lmap.New[string, *DirectiveRegistry]()

func SetSGraphDirectiveRegistry(schema Schema, registry *DirectiveRegistry) {
	cacheKey := buildSGraphSchemaIdentityKey(schema)
	if registry == nil {
		registry = NewDirectiveRegistry()
	}

	cloned := registry.Clone()
	sGraphDirectiveRegistryCache.Set(cacheKey, cloned)
	// 指令注册表变化后必须重建 engine，否则旧 plan/batch 里会继续绑定旧指令处理器。
	sGraphEngineCache.Set(cacheKey, NewSGraphEngine(schema, cloned))
}

func getSGraphEngineForSchema(schema Schema) *SGraphEngine {
	cacheKey := buildSGraphSchemaIdentityKey(schema)
	if engine, ok := sGraphEngineCache.Get(cacheKey); ok {
		return engine
	}

	registry, _ := sGraphDirectiveRegistryCache.Get(cacheKey)
	engine := NewSGraphEngine(schema, registry)
	sGraphEngineCache.Set(cacheKey, engine)
	return engine
}

// buildSGraphSchemaIdentityKey 只用于选择 engine 缓存实例；
// 请求级 plan/batch 缓存仍然由 engine 内部的 document + operationName 控制。
func buildSGraphSchemaIdentityKey(schema Schema) string {
	var raw strings.Builder

	raw.WriteString("query:")
	raw.WriteString(typePointerIdentity(schema.QueryType()))
	raw.WriteByte('\n')
	raw.WriteString("mutation:")
	raw.WriteString(typePointerIdentity(schema.MutationType()))
	raw.WriteByte('\n')
	raw.WriteString("subscription:")
	raw.WriteString(typePointerIdentity(schema.SubscriptionType()))
	raw.WriteByte('\n')

	typeNames := make([]string, 0, len(schema.typeMap))
	for name := range schema.typeMap {
		typeNames = append(typeNames, name)
	}
	sort.Strings(typeNames)
	for _, name := range typeNames {
		raw.WriteString("type:")
		raw.WriteString(name)
		raw.WriteByte('=')
		raw.WriteString(typePlanIdentity(schema.typeMap[name]))
		raw.WriteByte('\n')
	}

	directives := make([]string, 0, len(schema.directives))
	for _, directive := range schema.directives {
		if directive == nil {
			continue
		}
		directives = append(directives, fmt.Sprintf("%s@%p", directive.Name, directive))
	}
	sort.Strings(directives)
	for _, directive := range directives {
		raw.WriteString("directive:")
		raw.WriteString(directive)
		raw.WriteByte('\n')
	}

	sum := sha256.Sum256([]byte(raw.String()))
	return hex.EncodeToString(sum[:])
}

func typePlanIdentity(t Type) string {
	if t == nil {
		return "<nil>"
	}

	var raw strings.Builder
	raw.WriteString(typePointerIdentity(t))

	switch tt := t.(type) {
	case *Object:
		raw.WriteString(objectPlanIdentity(tt))
	case *Interface:
		raw.WriteString(interfacePlanIdentity(tt))
	case *Union:
		raw.WriteString(unionPlanIdentity(tt))
	}
	return raw.String()
}

func objectPlanIdentity(obj *Object) string {
	if obj == nil {
		return ""
	}

	var raw strings.Builder
	raw.WriteString("|isTypeOf:")
	raw.WriteString(funcIdentity(obj.IsTypeOf))
	raw.WriteString("|fields:")
	raw.WriteString(fieldMapPlanIdentity(obj.Fields()))
	return raw.String()
}

func interfacePlanIdentity(iface *Interface) string {
	if iface == nil {
		return ""
	}

	var raw strings.Builder
	raw.WriteString("|resolveType:")
	raw.WriteString(funcIdentity(iface.ResolveType))
	raw.WriteString("|fields:")
	raw.WriteString(fieldMapPlanIdentity(iface.Fields()))
	return raw.String()
}

func unionPlanIdentity(union *Union) string {
	if union == nil {
		return ""
	}

	var raw strings.Builder
	raw.WriteString("|resolveType:")
	raw.WriteString(funcIdentity(union.ResolveType))
	return raw.String()
}

func fieldMapPlanIdentity(fields FieldDefinitionMap) string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	var raw strings.Builder
	for _, name := range names {
		field := fields[name]
		if field == nil {
			continue
		}
		raw.WriteString(name)
		raw.WriteByte(':')
		raw.WriteString(outputTypeIdentity(field.Type))
		raw.WriteString(":resolve=")
		raw.WriteString(funcIdentity(field.Resolve))
		raw.WriteString(":batch=")
		raw.WriteString(funcIdentity(field.BatchResolve))
		raw.WriteString(":batchKey=")
		raw.WriteString(field.BatchResultMappedFieldName)
		raw.WriteByte(';')
	}
	return raw.String()
}

func outputTypeIdentity(t Type) string {
	switch tt := t.(type) {
	case *NonNull:
		return "!" + outputTypeIdentity(tt.OfType)
	case *List:
		return "[" + outputTypeIdentity(tt.OfType) + "]"
	default:
		return typePointerIdentity(t)
	}
}

func typePointerIdentity(t Type) string {
	if isNilInterfaceValue(t) {
		return "<nil>"
	}
	return fmt.Sprintf("%T:%s@%x", t, t.Name(), pointerOf(t))
}

func isNilInterfaceValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

func asListValue(value any) ([]any, bool) {
	if isNilInterfaceValue(value) {
		return nil, true
	}
	switch items := value.(type) {
	case []any:
		return items, true
	case []string:
		return copySliceToAny(items), true
	case []int:
		return copySliceToAny(items), true
	case []int8:
		return copySliceToAny(items), true
	case []int16:
		return copySliceToAny(items), true
	case []int32:
		return copySliceToAny(items), true
	case []int64:
		return copySliceToAny(items), true
	case []uint:
		return copySliceToAny(items), true
	case []uint8:
		return copySliceToAny(items), true
	case []uint16:
		return copySliceToAny(items), true
	case []uint32:
		return copySliceToAny(items), true
	case []uint64:
		return copySliceToAny(items), true
	case []float32:
		return copySliceToAny(items), true
	case []float64:
		return copySliceToAny(items), true
	case []bool:
		return copySliceToAny(items), true
	case []map[string]any:
		return copySliceToAny(items), true
	}

	// resolver 可能返回 []string、[]map[string]any 等 typed slice；
	// 常见类型先走上面的 type switch 快路径，未知 typed slice 再用反射兜底。
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		result := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			result[i] = rv.Index(i).Interface()
		}
		return result, true
	default:
		return nil, false
	}
}

func copySliceToAny[T any](items []T) []any {
	if items == nil {
		return nil
	}
	result := make([]any, len(items))
	for i := range items {
		result[i] = items[i]
	}
	return result
}

func funcIdentity(fn any) string {
	if fn == nil {
		return "0"
	}
	return fmt.Sprintf("%x", pointerOf(fn))
}

func pointerOf(v any) uintptr {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return 0
	}
	switch rv.Kind() {
	case reflect.Func, reflect.Ptr, reflect.Chan, reflect.Map, reflect.Slice, reflect.UnsafePointer:
		if rv.IsNil() {
			return 0
		}
		return rv.Pointer()
	default:
		return 0
	}
}

func NewSGraphEngine(schema Schema, registry ...*DirectiveRegistry) *SGraphEngine {
	directiveRegistry := NewDirectiveRegistry()
	if len(registry) > 0 && registry[0] != nil {
		// engine 创建后固定 directiveRegistry，避免外部继续 Register 后污染已缓存的 plan。
		directiveRegistry = registry[0].Clone()
	}
	return &SGraphEngine{
		schema:            schema,
		planCache:         Lmap.New[string, *SGraphPlan](),
		batchCache:        Lmap.New[string, []*Batch](),
		directiveRegistry: directiveRegistry,
	}
}

func (e *SGraphEngine) getBatchesFromCacheOrCreate(cacheKey string, plan *SGraphPlan) []*Batch {
	if plan == nil {
		return nil
	}
	// batch 只依赖 plan 结构，不包含请求变量/ctx/root；放在 plan 内用 sync.Once 缓存，避免每请求查 batchCache。
	plan.batchesOnce.Do(func() {
		plan.batches = BuildBatches(plan)
	})
	return plan.batches
}

func (e *SGraphEngine) Execute(document *ast.Document, args map[string]any, operationName *string, root map[string]any, ctx context.Context) *SGraphResult {
	if e == nil {
		return NewSGraphErrorResult(errors.New("sgraph engine is nil"))
	}
	if e.schema.QueryType() == nil {
		return NewSGraphErrorResult(errors.New("sgraph engine schema is nil"))
	}
	if e.planCache == nil {
		e.planCache = Lmap.New[string, *SGraphPlan]()
	}
	if e.batchCache == nil {
		e.batchCache = Lmap.New[string, []*Batch]()
	}
	if e.directiveRegistry == nil {
		e.directiveRegistry = NewDirectiveRegistry()
	}

	cacheKey := buildPlanCacheKey(document, operationName)
	plan, ok := e.planCache.Get(cacheKey)
	if !ok {
		var buildErr error
		plan, buildErr = BuildGraphPlan(document, &e.schema, operationName, e.directiveRegistry)
		if buildErr != nil {
			return NewSGraphErrorResult(buildErr)
		}
		if plan.GetOperationType() == SGraphOperationTypeSubscription {
			return NewSGraphErrorResult(errors.New("subscription is not supported by graphsoul execute"))
		}
		e.planCache.Set(cacheKey, plan)
	}

	inputs, inputErr := CoerceOperationVariables(document, &e.schema, args, operationName)
	if inputErr != nil {
		return NewSGraphErrorResult(inputErr)
	}

	return e.executePlan(plan, cacheKey, inputs, root, ctx)
}

func buildPlanCacheKey(document *ast.Document, operationName *string) string {
	hash := sha256.New()
	if document != nil && document.Loc != nil && document.Loc.Source != nil && len(document.Loc.Source.Body) > 0 {
		// parser 产出的 AST 保留了原始 query 字节；直接参与 hash，避免每个请求重新打印 AST。
		hash.Write(document.Loc.Source.Body)
	} else {
		// 兼容外部手工构造 AST 且没有 Source 的场景，保留旧的规范化打印逻辑兜底。
		hash.Write([]byte(fmt.Sprintf("%v", printer.Print(document))))
	}
	hash.Write([]byte{'\n'})
	if operationName != nil {
		hash.Write([]byte(*operationName))
	}
	// schema 和 directiveRegistry 已经由 SGraphEngine 实例固定；
	// 同一个 engine 内的 plan identity 只需要 document + operationName。
	return hex.EncodeToString(hash.Sum(nil))
}

func NewSGraphErrorResult(err error) *SGraphResult {
	result := &SGraphResult{}
	if err != nil {
		result.errors = []*FieldError{{
			err:       err,
			message:   err.Error(),
			errorType: FieldErrorTypeTree,
		}}
	}
	return result
}

func (e *SGraphEngine) executePlan(plan *SGraphPlan, cacheKey string, inputs map[string]any, root map[string]any, ctx context.Context) *SGraphResult {
	//组装Rundata和context
	maxFieldId := plan.MaxFieldId()
	rundata := NewRundata(inputs, maxFieldId)
	// 以下字段是本次请求的 ResolveInfo / extension 上下文，不能写入可缓存的 plan。
	rundata.schema = &e.schema
	rundata.rootValue = root
	rundata.operation = plan.operation
	rundata.fragments = plan.fragments
	rundata.extensions = sGraphExtensionsFromContext(ctx)
	// Rundata 是请求级对象；必须等 batch 执行和结果组装完成后再释放，避免并发读写和结果污染。
	defer ReleaseRundata(rundata)
	if ctx == nil {
		ctx = context.TODO()
	}
	//把本请求变量放进 ctx，供内省 resolver 运行期取（不再捕获 build 期 inputs）
	ctx = ContextWithInputs(ctx, rundata.originalParams)
	//组装Batches
	batches := e.getBatchesFromCacheOrCreate(cacheKey, plan)
	//遍历执行Batches，判断遇到中断则返回
	for _, batch := range batches {
		br := batch.Execute(rundata, ctx)
		if br.IsInterrupt() {
			break
		}
	}
	//组装结果
	result := e.assembleGraphResult(plan, rundata, root, ctx)
	return result
}

func (e *SGraphEngine) assembleGraphResult(plan *SGraphPlan, rundata *Rundata, root map[string]any, ctx context.Context) *SGraphResult {
	result := &SGraphResult{
		response:         make(map[string]any),
		orderedResponses: nil,
		errors:           nil,
		extensionErrors:  nil,
	}
	if plan == nil {
		return result
	}

	roots := plan.GetRoots()
	//responseMap := make(map[string]any, len(roots))
	orderedResponsesMap := NewSGraphResponseOrderedMap(len(roots))

	for _, rootField := range roots {
		rootValueMeta := rootField.GetFieldValueMetaInfo()
		included, includeErr := shouldIncludeField(rootField, rundata, ctx)
		if includeErr != nil {
			rundata.AddFieldError(rootField.GetFieldId(), FieldErrorTypeField, includeErr, rootField.GetPaths())
			if rootValueMeta.NotNil {
				orderedResponsesMap = nil
				break
			}
			orderedResponsesMap.Set(rootField.GetResponseName(), nil)
			continue
		}
		if !included {
			continue
		}

		if rootValueMeta.IsList {
			rootResult := e.buildListValues(rootField, root, rundata, ctx)
			//null值传递
			if rootResult == nil {
				if rootValueMeta.NotNil {
					//responseMap = nil
					orderedResponsesMap = nil
					break
				}
				orderedResponsesMap.Set(rootField.GetResponseName(), nil)
			} else {
				//responseMap[root.GetResponseName()] = rootResult
				orderedResponsesMap.Set(rootField.GetResponseName(), rootResult)
			}
		} else {
			if rootField.GetFieldType() == FieldValueTypeObject {
				rootResult := e.buildObjectValue(rootField, root, rundata, ctx)
				//null值传递
				if rootResult == nil && rootValueMeta.NotNil {
					//responseMap = nil
					orderedResponsesMap = nil
					break
				} else {
					//responseMap[root.GetResponseName()] = rootResult
					orderedResponsesMap.Set(rootField.GetResponseName(), rootResult)
				}
			} else if rootField.GetFieldType() == FieldValueTypeScalar || rootField.GetFieldType() == FieldValueTypeEnum {
				rootResult := e.buildScalarOrEnumValue(rootField, root, rundata, ctx)
				//null值传递
				if rootResult == nil && rootValueMeta.NotNil {
					//responseMap = nil
					orderedResponsesMap = nil
					break
				} else {
					//responseMap[root.GetResponseName()] = rootResult
					orderedResponsesMap.Set(rootField.GetResponseName(), rootResult)
				}
			}
		}

	}
	//result.response = responseMap
	result.orderedResponses = orderedResponsesMap
	result.errors = rundata.GetAllFieldErrors()
	// field hook 错误在 Rundata 中并发收集，组装 Result 时统一取副本。
	result.extensionErrors = rundata.GetExtensionErrors()
	return result

}

func (e *SGraphEngine) buildObjectValue(field *FieldPlan, parentResponse any, rundata *Rundata, ctx context.Context) *SGraphResponseOrderedMap {
	if field != nil {
		children := field.GetChildrenFields()
		//result := make(map[string]any, len(children))
		result := NewSGraphResponseOrderedMap(len(children))

		fieldResponse, extractErr := e.extractFieldResponse(field, parentResponse, rundata, ctx)
		//如果当前字段的结果为空，不再遍历子字段
		if isNilInterfaceValue(fieldResponse) || extractErr != nil {
			return nil
		}
		runtimeTypeName, valid := e.validateAbstractFieldValue(field, fieldResponse, rundata, ctx)
		if !valid {
			return nil
		}
		for _, child := range children {
			childValueMeta := child.GetFieldValueMetaInfo()
			included, includeErr := shouldIncludeField(child, rundata, ctx)
			if includeErr != nil {
				rundata.AddFieldError(child.GetFieldId(), FieldErrorTypeField, includeErr, child.GetPaths())
				if childValueMeta.NotNil {
					return nil
				}
				result.Set(child.GetResponseName(), nil)
				continue
			}
			if !included {
				continue
			}
			// fragment type condition 必须在组装阶段再次生效，避免无 resolver 字段绕过 step 执行阶段的动态类型过滤。
			if isFieldPlanCompiledType(child) {
				if !shouldExecuteFieldAsCompiledType(child, &ctx) {
					continue
				}
			} else if !shouldExecuteFieldAsResolvedRuntimeType(child, runtimeTypeName) {
				continue
			}
			if childValueMeta.IsList {
				//如果子字段是List类型且List为non-null但是出现nil，则判断当前字段是否为non-null，如果是则清空当前字段的数据返回nil
				childResult := e.buildListValues(child, fieldResponse, rundata, ctx)
				//null值传递，如果子字段non-null但返回nil，当前字段直接返回nil
				if childResult == nil {
					if childValueMeta.NotNil {
						return nil
					}
					result.Set(child.GetResponseName(), nil)
				} else {
					//result[child.GetResponseName()] = childResult
					result.Set(child.GetResponseName(), childResult)
				}
			} else {
				switch child.GetFieldType() {
				case FieldValueTypeObject:
					childResult := e.buildObjectValue(child, fieldResponse, rundata, ctx)
					//null值传递
					if childResult == nil && childValueMeta.NotNil {
						return nil
					}
					//result[child.GetResponseName()] = childResult
					result.Set(child.GetResponseName(), childResult)
				case FieldValueTypeScalar, FieldValueTypeEnum:
					childResult := e.buildScalarOrEnumValue(child, fieldResponse, rundata, ctx)
					//null值传递
					if childValueMeta.NotNil && childResult == nil {
						return nil
					}
					//result[child.GetResponseName()] = childResult
					result.Set(child.GetResponseName(), childResult)
				}
			}
		}
		return result
	}
	return nil
}

func (e *SGraphEngine) buildListValues(field *FieldPlan, parentResponse any, rundata *Rundata, ctx context.Context) []any {
	if field == nil {
		return nil
	}

	fieldResponse, extractErr := e.extractFieldResponse(field, parentResponse, rundata, ctx)
	if extractErr != nil || isNilInterfaceValue(fieldResponse) {
		return nil
	}
	fieldResponseAsList, ok := asListValue(fieldResponse)
	if !ok {
		return nil
	}

	fieldValueMeta := field.GetFieldValueMetaInfo()
	return e.buildListItems(field, fieldValueMeta.ElementType, fieldResponseAsList, rundata, ctx)
}

func (e *SGraphEngine) buildListItems(field *FieldPlan, elementMeta *FieldValueMetaInfo, items []any, rundata *Rundata, ctx context.Context) []any {
	if elementMeta == nil {
		return nil
	}

	// list 字段的顶层 meta 只说明“这是 list”，真正的元素类型在 ElementType 上；
	// 因此这里按元素 meta 递归组装 scalar/object/nested-list。
	result := make([]any, 0, len(items))
	for _, item := range items {
		if isNilInterfaceValue(item) {
			if elementMeta.NotNil {
				return nil
			}
			result = append(result, nil)
			continue
		}

		if elementMeta.IsList {
			childItems, ok := asListValue(item)
			if !ok {
				if elementMeta.NotNil {
					return nil
				}
				result = append(result, nil)
				continue
			}
			childResult := e.buildListItems(field, elementMeta.ElementType, childItems, rundata, ctx)
			if childResult == nil && elementMeta.NotNil {
				return nil
			}
			if childResult == nil {
				result = append(result, nil)
			} else {
				result = append(result, childResult)
			}
			continue
		}

		switch elementMeta.ValueType {
		case FieldValueTypeScalar, FieldValueTypeEnum:
			serialized := serializeLeafValue(field, item)
			if serialized == nil && elementMeta.NotNil {
				return nil
			}
			result = append(result, serialized)
		case FieldValueTypeObject:
			runtimeTypeName, valid := e.validateAbstractFieldValue(field, item, rundata, ctx)
			if !valid {
				if elementMeta.NotNil {
					return nil
				}
				result = append(result, nil)
				continue
			}
			objectResult := e.buildObjectValueInList(field, item, rundata, ctx, runtimeTypeName)
			if objectResult == nil && elementMeta.NotNil {
				return nil
			}
			result = append(result, objectResult)
		default:
			result = append(result, item)
		}
	}
	return result
}

func (e *SGraphEngine) buildObjectValueInList(field *FieldPlan, currentFieldResponse any, rundata *Rundata, ctx context.Context, runtimeTypeName string) *SGraphResponseOrderedMap {
	//var result map[string]any
	var result *SGraphResponseOrderedMap
	//如果当前元素的response为空，则直接返回，不再遍历子字段
	if isNilInterfaceValue(currentFieldResponse) {
		return result
	}
	//TODO 获取当前字段的子字段，遍历每个子字段的类型组装Map
	if field != nil {
		children := field.GetChildrenFields()
		//result = make(map[string]any, len(children))
		result = NewSGraphResponseOrderedMap(len(children))
		for _, child := range children {
			childValueMeta := child.GetFieldValueMetaInfo()
			included, includeErr := shouldIncludeField(child, rundata, ctx)
			if includeErr != nil {
				rundata.AddFieldError(child.GetFieldId(), FieldErrorTypeField, includeErr, child.GetPaths())
				if childValueMeta.NotNil {
					return nil
				}
				result.Set(child.GetResponseName(), nil)
				continue
			}
			if !included {
				continue
			}
			// fragment type condition 必须按当前 list 元素的运行时类型过滤，不能只依赖 resolver step 是否执行。
			if isFieldPlanCompiledType(child) {
				if !shouldExecuteFieldAsCompiledType(child, &ctx) {
					continue
				}
			} else if !shouldExecuteFieldAsResolvedRuntimeType(child, runtimeTypeName) {
				continue
			}
			// 动态 __typename 是当前 list item 的运行时类型派生值，不能依赖 parent key 绑定。
			// union list 没有稳定的 parentKeyFieldName，继续走 composite key 会让多个 item 覆盖到同一个 key。
			if child.IsIntrospectionTypeNameField() && child.GetRuntimeTypeResolverFunc() != nil {
				if runtimeTypeName == "" {
					rundata.AddFieldError(child.GetFieldId(), FieldErrorTypeField, errors.New("__typename resolved failed, value is empty"), child.GetPaths())
					if childValueMeta.NotNil {
						return nil
					}
					result.Set(child.GetResponseName(), nil)
					continue
				}
				result.Set(child.GetResponseName(), serializeLeafValue(child, runtimeTypeName))
				continue
			}
			if childValueMeta.IsList {
				childResult := e.buildListValuesInListObject(child, currentFieldResponse, rundata, ctx)
				//null值冒泡
				if childResult == nil {
					if childValueMeta.NotNil {
						return nil
					}
					result.Set(child.GetResponseName(), nil)
				} else {
					//result[child.GetResponseName()] = childResult
					result.Set(child.GetResponseName(), childResult)
				}
			} else {
				switch child.GetFieldType() {
				case FieldValueTypeObject:
					childResult := e.buildObjectValueInListObject(child, currentFieldResponse, rundata, ctx)
					//null值冒泡
					if childValueMeta.NotNil && childResult == nil {
						return nil
					}
					//result[child.GetResponseName()] = childResult
					result.Set(child.GetResponseName(), childResult)
				case FieldValueTypeScalar, FieldValueTypeEnum:
					childResult := rundata.GetFieldResultByFieldId(child.GetFieldId())
					if childResult != nil && childResult.HasParentResultBinding() {
						if currentFieldResponseMap, ok := currentFieldResponse.(map[string]any); ok {
							compositeKey := GenerateCompositeKey([]string{child.GetParentKeyFieldName()}, currentFieldResponseMap)
							if val, ok := childResult.LookupParentResult(compositeKey); ok {
								//result[child.GetResponseName()] = val
								result.Set(child.GetResponseName(), serializeLeafValue(child, val))
							} else {
								//null值传递
								if childValueMeta.NotNil {
									return nil
								}
								//result[child.GetResponseName()] = nil
								result.Set(child.GetResponseName(), nil)
							}
						} else {
							if childValueMeta.NotNil {
								return nil
							}
							result.Set(child.GetResponseName(), nil)
						}
					} else {
						//TODO 这里考虑修改buildScalarOrEnumValue方法，直接从父节点数据中组装，不用再查询rundata本节点的数据
						scalarOrEnumResult := e.buildScalarOrEnumValue(child, currentFieldResponse, rundata, ctx)
						if scalarOrEnumResult != nil {
							//result[child.GetResponseName()] = scalarOrEnumResult
							result.Set(child.GetResponseName(), scalarOrEnumResult)
						} else {
							//null值传递
							if childValueMeta.NotNil {
								return nil
							}
							//result[child.GetResponseName()] = nil
							result.Set(child.GetResponseName(), nil)
						}
					}
				}
			}
		}
	}
	return result
}

// 对于List类型的字段返回值，由于内部的null值冒泡在resolve阶段已经冒泡完成，组装阶段仅做组装处理
func (e *SGraphEngine) buildListValuesInListObject(field *FieldPlan, parentFieldResponse any, rundata *Rundata, ctx context.Context) []any {
	if field == nil {
		return nil
	}

	currentFieldResponse, extractErr := e.extractFieldResponse(field, parentFieldResponse, rundata, ctx)
	if extractErr != nil || isNilInterfaceValue(currentFieldResponse) {
		return nil
	}
	currentFieldResponseArray, ok := asListValue(currentFieldResponse)
	if !ok {
		return nil
	}

	fieldValueMetaInfo := field.GetFieldValueMetaInfo()
	return e.buildListItems(field, fieldValueMetaInfo.ElementType, currentFieldResponseArray, rundata, ctx)
}

func (e *SGraphEngine) buildObjectValueInListObject(field *FieldPlan, parentResponse any, rundata *Rundata, ctx context.Context) *SGraphResponseOrderedMap {
	//var result map[string]any
	var result *SGraphResponseOrderedMap
	if field != nil {
		fieldValueMetaInfo := field.GetFieldValueMetaInfo()

		//获取当前字段的结果，如果有运行过程取运行时数据，没有则从父字段结果读取
		fieldResponse, extractErr := e.extractFieldResponse(field, parentResponse, rundata, ctx)
		if extractErr != nil {
			return nil
		}
		//如果当前字段为non-null，返回值为nil，将nil直接返回
		if isNilInterfaceValue(fieldResponse) && fieldValueMetaInfo.NotNil {
			return nil
		}
		runtimeTypeName := ""
		if !isNilInterfaceValue(fieldResponse) {
			var valid bool
			runtimeTypeName, valid = e.validateAbstractFieldValue(field, fieldResponse, rundata, ctx)
			if !valid {
				return nil
			}
		}

		//根据子节点继续遍历生成map
		children := field.GetChildrenFields()
		//result = make(map[string]any, len(children))
		result = NewSGraphResponseOrderedMap(len(children))
		// 非空抽象字段已经由 validateAbstractFieldValue 解析完成，不再二次调用 ResolveType/IsTypeOf。
		runtimeTypeResolved := runtimeTypeName != ""
		resolveRuntimeTypeName := func(child *FieldPlan) string {
			if !runtimeTypeResolved && child.GetRuntimeTypeResolverFunc() != nil {
				// 嵌套对象组装期间 runtime type 固定，避免每个 fragment 字段重复 ResolveType/IsTypeOf。
				// fieldResponse 为 nil 时未进入抽象类型校验，保留原有兜底解析行为。
				runtimeTypeName = child.GetRuntimeTypeResolverFunc()(fieldResponse, &ctx)
				runtimeTypeResolved = true
			}
			return runtimeTypeName
		}
		for _, child := range children {
			childValueMetaInfo := child.GetFieldValueMetaInfo()
			included, includeErr := shouldIncludeField(child, rundata, ctx)
			if includeErr != nil {
				rundata.AddFieldError(child.GetFieldId(), FieldErrorTypeField, includeErr, child.GetPaths())
				if childValueMetaInfo.NotNil {
					return nil
				}
				result.Set(child.GetResponseName(), nil)
				continue
			}
			if !included {
				continue
			}
			// fragment type condition 属于字段收集/组装语义，嵌套对象也要按当前对象的运行时类型过滤。
			if isFieldPlanCompiledType(child) {
				if !shouldExecuteFieldAsCompiledType(child, &ctx) {
					continue
				}
			} else if !shouldExecuteFieldAsResolvedRuntimeType(child, resolveRuntimeTypeName(child)) {
				continue
			}
			if childValueMetaInfo.IsList {
				//对于List类型字段，null值冒泡已经在resolve阶段完成，这里只做组装
				childResult := e.buildListValuesInListObject(child, fieldResponse, rundata, ctx)
				//null值传递
				if childResult == nil {
					if childValueMetaInfo.NotNil {
						return nil
					}
					result.Set(child.GetResponseName(), nil)
				} else {
					//result[child.GetResponseName()] = childResult
					result.Set(child.GetResponseName(), childResult)
				}
			} else {
				switch child.GetFieldType() {
				case FieldValueTypeObject:
					childResult := e.buildObjectValueInListObject(child, fieldResponse, rundata, ctx)
					//null值传递
					if childResult == nil && childValueMetaInfo.NotNil {
						return nil
					}
					//result[child.GetResponseName()] = childResult
					result.Set(child.GetResponseName(), childResult)
				case FieldValueTypeScalar, FieldValueTypeEnum:
					childResult := e.buildScalarOrEnumValueInListObject(child, fieldResponse, rundata, ctx)
					//null值传递
					if childResult == nil && childValueMetaInfo.NotNil {
						return nil
					}
					//result[child.GetResponseName()] = childResult
					result.Set(child.GetResponseName(), childResult)
				}
			}
		}
	}
	return result
}

func (e *SGraphEngine) buildScalarOrEnumValueInListObject(field *FieldPlan, parentResponse any, rundata *Rundata, ctx context.Context) any {
	if field != nil {
		//TODO 先获取当前节点的全部数据
		val, extractErr := e.extractFieldResponse(field, parentResponse, rundata, ctx)
		if extractErr != nil {
			return nil
		}
		return serializeLeafValue(field, val)
		//fieldResult := rundata.GetFieldResultByFieldId(field.GetFieldId())

		//if fieldResult != nil {
		//	arrParamPlan := field.GetArrParamPlan()
		//	if arrParamPlan == nil {
		//		return parentResponse[field.GetFieldName()]
		//	}
		//	//获取parentKey的名称
		//	parentKeyFieldName := arrParamPlan.GetParamKey()
		//	//根据parentKey获取父节点response里对应的key值
		//	parentKeyFieldVal, ok := parentResponse[parentKeyFieldName]
		//	if ok {
		//		//根据key值获取到当前节点里对应的数据
		//		response, _ := fieldResult.LookupParentResult(parentKeyFieldVal)
		//		return response
		//	}
		//} else {
		//	//TODO 如果当前节点数据为空，直接取父节点的数据写入并返回
		//	return parentResponse[field.GetFieldName()]
		//}
	}
	return nil
}

func (e *SGraphEngine) buildScalarOrEnumValue(currentField *FieldPlan, parentResponse any, rundata *Rundata, ctx context.Context) any {
	if currentField != nil {
		filedType := currentField.GetFieldType()
		switch filedType {
		case FieldValueTypeScalar, FieldValueTypeEnum:
			originalResponse, extractErr := e.extractFieldResponse(currentField, parentResponse, rundata, ctx)
			if extractErr != nil {
				return nil
			}
			return serializeLeafValue(currentField, originalResponse)
		default:
			return nil
		}
	}
	return nil
}

func (e *SGraphEngine) extractFieldResponse(fieldPlan *FieldPlan, parentResponse any, rundata *Rundata, ctx context.Context) (any, error) {
	// 当前字段有 Resolve 或 BatchResolve 时，优先从 rundata 读取执行结果。
	if fieldPlan == nil {
		return nil, nil
	}
	if rundata == nil {
		return nil, fmt.Errorf("rundata is nil")
	}
	hasResolver := fieldPlan.GetResolverFunc() != nil || fieldPlan.GetArrayResolverFunc() != nil
	if hasResolver {
		fieldResult := rundata.GetFieldResultByFieldId(fieldPlan.GetFieldId())
		if fieldResult != nil {
			// list parent 下的 resolver 子字段必须按 composite key 取当前父元素对应的结果。
			if fieldResult.HasParentResultBinding() {
				parentResponseMap, ok := parentResponse.(map[string]any)
				if !ok {
					return nil, nil
				}
				parentKeyFieldName := fieldPlan.GetParentKeyFieldName()
				if parentKeyFieldName == "" {
					return nil, nil
				}
				compositeKey := GenerateCompositeKey([]string{parentKeyFieldName}, parentResponseMap)
				if bindingChildResponse, ok := fieldResult.LookupParentResult(compositeKey); ok {
					return bindingChildResponse, nil
				}
				// BatchResolve 的 list 字段未命中当前父 key，表示该父元素没有子结果。
				if fieldPlan.GetArrayResolverFunc() != nil && fieldPlan.GetFieldValueMetaInfo().IsList {
					return []any{}, nil
				}
				return nil, nil
			}
			if fieldPlan.GetFieldValueMetaInfo().IsList {
				// 没有父级绑定时，直接返回整个字段结果。
				return fieldResult.responses, nil
			}
			if len(fieldResult.responses) > 0 {
				return fieldResult.responses[0], nil
			}
		}
		return nil, nil
	}
	// 当前字段没有 resolver 时复用 graphql-go 默认 resolver，兼容 map、struct、tag 和 FieldResolver。
	if !isNilInterfaceValue(parentResponse) {
		if parentResponseMap, ok := parentResponse.(map[string]any); ok {
			// 普通业务对象的 map key 是 schema fieldName；GraphSoul 内省生成的 map 已经是 responseName 形状。
			propertyKey := fieldPlan.GetFieldName()
			if fieldPlan.GetResponseName() != "" && fieldPlan.GetResponseName() != propertyKey {
				switch fieldPlan.GetCompiledTypeName() {
				case "__Schema", "__Type", "__Field", "__InputValue", "__EnumValue", "__Directive":
					propertyKey = fieldPlan.GetResponseName()
				}
			}
			// graphsoul 的默认组装热点主要是 map[string]any；这里直接取值，避免进入 DefaultResolveFn 的反射路径。
			property := parentResponseMap[propertyKey]
			if propertyFn, ok := property.(func() interface{}); ok {
				property = propertyFn()
			}
			// 没有 extension 时，map 默认解析不需要 ResolveInfo；直接返回可避开 path/context 的构造。
			if len(rundata.extensions) == 0 {
				return property, nil
			}
			// 有 extension 时仍完整触发字段 hook，保持 extension 的可观测行为不变。
			_, finishHook := startSGraphResolveFieldHook(rundata, fieldPlan, ctx, -1)
			finishSGraphResolveFieldHook(rundata, finishHook, property, nil)
			return property, nil
		}
		// struct/tag/FieldResolver 仍通过原有默认 resolver，并保留 ResolveInfo。
		fieldCtx, finishHook := startSGraphResolveFieldHook(rundata, fieldPlan, ctx, -1)
		info := ResolveInfo{}
		if currentInfo := resolveInfoFromContext(fieldCtx); currentInfo != nil {
			info = *currentInfo
		}
		result, err := DefaultResolveFn(ResolveParams{
			Source:  parentResponse,
			Info:    info,
			Context: fieldCtx,
		})
		finishSGraphResolveFieldHook(rundata, finishHook, result, err)
		return result, err
	}
	return nil, nil
}

func (e *SGraphEngine) validateAbstractFieldValue(field *FieldPlan, value any, rundata *Rundata, ctx context.Context) (string, bool) {
	if field == nil || isNilInterfaceValue(value) {
		return "", true
	}

	fieldValueMetaInfo := field.GetFieldValueMetaInfo()
	var abs Abstract
	switch t := fieldValueMetaInfo.GetBaseElementOriginalType().(type) {
	case *Interface:
		abs = t
	case *Union:
		abs = t
	default:
		return "", true
	}

	possibleTypes := e.schema.PossibleTypes(abs)
	typeName := ""
	switch t := abs.(type) {
	case *Interface:
		if t.ResolveType != nil {
			if obj := t.ResolveType(ResolveTypeParams{Value: value, Context: ctx}); obj != nil {
				typeName = obj.Name()
			}
		}
	case *Union:
		if t.ResolveType != nil {
			if obj := t.ResolveType(ResolveTypeParams{Value: value, Context: ctx}); obj != nil {
				typeName = obj.Name()
			}
		}
	}

	if typeName == "" {
		for _, possible := range possibleTypes {
			if possible.IsTypeOf == nil {
				continue
			}
			if possible.IsTypeOf(IsTypeOfParams{Value: value, Context: ctx}) {
				typeName = possible.Name()
				break
			}
		}
	}

	if typeName == "" {
		// 抽象类型字段必须能解析成某个运行时 Object；解析失败时当前字段按规范置空并记录错误。
		rundata.AddFieldError(field.GetFieldId(), FieldErrorTypeField, fmt.Errorf("abstract type %s must resolve to an Object type at runtime", abs.Name()), field.GetPaths())
		return "", false
	}

	for _, possible := range possibleTypes {
		if possible.Name() == typeName {
			return typeName, true
		}
	}

	// ResolveType 返回的类型还必须属于当前 interface/union 的 possible types。
	rundata.AddFieldError(field.GetFieldId(), FieldErrorTypeField, fmt.Errorf("runtime object type %q is not a possible type for %q", typeName, abs.Name()), field.GetPaths())
	return "", false
}

func serializeLeafValue(field *FieldPlan, value any) any {
	if value == nil {
		return nil
	}
	info := field.GetFieldValueMetaInfo()
	switch t := info.GetBaseElementOriginalType().(type) {
	case *Scalar:
		return t.Serialize(value)
	case *Enum:
		return t.Serialize(value)
	default:
		return value
	}
}
