package graphql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

var benchmarkQueryResultSink *Result

func BenchmarkQueryExecute(b *testing.B) {
	schema := newBenchmarkQuerySchema(b)
	soulEngine := NewSGraphEngine(schema)
	cases := []struct {
		name      string
		query     string
		variables map[string]any
	}{
		{
			name:  "RootScalarFlat",
			query: `{ a b c d e }`,
		},
		{
			name:  "RootWide64",
			query: benchmarkWideQuery(64),
		},
		{
			name: "NestedObjectDefaultResolver",
			query: `{
				user {
					id
					name
					age
					profile {
						email
						city
					}
				}
			}`,
		},
		{
			name:  "StructDefaultResolver",
			query: `{ structProfile { name email city } }`,
		},
		{
			name: "DeepNestedObject",
			query: `{
				org {
					team {
						project {
							owner {
								profile {
									name
								}
							}
						}
					}
				}
			}`,
		},
		{
			name:      "ListObject100",
			query:     `query($n: Int!) { users(n: $n) { id name age profile { email city } } }`,
			variables: map[string]any{"n": 100},
		},
		{
			name:      "ListObject1000",
			query:     `query($n: Int!) { users(n: $n) { id name age profile { email city } } }`,
			variables: map[string]any{"n": 1000},
		},
		{
			name:      "ListNestedObject100",
			query:     `query($n: Int!) { orders(n: $n) { id total items { sku qty product { name price } } } }`,
			variables: map[string]any{"n": 100},
		},
		{
			name:      "ArgumentsAndDirectives",
			query:     `query($id: ID!, $withEmail: Boolean!, $skipAge: Boolean!) { userById(id: $id) { id name email @include(if: $withEmail) age @skip(if: $skipAge) } }`,
			variables: map[string]any{"id": "42", "withEmail": true, "skipAge": false},
		},
		{
			name: "FragmentsAliasesAndMerge",
			query: `{
				user {
					id
					displayName: name
					...UserBenchFields
					name
				}
			}
			fragment UserBenchFields on User {
				name
				age
				profile { email }
			}`,
		},
		{
			name:      "InterfaceResolveType100",
			query:     `query($n: Int!) { nodes(n: $n) { __typename id ... on User { name } ... on Robot { serial } } }`,
			variables: map[string]any{"n": 100},
		},
		{
			name:      "UnionResolveType100",
			query:     `query($n: Int!) { search(n: $n) { __typename ... on User { id name } ... on Robot { id serial } } }`,
			variables: map[string]any{"n": 100},
		},
		{
			name:      "UnionIsTypeOf100",
			query:     `query($n: Int!) { results(n: $n) { __typename ... on Alpha { value } ... on Beta { value } } }`,
			variables: map[string]any{"n": 100},
		},
		{
			name:  "IntrospectionType",
			query: `{ __type(name: "User") { name fields { name type { name kind ofType { name kind } } } } }`,
		},
		{
			name:      "FoldNestedWideIndependent",
			query:     benchmarkFoldNestedWideQuery(32),
			variables: map[string]any{"id": "fold-1"},
		},
		{
			name: "FoldDeepIndependent",
			query: `query($id: ID!) {
				foldPage {
					section(id: $id) {
						panel(id: $id) {
							widget(id: $id) {
								value(id: $id)
							}
						}
					}
				}
			}`,
			variables: map[string]any{"id": "fold-1"},
		},
		{
			name: "FoldMixedDependencies",
			query: `query($id: ID!, $n: Int!) {
				foldPage {
					independentA(id: $id)
					independentB(id: $id)
					foldUsers(n: $n) {
						id
						score(id: $id)
					}
				}
			}`,
			variables: map[string]any{"id": "fold-1", "n": 32},
		},
		{
			name:      "FoldLatencyAmplification",
			query:     benchmarkFoldLatencyQuery(16),
			variables: map[string]any{"id": "fold-1"},
		},
		{
			name: "FoldNestedListIndependent",
			query: `query($id: ID!, $n: Int!) {
				foldListPage {
					users(n: $n) {
						id
						profile(id: $id) { email }
						stats(id: $id) { points }
					}
				}
			}`,
			variables: map[string]any{"id": "fold-1", "n": 32},
		},
	}

	for _, tc := range cases {
		tc := tc
		doc := benchmarkParseAndValidate(b, schema, tc.name, tc.query)
		b.Run(tc.name+"/GraphSoul", func(b *testing.B) {
			benchmarkExecuteQuery(b, ExecuteParams{
				Schema:       schema,
				SGraphEngine: soulEngine,
				AST:          doc,
				Args:         tc.variables,
				Context:      context.Background(),
			}, Execute)
		})
		b.Run(tc.name+"/GraphQLGo", func(b *testing.B) {
			benchmarkExecuteQuery(b, ExecuteParams{
				Schema:  schema,
				AST:     doc,
				Args:    tc.variables,
				Context: context.Background(),
			}, ExecuteGraphQLGo)
		})
	}
}

