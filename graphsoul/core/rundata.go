package core

import lmap "github.com/liuzhaodong89/lockfree-collection/map"

type Rundata struct {
	originalParams map[string]any
	fieldResultMap *lmap.Lmap[uint32, *FieldResponse]
	fieldErrorMap  *lmap.Lmap[uint32, *FieldError]
}

func NewRundata(originalParams map[string]any) *Rundata {
	rundata := &Rundata{
		originalParams: originalParams,
		fieldResultMap: lmap.New[uint32, *FieldResponse](),
		fieldErrorMap:  lmap.New[uint32, *FieldError](),
	}
	return rundata
}

func (r *Rundata) SetFieldResult(fieldId uint32, fieldResponse *FieldResponse) {
	r.fieldResultMap.Set(fieldId, fieldResponse)
}

func (r *Rundata) GetFieldResultByFieldId(fieldId uint32) *FieldResponse {
	val, existed := r.fieldResultMap.Get(fieldId)
	if !existed {
		return nil
	}
	return val
}

func (r *Rundata) AddFieldError(fieldId uint32, err error, path []string) *FieldError {
	fieldError := &FieldError{
		fieldPath: path,
		err:       err,
	}
	r.fieldErrorMap.Set(fieldId, fieldError)
	return fieldError
}

func (r *Rundata) GetFieldErrorByFieldId(fieldId uint32) *FieldError {
	val, existed := r.fieldErrorMap.Get(fieldId)
	if !existed {
		return nil
	}
	return val
}

func (r *Rundata) GetOriginalParamByKey(inputKey string) any {
	return r.originalParams[inputKey]
}

func (r *Rundata) GetAllFieldErrors() []*FieldError {
	result := make([]*FieldError, 0)
	for _, key := range r.fieldErrorMap.Keys() {
		result = append(result, r.GetFieldErrorByFieldId(key))
	}
	return result
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
