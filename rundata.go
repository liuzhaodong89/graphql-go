package graphql

import (
	"sync"
	"sync/atomic"

	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
)

type Rundata struct {
	originalParams   map[string]any
	schema           *Schema                   //请求级 ResolveInfo.Schema，来自当前 engine 绑定的 schema
	rootValue        any                       //请求级 ResolveInfo.RootValue，只用于默认 resolver / extension 信息透传
	operation        ast.Definition            //当前请求选择的 operation，供 ResolveInfo.Operation 使用
	fragments        map[string]ast.Definition //当前 document 的 fragment 定义，供 ResolveInfo.Fragments 使用
	extensions       []Extension               //public Execute 透传进来的 extensions，底层 engine 直接调用时保持为空
	fieldResultSlice []atomic.Pointer[FieldResponse]
	fieldErrorSlice  []atomic.Pointer[FieldError]
	fieldErrorCount  atomic.Int32
	extensionErrMu   sync.Mutex
	extensionErrors  []gqlerrors.FormattedError //field hook 可并发执行，extension 错误单独收集后合并进 Result.Errors
}

const sGraphRundataPoolMaxFieldSlots = 8192

var rundataPool = sync.Pool{
	New: func() any {
		return &Rundata{}
	},
}

func NewRundata(originalParams map[string]any, maxFieldId uint32) *Rundata {
	size := int(maxFieldId) + 1
	rundata := rundataPool.Get().(*Rundata)
	rundata.originalParams = originalParams
	rundata.fieldErrorCount.Store(0)
	rundata.extensionErrors = rundata.extensionErrors[:0]
	if cap(rundata.fieldResultSlice) < size {
		rundata.fieldResultSlice = make([]atomic.Pointer[FieldResponse], size)
	} else {
		rundata.fieldResultSlice = rundata.fieldResultSlice[:size]
	}
	if cap(rundata.fieldErrorSlice) < size {
		rundata.fieldErrorSlice = make([]atomic.Pointer[FieldError], size)
	} else {
		rundata.fieldErrorSlice = rundata.fieldErrorSlice[:size]
	}
	return rundata
}

func ReleaseRundata(rundata *Rundata) {
	if rundata == nil {
		return
	}
	for i := range rundata.fieldResultSlice {
		if fieldResponse := rundata.fieldResultSlice[i].Load(); fieldResponse != nil {
			ReleaseFieldResponse(fieldResponse)
			rundata.fieldResultSlice[i].Store(nil)
		}
	}
	for i := range rundata.fieldErrorSlice {
		rundata.fieldErrorSlice[i].Store(nil)
	}
	rundata.originalParams = nil
	// Rundata 会复用到后续请求，所有请求级引用必须清空，避免 schema/root/fragments/extensions 泄漏。
	rundata.schema = nil
	rundata.rootValue = nil
	rundata.operation = nil
	rundata.fragments = nil
	rundata.extensions = nil
	rundata.extensionErrMu.Lock()
	rundata.extensionErrors = rundata.extensionErrors[:0]
	rundata.extensionErrMu.Unlock()
	rundata.fieldErrorCount.Store(0)
	if cap(rundata.fieldResultSlice) > sGraphRundataPoolMaxFieldSlots {
		rundata.fieldResultSlice = nil
	} else {
		rundata.fieldResultSlice = rundata.fieldResultSlice[:0]
	}
	if cap(rundata.fieldErrorSlice) > sGraphRundataPoolMaxFieldSlots {
		rundata.fieldErrorSlice = nil
	} else {
		rundata.fieldErrorSlice = rundata.fieldErrorSlice[:0]
	}
	rundataPool.Put(rundata)
}

func (r *Rundata) AddExtensionErrors(errs []gqlerrors.FormattedError) {
	if r == nil || len(errs) == 0 {
		return
	}
	// field resolver 可能在同一个 batch 内并发执行，extension 错误追加必须串行化。
	r.extensionErrMu.Lock()
	r.extensionErrors = append(r.extensionErrors, errs...)
	r.extensionErrMu.Unlock()
}

func (r *Rundata) GetExtensionErrors() []gqlerrors.FormattedError {
	if r == nil {
		return nil
	}
	r.extensionErrMu.Lock()
	defer r.extensionErrMu.Unlock()
	if len(r.extensionErrors) == 0 {
		return nil
	}
	// 返回副本，防止 Result 持有 Rundata 池化对象内部 slice。
	result := make([]gqlerrors.FormattedError, len(r.extensionErrors))
	copy(result, r.extensionErrors)
	return result
}

