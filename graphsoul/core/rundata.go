package core

import (
	"sync"
	"sync/atomic"
)

type Rundata struct {
	originalParams   map[string]any
	fieldResultSlice []atomic.Pointer[FieldResponse]
	fieldErrorSlice  []atomic.Pointer[FieldError]
	fieldErrorCount  atomic.Int32
}

func NewRundata(originalParams map[string]any, maxFieldId uint32) *Rundata {
	size := int(maxFieldId) + 1
	rundata := &Rundata{
		originalParams:   originalParams,
		fieldResultSlice: make([]atomic.Pointer[FieldResponse], size),
		fieldErrorSlice:  make([]atomic.Pointer[FieldError], size),
	}
	return rundata
}

func (r *Rundata) SetFieldResult(fieldId uint32, fieldResponse *FieldResponse) {
	r.fieldResultSlice[fieldId].Store(fieldResponse)
}

func (r *Rundata) GetFieldResultByFieldId(fieldId uint32) *FieldResponse {
	val := r.fieldResultSlice[fieldId].Load()
	return val
}

func (r *Rundata) AddFieldError(fieldId uint32, err error, path []string) *FieldError {
	fieldError := &FieldError{
		fieldPath: path,
		err:       err,
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

const FIELD_ERROR_TYPE_FIELD FieldErrorType = 0
const FIELD_ERROR_TYPE_TREE FieldErrorType = 1

type FieldError struct {
	err       error
	message   string
	fieldType FieldErrorType
	fieldPath []string
}

type FieldResponseType uint8

const FIELD_RESPONSE_TYPE_NORMAL FieldResponseType = 0
const FIELD_RESPONSE_TYPE_ARRAY FieldResponseType = 1

type FieldResponse struct {
	responseType       FieldResponseType
	responses          []any
	fieldPaths         [][]string
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
	return fr != nil && fr.arrayParentKeyMap != nil
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

var fieldResponsePool = sync.Pool{
	New: func() any {
		return &FieldResponse{
			responses:         make([]any, 0),
			fieldPaths:        make([][]string, 0),
			arrayParentKeyMap: make(map[any]any),
		}
	},
}

func AcquireFieldResponse(reponseType FieldResponseType) *FieldResponse {
	frVal := fieldResponsePool.Get().(*FieldResponse)
	frVal.responseType = reponseType
	frVal.responses = frVal.responses[:0]
	frVal.fieldPaths = frVal.fieldPaths[:0]
	frVal.indexOfParentArray = 0
	frVal.arrayParentKeyMap = nil
	return frVal
}

func ReleaseFieldResponse(fr *FieldResponse) {
	if fr == nil {
		return
	}
	for i := range fr.responses {
		fr.responses[i] = nil
	}
	fr.responses = fr.responses[:0]
	for i := range fr.fieldPaths {
		fr.fieldPaths[i] = nil
	}
	fr.fieldPaths = fr.fieldPaths[:0]
	fr.indexOfParentArray = 0
	fr.arrayParentKeyMap = nil
	fieldResponsePool.Put(fr)
}