func benchmarkExecuteQuery(b *testing.B, params ExecuteParams, execute func(ExecuteParams) *Result) {
	b.Helper()
	warmResult := execute(params)
	if warmResult == nil {
		b.Fatal("execute returned nil result")
	}
	if len(warmResult.Errors) > 0 {
		b.Fatalf("execute returned errors: %#v", warmResult.Errors)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkQueryResultSink = execute(params)
	}
}

func benchmarkParseAndValidate(b *testing.B, schema Schema, name string, query string) *ast.Document {
	b.Helper()
	src := source.NewSource(&source.Source{
		Body: []byte(query),
		Name: name + ".graphql",
	})
	doc, err := parser.Parse(parser.ParseParams{Source: src})
	if err != nil {
		b.Fatalf("parse %s failed: %v", name, err)
	}
	validationResult := ValidateDocument(&schema, doc, nil)
	if !validationResult.IsValid {
		b.Fatalf("validate %s failed: %#v", name, validationResult.Errors)
	}
	return doc
}

func benchmarkWideQuery(width int) string {
	var query strings.Builder
	query.WriteByte('{')
	for i := 0; i < width; i++ {
		query.WriteString(fmt.Sprintf(" f%02d", i))
	}
	query.WriteString(" }")
	return query.String()
}

func benchmarkFoldNestedWideQuery(width int) string {
	var query strings.Builder
	query.WriteString("query($id: ID!) { foldPage {")
	for i := 0; i < width; i++ {
		query.WriteString(fmt.Sprintf(" f%02d(id: $id)", i))
	}
	query.WriteString(" } }")
	return query.String()
}

func benchmarkFoldLatencyQuery(width int) string {
	var query strings.Builder
	query.WriteString("query($id: ID!) { foldLatency {")
	for i := 0; i < width; i++ {
		query.WriteString(fmt.Sprintf(" l%02d(id: $id)", i))
	}
	query.WriteString(" } }")
	return query.String()
}

