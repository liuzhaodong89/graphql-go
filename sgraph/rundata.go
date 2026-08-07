package sgraph

import (
	"bytes"
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
)

type Rundata struct {
	//请求输入和ResolveInfo基础数据，运行时只读
	operation            ast.Definition            //当前请求选择的operation，供ResolveInfo.Operation使用
	schema               *graphql.Schema           //请求级ResolveInfo.Schema，来自当前engine绑定的schema
	resolveInfoRootValue any                       //请求级ResolveInfo.RootValue，只用于默认resolver/extension信息透传
	originalParams       map[string]any            //请求原始参数
	fragments            map[string]ast.Definition //当前document的fragment定义，供ResolveInfo.Fragments使用

	//本期请求的extensions，运行时只读
	extensions []Extension

	//按FieldId保存Step执行结果，每个FieldId对应一个Step
	fieldResponses []atomic.Pointer[FieldResponse]

	//按FieldId保存Step执行错误
	fieldErrors     []atomic.Pointer[FieldError]
	fieldErrorCount atomic.Int32

	//不参与null值冒泡的extensions执行错误
	extensionErrMu  sync.Mutex
	extensionErrors []gqlerrors.FormattedError
}

var rundataPool = sync.Pool{
	New: func() any {
		return &Rundata{}
	},
}

func newRundata(originalParams map[string]any, maxFieldId uint32) *Rundata {
	size := int(maxFieldId) + 1
	rundata := rundataPool.Get().(*Rundata)
	rundata.originalParams = originalParams
	rundata.fieldErrorCount.Store(0)
	rundata.extensionErrors = rundata.extensionErrors[:0]
	if cap(rundata.fieldResponses) < size {
		rundata.fieldResponses = make([]atomic.Pointer[FieldResponse], size)
	} else {
		rundata.fieldResponses = rundata.fieldResponses[:size]
	}
	if cap(rundata.fieldErrors) < size {
		rundata.fieldErrors = make([]atomic.Pointer[FieldError], size)
	} else {
		rundata.fieldErrors = rundata.fieldErrors[:size]
	}
	return rundata
}

func releaseRundata(rundata *Rundata) {
	if rundata == nil {
		return
	}
	for i := range rundata.fieldResponses {
		if fieldResponse := rundata.fieldResponses[i].Load(); fieldResponse != nil {
			releaseFieldResponse(fieldResponse)
			rundata.fieldResponses[i].Store(nil)
		}
	}
	for i := range rundata.fieldErrors {
		rundata.fieldErrors[i].Store(nil)
	}
	rundata.originalParams = nil
	// Rundata 会复用到后续请求，所有请求级引用必须清空，避免 schema/root/fragments/extensions 泄漏。
	rundata.schema = nil
	rundata.resolveInfoRootValue = nil
	rundata.operation = nil
	rundata.fragments = nil
	rundata.extensions = nil
	rundata.extensionErrMu.Lock()
	rundata.extensionErrors = rundata.extensionErrors[:0]
	rundata.extensionErrMu.Unlock()
	rundata.fieldErrorCount.Store(0)
	if cap(rundata.fieldResponses) > sGraphRundataPoolMaxFieldSlots {
		rundata.fieldResponses = nil
	} else {
		rundata.fieldResponses = rundata.fieldResponses[:0]
	}
	if cap(rundata.fieldErrors) > sGraphRundataPoolMaxFieldSlots {
		rundata.fieldErrors = nil
	} else {
		rundata.fieldErrors = rundata.fieldErrors[:0]
	}
	rundataPool.Put(rundata)
}

func (rundata *Rundata) setFieldResponse(fieldId uint32, fieldResponse *FieldResponse) {
	rundata.fieldResponses[fieldId].Store(fieldResponse)
}

func (rundata *Rundata) getFieldResponseByFieldId(fieldId uint32) *FieldResponse {
	val := rundata.fieldResponses[fieldId].Load()
	return val
}

func (rundata *Rundata) addFieldError(fieldId uint32, errorType FieldErrorTypeEnum, err error, path []string) *FieldError {
	fieldError := &FieldError{
		fieldPaths: path,
		errorType:  errorType,
		err:        err,
	}
	rundata.fieldErrors[fieldId].Store(fieldError)
	rundata.fieldErrorCount.Add(1)
	return fieldError
}

func (rundata *Rundata) getFieldErrorByFieldId(fieldId uint32) *FieldError {
	val := rundata.fieldErrors[fieldId].Load()
	return val
}

