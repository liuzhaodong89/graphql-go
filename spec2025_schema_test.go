package graphql

// spec2025_schema_test.go
//
// GraphQL September 2025 一致性语料使用的 schema 构造器。
// 每个构造器每次调用都返回全新的类型实例和全新的 Schema，
// 因此不同用例之间不会共享 SGraphEngine 缓存（缓存 key 是 schema.typeMap 指针）。

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 通用小 schema 构造器
// ---------------------------------------------------------------------------

// s25NewSchema 构造一个只有 Query 根类型的 schema。
func s25NewSchema(t testing.TB, fields Fields, extraTypes ...Type) Schema {
	t.Helper()
	schema, err := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: fields}),
		Types: extraTypes,
	})
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	return schema
}

// s25Const 返回一个固定值的 resolver。
func s25Const(value any) FieldResolveFn {
	return func(ResolveParams) (any, error) { return value, nil }
}

// s25Fail 返回一个总是报错的 resolver。
func s25Fail(message string) FieldResolveFn {
	return func(ResolveParams) (any, error) { return nil, errors.New(message) }
}

// ---------------------------------------------------------------------------
// Core schema：§2 / §5 / §6 / §7 / 交叉 / 边界 的主语料
// ---------------------------------------------------------------------------

// s25CoreData 是 core schema 的固定数据集。
func s25CorePeople() []any {
	return []any{
		map[string]any{"id": "p1", "name": "Ada", "kind": "S25User"},
		map[string]any{"id": "p2", "name": "Bob", "kind": "S25User"},
		map[string]any{"id": "p3", "name": "Cid", "kind": "S25User"},
	}
}

func s25CoreNodes() []any {
	return []any{
		map[string]any{"id": "u1", "name": "Ada", "kind": "S25User"},
		map[string]any{"id": "r1", "serial": "RX-2", "kind": "S25Robot"},
	}
}

