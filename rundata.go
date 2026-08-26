package graphql

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
)

type Rundata struct {
	//请求输入和ResolveInfo基础数据，运行时只读
	operation            ast.Definition            //当前请求选择的operation，供ResolveInfo.Operation使用
	schema               *Schema                   //请求级ResolveInfo.Schema，来自当前engine绑定的schema
	executionPlan        *SGraphExecutionPlan      //当前请求的只读执行计划，供错误链路按fieldId取字段AST
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
	// Bulk绑定错误只有在组装到具体父occurrence时才能补齐动态路径。
	hasPendingBulkBindingErrors atomic.Bool

	// 结果组装由单goroutine执行，所有List分支复用同一个下标栈。
	assemblyListIndexes []int

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
	rundata.hasPendingBulkBindingErrors.Store(false)
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
	clear(rundata.assemblyListIndexes)
	if cap(rundata.assemblyListIndexes) > sGraphRundataPoolMaxFieldSlots {
		rundata.assemblyListIndexes = nil
	} else {
		rundata.assemblyListIndexes = rundata.assemblyListIndexes[:0]
	}
	rundata.originalParams = nil
	// Rundata 会复用到后续请求，所有请求级引用必须清空，避免 schema/plan/root/fragments/extensions 泄漏。
	rundata.schema = nil
	rundata.executionPlan = nil
	rundata.resolveInfoRootValue = nil
	rundata.operation = nil
	rundata.fragments = nil
	rundata.extensions = nil
	rundata.extensionErrMu.Lock()
	rundata.extensionErrors = rundata.extensionErrors[:0]
	rundata.extensionErrMu.Unlock()
	rundata.fieldErrorCount.Store(0)
	rundata.hasPendingBulkBindingErrors.Store(false)
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
	responsePath := make([]any, len(path))
	for index, key := range path {
		responsePath[index] = key
	}
	return rundata.storeFieldError(fieldId, errorType, err, responsePath)
}

func (rundata *Rundata) addFieldErrorAtResponsePath(fieldId uint32, errorType FieldErrorTypeEnum, err error, path *ResponsePath) *FieldError {
	var responsePath []any
	if path != nil {
		responsePath = path.AsArray()
	}
	return rundata.storeFieldError(fieldId, errorType, err, responsePath)
}

func (rundata *Rundata) addFieldErrorAtPath(fieldId uint32, errorType FieldErrorTypeEnum, err error, path []any) *FieldError {
	return rundata.storeFieldError(fieldId, errorType, err, append([]any(nil), path...))
}

func (rundata *Rundata) addFieldErrorAtPlanPath(fieldPlan *FieldPlan, errorType FieldErrorTypeEnum, err error, listIndexes []int) *FieldError {
	if fieldPlan == nil {
		return rundata.storeFieldError(0, FieldErrorTypeTree, errors.New("field plan is nil while recording field error"), nil)
	}
	return rundata.storeFieldError(fieldPlan.fieldId, errorType, err, materializeFieldPlanPath(fieldPlan, listIndexes))
}

func (rundata *Rundata) storeFieldError(fieldId uint32, errorType FieldErrorTypeEnum, err error, responsePath []any) *FieldError {
	if rundata == nil {
		return nil
	}
	if len(rundata.fieldErrors) == 0 {
		return nil
	}
	if uint64(fieldId) >= uint64(len(rundata.fieldErrors)) {
		err = fmt.Errorf("field id %d is out of range while recording error: %w", fieldId, err)
		fieldId = 0
		errorType = FieldErrorTypeTree
		responsePath = nil
	}
	if fieldId != 0 && rundata.executionPlan != nil {
		fieldPlan := rundata.executionPlan.fieldPlansById[fieldId]
		if fieldPlan != nil && len(fieldPlan.fieldASTs) != 0 {
			// 在错误发布到并发链表前补齐AST位置，并通过OriginalError保留resolver extensions。
			err = NewLocatedErrorWithPath(err, FieldASTsToNodeASTs(fieldPlan.fieldASTs), responsePath)
		}
	}
	fieldError := &FieldError{
		responsePath: responsePath,
		errorType:    errorType,
		err:          err,
	}
	// 每个fieldId使用不可变单向链表保存全部错误，CAS插入避免并发Step覆盖错误。
	fieldErrorSlot := &rundata.fieldErrors[fieldId]
	for {
		currentHead := fieldErrorSlot.Load()
		fieldError.next = currentHead
		if fieldErrorSlot.CompareAndSwap(currentHead, fieldError) {
			break
		}
	}
	rundata.fieldErrorCount.Add(1)
	return fieldError
}

