package graphql

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

// FieldParamTarget使用最终responseName路径定位合并后的FieldPlan。ResponsePath包含当前字段自身，不含list index
type FieldParamTarget struct {
	ResponsePath   []string `json:"responsePath"`
	ParentTypeName string   `json:"parentTypeName"`
	FieldName      string   `json:"fieldName"`
	ParamName      string   `json:"paramName"`
}

type ParamSourceKind string

const (
	ParamSourceConst         ParamSourceKind = "CONST"
	ParamSourceInput         ParamSourceKind = "INPUT"
	ParamSourceFieldResponse ParamSourceKind = "FIELD_RESPONSE"
)

// 定位实际产生FieldResponse的resolver字段。ResultPath为空代表读取resolver的完整返回结果，不表示读取引擎内部的FieldResponse封装
type FieldResponseParamSource struct {
	ResponsePath   []string `json:"responsePath"`
	ParentTypeName string   `json:"parentTypeName"`
	FieldName      string   `json:"fieldName"`
	ResultPath     []string `json:"resultPath,omitempty"`
}

type ParamSource struct {
	Kind          ParamSourceKind           `json:"kind"`
	ConstValue    any                       `json:"constValue,omitempty"`
	InputName     string                    `json:"inputName,omitempty"`
	FieldResponse *FieldResponseParamSource `json:"fieldResponse,omitempty"`
}

type FieldParamBinding struct {
	Target FieldParamTarget `json:"target"`
	Source ParamSource      `json:"source"`
}

// 描述一个operation的全部外部参数来源配置，DocumentBody必须与parser使用的原始query文本完全一致
type QueryParamConfig struct {
	DocumentBody    string                  `json:"documentBody"`
	OperationName   string                  `json:"operationName,omitempty"`
	FieldParams     []FieldParamBinding     `json:"fieldParams,omitempty"`
	DirectiveParams []DirectiveParamBinding `json:"directiveParams,omitempty"`
}

type DirectiveParamBinding struct {
	Target DirectiveParamTarget `json:"target"`
	Source ParamSource          `json:"source"`
}

type DirectiveParamTarget struct {
	Location       string   `json:"location"`
	DirectiveName  string   `json:"directiveName"`
	ParamName      string   `json:"paramName"`
	FragmentName   string   `json:"fragmentName,omitempty"`
	ResponsePath   []string `json:"responsePath,omitempty"`
	ParentTypeName string   `json:"parentTypeName,omitempty"`
	FieldName      string   `json:"fieldName,omitempty"`
	SourceOffset   *int     `json:"sourceOffset,omitempty"`
}

type ParamRegistry struct {
	mu      sync.RWMutex
	frozen  bool
	queries map[string]*queryParamBindings
}