// s25NewCoreSchema 构造执行语料主 schema。counter 可为 nil。
func s25NewCoreSchema(t testing.TB, counter *s25Counter) (Schema, *s25CoreTypes) {
	t.Helper()
	if counter == nil {
		counter = s25NewCounter()
	}
	types := &s25CoreTypes{
		Opaque: s25NewOpaqueScalar(),
		Odd:    s25NewOddScalar(),
		Mode:   s25NewMode(),
	}

	types.Inner = NewInputObject(InputObjectConfig{
		Name: "S25Inner",
		Fields: InputObjectConfigFieldMap{
			"flag":   &InputObjectFieldConfig{Type: Boolean, DefaultValue: false},
			"values": &InputObjectFieldConfig{Type: NewList(NewNonNull(Int))},
		},
	})
	types.Filter = NewInputObject(InputObjectConfig{
		Name: "S25Filter",
		Fields: InputObjectConfigFieldMap{
			"text":   &InputObjectFieldConfig{Type: NewNonNull(String)},
			"count":  &InputObjectFieldConfig{Type: Int, DefaultValue: 7},
			"mode":   &InputObjectFieldConfig{Type: types.Mode, DefaultValue: "A"},
			"tags":   &InputObjectFieldConfig{Type: NewList(NewNonNull(String))},
			"nested": &InputObjectFieldConfig{Type: types.Inner},
		},
	})

	resolveKind := func(value any) string {
		if m, ok := value.(map[string]any); ok {
			if kind, ok := m["kind"].(string); ok {
				return kind
			}
		}
		return ""
	}

	types.Node = NewInterface(InterfaceConfig{
		Name:   "S25Node",
		Fields: Fields{"id": &Field{Type: NewNonNull(ID)}},
	})

	// upper: 唯一需要父对象注入的字段。参数可空、无默认值，原生链路下不会进入 p.Args。
	upperField := func() *Field {
		return &Field{
			Type: String,
			Args: s25ParentArg(types.Opaque),
			Resolve: func(p ResolveParams) (any, error) {
				counter.inc("upper")
				name, ok := s25ParentString(p, "name")
				if !ok {
					return nil, nil
				}
				return "UP:" + name, nil
			},
		}
	}

	types.User = NewObject(ObjectConfig{
		Name:        "S25User",
		Description: "A user of the conformance corpus.",
		Interfaces:  []*Interface{types.Node},
		IsTypeOf: func(p IsTypeOfParams) bool {
			return resolveKind(p.Value) == "S25User"
		},
		Fields: Fields{
			"id":    &Field{Type: NewNonNull(ID)},
			"name":  &Field{Type: String, Description: "The user display name."},
			"upper": upperField(),
			"boom": &Field{
				Type: String,
				Resolve: func(p ResolveParams) (any, error) {
					counter.inc("boom")
					return nil, errors.New("boom failed")
				},
			},
			"mustFail": &Field{
				Type: NewNonNull(String),
				Resolve: func(p ResolveParams) (any, error) {
					counter.inc("mustFail")
					return nil, errors.New("mustFail failed")
				},
			},
			"failForBob": &Field{
				Type: String,
				Args: s25ParentArg(types.Opaque),
				Resolve: func(p ResolveParams) (any, error) {
					counter.inc("failForBob")
					name, _ := s25ParentString(p, "name")
					if name == "Bob" {
						return nil, errors.New("bob is not allowed")
					}
					return name, nil
				},
			},
			"legacy": &Field{
				Type:              String,
				DeprecationReason: "use name",
				Resolve:           s25Const("legacy"),
			},
		},
	})

	types.Robot = NewObject(ObjectConfig{
		Name:       "S25Robot",
		Interfaces: []*Interface{types.Node},
		IsTypeOf: func(p IsTypeOfParams) bool {
			return resolveKind(p.Value) == "S25Robot"
		},
		Fields: Fields{
			"id":     &Field{Type: NewNonNull(ID)},
			"serial": &Field{Type: String},
		},
	})

	types.Node.ResolveType = func(p ResolveTypeParams) *Object {
		switch resolveKind(p.Value) {
		case "S25Robot":
			return types.Robot
		case "S25User":
			return types.User
		}
		return nil
	}

	types.Search = NewUnion(UnionConfig{
		Name:  "S25Search",
		Types: []*Object{types.User, types.Robot},
		ResolveType: func(p ResolveTypeParams) *Object {
			switch resolveKind(p.Value) {
			case "S25Robot":
				return types.Robot
			case "S25User":
				return types.User
			}
			return nil
		},
	})

	fields := Fields{
		"greeting": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) {
			counter.inc("greeting")
			return "hello", nil
		}},
		"echo": &Field{
			Type: String,
			Args: FieldConfigArgument{
				"text":   &ArgumentConfig{Type: NewNonNull(String)},
				"suffix": &ArgumentConfig{Type: String, DefaultValue: "!"},
			},
			Resolve: func(p ResolveParams) (any, error) {
				counter.inc("echo")
				text, _ := p.Args["text"].(string)
				suffix, ok := p.Args["suffix"]
				if !ok {
					return text + "<missing>", nil
				}
				if suffix == nil {
					return text + "<null>", nil
				}
				return text + fmt.Sprintf("%v", suffix), nil
			},
		},
		"echoOptional": &Field{
			Type: String,
			Args: FieldConfigArgument{
				"value": &ArgumentConfig{Type: String, DefaultValue: "argDefault"},
			},
			Resolve: func(p ResolveParams) (any, error) {
				value, ok := p.Args["value"]
				if !ok {
					return "<missing>", nil
				}
				if value == nil {
					return "<null>", nil
				}
				return fmt.Sprintf("%v", value), nil
			},
		},
		"echoNoDefault": &Field{
			Type: String,
			Args: FieldConfigArgument{
				"value": &ArgumentConfig{Type: String},
			},
			Resolve: func(p ResolveParams) (any, error) {
				value, ok := p.Args["value"]
				if !ok {
					return "<missing>", nil
				}
				if value == nil {
					return "<null>", nil
				}
				return fmt.Sprintf("%v", value), nil
			},
		},
		"echoFilter": &Field{
			Type: String,
			Args: FieldConfigArgument{
				"filter": &ArgumentConfig{Type: NewNonNull(types.Filter)},
			},
			Resolve: func(p ResolveParams) (any, error) {
				counter.inc("echoFilter")
				return s25DescribeFilter(p.Args["filter"]), nil
			},
		},
		"echoMode": &Field{
			Type: String,
			Args: FieldConfigArgument{
				"mode": &ArgumentConfig{Type: types.Mode, DefaultValue: "A"},
			},
			Resolve: func(p ResolveParams) (any, error) {
				return fmt.Sprintf("%v", p.Args["mode"]), nil
			},
		},
		"echoList": &Field{
			Type: String,
			Args: FieldConfigArgument{
				"values": &ArgumentConfig{Type: NewList(NewNonNull(Int))},
			},
			Resolve: func(p ResolveParams) (any, error) {
				return fmt.Sprintf("%v", p.Args["values"]), nil
			},
		},
		"echoNestedList": &Field{
			Type: String,
			Args: FieldConfigArgument{
				"values": &ArgumentConfig{Type: NewNonNull(NewList(NewNonNull(NewList(NewNonNull(Int)))))},
			},
			Resolve: func(p ResolveParams) (any, error) {
				return fmt.Sprintf("%v", p.Args["values"]), nil
			},
		},
		"echoInt": &Field{
			Type: String,
			Args: FieldConfigArgument{"value": &ArgumentConfig{Type: NewNonNull(Int)}},
			Resolve: func(p ResolveParams) (any, error) {
				return fmt.Sprintf("%v", p.Args["value"]), nil
			},
		},
		"echoFloat": &Field{
			Type: String,
			Args: FieldConfigArgument{"value": &ArgumentConfig{Type: Float}},
			Resolve: func(p ResolveParams) (any, error) {
				value, ok := p.Args["value"]
				if !ok {
					return "<missing>", nil
				}
				if value == nil {
					return "<null>", nil
				}
				return fmt.Sprintf("%v", value), nil
			},
		},
		"echoBool": &Field{
			Type: String,
			Args: FieldConfigArgument{"value": &ArgumentConfig{Type: Boolean}},
			Resolve: func(p ResolveParams) (any, error) {
				value, ok := p.Args["value"]
				if !ok {
					return "<missing>", nil
				}
				if value == nil {
					return "<null>", nil
				}
				return fmt.Sprintf("%v", value), nil
			},
		},
		"echoID": &Field{
			Type: String,
			Args: FieldConfigArgument{"value": &ArgumentConfig{Type: ID}},
			Resolve: func(p ResolveParams) (any, error) {
				value, ok := p.Args["value"]
				if !ok {
					return "<missing>", nil
				}
				if value == nil {
					return "<null>", nil
				}
				return fmt.Sprintf("%T:%v", value, value), nil
			},
		},
		"odd": &Field{
			Type: types.Odd,
			Args: FieldConfigArgument{"value": &ArgumentConfig{Type: types.Odd}},
			Resolve: func(p ResolveParams) (any, error) {
				return p.Args["value"], nil
			},
		},
		"oddOut": &Field{
			Type:    types.Odd,
			Resolve: s25Const(4), // 偶数：序列化必然失败
		},
		"user":   &Field{Type: types.User, Resolve: s25Const(map[string]any{"id": "u1", "name": "Ada", "kind": "S25User"})},
		"noUser": &Field{Type: types.User, Resolve: s25Const(nil)},
		"people": &Field{Type: NewList(types.User), Resolve: func(p ResolveParams) (any, error) {
			counter.inc("people")
			return s25CorePeople(), nil
		}},
		"nodes":  &Field{Type: NewList(types.Node), Resolve: func(ResolveParams) (any, error) { return s25CoreNodes(), nil }},
		"search": &Field{Type: NewList(types.Search), Resolve: func(ResolveParams) (any, error) { return s25CoreNodes(), nil }},
		"badNode": &Field{
			Type:    NewList(types.Node),
			Resolve: s25Const([]any{map[string]any{"id": "x", "kind": "S25Unknown"}}),
		},

		// 空值与列表补全
		"nullableScalar":  &Field{Type: String, Resolve: s25Const(nil)},
		"nonNullNull":     &Field{Type: NewNonNull(String), Resolve: s25Const(nil)},
		"stringList":      &Field{Type: NewList(String), Resolve: s25Const([]any{"a", nil, "c"})},
		"nonNullItemList": &Field{Type: NewList(NewNonNull(String)), Resolve: s25Const([]any{"a", nil, "c"})},
		"emptyList":       &Field{Type: NewList(String), Resolve: s25Const([]any{})},
		"nullList":        &Field{Type: NewList(String), Resolve: s25Const(nil)},
		"nonNullList":     &Field{Type: NewNonNull(NewList(String)), Resolve: s25Const(nil)},
		"matrix":          &Field{Type: NewList(NewList(String)), Resolve: s25Const([]any{[]any{"a", "b"}, []any{"c"}})},
		"matrixWithNull":  &Field{Type: NewList(NewList(NewNonNull(String))), Resolve: s25Const([]any{[]any{"a", nil}, []any{nil, "d"}})},
		"typedList":       &Field{Type: NewList(String), Resolve: s25Const([]string{"a", "b"})},
		"arrayList":       &Field{Type: NewList(Int), Resolve: s25Const([3]int{1, 2, 3})},
		"pointerList": &Field{Type: NewList(String), Resolve: func(ResolveParams) (any, error) {
			values := []string{"a", "b"}
			return &values, nil
		}},
		"typedNilList": &Field{Type: NewList(String), Resolve: func(ResolveParams) (any, error) {
			var values *[]string
			return values, nil
		}},
		"notAList": &Field{Type: NewList(String), Resolve: s25Const("oops")},

		// 叶子序列化边界
		"bigInt":    &Field{Type: Int, Resolve: s25Const(int64(3000000000))},
		"badEnum":   &Field{Type: types.Mode, Resolve: s25Const("NOT_A_MODE")},
		"goodEnum":  &Field{Type: types.Mode, Resolve: s25Const("A")},
		"floatOut":  &Field{Type: Float, Resolve: s25Const(1.5)},
		"boolOut":   &Field{Type: Boolean, Resolve: s25Const(true)},
		"idOut":     &Field{Type: ID, Resolve: s25Const(42)},
		"stringOut": &Field{Type: String, Resolve: s25Const("")},

		// 错误
		"boom":        &Field{Type: String, Resolve: s25Fail("boom failed")},
		"boomNonNull": &Field{Type: NewNonNull(String), Resolve: s25Fail("boomNonNull failed")},
		"boomPanic": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
			panic("panic in resolver")
		}},
		"boomPanicString": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
			panic(errors.New("panic error in resolver"))
		}},
		"boomExtended": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
			return nil, s25ExtendedError{message: "extended failure", code: "S25_CODE"}
		}},

		// 顺序断言用（query 中按 z,a,m 书写）
		"z": &Field{Type: String, Resolve: s25Const("z")},
		"a": &Field{Type: String, Resolve: s25Const("a")},
		"m": &Field{Type: String, Resolve: s25Const("m")},

		// ResolveInfo / context
		"infoField": &Field{
			Type: String,
			Args: FieldConfigArgument{"tag": &ArgumentConfig{Type: String}},
			Resolve: func(p ResolveParams) (any, error) {
				return fmt.Sprintf("%s|%v", p.Info.FieldName, p.Info.Path.AsArray()), nil
			},
		},
		"ctxField": &Field{Type: String, Resolve: func(p ResolveParams) (any, error) {
			if p.Context == nil {
				return "<nil-ctx>", nil
			}
			if value := p.Context.Value(s25CtxKey{}); value != nil {
				return fmt.Sprintf("%v", value), nil
			}
			return "<no-value>", nil
		}},
	}

	schema, err := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: fields}),
		Types: []Type{types.User, types.Robot, types.Node, types.Search},
	})
	if err != nil {
		t.Fatalf("build core schema: %v", err)
	}
	types.Query = schema.QueryType()
	return schema, types
}