func (rundata *Rundata) getAllFieldErrors() []*FieldError {
	if rundata == nil || len(rundata.fieldErrors) == 0 {
		return nil
	}
	result := make([]*FieldError, len(rundata.fieldErrors))
	for _, f := range rundata.fieldErrors {
		if fieldError := f.Load(); fieldError != nil {
			result = append(result, fieldError)
		}
	}
	return result
}

func (rundata *Rundata) getAllExtensionErrors() []gqlerrors.FormattedError {
	if rundata == nil {
		return nil
	}
	rundata.extensionErrMu.Lock()
	defer rundata.extensionErrMu.Unlock()
	if len(rundata.extensionErrors) == 0 {
		return nil
	}
	// 返回副本，防止 Result 持有 Rundata 池化对象内部 slice。
	result := make([]gqlerrors.FormattedError, len(rundata.extensionErrors))
	copy(result, rundata.extensionErrors)
	return result
}

func (rundata *Rundata) addExtensionErrors(errs []gqlerrors.FormattedError) {
	if rundata == nil || len(errs) == 0 {
		return
	}
	// field resolver 可能在同一个 batch 内并发执行，extension 错误追加必须串行化。
	rundata.extensionErrMu.Lock()
	rundata.extensionErrors = append(rundata.extensionErrors, errs...)
	rundata.extensionErrMu.Unlock()
}

type FieldResponse struct {
	responseRaws            []any       //当前FieldPlan产生的有序完成结果，普通step通常只有一个元素，list step可能包含多个元素
	bulkResolveParentKeyMap map[any]any // 父元素composite key到当前字段结果的映射，当字段位于list节点下时，用于将结果关联到具体父元素，避免只依赖数组下标无法支持返回结果乱序或一对多的情况。
}

func (fieldResponse *FieldResponse) bindBulkResponsesWithCompositeKey(compositeKey any, currentResponse any) {
	if fieldResponse.bulkResolveParentKeyMap == nil {
		fieldResponse.bulkResolveParentKeyMap = make(map[any]any)
	}
	fieldResponse.bulkResolveParentKeyMap[compositeKey] = currentResponse
}

func (fieldResponse *FieldResponse) lookResponseByCompositeKey(compositeKey any) (any, bool) {
	if fieldResponse.bulkResolveParentKeyMap == nil {
		return nil, false
	}
	val, ok := fieldResponse.bulkResolveParentKeyMap[compositeKey]
	return val, ok
}

func (fieldResponse *FieldResponse) hasBulkResponseBinding() bool {
	return fieldResponse != nil && len(fieldResponse.bulkResolveParentKeyMap) > 0
}

type FieldErrorTypeEnum uint8

const FieldErrorTypeField FieldErrorTypeEnum = 0
const FieldErrorTypeTree FieldErrorTypeEnum = 1

type FieldError struct {
	err        error              //原始报错
	errorType  FieldErrorTypeEnum //错误类型，字段级还是AST tree级，如果是AST tree级整个计划会中断执行
	fieldPaths []string           //从根到错误字段的静态responseName路径，包含alias
}

// 单次超大 batch 不应通过 sync.Pool 长期保留其父结果映射；常规 list 查询复用映射容量。
const fieldResponsePoolMaxParentBindings = 1024
const fieldResponsePoolMaxResponseCapacity = 8192

var fieldResponsePool = sync.Pool{
	New: func() any {
		return &FieldResponse{}
	},
}

func acquireFieldResponse() *FieldResponse {
	frVal := fieldResponsePool.Get().(*FieldResponse)
	frVal.responseRaws = frVal.responseRaws[:0]
	return frVal
}

func releaseFieldResponse(fr *FieldResponse) {
	if fr == nil {
		return
	}

	if cap(fr.responseRaws) > fieldResponsePoolMaxResponseCapacity {
		//超大切片不保留到底层对象池，避免异常大查询持续抬高池内存。
		fr.responseRaws = nil
	} else {
		clear(fr.responseRaws)
		fr.responseRaws = fr.responseRaws[:0]
	}

	if fr.bulkResolveParentKeyMap != nil {
		if len(fr.bulkResolveParentKeyMap) > fieldResponsePoolMaxParentBindings {
			fr.bulkResolveParentKeyMap = nil
		} else {
			clear(fr.bulkResolveParentKeyMap)
		}
	}

	fieldResponsePool.Put(fr)
}

type OrderedFieldResponse struct {
	key   string
	value any
}

type SGraphResponseOrderedMap struct {
	fieldResponses []OrderedFieldResponse
	indexs         map[string]int
}

const sSGraphOrderedMapIndexMin = 8