func (r *ParamRegistry) cloneAndFreeze() *ParamRegistry {
	result := NewParamRegistry()
	result.frozen = true
	if r == nil {
		return result
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for key, bindings := range r.queries {
		result.queries[key] = cloneQueryParamBindings(bindings)
	}
	return result
}

func (r *ParamRegistry) bindings(queryKey string) *queryParamBindings {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	bindings := r.queries[queryKey]
	return bindings
}

// queryParamBindings在registry冻结后只读，PlanCompiler只维护自己的消费状态
type queryParamBindings struct {
	fieldBindings     []FieldParamBinding
	fieldIndex        map[fieldOwnerKey][]int
	fieldTargets      map[fieldBindingKey]struct{}
	directiveBindings []DirectiveParamBinding
	directiveIndex    map[directiveOwnerKey][]int
	directiveTargets  map[directiveBindingKey]struct{}
}

type fieldOwnerKey struct {
	responsePath   string
	parentTypeName string
	fieldName      string
}

type fieldBindingKey struct {
	owner     fieldOwnerKey
	paramName string
}

type directiveOwnerKey struct {
	location       string
	directiveName  string
	fragmentName   string
	responsePath   string
	parentTypeName string
	fieldName      string
}

type directiveBindingKey struct {
	owner           directiveOwnerKey
	paramName       string
	hasSourceOffset bool
	sourceOffset    int
}

func NewParamRegistry() *ParamRegistry {
	return &ParamRegistry{queries: make(map[string]*queryParamBindings)}
}

// RegisterQuery可以分多次注册同一个operation，但同一个参数目标不允许重复。先在副本上完成校验，失败时不会写入半份配置
func (r *ParamRegistry) RegisterQuery(config QueryParamConfig) error {
	if r == nil {
		return fmt.Errorf("ParamRegistry is nil")
	}
	if config.DocumentBody == "" {
		return fmt.Errorf("param registry document body is empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.frozen {
		return fmt.Errorf("ParamRegistry already frozen")
	}
	if r.queries == nil {
		r.queries = make(map[string]*queryParamBindings)
	}

	canonicalOperationName, operationErr := canonicalRegistryOperationName(config.DocumentBody, config.OperationName)
	if operationErr != nil {
		return operationErr
	}
	queryKey := buildDocumentOperationKey([]byte(config.DocumentBody), canonicalOperationName)
	candidate := cloneQueryParamBindings(r.queries[queryKey])
	if candidate == nil {
		candidate = newQueryParamBindings()
	}

	for _, binding := range config.FieldParams {
		cloned := cloneFieldParamBinding(binding)
		if err := validateFieldParamBinding(cloned); err != nil {
			return err
		}
		owner := fieldOwnerKeyFromTarget(cloned.Target)
		targetKey := fieldBindingKey{
			owner:     owner,
			paramName: cloned.Target.ParamName,
		}
		if _, exists := candidate.fieldTargets[targetKey]; exists {
			return fmt.Errorf("duplicate field parameter target %s.%s at %v", cloned.Target.ParentTypeName, cloned.Target.FieldName, cloned.Target.ResponsePath)
		}
		index := len(candidate.fieldBindings)
		candidate.fieldBindings = append(candidate.fieldBindings, cloned)
		candidate.fieldIndex[owner] = append(candidate.fieldIndex[owner], index)
		candidate.fieldTargets[targetKey] = struct{}{}
	}

	for _, binding := range config.DirectiveParams {
		cloned := cloneDirectiveParamBinding(binding)
		if err := validateDirectiveParamBinding(cloned); err != nil {
			return err
		}
		owner := directiveOwnerKeyFromTarget(cloned.Target)
		targetKey := directiveBindingKey{
			owner:           owner,
			paramName:       cloned.Target.ParamName,
			hasSourceOffset: cloned.Target.SourceOffset != nil,
		}
		if cloned.Target.SourceOffset != nil {
			targetKey.sourceOffset = *cloned.Target.SourceOffset
		}
		if directiveTargetOverlaps(candidate.directiveTargets, targetKey) {
			return fmt.Errorf("duplicate directive parameter target @%s(%s:) at %s", cloned.Target.DirectiveName, cloned.Target.ParamName, cloned.Target.Location)
		}
		index := len(candidate.directiveBindings)
		candidate.directiveBindings = append(candidate.directiveBindings, cloned)
		candidate.directiveIndex[owner] = append(candidate.directiveIndex[owner], index)
		candidate.directiveTargets[targetKey] = struct{}{}
	}

	r.queries[queryKey] = candidate
	return nil
}

func directiveTargetOverlaps(existing map[directiveBindingKey]struct{}, candidate directiveBindingKey) bool {
	for current := range existing {
		if current.owner != candidate.owner || current.paramName != candidate.paramName {
			continue
		}
		if !current.hasSourceOffset || !candidate.hasSourceOffset || current.sourceOffset == candidate.sourceOffset {
			return true
		}
	}
	return false
}

func canonicalRegistryOperationName(documentBody string, requestedName string) (string, error) {
	document, err := parser.Parse(parser.ParseParams{
		Source: source.NewSource(&source.Source{
			Body: []byte(documentBody),
			Name: "ParamRegistry document",
		}),
	})
	if err != nil {
		return "", fmt.Errorf("param registry document is invalid: %w", err)
	}

	var selected *ast.OperationDefinition
	for _, definition := range document.Definitions {
		operation, ok := definition.(*ast.OperationDefinition)
		if !ok {
			continue
		}
		if requestedName != "" {
			if operation.Name != nil && operation.Name.Value == requestedName {
				return requestedName, nil
			}
			continue
		}
		if selected != nil {
			return "", fmt.Errorf("param registry operation name is required for a multi-operation document")
		}
		selected = operation
	}
	if requestedName != "" {
		return "", fmt.Errorf("param registry operation %q was not found", requestedName)
	}
	if selected == nil {
		return "", fmt.Errorf("param registry document has no operation")
	}
	if selected.Name == nil {
		return "", nil
	}
	return selected.Name.Value, nil
}

func buildDocumentOperationKey(documentBody []byte, operationName string) string {
	hash := sha256.New()
	hash.Write(documentBody)
	hash.Write([]byte{'\n'})
	hash.Write([]byte(operationName))
	return hex.EncodeToString(hash.Sum(nil))
}

func cloneQueryParamBindings(src *queryParamBindings) *queryParamBindings {
	if src == nil {
		return nil
	}
	dst := newQueryParamBindings()
	for _, binding := range src.fieldBindings {
		cloned := cloneFieldParamBinding(binding)
		owner := fieldOwnerKeyFromTarget(cloned.Target)
		index := len(dst.fieldBindings)
		dst.fieldBindings = append(dst.fieldBindings, cloned)
		dst.fieldIndex[owner] = append(dst.fieldIndex[owner], index)
		dst.fieldTargets[fieldBindingKey{owner: owner, paramName: cloned.Target.ParamName}] = struct{}{}
	}
	for _, binding := range src.directiveBindings {
		cloned := cloneDirectiveParamBinding(binding)
		owner := directiveOwnerKeyFromTarget(cloned.Target)
		index := len(dst.directiveBindings)
		dst.directiveBindings = append(dst.directiveBindings, cloned)
		dst.directiveIndex[owner] = append(dst.directiveIndex[owner], index)
		key := directiveBindingKey{owner: owner, paramName: cloned.Target.ParamName, hasSourceOffset: cloned.Target.SourceOffset != nil}
		if cloned.Target.SourceOffset != nil {
			key.sourceOffset = *cloned.Target.SourceOffset
		}
		dst.directiveTargets[key] = struct{}{}
	}
	return dst
}

func cloneFieldParamBinding(binding FieldParamBinding) FieldParamBinding {
	binding.Target.ResponsePath = append([]string(nil), binding.Target.ResponsePath...)
	binding.Source = cloneParamSource(binding.Source)
	return binding
}

func cloneParamSource(source ParamSource) ParamSource {
	source.ConstValue = cloneParamValue(source.ConstValue)
	if source.FieldResponse != nil {
		cloned := *source.FieldResponse
		cloned.ResponsePath = append([]string(nil), cloned.ResponsePath...)
		cloned.ResultPath = append([]string(nil), cloned.ResultPath...)
		source.FieldResponse = &cloned
	}
	return source
}

func cloneParamValue(v any) any {
	if v == nil {
		return nil
	}
	return cloneParamReflectValue(reflect.ValueOf(v)).Interface()
}

func cloneParamReflectValue(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		cloned := cloneParamReflectValue(v.Elem())
		result := reflect.New(v.Type()).Elem()
		result.Set(cloned)
		return result
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		result := reflect.MakeMapWithSize(v.Type(), v.Len())
		iterator := v.MapRange()
		for iterator.Next() {
			result.SetMapIndex(iterator.Key(), cloneParamReflectValue(iterator.Value()))
		}
		return result
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		result := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for index := 0; index < v.Len(); index++ {
			result.Index(index).Set(cloneParamReflectValue(v.Index(index)))
		}
		return result
	case reflect.Array:
		result := reflect.New(v.Type()).Elem()
		for index := 0; index < v.Len(); index++ {
			result.Index(index).Set(cloneParamReflectValue(v.Index(index)))
		}
		return result
	case reflect.Ptr:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		result := reflect.New(v.Type().Elem())
		result.Elem().Set(cloneParamReflectValue(v.Elem()))
		return result
	default:
		return v
	}
}

func cloneDirectiveParamBinding(binding DirectiveParamBinding) DirectiveParamBinding {
	binding.Target.ResponsePath = append([]string(nil), binding.Target.ResponsePath...)
	if binding.Target.SourceOffset != nil {
		offset := *binding.Target.SourceOffset
		binding.Target.SourceOffset = &offset
	}
	binding.Source = cloneParamSource(binding.Source)
	return binding
}

func newQueryParamBindings() *queryParamBindings {
	return &queryParamBindings{
		fieldIndex:       make(map[fieldOwnerKey][]int),
		fieldTargets:     make(map[fieldBindingKey]struct{}),
		directiveIndex:   make(map[directiveOwnerKey][]int),
		directiveTargets: make(map[directiveBindingKey]struct{}),
	}
}

func fieldOwnerKeyFromTarget(target FieldParamTarget) fieldOwnerKey {
	return fieldOwnerKey{
		responsePath:   encodeResponsePath(target.ResponsePath),
		parentTypeName: target.ParentTypeName,
		fieldName:      target.FieldName,
	}
}

func fieldOwnerKeyFromSource(source FieldResponseParamSource) fieldOwnerKey {
	return fieldOwnerKey{
		responsePath:   encodeResponsePath(source.ResponsePath),
		parentTypeName: source.ParentTypeName,
		fieldName:      source.FieldName,
	}
}

func directiveOwnerKeyFromTarget(target DirectiveParamTarget) directiveOwnerKey {
	return directiveOwnerKey{
		location:       target.Location,
		directiveName:  target.DirectiveName,
		fragmentName:   target.FragmentName,
		responsePath:   encodeResponsePath(target.ResponsePath),
		parentTypeName: target.ParentTypeName,
		fieldName:      target.FieldName,
	}
}

func encodeResponsePath(path []string) string {
	return strings.Join(path, "\x00")
}

func validateFieldParamBinding(binding FieldParamBinding) error {
	if err := validateFieldTarget(binding.Target); err != nil {
		return err
	}
	return validateParamSource(binding.Source)
}

func validateFieldTarget(target FieldParamTarget) error {
	if len(target.ResponsePath) == 0 || target.ParentTypeName == "" || target.FieldName == "" || target.ParamName == "" {
		return fmt.Errorf("field parameter target is incomplete")
	}
	for _, item := range target.ResponsePath {
		if item == "" {
			return fmt.Errorf("field parameter response path contains any empty item")
		}
	}
	if isIntrospectionCoordinate(target.ParentTypeName, target.FieldName) {
		return fmt.Errorf("introspection field %s.%s cannot be overridden", target.ParentTypeName, target.FieldName)
	}
	return nil
}

func validateParamSource(source ParamSource) error {
	switch source.Kind {
	case ParamSourceConst:
		if source.InputName != "" || source.FieldResponse != nil {
			return fmt.Errorf("CONST source contains fields from another source kind")
		}
	case ParamSourceInput:
		if source.InputName == "" {
			return fmt.Errorf("INPUT source requires an input name")
		}
		if source.FieldResponse != nil || source.ConstValue != nil {
			return fmt.Errorf("INPUT source contains FIELD_RESPONSE configuration")
		}
	case ParamSourceFieldResponse:
		if source.ConstValue != nil || source.InputName != "" || source.FieldResponse == nil {
			return fmt.Errorf("FIELD_RESPONSE source is incomplete")
		}
		fieldSource := source.FieldResponse
		if len(fieldSource.ResponsePath) == 0 || fieldSource.ParentTypeName == "" || fieldSource.FieldName == "" {
			return fmt.Errorf("FIELD_RESPONSE coordinate is incomplete")
		}
		if isIntrospectionCoordinate(fieldSource.ParentTypeName, fieldSource.FieldName) {
			return fmt.Errorf("introspection field cannot be a parameter source")
		}
		for _, item := range fieldSource.ResponsePath {
			if item == "" {
				return fmt.Errorf("FIELD_RESPONSE source path contains an empty item")
			}
		}
		for _, item := range fieldSource.ResultPath {
			if item == "" {
				return fmt.Errorf("FIELD_RESPONSE result path contains an empty item")
			}
		}
	default:
		return fmt.Errorf("unsupported parameter source kind %s", source.Kind)
	}
	return nil
}

func validateDirectiveParamBinding(binding DirectiveParamBinding) error {
	target := binding.Target
	if target.DirectiveName == "" || target.ParamName == "" {
		return fmt.Errorf("directive name or param name is incomplete")
	}
	switch target.Location {
	case DirectiveLocationQuery:
	case DirectiveLocationFragmentDefinition:
		if target.FragmentName == "" {
			return fmt.Errorf("fragment definition target requires fragment name")
		}
	case DirectiveLocationFragmentSpread:
		if target.FragmentName == "" {
			return fmt.Errorf("fragment spread target requires fragment name")
		}
	case DirectiveLocationInlineFragment:
	case DirectiveLocationField:
		if err := validateFieldTarget(FieldParamTarget{
			ResponsePath:   target.ResponsePath,
			ParentTypeName: target.ParentTypeName,
			FieldName:      target.FieldName,
			ParamName:      target.ParamName,
		}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported directive location %s", target.Location)
	}

	if err := validateParamSource(binding.Source); err != nil {
		return err
	}
	if binding.Source.Kind == ParamSourceFieldResponse {
		if target.DirectiveName == "skip" || target.DirectiveName == "include" {
			return fmt.Errorf("@%s does not support FIELD_RESPONSE parameters", target.DirectiveName)
		}
		if target.Location != DirectiveLocationField {
			return fmt.Errorf("FIELD_RESPONSE directive parameters are only supported at FIELD location")
		}
	}
	return nil
}

func isIntrospectionCoordinate(parentTypeName, fieldName string) bool {
	return strings.HasPrefix(parentTypeName, "__") || strings.HasPrefix(fieldName, "__")
}