type s25CtxKey struct{}

// s25DescribeFilter 把 S25Filter 输入序列化成稳定字符串，用于输入协变断言。
func s25DescribeFilter(value any) string {
	filter, ok := value.(map[string]any)
	if !ok {
		return fmt.Sprintf("<not-object:%T>", value)
	}
	return fmt.Sprintf("text=%v|count=%v|mode=%v|tags=%v|nested=%v",
		s25FilterField(filter, "text"),
		s25FilterField(filter, "count"),
		s25FilterField(filter, "mode"),
		s25FilterField(filter, "tags"),
		s25FilterField(filter, "nested"),
	)
}

func s25FilterField(filter map[string]any, key string) string {
	value, ok := filter[key]
	if !ok {
		return "<missing>"
	}
	if value == nil {
		return "<null>"
	}
	return fmt.Sprintf("%v", value)
}

// ---------------------------------------------------------------------------
// Introspection schema：不含任何注入参数
// ---------------------------------------------------------------------------

func s25NewIntrospectSchema(t testing.TB) Schema {
	t.Helper()
	mode := s25NewMode()
	inner := NewInputObject(InputObjectConfig{
		Name:        "S25IntroInner",
		Description: "Nested input object.",
		Fields: InputObjectConfigFieldMap{
			"flag": &InputObjectFieldConfig{Type: Boolean, DefaultValue: false, Description: "A flag."},
		},
	})
	input := NewInputObject(InputObjectConfig{
		Name:        "S25IntroInput",
		Description: "Input object with defaults of every kind.",
		Fields: InputObjectConfigFieldMap{
			"text":   &InputObjectFieldConfig{Type: NewNonNull(String), Description: "Required text."},
			"count":  &InputObjectFieldConfig{Type: Int, DefaultValue: 7},
			"mode":   &InputObjectFieldConfig{Type: mode, DefaultValue: "A"},
			"tags":   &InputObjectFieldConfig{Type: NewList(String), DefaultValue: []any{"x", "y"}},
			"nested": &InputObjectFieldConfig{Type: inner, DefaultValue: map[string]any{"flag": true}},
		},
	})

	named := NewInterface(InterfaceConfig{
		Name:        "S25IntroNamed",
		Description: "Anything with a name.",
		Fields:      Fields{"name": &Field{Type: String, Description: "The name."}},
	})
	entity := NewInterface(InterfaceConfig{
		Name:        "S25IntroEntity",
		Description: "An entity implementing another interface.",
		Fields: Fields{
			"id":   &Field{Type: NewNonNull(ID)},
			"name": &Field{Type: String, Description: "The name."},
		},
	})

	item := NewObject(ObjectConfig{
		Name:        "S25IntroItem",
		Description: "An introspectable item.",
		Interfaces:  []*Interface{named, entity},
		Fields: Fields{
			"id":   &Field{Type: NewNonNull(ID)},
			"name": &Field{Type: String, Description: "The name."},
			"wrapped": &Field{
				Type:        NewNonNull(NewList(NewNonNull(String))),
				Description: "A deeply wrapped type.",
			},
			"withArgs": &Field{
				Type: String,
				// sgraph 要求"带参数的字段必须有 resolver"（plan_coordinator.go 的
				// "field %d has param plan but no resolver"），这里按该约束补齐。
				Resolve: func(p ResolveParams) (any, error) {
					return fmt.Sprintf("first=%v", p.Args["first"]), nil
				},
				Args: FieldConfigArgument{
					"first":  &ArgumentConfig{Type: Int, DefaultValue: 10, Description: "How many."},
					"filter": &ArgumentConfig{Type: input},
					"mode":   &ArgumentConfig{Type: mode, DefaultValue: "A"},
					"tags":   &ArgumentConfig{Type: NewList(String), DefaultValue: []any{"x"}},
				},
			},
			"legacy": &Field{
				Type:              String,
				DeprecationReason: "use name",
				Description:       "Deprecated field.",
			},
		},
	})

	other := NewObject(ObjectConfig{
		Name:       "S25IntroOther",
		Interfaces: []*Interface{named},
		Fields:     Fields{"name": &Field{Type: String, Description: "The name."}},
	})
	union := NewUnion(UnionConfig{
		Name:        "S25IntroUnion",
		Description: "A union of two objects.",
		Types:       []*Object{item, other},
		ResolveType: func(ResolveTypeParams) *Object { return item },
	})
	named.ResolveType = func(ResolveTypeParams) *Object { return item }
	entity.ResolveType = func(ResolveTypeParams) *Object { return item }

	custom := NewDirective(DirectiveConfig{
		Name:        "s25Tag",
		Description: "A custom directive covering executable locations.",
		Locations: []string{
			DirectiveLocationQuery,
			DirectiveLocationMutation,
			DirectiveLocationField,
			DirectiveLocationFragmentDefinition,
			DirectiveLocationFragmentSpread,
			DirectiveLocationInlineFragment,
			s25VariableDefinitionLocation,
		},
		Args: FieldConfigArgument{
			"label": &ArgumentConfig{Type: String, DefaultValue: "none", Description: "Tag label."},
		},
	})

	schema, err := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{
			Name:        "Query",
			Description: "The introspection corpus root.",
			Fields: Fields{
				"item":     &Field{Type: item, Resolve: s25Const(map[string]any{"id": "i1", "name": "Item"})},
				"items":    &Field{Type: NewList(item), Resolve: s25Const([]any{map[string]any{"id": "i1", "name": "Item"}})},
				"union":    &Field{Type: union, Resolve: s25Const(map[string]any{"id": "i1", "name": "Item"})},
				"named":    &Field{Type: named, Resolve: s25Const(map[string]any{"id": "i1", "name": "Item"})},
				"mode":     &Field{Type: mode, Resolve: s25Const("A")},
				"deprecat": &Field{Type: String, DeprecationReason: "gone", Resolve: s25Const("x")},
			},
		}),
		Types:      []Type{item, other, union, named, entity, input, inner, mode},
		Directives: append(append([]*Directive{}, specifiedRules25Directives()...), custom),
	})
	if err != nil {
		t.Fatalf("build introspection schema: %v", err)
	}
	return schema
}