func newBenchmarkQuerySchema(tb testing.TB) Schema {
	tb.Helper()

	profileType := NewObject(ObjectConfig{
		Name: "BenchProfile",
		Fields: Fields{
			"name":  &Field{Type: String},
			"email": &Field{Type: String},
			"city":  &Field{Type: String},
		},
	})

	var nodeInterface *Interface
	var userType *Object
	var robotType *Object

	nodeInterface = NewInterface(InterfaceConfig{
		Name: "BenchNode",
		Fields: Fields{
			"id": &Field{Type: NewNonNull(ID)},
		},
		ResolveType: func(p ResolveTypeParams) *Object {
			item, _ := p.Value.(map[string]any)
			switch item["kind"] {
			case "User":
				return userType
			case "Robot":
				return robotType
			default:
				return nil
			}
		},
	})

	userType = NewObject(ObjectConfig{
		Name:       "User",
		Interfaces: []*Interface{nodeInterface},
		Fields: Fields{
			"id":      &Field{Type: NewNonNull(ID)},
			"name":    &Field{Type: String},
			"email":   &Field{Type: String},
			"age":     &Field{Type: Int},
			"profile": &Field{Type: profileType},
		},
	})

	robotType = NewObject(ObjectConfig{
		Name:       "Robot",
		Interfaces: []*Interface{nodeInterface},
		Fields: Fields{
			"id":     &Field{Type: NewNonNull(ID)},
			"serial": &Field{Type: String},
		},
	})

	searchResultType := NewUnion(UnionConfig{
		Name:  "BenchSearchResult",
		Types: []*Object{userType, robotType},
		ResolveType: func(p ResolveTypeParams) *Object {
			item, _ := p.Value.(map[string]any)
			switch item["kind"] {
			case "User":
				return userType
			case "Robot":
				return robotType
			default:
				return nil
			}
		},
	})

	alphaType := NewObject(ObjectConfig{
		Name: "Alpha",
		Fields: Fields{
			"value": &Field{Type: String},
		},
		IsTypeOf: func(p IsTypeOfParams) bool {
			item, _ := p.Value.(map[string]any)
			return item["kind"] == "Alpha"
		},
	})
	betaType := NewObject(ObjectConfig{
		Name: "Beta",
		Fields: Fields{
			"value": &Field{Type: String},
		},
		IsTypeOf: func(p IsTypeOfParams) bool {
			item, _ := p.Value.(map[string]any)
			return item["kind"] == "Beta"
		},
	})
	fallbackResultType := NewUnion(UnionConfig{
		Name:  "BenchFallbackResult",
		Types: []*Object{alphaType, betaType},
	})

	productType := NewObject(ObjectConfig{
		Name: "BenchProduct",
		Fields: Fields{
			"name":  &Field{Type: String},
			"price": &Field{Type: Float},
		},
	})
	itemType := NewObject(ObjectConfig{
		Name: "BenchOrderItem",
		Fields: Fields{
			"sku":     &Field{Type: String},
			"qty":     &Field{Type: Int},
			"product": &Field{Type: productType},
		},
	})
	orderType := NewObject(ObjectConfig{
		Name: "BenchOrder",
		Fields: Fields{
			"id":    &Field{Type: ID},
			"total": &Field{Type: Float},
			"items": &Field{Type: NewList(itemType)},
		},
	})

	deepProfileType := NewObject(ObjectConfig{Name: "BenchDeepProfile", Fields: Fields{"name": &Field{Type: String}}})
	deepOwnerType := NewObject(ObjectConfig{Name: "BenchDeepOwner", Fields: Fields{"profile": &Field{Type: deepProfileType}}})
	deepProjectType := NewObject(ObjectConfig{Name: "BenchDeepProject", Fields: Fields{"owner": &Field{Type: deepOwnerType}}})
	deepTeamType := NewObject(ObjectConfig{Name: "BenchDeepTeam", Fields: Fields{"project": &Field{Type: deepProjectType}}})
	deepOrgType := NewObject(ObjectConfig{Name: "BenchOrg", Fields: Fields{"team": &Field{Type: deepTeamType}}})

	foldIDArgs := func() FieldConfigArgument {
		return FieldConfigArgument{
			"id": &ArgumentConfig{Type: NewNonNull(ID)},
		}
	}
	foldNArgs := func() FieldConfigArgument {
		return FieldConfigArgument{
			"n": &ArgumentConfig{Type: NewNonNull(Int)},
		}
	}
	foldValue := func(prefix string) FieldResolveFn {
		return func(p ResolveParams) (any, error) {
			return prefix + fmt.Sprintf("%v", p.Args["id"]), nil
		}
	}

	foldWidgetType := NewObject(ObjectConfig{
		Name: "BenchFoldWidget",
		Fields: Fields{
			"id": &Field{Type: ID},
			"value": &Field{
				Type:    String,
				Args:    foldIDArgs(),
				Resolve: foldValue("widget-value-"),
			},
		},
	})
	foldPanelType := NewObject(ObjectConfig{
		Name: "BenchFoldPanel",
		Fields: Fields{
			"id": &Field{Type: ID},
			"widget": &Field{
				Type: foldWidgetType,
				Args: foldIDArgs(),
				Resolve: func(p ResolveParams) (any, error) {
					return map[string]any{"id": "widget-" + fmt.Sprintf("%v", p.Args["id"])}, nil
				},
			},
		},
	})
	foldSectionType := NewObject(ObjectConfig{
		Name: "BenchFoldSection",
		Fields: Fields{
			"id": &Field{Type: ID},
			"panel": &Field{
				Type: foldPanelType,
				Args: foldIDArgs(),
				Resolve: func(p ResolveParams) (any, error) {
					return map[string]any{"id": "panel-" + fmt.Sprintf("%v", p.Args["id"])}, nil
				},
			},
		},
	})
	foldProfileType := NewObject(ObjectConfig{
		Name: "BenchFoldProfile",
		Fields: Fields{
			"email": &Field{Type: String},
		},
	})
	foldStatsType := NewObject(ObjectConfig{
		Name: "BenchFoldStats",
		Fields: Fields{
			"points": &Field{Type: Int},
		},
	})
	foldUserType := NewObject(ObjectConfig{
		Name: "BenchFoldUser",
		Fields: Fields{
			"id": &Field{Type: NewNonNull(ID)},
			"score": &Field{
				Type: Int,
				Args: foldIDArgs(),
				Resolve: func(p ResolveParams) (any, error) {
					return len(fmt.Sprintf("%v", p.Args["id"])), nil
				},
			},
			"profile": &Field{
				Type: foldProfileType,
				Args: foldIDArgs(),
				Resolve: func(p ResolveParams) (any, error) {
					return map[string]any{"email": fmt.Sprintf("%v@example.com", p.Args["id"])}, nil
				},
			},
			"stats": &Field{
				Type: foldStatsType,
				Args: foldIDArgs(),
				Resolve: func(p ResolveParams) (any, error) {
					return map[string]any{"points": len(fmt.Sprintf("%v", p.Args["id"]))}, nil
				},
			},
		},
	})
	foldPageFields := Fields{
		"id": &Field{Type: ID},
		"independentA": &Field{
			Type:    String,
			Args:    foldIDArgs(),
			Resolve: foldValue("independent-a-"),
		},
		"independentB": &Field{
			Type:    String,
			Args:    foldIDArgs(),
			Resolve: foldValue("independent-b-"),
		},
		"section": &Field{
			Type: foldSectionType,
			Args: foldIDArgs(),
			Resolve: func(p ResolveParams) (any, error) {
				return map[string]any{"id": "section-" + fmt.Sprintf("%v", p.Args["id"])}, nil
			},
		},
		"foldUsers": &Field{
			Type: NewList(foldUserType),
			Args: foldNArgs(),
			Resolve: func(p ResolveParams) (any, error) {
				return benchmarkFoldUsers(p.Args["n"].(int)), nil
			},
		},
	}
	for i := 0; i < 32; i++ {
		fieldName := fmt.Sprintf("f%02d", i)
		prefix := fieldName + "-"
		foldPageFields[fieldName] = &Field{
			Type:    String,
			Args:    foldIDArgs(),
			Resolve: foldValue(prefix),
		}
	}
	foldPageType := NewObject(ObjectConfig{Name: "BenchFoldPage", Fields: foldPageFields})
	foldLatencyFields := Fields{
		"id": &Field{Type: ID},
	}
	for i := 0; i < 16; i++ {
		fieldName := fmt.Sprintf("l%02d", i)
		prefix := fieldName + "-"
		foldLatencyFields[fieldName] = &Field{
			Type: String,
			Args: foldIDArgs(),
			Resolve: func(p ResolveParams) (any, error) {
				time.Sleep(200 * time.Microsecond)
				return prefix + fmt.Sprintf("%v", p.Args["id"]), nil
			},
		}
	}
	foldLatencyType := NewObject(ObjectConfig{Name: "BenchFoldLatency", Fields: foldLatencyFields})
	foldListPageType := NewObject(ObjectConfig{
		Name: "BenchFoldListPage",
		Fields: Fields{
			"users": &Field{
				Type: NewList(foldUserType),
				Args: foldNArgs(),
				Resolve: func(p ResolveParams) (any, error) {
					return benchmarkFoldUsers(p.Args["n"].(int)), nil
				},
			},
		},
	})

	queryFields := Fields{
		"a": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) { return "a", nil }},
		"b": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) { return "b", nil }},
		"c": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) { return "c", nil }},
		"d": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) { return "d", nil }},
		"e": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) { return "e", nil }},
		"user": &Field{
			Type:    userType,
			Resolve: func(p ResolveParams) (any, error) { return benchmarkUser(1), nil },
		},
		"structProfile": &Field{
			Type: profileType,
			Resolve: func(p ResolveParams) (any, error) {
				return benchmarkProfile{Name: "Ada", Email: "ada@example.com", City: "London"}, nil
			},
		},
		"org": &Field{
			Type:    deepOrgType,
			Resolve: func(p ResolveParams) (any, error) { return benchmarkDeepOrg(), nil },
		},
		"users": &Field{
			Type: NewList(userType),
			Args: FieldConfigArgument{
				"n": &ArgumentConfig{Type: NewNonNull(Int)},
			},
			Resolve: func(p ResolveParams) (any, error) {
				return benchmarkUsers(p.Args["n"].(int)), nil
			},
		},
		"userById": &Field{
			Type: userType,
			Args: FieldConfigArgument{
				"id": &ArgumentConfig{Type: NewNonNull(ID)},
			},
			Resolve: func(p ResolveParams) (any, error) {
				return benchmarkUserFromID(fmt.Sprintf("%v", p.Args["id"])), nil
			},
		},
		"orders": &Field{
			Type: NewList(orderType),
			Args: FieldConfigArgument{
				"n": &ArgumentConfig{Type: NewNonNull(Int)},
			},
			Resolve: func(p ResolveParams) (any, error) {
				return benchmarkOrders(p.Args["n"].(int)), nil
			},
		},
		"nodes": &Field{
			Type: NewList(nodeInterface),
			Args: FieldConfigArgument{
				"n": &ArgumentConfig{Type: NewNonNull(Int)},
			},
			Resolve: func(p ResolveParams) (any, error) {
				return benchmarkAbstractValues(p.Args["n"].(int)), nil
			},
		},
		"search": &Field{
			Type: NewList(searchResultType),
			Args: FieldConfigArgument{
				"n": &ArgumentConfig{Type: NewNonNull(Int)},
			},
			Resolve: func(p ResolveParams) (any, error) {
				return benchmarkAbstractValues(p.Args["n"].(int)), nil
			},
		},
		"results": &Field{
			Type: NewList(fallbackResultType),
			Args: FieldConfigArgument{
				"n": &ArgumentConfig{Type: NewNonNull(Int)},
			},
			Resolve: func(p ResolveParams) (any, error) {
				return benchmarkFallbackValues(p.Args["n"].(int)), nil
			},
		},
		"foldPage": &Field{
			Type:    foldPageType,
			Resolve: func(p ResolveParams) (any, error) { return map[string]any{"id": "fold-page"}, nil },
		},
		"foldLatency": &Field{
			Type:    foldLatencyType,
			Resolve: func(p ResolveParams) (any, error) { return map[string]any{"id": "fold-latency"}, nil },
		},
		"foldListPage": &Field{
			Type:    foldListPageType,
			Resolve: func(p ResolveParams) (any, error) { return map[string]any{"id": "fold-list-page"}, nil },
		},
	}
	for i := 0; i < 64; i++ {
		value := fmt.Sprintf("v%02d", i)
		queryFields[fmt.Sprintf("f%02d", i)] = &Field{
			Type:    String,
			Resolve: func(p ResolveParams) (any, error) { return value, nil },
		}
	}

	queryType := NewObject(ObjectConfig{Name: "Query", Fields: queryFields})
	schema, err := NewSchema(SchemaConfig{
		Query: queryType,
		Types: []Type{
			profileType,
			userType,
			robotType,
			nodeInterface,
			searchResultType,
			alphaType,
			betaType,
			fallbackResultType,
			productType,
			itemType,
			orderType,
			deepOrgType,
			foldPageType,
			foldSectionType,
			foldPanelType,
			foldWidgetType,
			foldUserType,
			foldProfileType,
			foldStatsType,
			foldLatencyType,
			foldListPageType,
		},
	})
	if err != nil {
		tb.Fatalf("NewSchema failed: %v", err)
	}
	return schema
}