func (rundata *Rundata) getFieldErrorByFieldId(fieldId uint32) *FieldError {
	if rundata == nil || uint64(fieldId) >= uint64(len(rundata.fieldErrors)) {
		return nil
	}
	val := rundata.fieldErrors[fieldId].Load()
	return val
}

func (rundata *Rundata) hasFieldErrorAtPath(fieldId uint32, path []any) bool {
	if rundata == nil || uint64(fieldId) >= uint64(len(rundata.fieldErrors)) {
		return false
	}
	for fieldError := rundata.fieldErrors[fieldId].Load(); fieldError != nil; fieldError = fieldError.next {
		if equalResponsePath(fieldError.responsePath, path) {
			return true
		}
	}
	return false
}

func (rundata *Rundata) hasFieldErrorAtPlanPath(fieldPlan *FieldPlan, listIndexes []int) bool {
	if fieldPlan == nil {
		return false
	}
	return rundata.hasFieldErrorAtPath(fieldPlan.fieldId, materializeFieldPlanPath(fieldPlan, listIndexes))
}

func equalResponsePath(left, right []any) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		switch leftKey := left[index].(type) {
		case string:
			rightKey, ok := right[index].(string)
			if !ok || leftKey != rightKey {
				return false
			}
		case int:
			rightKey, ok := right[index].(int)
			if !ok || leftKey != rightKey {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func materializeFieldPlanPath(fieldPlan *FieldPlan, listIndexes []int) []any {
	if fieldPlan == nil {
		return nil
	}
	result := make([]any, 0, len(fieldPlan.paths)+len(listIndexes))
	listIndexOffset := 0
	for pathIndex, responseName := range fieldPlan.paths {
		result = append(result, responseName)
		if pathIndex >= len(fieldPlan.pathListDepths) {
			continue
		}
		for depth := 0; depth < fieldPlan.pathListDepths[pathIndex] && listIndexOffset < len(listIndexes); depth++ {
			result = append(result, listIndexes[listIndexOffset])
			listIndexOffset++
		}
	}
	// 对旧Plan或结构错误保留未消费的下标，避免错误路径静默丢失信息。
	for listIndexOffset < len(listIndexes) {
		result = append(result, listIndexes[listIndexOffset])
		listIndexOffset++
	}
	return result
}

func (rundata *Rundata) getAllFieldErrors() []*FieldError {
	if rundata == nil || len(rundata.fieldErrors) == 0 {
		return nil
	}
	result := make([]*FieldError, 0, int(rundata.fieldErrorCount.Load()))
	for index := range rundata.fieldErrors {
		start := len(result)
		for fieldError := rundata.fieldErrors[index].Load(); fieldError != nil; fieldError = fieldError.next {
			result = append(result, fieldError)
		}
		// CAS按头插法写入；反转当前fieldId区间以恢复该字段的发生顺序。
		for left, right := start, len(result)-1; left < right; left, right = left+1, right-1 {
			result[left], result[right] = result[right], result[left]
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

type bulkIterationState uint8

const (
	bulkIterationUnused bulkIterationState = iota
	bulkIterationPending
	bulkIterationReady
)

type fieldResponseOccurrence struct {
	responseRaw  any
	responsePath *ResponsePath
}

type bulkBindingError struct {
	err          error
	listIndex    int
	hasListIndex bool
}

type bulkFieldResponseState struct {
	mu sync.Mutex

	iterationState     bulkIterationState
	iterationResponses []fieldResponseOccurrence
	fieldPlan          *FieldPlan
	bindingErrors      map[any][]bulkBindingError
}

type fieldResponseBindingMode uint8

const (
	//当前字段结果不需要按父occurrence区分。
	fieldResponseBindingNone fieldResponseBindingMode = iota
	//通过父对象中的业务字段生成composite key。
	fieldResponseBindingCompositeKey
	//通过包含List下标的父occurrence ResponsePath生成key。
	fieldResponseBindingResponsePath
)

type FieldResponse struct {
	responseRaws  []any
	responsePaths []*ResponsePath // 与responseRaws平行，仅在下游逐元素Step需要时保存。

	parentBindingMap  map[string]any           // 父occurrence绑定key到当前字段结果的映射。key可能是composite key也可能是response path
	parentBindingMode fieldResponseBindingMode // Bulk返回空集合时，仍需保留Composite Key模式。
	bulkState         *bulkFieldResponseState
}

func (fieldResponse *FieldResponse) ensureBulkState(fieldPlan *FieldPlan) *bulkFieldResponseState {
	if fieldResponse.bulkState == nil {
		fieldResponse.bulkState = acquireBulkFieldResponseState()
	}
	if fieldPlan != nil && fieldResponse.bulkState.fieldPlan == nil {
		fieldResponse.bulkState.fieldPlan = fieldPlan
	}
	return fieldResponse.bulkState
}

// iterationResponseData返回下游Iterator所需的逐元素数据。普通List直接复用平行切片，
// 多层List和Bulk结果使用occurrences，避免为快路径额外复制。
func (fieldResponse *FieldResponse) iterationResponseData(rundata *Rundata) ([]any, []*ResponsePath, []fieldResponseOccurrence, error) {
	if fieldResponse == nil {
		return nil, nil, nil, nil
	}
	state := fieldResponse.bulkState
	if state == nil || state.iterationState == bulkIterationUnused {
		return fieldResponse.responseRaws, fieldResponse.responsePaths, nil, nil
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.iterationState == bulkIterationReady {
		return nil, nil, state.iterationResponses, nil
	}

	fieldPlan := state.fieldPlan
	if fieldPlan == nil || rundata == nil {
		return nil, nil, nil, errors.New("bulk iteration response path context is invalid")
	}
	if fieldResponse.parentBindingMode != fieldResponseBindingCompositeKey {
		return nil, nil, nil, fmt.Errorf("bulk field %s has invalid parent binding mode", fieldPlan.fieldName)
	}
	parentResponse := rundata.getFieldResponseByFieldId(fieldPlan.parentFieldId)
	if parentResponse == nil {
		return nil, nil, nil, fmt.Errorf("parent field %d has no response", fieldPlan.parentFieldId)
	}
	parentRaws, parentPaths, parentOccurrences, err := parentResponse.iterationResponseData(rundata)
	if err != nil {
		return nil, nil, nil, err
	}
	parentCount := len(parentOccurrences)
	if parentOccurrences == nil {
		if len(parentRaws) != len(parentPaths) {
			return nil, nil, nil, fmt.Errorf("parent field %d response paths are not aligned with response values", fieldPlan.parentFieldId)
		}
		parentCount = len(parentRaws)
	}

	keyFieldName := fieldPlan.parentKeyFieldName
	if keyFieldName == "" {
		return nil, nil, nil, fmt.Errorf("parent key field name is empty for field %s", fieldPlan.fieldName)
	}
	for index := 0; index < parentCount; index++ {
		var parentRaw any
		var parentPath *ResponsePath
		if parentOccurrences != nil {
			parentRaw = parentOccurrences[index].responseRaw
			parentPath = parentOccurrences[index].responsePath
		} else {
			parentRaw = parentRaws[index]
			parentPath = parentPaths[index]
		}
		if isNilInterfaceValue(parentRaw) {
			continue
		}
		parentMap, ok := parentRaw.(map[string]any)
		if !ok {
			return nil, nil, nil, fmt.Errorf("parent response for field %s does not support composite-key mapping", fieldPlan.fieldName)
		}
		if _, exists := parentMap[keyFieldName]; !exists {
			return nil, nil, nil, fmt.Errorf("parent key field %q is missing for field %s", keyFieldName, fieldPlan.fieldName)
		}
		if parentPath == nil {
			return nil, nil, nil, fmt.Errorf("parent field %d response path %d is nil", fieldPlan.parentFieldId, index)
		}
		compositeKey := generateCompositeKey([]string{keyFieldName}, parentMap)
		bound, exists := fieldResponse.lookParentResponse(compositeKey)
		if !exists || isNilInterfaceValue(bound) {
			continue
		}
		responsePath := parentPath.WithKey(fieldPlan.responseName)
		appendFieldResponseOccurrences(&state.iterationResponses, &fieldPlan.fieldWrapperTypeInfo, bound, responsePath)
	}
	state.iterationState = bulkIterationReady
	return nil, nil, state.iterationResponses, nil
}

func appendFieldResponseOccurrences(result *[]fieldResponseOccurrence, wrapperTypeInfo *FieldWrapperTypeInfo, value any, responsePath *ResponsePath) {
	if result == nil || wrapperTypeInfo == nil || isNilInterfaceValue(value) {
		return
	}
	if !wrapperTypeInfo.isList {
		*result = append(*result, fieldResponseOccurrence{responseRaw: value, responsePath: responsePath})
		return
	}
	items, ok := asListValue(value)
	if !ok {
		return
	}
	for index, item := range items {
		appendFieldResponseOccurrences(result, wrapperTypeInfo.elementWrapperTypeInfo, item, responsePath.WithKey(index))
	}
}

func (fieldResponse *FieldResponse) reportBulkBindingErrors(compositeKey any, fieldPlan *FieldPlan, rundata *Rundata) {
	if fieldResponse == nil || fieldPlan == nil || rundata == nil || fieldResponse.bulkState == nil {
		return
	}
	state := fieldResponse.bulkState
	state.mu.Lock()
	bindingErrors := state.bindingErrors[compositeKey]
	delete(state.bindingErrors, compositeKey)
	state.mu.Unlock()

	for _, bindingError := range bindingErrors {
		listIndexes := rundata.assemblyListIndexes
		if bindingError.hasListIndex {
			listIndexes = append(listIndexes, bindingError.listIndex)
		}
		rundata.addFieldErrorAtPlanPath(fieldPlan, FieldErrorTypeField, bindingError.err, listIndexes)
	}
}

func (fieldResponse *FieldResponse) flushBulkBindingErrors(rundata *Rundata) {
	if fieldResponse == nil || rundata == nil || fieldResponse.bulkState == nil {
		return
	}
	state := fieldResponse.bulkState
	state.mu.Lock()
	fieldPlan := state.fieldPlan
	var bindingErrors []bulkBindingError
	for compositeKey, errorsForKey := range state.bindingErrors {
		bindingErrors = append(bindingErrors, errorsForKey...)
		delete(state.bindingErrors, compositeKey)
	}
	state.mu.Unlock()
	if fieldPlan == nil {
		return
	}
	for _, bindingError := range bindingErrors {
		// 找不到父occurrence时退回静态字段路径，保证错误不会丢失。
		rundata.addFieldError(fieldPlan.fieldId, FieldErrorTypeField, bindingError.err, fieldPlan.paths)
	}
}

func (rundata *Rundata) flushPendingBulkBindingErrors() {
	if rundata == nil || !rundata.hasPendingBulkBindingErrors.Swap(false) {
		return
	}
	for index := range rundata.fieldResponses {
		if fieldResponse := rundata.fieldResponses[index].Load(); fieldResponse != nil {
			fieldResponse.flushBulkBindingErrors(rundata)
		}
	}
}

func (fieldResponse *FieldResponse) bindParentResponse(bindingMode fieldResponseBindingMode, bindingKey string, currentResponse any) {
	fieldResponse.parentBindingMode = bindingMode
	if fieldResponse.parentBindingMap == nil {
		fieldResponse.parentBindingMap = make(map[string]any)
	}
	fieldResponse.parentBindingMap[bindingKey] = currentResponse
}

func (fieldResponse *FieldResponse) lookParentResponse(bindingKey string) (any, bool) {
	if fieldResponse == nil || fieldResponse.parentBindingMap == nil {
		return nil, false
	}
	val, ok := fieldResponse.parentBindingMap[bindingKey]
	return val, ok
}

func (fieldResponse *FieldResponse) hasParentBinding() bool {
	return fieldResponse != nil && fieldResponse.parentBindingMode != fieldResponseBindingNone
}

type FieldErrorTypeEnum uint8

const FieldErrorTypeField FieldErrorTypeEnum = 0
const FieldErrorTypeTree FieldErrorTypeEnum = 1

type FieldError struct {
	err          error              //原始报错
	errorType    FieldErrorTypeEnum //字段错误继续执行，Tree错误中断整个执行计划
	responsePath []any              //包含responseName和List整数下标的请求级动态响应路径
	next         *FieldError        //同一fieldId的前一个错误；发布后保持不可变
}

// 单次超大 batch 不应通过 sync.Pool 长期保留其父结果映射；常规 list 查询复用映射容量。
const fieldResponsePoolMaxParentBindings = 1024
const fieldResponsePoolMaxResponseCapacity = 8192

var fieldResponsePool = sync.Pool{
	New: func() any {
		return &FieldResponse{}
	},
}

var bulkFieldResponseStatePool = sync.Pool{
	New: func() any {
		return &bulkFieldResponseState{}
	},
}

func acquireBulkFieldResponseState() *bulkFieldResponseState {
	state := bulkFieldResponseStatePool.Get().(*bulkFieldResponseState)
	state.iterationState = bulkIterationUnused
	if state.iterationResponses == nil {
		state.iterationResponses = make([]fieldResponseOccurrence, 0)
	} else {
		state.iterationResponses = state.iterationResponses[:0]
	}
	state.fieldPlan = nil
	return state
}

func releaseBulkFieldResponseState(state *bulkFieldResponseState) {
	if state == nil {
		return
	}
	if cap(state.iterationResponses) > fieldResponsePoolMaxResponseCapacity {
		state.iterationResponses = nil
	} else {
		clear(state.iterationResponses)
		state.iterationResponses = state.iterationResponses[:0]
	}
	if state.bindingErrors != nil {
		clear(state.bindingErrors)
		state.bindingErrors = nil
	}
	state.iterationState = bulkIterationUnused
	state.fieldPlan = nil
	bulkFieldResponseStatePool.Put(state)
}

func acquireFieldResponse() *FieldResponse {
	frVal := fieldResponsePool.Get().(*FieldResponse)
	if frVal.responseRaws == nil {
		frVal.responseRaws = make([]any, 0)
	} else {
		frVal.responseRaws = frVal.responseRaws[:0]
	}
	frVal.responsePaths = frVal.responsePaths[:0]
	frVal.parentBindingMode = fieldResponseBindingNone
	frVal.bulkState = nil
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
	if cap(fr.responsePaths) > fieldResponsePoolMaxResponseCapacity {
		fr.responsePaths = nil
	} else {
		clear(fr.responsePaths)
		fr.responsePaths = fr.responsePaths[:0]
	}

	if fr.parentBindingMap != nil {
		if len(fr.parentBindingMap) > fieldResponsePoolMaxParentBindings {
			fr.parentBindingMap = nil
		} else {
			clear(fr.parentBindingMap)
		}
	}
	fr.parentBindingMode = fieldResponseBindingNone
	if fr.bulkState != nil {
		releaseBulkFieldResponseState(fr.bulkState)
		fr.bulkState = nil
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

	// 按常见短标量响应预留容量；容量不足时由 append 正常扩容，不影响序列化结果。
	result := make([]byte, 0, 2+len(m.fieldResponses)*16)
	result = append(result, '{')
	for i := range m.fieldResponses {
		if i > 0 {
			result = append(result, ',')
		}

		fieldResponse := &m.fieldResponses[i]
		// key 来自已校验 AST 的 responseName，只包含 GraphQL Name 允许的字符，可直接写入 JSON 字符串。
		result = append(result, '"')
		result = append(result, fieldResponse.key...)
		result = append(result, '"', ':')

		// value 继续使用标准库编码，保留自定义 Scalar 和 json.Marshaler 的既有行为。
		value, err := json.Marshal(fieldResponse.value)
		if err != nil {
			return nil, err
		}
		result = append(result, value...)
	}
	result = append(result, '}')
	return result, nil
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
func (r *SGraphResult) toGraphQLResult() *Result {
	result := &Result{}
	if r == nil {
		return result
	}
	// 没有执行数据时保持 interface 本身为 nil，避免 typed nil 被调用方误判为已产生响应数据。
	if orderedResponses := r.getOrderedResponses(); orderedResponses != nil {
		result.Data = orderedResponses
	}
	for _, fieldErr := range r.getErrors() {
		if fieldErr == nil || fieldErr.err == nil {
			continue
		}
		formatted := gqlerrors.FormatError(fieldErr.err)
		formatted.Message = fieldErr.err.Error()
		if len(fieldErr.responsePath) > 0 {
			formatted.Path = append([]any(nil), fieldErr.responsePath...)
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