// s25VariableDefinitionLocation 是 September 2025 的 VARIABLE_DEFINITION 指令位置。
// 当前框架 directives.go 中没有对应常量，这里按规范字面量声明。
const s25VariableDefinitionLocation = "VARIABLE_DEFINITION"

// specifiedRules25Directives 返回框架内置指令集合，保证自定义指令不会覆盖它们。
func specifiedRules25Directives() []*Directive {
	return []*Directive{IncludeDirective, SkipDirective, DeprecatedDirective}
}

// ---------------------------------------------------------------------------
// Mutation schema
// ---------------------------------------------------------------------------

// s25NewMutationSchema 返回 schema 和一个记录执行顺序的指针。
func s25NewMutationSchema(t testing.TB, order *[]string, mu *sync.Mutex) Schema {
	t.Helper()
	opaque := s25NewOpaqueScalar()
	record := func(name string) {
		mu.Lock()
		*order = append(*order, name)
		mu.Unlock()
	}
	child := NewObject(ObjectConfig{
		Name: "S25MutationChild",
		Fields: Fields{
			"id": &Field{Type: String},
			"slow": &Field{
				Type: String,
				Args: s25ParentArg(opaque),
				Resolve: func(p ResolveParams) (any, error) {
					id, _ := s25ParentString(p, "id")
					time.Sleep(20 * time.Millisecond)
					record("child:" + id)
					return "child:" + id, nil
				},
			},
		},
	})
	mutationField := func(name string, fail bool) *Field {
		return &Field{
			Type: child,
			Resolve: func(ResolveParams) (any, error) {
				record(name)
				if fail {
					return nil, errors.New(name + " failed")
				}
				return map[string]any{"id": name}, nil
			},
		}
	}
	schema, err := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
			"ping": &Field{Type: String, Resolve: s25Const("pong")},
		}}),
		Mutation: NewObject(ObjectConfig{Name: "Mutation", Fields: Fields{
			"first":  mutationField("first", false),
			"second": mutationField("second", false),
			"third":  mutationField("third", false),
			"failing": &Field{Type: String, Resolve: func(ResolveParams) (any, error) {
				record("failing")
				return nil, errors.New("failing failed")
			}},
		}}),
	})
	if err != nil {
		t.Fatalf("build mutation schema: %v", err)
	}
	return schema
}

