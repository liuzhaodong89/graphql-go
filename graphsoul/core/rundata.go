package core

import lmap "github.com/liuzhaodong89/lockfree-collection/map"

type Rundata struct {
	originalParams map[string]any
	fieldResultMap *lmap.Lmap
	fieldErrorMap  *lmap.Lmap
}

func NewRundata(originalParams map[string]any) *Rundata {
	rundata := &Rundata{
		originalParams: originalParams,
		fieldResultMap: lmap.New(),
		fieldErrorMap:  lmap.New(),
	}
	return rundata
}

func (r *Rundata) SetFieldResult(fieldId uint32, fieldResult any, path []string) {
	fieldResponse := &FieldResponse{
		response:  fieldResult,
		fieldPath: path,
	}
	r.fieldResultMap.Set(fieldId, fieldResponse)
}

func (r *Rundata) GetFieldResultByFieldId(fieldId uint32) *FieldResponse {
	val, existed := r.fieldResultMap.Get(fieldId)
	if !existed {
		return nil
	}
	if fr, ok := val.(*FieldResponse); ok {
		return fr
	}
	return nil
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
	if fr, ok := val.(*FieldError); ok {
		return fr
	}
	return nil
}

func (r *Rundata) GetOriginalParamByKey(inputKey string) any {
	return r.originalParams[inputKey]
}

type FieldErrorType uint8

const FIELD_ERROR_TYPE_FIELD = 0
const FIELD_ERROR_TYPE_TREE = 1

type FieldError struct {
	err       error
	message   string
	fieldType FieldErrorType
	fieldPath []string
}

type FieldResponse struct {
	response  any
	fieldPath []string
}
