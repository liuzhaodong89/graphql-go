package graphql

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func executeSGraphOccurrenceTest(t *testing.T, schema Schema, query string) *Result {
	t.Helper()
	document := parseGraphQLSpecQuery(t, query)
	validationResult := ValidateDocument(&schema, document, nil)
	if !validationResult.IsValid {
		t.Fatalf("validation failed: %#v", validationResult.Errors)
	}
	engine, err := NewSGraphEngine(&schema, nil, nil)
	if err != nil {
		t.Fatalf("NewSGraphEngine failed: %v", err)
	}
	return engine.Execute(document, nil, nil, nil, context.Background()).toGraphQLResult()
}

func requireGraphQLErrorPaths(t *testing.T, result *Result, expected ...[]any) {
	t.Helper()
	if result == nil {
		t.Fatal("result is nil")
	}
	if len(result.Errors) != len(expected) {
		t.Fatalf("error count = %d, want %d: %#v", len(result.Errors), len(expected), result.Errors)
	}
	for _, expectedPath := range expected {
		found := false
		for _, actual := range result.Errors {
			if reflect.DeepEqual(actual.Path, expectedPath) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing error path %#v in %#v", expectedPath, result.Errors)
		}
	}
}

func TestSGraphConcurrentFieldErrorsAreNotOverwritten(t *testing.T) {
	rundata := newRundata(nil, 1)
	defer releaseRundata(rundata)

	const errorCount = 100
	var waitGroup sync.WaitGroup
	waitGroup.Add(errorCount)
	for index := 0; index < errorCount; index++ {
		go func(index int) {
			defer waitGroup.Done()
			rundata.addFieldErrorAtPath(1, FieldErrorTypeField, fmt.Errorf("error %d", index), []any{"items", index, "value"})
		}(index)
	}
	waitGroup.Wait()

	if errors := rundata.getAllFieldErrors(); len(errors) != errorCount {
		t.Fatalf("error count = %d, want %d", len(errors), errorCount)
	}
}

func TestSGraphNestedListCompletionReportsEveryOccurrence(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "SGraphNestedListErrorQuery",
		Fields: Fields{
			"matrix": &Field{
				Type: NewList(NewList(NewNonNull(String))),
				Resolve: func(ResolveParams) (any, error) {
					return [][]any{{nil}, {nil}}, nil
				},
			},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatal(err)
	}

	result := executeSGraphOccurrenceTest(t, schema, `{ matrix }`)
	requireGraphQLErrorPaths(t, result, []any{"matrix", 0, 0}, []any{"matrix", 1, 0})
	data := toPlainValue(result.Data).(map[string]any)
	if !reflect.DeepEqual(data["matrix"], []any{nil, nil}) {
		t.Fatalf("matrix = %#v", data["matrix"])
	}
}