// ---------------------------------------------------------------------------
// Subscription schema
// ---------------------------------------------------------------------------

func s25NewSubscriptionSchema(t testing.TB) Schema {
	t.Helper()
	schema, err := NewSchema(SchemaConfig{
		Query: NewObject(ObjectConfig{Name: "Query", Fields: Fields{
			"ping": &Field{Type: String, Resolve: s25Const("pong")},
		}}),
		Subscription: NewObject(ObjectConfig{Name: "Subscription", Fields: Fields{
			"ticks": &Field{
				Type: String,
				Subscribe: func(p ResolveParams) (any, error) {
					stream := make(chan any, 3)
					for index := 0; index < 3; index++ {
						stream <- fmt.Sprintf("tick-%d", index)
					}
					close(stream)
					return stream, nil
				},
				Resolve: func(p ResolveParams) (any, error) {
					return fmt.Sprintf("%v", p.Source), nil
				},
			},
			"other": &Field{
				Type:      String,
				Subscribe: func(ResolveParams) (any, error) { return nil, nil },
				Resolve:   s25Const("other"),
			},
		}}),
	})
	if err != nil {
		t.Fatalf("build subscription schema: %v", err)
	}
	return schema
}

// ---------------------------------------------------------------------------
// 默认 resolver schema（非根字段的四个分支）
// ---------------------------------------------------------------------------