func newSGraphResponseOrderedMap(capacity int) *SGraphResponseOrderedMap {
	result := &SGraphResponseOrderedMap{
		fieldResponses: make([]OrderedFieldResponse, 0, capacity),
	}
	if capacity > sSGraphOrderedMapIndexMin {
		result.indexs = make(map[string]int, capacity)
	}
	return result
}

func (m *SGraphResponseOrderedMap) set(key string, value any) {
	if m == nil {
		return
	}

	//index不为空的时候，走map查询，时间复杂度O(1)
	if m.indexs != nil {
		if index, ok := m.indexs[key]; ok {
			m.fieldResponses[index].value = value
			return
		}
		m.indexs[key] = len(m.fieldResponses)
		m.fieldResponses = append(m.fieldResponses, OrderedFieldResponse{key, value})
		return
	}

	//index为空的时候，线性扫描
	for i := range m.fieldResponses {
		if m.fieldResponses[i].key == key {
			m.fieldResponses[i].value = value
			return
		}
	}

	//超过阈值时，从线性扫描转换为index的map查询。暂不考虑退火算法。
	if len(m.fieldResponses)+1 >= sSGraphOrderedMapIndexMin {
		m.indexs = make(map[string]int, len(m.fieldResponses)+1)
		for i, fieldResponse := range m.fieldResponses {
			m.indexs[fieldResponse.key] = i
		}
		m.indexs[key] = len(m.fieldResponses)
	}
	m.fieldResponses = append(m.fieldResponses, OrderedFieldResponse{key, value})
}

func (m *SGraphResponseOrderedMap) get(key string) (any, bool) {
	if m == nil {
		return nil, false
	}

	//index的map查询
	if m.indexs != nil {
		if index, ok := m.indexs[key]; !ok {
			return nil, false
		} else {
			return m.fieldResponses[index].value, true
		}
	}

	//线性查询
	for i := range m.fieldResponses {
		if m.fieldResponses[i].key == key {
			return m.fieldResponses[i].value, true
		}
	}
	return nil, false
}

func (m *SGraphResponseOrderedMap) fields() []OrderedFieldResponse {
	if m == nil {
		return nil
	}
	return m.fieldResponses
}

// MarshalJSON 按查询字段顺序序列化（spec §3.3 响应保序）；嵌套有序 map 递归生效，AI生成的
// nil 指针输出 null。
func (m *SGraphResponseOrderedMap) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range m.fieldResponses {
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

type SGraphResult struct {
	orderedResponses *SGraphResponseOrderedMap
	errors           []*FieldError
	extensionErrors  []gqlerrors.FormattedError
}

func (r *SGraphResult) getOrderedResponses() *SGraphResponseOrderedMap {
	if r.orderedResponses == nil {
		return nil
	}
	return r.orderedResponses
}

func (r *SGraphResult) getErrors() []*FieldError {
	return r.errors
}

func (r *SGraphResult) getResponse() map[string]any {
	if r == nil || r.orderedResponses == nil {
		return nil
	}
	if plain, ok := toPlainValue(r.orderedResponses).(map[string]any); ok {
		return plain
	}
	return nil
}

// 保留SGraphResult的有序响应结构，避免转换成普通map后丢失query字段顺序。
func (r *SGraphResult) toGraphQLResult() *graphql.Result {
	result := &graphql.Result{}
	if r == nil {
		return result
	}
	result.Data = r.getOrderedResponses()
	for _, fieldErr := range r.getErrors() {
		if fieldErr == nil || fieldErr.err == nil {
			continue
		}
		formatted := gqlerrors.FormatError(fieldErr.err)
		formatted.Message = fieldErr.err.Error()
		if len(fieldErr.fieldPaths) > 0 {
			formatted.Path = make([]any, 0, len(fieldErr.fieldPaths))
			for _, path := range fieldErr.fieldPaths {
				formatted.Path = append(formatted.Path, path)
			}
		}
		result.Errors = append(result.Errors, formatted)
	}
	result.Errors = append(result.Errors, r.extensionErrors...)
	return result
}

// toPlainValue 把有序响应树（含嵌套 *SGraphResponseOrderedMap / []any）递归转成
// 纯 map[string]any / []any。map 无序，仅供需要普通 map 的调用方；
// 要按查询字段顺序输出 JSON 请用 SGraphResponseOrderedMap.MarshalJSON。AI生成的
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

func newSGraphErrorResult(err error) *SGraphResult {
	result := &SGraphResult{}
	if err != nil {
		result.errors = []*FieldError{{
			err:       err,
			errorType: FieldErrorTypeTree,
		}}
	}
	return result
}