func TestSGraphIteratorContinuesAfterOccurrenceErrors(t *testing.T) {
	itemType := NewObject(ObjectConfig{
		Name: "SGraphOccurrenceItem",
		Fields: Fields{
			"id": &Field{Type: NewNonNull(ID)},
			"value": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) {
				itemIndex := p.Info.Path.AsArray()[1].(int)
				if itemIndex == 0 || itemIndex == 2 {
					return nil, fmt.Errorf("value %d failed", itemIndex)
				}
				return "ok", nil
			}},
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "SGraphOccurrenceQuery",
		Fields: Fields{
			"items": &Field{Type: NewList(itemType), Resolve: func(ResolveParams) (any, error) {
				return []map[string]any{{"id": 1}, {"id": 2}, {"id": 3}}, nil
			}},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatal(err)
	}

	result := executeSGraphOccurrenceTest(t, schema, `{ items { value } }`)
	requireGraphQLErrorPaths(t, result, []any{"items", 0, "value"}, []any{"items", 2, "value"})
	items := toPlainValue(result.Data).(map[string]any)["items"].([]any)
	if items[0].(map[string]any)["value"] != nil || items[1].(map[string]any)["value"] != "ok" || items[2].(map[string]any)["value"] != nil {
		t.Fatalf("items = %#v", items)
	}
}

func TestSGraphNestedListIteratorUsesCompletePath(t *testing.T) {
	itemType := NewObject(ObjectConfig{
		Name: "SGraphNestedOccurrenceItem",
		Fields: Fields{
			"id": &Field{Type: NewNonNull(ID)},
			"name": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) {
				if reflect.DeepEqual(p.Info.Path.AsArray(), []any{"matrix", 1, 2, "name"}) {
					return nil, errors.New("target name failed")
				}
				return "ok", nil
			}},
		},
	})
	queryType := NewObject(ObjectConfig{
		Name: "SGraphNestedOccurrenceQuery",
		Fields: Fields{
			"matrix": &Field{Type: NewList(NewList(itemType)), Resolve: func(ResolveParams) (any, error) {
				return [][]map[string]any{
					{{"id": 1}},
					{{"id": 2}, {"id": 3}, {"id": 4}},
				}, nil
			}},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatal(err)
	}

	result := executeSGraphOccurrenceTest(t, schema, `{ matrix { name } }`)
	requireGraphQLErrorPaths(t, result, []any{"matrix", 1, 2, "name"})
}

func TestSGraphBulkDownstreamIteratorUsesParentOccurrencePath(t *testing.T) {
	userType := NewObject(ObjectConfig{
		Name: "SGraphBulkOccurrenceUser",
		Fields: Fields{
			"id":      &Field{Type: NewNonNull(ID)},
			"groupId": &Field{Type: NewNonNull(ID)},
			"name": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) {
				path := p.Info.Path.AsArray()
				if reflect.DeepEqual(path, []any{"groups", 1, "users", 0, "name"}) || reflect.DeepEqual(path, []any{"groups", 1, "users", 1, "name"}) {
					return nil, errors.New("bulk target name failed")
				}
				return "ok", nil
			}},
		},
	})
	groupType := NewObject(ObjectConfig{
		Name: "SGraphBulkOccurrenceGroup",
		Fields: Fields{
			"id":    &Field{Type: NewNonNull(ID)},
			"users": &Field{Type: NewList(userType)},
		},
	})
	usersDefinition := groupType.Fields()["users"]
	usersDefinition.BulkResultMappedFieldName = "groupId"
	usersDefinition.BulkResolve = func(ResolveParams) (any, error) {
		return []map[string]any{
			{"id": 20, "groupId": 2},
			{"id": 10, "groupId": 1},
			{"id": 21, "groupId": 2},
		}, nil
	}
	queryType := NewObject(ObjectConfig{
		Name: "SGraphBulkOccurrenceQuery",
		Fields: Fields{
			"groups": &Field{Type: NewList(groupType), Resolve: func(ResolveParams) (any, error) {
				return []map[string]any{{"id": 1}, {"id": 2}}, nil
			}},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatal(err)
	}

	result := executeSGraphOccurrenceTest(t, schema, `{ groups { users { name } } }`)
	requireGraphQLErrorPaths(t, result,
		[]any{"groups", 1, "users", 0, "name"},
		[]any{"groups", 1, "users", 1, "name"},
	)
}

func TestSGraphBulkBindingErrorIsMappedDuringAssembly(t *testing.T) {
	userType := NewObject(ObjectConfig{
		Name: "SGraphBulkBindingUser",
		Fields: Fields{
			"id":      &Field{Type: NewNonNull(ID)},
			"groupId": &Field{Type: NewNonNull(ID)},
		},
	})
	groupType := NewObject(ObjectConfig{
		Name: "SGraphBulkBindingGroup",
		Fields: Fields{
			"id":   &Field{Type: NewNonNull(ID)},
			"user": &Field{Type: userType},
		},
	})
	userDefinition := groupType.Fields()["user"]
	userDefinition.BulkResultMappedFieldName = "groupId"
	userDefinition.BulkResolve = func(ResolveParams) (any, error) {
		return []map[string]any{
			{"id": 10, "groupId": 1},
			{"id": 20, "groupId": 2},
			{"id": 21, "groupId": 2},
		}, nil
	}
	queryType := NewObject(ObjectConfig{
		Name: "SGraphBulkBindingQuery",
		Fields: Fields{
			"groups": &Field{Type: NewList(groupType), Resolve: func(ResolveParams) (any, error) {
				return []map[string]any{{"id": 1}, {"id": 2}}, nil
			}},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatal(err)
	}

	result := executeSGraphOccurrenceTest(t, schema, `{ groups { user { id } } }`)
	requireGraphQLErrorPaths(t, result, []any{"groups", 1, "user"})
}

func TestSGraphTreeErrorSkipsResultAssembly(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "SGraphTreeErrorQuery",
		Fields: Fields{
			"value": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				return "must not be assembled", nil
			}},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatal(err)
	}
	document := parseGraphQLSpecQuery(t, `{ value }`)
	engine, err := NewSGraphEngine(&schema, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compileExecutionPlan(document, &schema, nil, engine.directiveRegistry, engine.paramRegistry)
	if err != nil {
		t.Fatal(err)
	}
	plan.batches = []*BatchPlan{{batchId: 0, steps: []Step{nil}}}

	result := engine.executePlan(plan, nil, nil, context.Background(), nil).toGraphQLResult()
	if result.Data != nil {
		t.Fatalf("tree error data = %#v, want nil", result.Data)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("tree error result = %#v", result)
	}
}

func TestSGraphTreeErrorKeepsFieldErrorsFromSameBatch(t *testing.T) {
	queryType := NewObject(ObjectConfig{
		Name: "SGraphMixedErrorQuery",
		Fields: Fields{
			"value": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				return nil, errors.New("field failed")
			}},
		},
	})
	schema, err := NewSchema(SchemaConfig{Query: queryType})
	if err != nil {
		t.Fatal(err)
	}
	document := parseGraphQLSpecQuery(t, `{ value }`)
	engine, err := NewSGraphEngine(&schema, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compileExecutionPlan(document, &schema, nil, engine.directiveRegistry, engine.paramRegistry)
	if err != nil {
		t.Fatal(err)
	}
	plan.batches = []*BatchPlan{{
		batchId:    0,
		concurrent: true,
		steps:      []Step{&SingleCallStep{fieldPlan: plan.roots[0]}, nil},
	}}

	result := engine.executePlan(plan, nil, nil, context.Background(), nil)
	if result.orderedResponses != nil {
		t.Fatalf("tree error data = %#v, want nil", result.orderedResponses)
	}
	if len(result.errors) != 2 {
		t.Fatalf("mixed error count = %d, want 2", len(result.errors))
	}
	seenFieldError := false
	seenTreeError := false
	for _, fieldError := range result.errors {
		if fieldError.errorType == FieldErrorTypeField {
			seenFieldError = true
		}
		if fieldError.errorType == FieldErrorTypeTree {
			seenTreeError = true
		}
	}
	if !seenFieldError || !seenTreeError {
		t.Fatalf("mixed errors = %#v", result.errors)
	}
}

func TestSGraphPoolsClearOccurrenceState(t *testing.T) {
	rundata := newRundata(nil, 1)
	rundata.addFieldErrorAtPath(1, FieldErrorTypeField, errors.New("old error"), []any{"items", 3, "name"})
	rundata.assemblyListIndexes = append(rundata.assemblyListIndexes, 3)
	rundata.hasPendingBulkBindingErrors.Store(true)
	releaseRundata(rundata)

	reusedRundata := newRundata(nil, 1)
	if len(reusedRundata.getAllFieldErrors()) != 0 || len(reusedRundata.assemblyListIndexes) != 0 || reusedRundata.hasPendingBulkBindingErrors.Load() {
		t.Fatalf("reused rundata retained request state: %#v", reusedRundata)
	}
	releaseRundata(reusedRundata)

	fieldResponse := acquireFieldResponse()
	fieldResponse.responsePaths = append(fieldResponse.responsePaths, (&ResponsePath{}).WithKey("old"))
	state := fieldResponse.ensureBulkState(&FieldPlan{fieldId: 1})
	state.iterationState = bulkIterationReady
	state.iterationResponses = append(state.iterationResponses, fieldResponseOccurrence{responseRaw: "old"})
	state.bindingErrors = map[any][]bulkBindingError{"old": {{err: errors.New("old")}}}
	releaseFieldResponse(fieldResponse)

	reusedFieldResponse := acquireFieldResponse()
	if len(reusedFieldResponse.responsePaths) != 0 || reusedFieldResponse.bulkState != nil {
		t.Fatalf("reused field response retained request state: %#v", reusedFieldResponse)
	}
	releaseFieldResponse(reusedFieldResponse)
}