type s25TaggedProfile struct {
	DisplayName string `graphql:"name"`
	Age         int    `json:"age"`
}

type s25FieldResolverSource struct{}

func (s25FieldResolverSource) Resolve(p ResolveParams) (any, error) {
	return "resolved:" + p.Info.FieldName, nil
}

func s25NewDefaultResolverSchema(t testing.TB) Schema {
	t.Helper()
	mapChild := NewObject(ObjectConfig{Name: "S25MapChild", Fields: Fields{
		"plain": &Field{Type: String},
		"thunk": &Field{Type: String},
	}})
	structChild := NewObject(ObjectConfig{Name: "S25StructChild", Fields: Fields{
		"name": &Field{Type: String},
		"age":  &Field{Type: Int},
	}})
	resolverChild := NewObject(ObjectConfig{Name: "S25ResolverChild", Fields: Fields{
		"value": &Field{Type: String},
	}})
	return s25NewSchema(t, Fields{
		"fromMap": &Field{Type: mapChild, Resolve: func(ResolveParams) (any, error) {
			return map[string]any{
				"plain": "plainValue",
				"thunk": func() any { return "thunkValue" },
			}, nil
		}},
		"fromStruct": &Field{Type: structChild, Resolve: func(ResolveParams) (any, error) {
			return &s25TaggedProfile{DisplayName: "Ada", Age: 36}, nil
		}},
		"fromResolver": &Field{Type: resolverChild, Resolve: func(ResolveParams) (any, error) {
			return s25FieldResolverSource{}, nil
		}},
	})
}