type benchmarkProfile struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	City  string `json:"city"`
}

func benchmarkUser(id int) map[string]any {
	return benchmarkUserFromID(fmt.Sprintf("%d", id))
}

func benchmarkUserFromID(id string) map[string]any {
	return map[string]any{
		"kind":  "User",
		"id":    id,
		"name":  "User " + id,
		"email": "user" + id + "@example.com",
		"age":   30,
		"profile": map[string]any{
			"name":  "User " + id,
			"email": "user" + id + "@example.com",
			"city":  "City " + id,
		},
	}
}

func benchmarkUsers(n int) []any {
	users := make([]any, n)
	for i := 0; i < n; i++ {
		users[i] = benchmarkUser(i + 1)
	}
	return users
}

func benchmarkFoldUsers(n int) []any {
	users := make([]any, n)
	for i := 0; i < n; i++ {
		users[i] = map[string]any{
			"id": fmt.Sprintf("fold-user-%d", i+1),
		}
	}
	return users
}

func benchmarkDeepOrg() map[string]any {
	return map[string]any{
		"team": map[string]any{
			"project": map[string]any{
				"owner": map[string]any{
					"profile": map[string]any{
						"name": "Deep Ada",
					},
				},
			},
		},
	}
}

func benchmarkOrders(n int) []any {
	orders := make([]any, n)
	for i := 0; i < n; i++ {
		items := make([]any, 3)
		for j := 0; j < 3; j++ {
			items[j] = map[string]any{
				"sku": fmt.Sprintf("SKU-%d-%d", i, j),
				"qty": j + 1,
				"product": map[string]any{
					"name":  fmt.Sprintf("Product %d-%d", i, j),
					"price": float64(10 + j),
				},
			}
		}
		orders[i] = map[string]any{
			"id":    fmt.Sprintf("order-%d", i),
			"total": float64(100 + i),
			"items": items,
		}
	}
	return orders
}

func benchmarkAbstractValues(n int) []any {
	values := make([]any, n)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			values[i] = map[string]any{"kind": "User", "id": fmt.Sprintf("u-%d", i), "name": fmt.Sprintf("User %d", i)}
		} else {
			values[i] = map[string]any{"kind": "Robot", "id": fmt.Sprintf("r-%d", i), "serial": fmt.Sprintf("RX-%d", i)}
		}
	}
	return values
}

func benchmarkFallbackValues(n int) []any {
	values := make([]any, n)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			values[i] = map[string]any{"kind": "Alpha", "value": fmt.Sprintf("A-%d", i)}
		} else {
			values[i] = map[string]any{"kind": "Beta", "value": fmt.Sprintf("B-%d", i)}
		}
	}
	return values
}