func (r *Rundata) SetFieldResult(fieldId uint32, fieldResponse *FieldResponse) {
	r.fieldResultSlice[fieldId].Store(fieldResponse)
}

func (r *Rundata) GetFieldResultByFieldId(fieldId uint32) *FieldResponse {
	val := r.fieldResultSlice[fieldId].Load()
	return val
}

func (r *Rundata) AddFieldError(fieldId uint32, errorType FieldErrorType, err error, path []string) *FieldError {
	fieldError := &FieldError{
		fieldPath: path,
		err:       err,
		errorType: errorType,
		message:   err.Error(),
	}
	r.fieldErrorSlice[fieldId].Store(fieldError)
	r.fieldErrorCount.Add(1)
	return fieldError
}

func (r *Rundata) GetFieldErrorByFieldId(fieldId uint32) *FieldError {
	val := r.fieldErrorSlice[fieldId].Load()
	return val
}

func (r *Rundata) GetOriginalParamByKey(inputKey string) any {
	return r.originalParams[inputKey]
}

func (r *Rundata) GetAllFieldErrors() []*FieldError {
	if r.GetFieldErrorCount() == 0 {
		return nil
	}
	result := make([]*FieldError, 0, r.GetFieldErrorCount())
	for i := range r.fieldErrorSlice {
		if fieldError := r.fieldErrorSlice[i].Load(); fieldError != nil {
			result = append(result, fieldError)
		}
	}
	return result
}

func (r *Rundata) GetFieldErrorCount() int32 {
	return r.fieldErrorCount.Load()
}

type FieldErrorType uint8

const FieldErrorTypeField FieldErrorType = 0
const FieldErrorTypeTree FieldErrorType = 1

type FieldError struct {
	err       error
	message   string
	errorType FieldErrorType
	fieldPath []string
}

type FieldResponseType uint8

const FieldResponseTypeNormal FieldResponseType = 0
const FieldResponseTypeArray FieldResponseType = 1

type FieldResponse struct {
	responseType       FieldResponseType
	responses          []any
	arrayParentKeyMap  map[any]any
	indexOfParentArray uint32
}

// BindParentResult stores the child result by parent correlation key.
func (fr *FieldResponse) BindParentResult(parentKey any, childResult any) {
	if fr.arrayParentKeyMap == nil {
		fr.arrayParentKeyMap = make(map[any]any)
	}
	fr.arrayParentKeyMap[parentKey] = childResult
}

// LookupParentResult fetches child result by parent correlation key.
func (fr *FieldResponse) LookupParentResult(parentKey any) (any, bool) {
	if fr.arrayParentKeyMap == nil {
		return nil, false
	}
	val, ok := fr.arrayParentKeyMap[parentKey]
	return val, ok
}

// HasParentResultBinding indicates whether parent-key bindings exist.
func (fr *FieldResponse) HasParentResultBinding() bool {
	return fr != nil && len(fr.arrayParentKeyMap) > 0
}

// GetFirstResponse returns the first raw response value, or nil if the
// response is nil or empty. External packages (e.g. prepare1) use this to
// extract the actual parent object when wrapping standard GraphQL resolvers.
func (fr *FieldResponse) GetFirstResponse() any {
	if fr == nil || len(fr.responses) == 0 {
		return nil
	}
	return fr.responses[0]
}

// 单次超大 batch 不应通过 sync.Pool 长期保留其父结果映射；常规 list 查询复用映射容量。
const fieldResponsePoolMaxParentBindings = 1024

var fieldResponsePool = sync.Pool{
	New: func() any {
		return &FieldResponse{}
	},
}

func AcquireFieldResponse(reponseType FieldResponseType) *FieldResponse {
	frVal := fieldResponsePool.Get().(*FieldResponse)
	frVal.responseType = reponseType
	frVal.responses = frVal.responses[:0]
	frVal.indexOfParentArray = 0
	return frVal
}

func ReleaseFieldResponse(fr *FieldResponse) {
	if fr == nil {
		return
	}
	clear(fr.responses)
	fr.responses = fr.responses[:0]
	fr.indexOfParentArray = 0
	if fr.arrayParentKeyMap != nil {
		if len(fr.arrayParentKeyMap) > fieldResponsePoolMaxParentBindings {
			fr.arrayParentKeyMap = nil
		} else {
			// 清除 key/value 对，避免 sync.Pool 持有上一个请求的对象，同时保留常规 batch 的 map 容量。
			clear(fr.arrayParentKeyMap)
		}
	}
	fieldResponsePool.Put(fr)
}