// ---------------------------------------------------------------------------
// 深度 / 广度 / 极限 schema
// ---------------------------------------------------------------------------

// s25NewDeepSchema 构造自引用类型，用于选择集深度边界。
func s25NewDeepSchema(t testing.TB) Schema {
	t.Helper()
	var node *Object
	node = NewObject(ObjectConfig{
		Name: "S25DeepNode",
		Fields: FieldsThunk(func() Fields {
			return Fields{
				"depth": &Field{Type: Int, Resolve: s25Const(1)},
				"next": &Field{Type: node, Resolve: func(ResolveParams) (any, error) {
					return map[string]any{"depth": 1}, nil
				}},
			}
		}),
	})
	return s25NewSchema(t, Fields{
		"root": &Field{Type: node, Resolve: func(ResolveParams) (any, error) {
			return map[string]any{"depth": 0}, nil
		}},
	}, node)
}

// s25NewWideSchema 构造 n 个标量根字段，用于广度边界。
func s25NewWideSchema(t testing.TB, n int) Schema {
	t.Helper()
	fields := Fields{}
	for index := 0; index < n; index++ {
		value := fmt.Sprintf("v%d", index)
		fields[fmt.Sprintf("f%d", index)] = &Field{Type: String, Resolve: s25Const(value)}
	}
	return s25NewSchema(t, fields)
}

// s25NewBigListSchema 构造一个返回 n 个元素的列表字段。
func s25NewBigListSchema(t testing.TB, n int) Schema {
	t.Helper()
	values := make([]any, 0, n)
	for index := 0; index < n; index++ {
		values = append(values, fmt.Sprintf("item-%d", index))
	}
	return s25NewSchema(t, Fields{
		"items": &Field{Type: NewList(String), Resolve: s25Const(values)},
	})
}
