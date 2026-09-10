package graphql

// spec2025_validation_test.go
//
// GraphQL September 2025 规范 §5 Validation 一致性用例。
// https://spec.graphql.org/September2025/#sec-Validation
//
// 每条命名规则至少一个 VALID 用例（必须通过校验）和一个 INVALID 用例（必须被拒绝）。
// 全部期望直接来自规范文本，不来自当前实现行为。

import (
	"testing"
)

// ---------------------------------------------------------------------------
// 本文件私有的辅助 schema
// ---------------------------------------------------------------------------

// s25valNewPetSchema 提供两个互斥的对象类型，且它们上有同名、带参数的字段。
// 用于 §5.3.2 “互斥父类型下同名字段可以携带不同参数”这一 VALID 分支，
// core schema 里没有这种形状。
func s25valNewPetSchema(t testing.TB) Schema {
	t.Helper()
	pet := NewInterface(InterfaceConfig{
		Name:   "S25valPet",
		Fields: Fields{"name": &Field{Type: String}},
	})
	volumeArgs := func() FieldConfigArgument {
		return FieldConfigArgument{"scale": &ArgumentConfig{Type: Int}}
	}
	dog := NewObject(ObjectConfig{
		Name:       "S25valDog",
		Interfaces: []*Interface{pet},
		Fields: Fields{
			"name":       &Field{Type: String},
			"volume":     &Field{Type: Int, Args: volumeArgs()},
			"barkVolume": &Field{Type: Int, Args: volumeArgs()},
		},
	})
	cat := NewObject(ObjectConfig{
		Name:       "S25valCat",
		Interfaces: []*Interface{pet},
		Fields: Fields{
			"name":       &Field{Type: String},
			"volume":     &Field{Type: Int, Args: volumeArgs()},
			"meowVolume": &Field{Type: Int, Args: volumeArgs()},
		},
	})
	pet.ResolveType = func(ResolveTypeParams) *Object { return dog }

	return s25NewSchema(t, Fields{
		"pet": &Field{Type: pet, Resolve: s25Const(map[string]any{"name": "Rex"})},
	}, dog, cat, pet)
}

// s25valNewDisjointAbstractSchema 提供两个可能类型集合完全不相交的抽象类型，
// 用于 §5.5.2.3.4 的 INVALID 分支（core schema 的 S25Node / S25Search 完全重叠）。
func s25valNewDisjointAbstractSchema(t testing.TB) Schema {
	t.Helper()
	alpha := NewInterface(InterfaceConfig{
		Name:   "S25valAlpha",
		Fields: Fields{"a": &Field{Type: String}},
	})
	beta := NewInterface(InterfaceConfig{
		Name:   "S25valBeta",
		Fields: Fields{"b": &Field{Type: String}},
	})
	alphaOnly := NewObject(ObjectConfig{
		Name:       "S25valAlphaOnly",
		Interfaces: []*Interface{alpha},
		Fields:     Fields{"a": &Field{Type: String}},
	})
	betaOnly := NewObject(ObjectConfig{
		Name:       "S25valBetaOnly",
		Interfaces: []*Interface{beta},
		Fields:     Fields{"b": &Field{Type: String}},
	})
	alpha.ResolveType = func(ResolveTypeParams) *Object { return alphaOnly }
	beta.ResolveType = func(ResolveTypeParams) *Object { return betaOnly }

	return s25NewSchema(t, Fields{
		"alpha": &Field{Type: alpha, Resolve: s25Const(map[string]any{"a": "a"})},
		"beta":  &Field{Type: beta, Resolve: s25Const(map[string]any{"b": "b"})},
	}, alpha, beta, alphaOnly, betaOnly)
}

// ---------------------------------------------------------------------------
// §5.1 Documents
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_Documents(t *testing.T) {
	schema, _ := s25NewCoreSchema(t, nil)

	// §5.1.1 Executable Definitions:
	// "For each definition in the document, definition must be
	//  ExecutableDefinition (it must not be a TypeSystemDefinitionOrExtension)."
	t.Run("5.1.1_ExecutableDefinitions", func(t *testing.T) {
		s25RequireValid(t, schema, `
			query Q {
				greeting
				user { ...UserFields }
			}
			fragment UserFields on S25User { name }
		`)

		// 文档里混入类型系统定义：必须被拒绝。
		s25RequireInvalid(t, schema, `
			query Q { greeting }
			type Foo { a: Int }
		`)

		// 单独一个类型系统定义同样不是可执行文档。
		s25RequireInvalid(t, schema, `
			type Foo { a: Int }
		`)

		// schema 定义 / 扩展也属于 TypeSystemDefinitionOrExtension。
		s25RequireInvalid(t, schema, `
			query Q { greeting }
			extend type Foo { b: Int }
		`)
	})
}

// ---------------------------------------------------------------------------
// §5.2 Operations
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_Operations(t *testing.T) {
	schema, _ := s25NewCoreSchema(t, nil)
	subscriptionSchema := s25NewSubscriptionSchema(t)

	// §5.2.1.1 Operation Type Existence:
	// "For each operation definition operation in the document, the root
	//  operation type for operation must exist in the schema."
	t.Run("5.2.1.1_OperationTypeExistence", func(t *testing.T) {
		// core schema 只有 Query 根类型。
		s25RequireValid(t, schema, `query Q { greeting }`)
		s25RequireValid(t, schema, `{ greeting }`)
		// subscription schema 同时有 Query 和 Subscription 根类型。
		s25RequireValid(t, subscriptionSchema, `subscription S { ticks }`)

		// core schema 没有 Mutation 根类型。
		s25RequireInvalid(t, schema, `mutation M { greeting }`)
		// core schema 没有 Subscription 根类型。
		s25RequireInvalid(t, schema, `subscription S { greeting }`)
		// subscription schema 没有 Mutation 根类型。
		s25RequireInvalid(t, subscriptionSchema, `mutation M { ping }`)
	})

	// §5.2.2.1 Operation Name Uniqueness:
	// "For each operation definition with a name in the document, that name
	//  must be unique."
	t.Run("5.2.2.1_OperationNameUniqueness", func(t *testing.T) {
		s25RequireValid(t, schema, `
			query A { greeting }
			query B { greeting }
		`)
		s25RequireInvalid(t, schema, `
			query A { greeting }
			query A { greeting }
		`)
		// 操作类型不同也不解除重名冲突。
		s25RequireInvalid(t, subscriptionSchema, `
			query Same { ping }
			subscription Same { ticks }
		`)
	})

	// §5.2.3.1 Lone Anonymous Operation:
	// "Let operations be all operation definitions in the document.
	//  Let anonymous be all anonymous operation definitions in the document.
	//  If operations is a set of more than 1, anonymous must be empty."
	t.Run("5.2.3.1_LoneAnonymousOperation", func(t *testing.T) {
		s25RequireValid(t, schema, `{ greeting }`)
		s25RequireValid(t, schema, `
			query A { greeting }
			query B { greeting }
		`)

		s25RequireInvalid(t, schema, `
			{ greeting }
			query Named { greeting }
		`)
		s25RequireInvalid(t, schema, `
			{ greeting }
			{ user { name } }
		`)
	})

	// §5.2.4.1 Single Root Field:
	// "Let subscriptionType be the root Subscription type in schema.
	//  Let selectionSet be the top level selection set on subscription.
	//  Let groupedFieldSet be the result of CollectFields(...).
	//  groupedFieldSet must have exactly one entry, which must not be an
	//  introspection field."
	t.Run("5.2.4.1_SingleRootField", func(t *testing.T) {
		s25RequireValid(t, subscriptionSchema, `subscription S { ticks }`)
		s25RequireValid(t, subscriptionSchema, `
			subscription S { ...OnlyOne }
			fragment OnlyOne on Subscription { ticks }
		`)

		// 两个不同的根字段。
		s25RequireInvalid(t, subscriptionSchema, `subscription S { ticks other }`)
		// 同一个根字段的两个别名：groupedFieldSet 有两个条目。
		s25RequireInvalid(t, subscriptionSchema, `subscription S { a: ticks b: ticks }`)
		// 唯一根字段是内省字段。
		s25RequireInvalid(t, subscriptionSchema, `subscription S { __typename }`)
		// 通过片段引入的第二个根字段同样违规。
		s25RequireInvalid(t, subscriptionSchema, `
			subscription S { ticks ...Extra }
			fragment Extra on Subscription { other }
		`)
	})
}

// ---------------------------------------------------------------------------
// §5.3.1 / §5.3.3 Fields
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_Fields(t *testing.T) {
	schema, _ := s25NewCoreSchema(t, nil)

	// §5.3.1 Field Selections:
	// "The target field of a field selection must be defined on the scoped
	//  type of the selection set."
	t.Run("5.3.1_FieldSelections", func(t *testing.T) {
		s25RequireValid(t, schema, `{ greeting }`)
		s25RequireValid(t, schema, `{ user { id name } }`)
		// 内省元字段在对象作用域内始终可用。
		s25RequireValid(t, schema, `{ user { __typename } }`)
		// 接口上定义的字段。
		s25RequireValid(t, schema, `{ nodes { id } }`)

		s25RequireInvalid(t, schema, `{ notAFieldOnQuery }`)
		s25RequireInvalid(t, schema, `{ user { notAFieldOnUser } }`)
		// serial 只在 S25Robot 上，不在接口 S25Node 上。
		s25RequireInvalid(t, schema, `{ nodes { serial } }`)
		// union 上除内省元字段外没有任何字段。
		s25RequireInvalid(t, schema, `{ search { id } }`)
	})

	// §5.3.3 Leaf Field Selections:
	// "For each selection in the document: let selectionType be the result
	//  type of selection. If selectionType is a scalar or enum, the
	//  subselection set of that selection must be empty. If selectionType is
	//  an interface, union, or object, the subselection set must NOT be empty."
	t.Run("5.3.3_LeafFieldSelections", func(t *testing.T) {
		s25RequireValid(t, schema, `{ greeting }`)
		s25RequireValid(t, schema, `{ echoMode }`)
		s25RequireValid(t, schema, `{ user { name } }`)
		s25RequireValid(t, schema, `{ nodes { id } }`)

		// 标量上带子选择集。
		s25RequireInvalid(t, schema, `{ greeting { somethingElse } }`)
		// 枚举上带子选择集。
		s25RequireInvalid(t, schema, `{ echoMode { somethingElse } }`)
		// 对象类型缺少子选择集。
		s25RequireInvalid(t, schema, `{ user }`)
		// 接口类型缺少子选择集。
		s25RequireInvalid(t, schema, `{ nodes }`)
		// union 类型缺少子选择集。
		s25RequireInvalid(t, schema, `{ search }`)
	})
}

// ---------------------------------------------------------------------------
// §5.3.2 Field Selection Merging
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_FieldSelectionMerging(t *testing.T) {
	schema, _ := s25NewCoreSchema(t, nil)
	petSchema := s25valNewPetSchema(t)

	// §5.3.2 Field Selection Merging:
	// "If multiple field selections with the same response names are
	//  encountered during execution, the field and arguments to execute and
	//  the resulting value should be unambiguous."
	t.Run("5.3.2_FieldSelectionMerging", func(t *testing.T) {
		// --- VALID ---

		// 完全相同的字段。
		s25RequireValid(t, schema, `{ user { name name } }`)
		s25RequireValid(t, schema, `
			{ user { ...A ...B } }
			fragment A on S25User { name }
			fragment B on S25User { name }
		`)

		// 相同字段 + 相同参数。
		s25RequireValid(t, schema, `{ echo(text: "a", suffix: "b") echo(text: "a", suffix: "b") }`)
		// 参数书写顺序不同但参数集合相同：仍然可合并。
		s25RequireValid(t, schema, `{ echo(text: "a", suffix: "b") echo(suffix: "b", text: "a") }`)

		// 互斥的对象类型片段里，同一个响应名指向不同字段：规范明确判定为 VALID。
		s25RequireValid(t, schema, `
			{
				nodes {
					... on S25User { x: name }
					... on S25Robot { x: serial }
				}
			}
		`)
		s25RequireValid(t, schema, `
			{ search { ...UserPart ...RobotPart } }
			fragment UserPart on S25User { handle: name }
			fragment RobotPart on S25Robot { handle: serial }
		`)

		// 互斥的对象类型片段里，同名字段携带不同参数：父类型不同，
		// 只需要满足 SameResponseShape，因此 VALID。
		s25RequireValid(t, petSchema, `
			{
				pet {
					... on S25valDog { volume(scale: 10) }
					... on S25valCat { volume(scale: 20) }
				}
			}
		`)
		s25RequireValid(t, petSchema, `
			{
				pet {
					... on S25valDog { v: barkVolume(scale: 10) }
					... on S25valCat { v: meowVolume(scale: 20) }
				}
			}
		`)

		// --- INVALID ---

		// 同一响应名、同一字段、不同参数。
		s25RequireInvalid(t, schema, `{ echo(text: "a") echo(text: "b") }`)
		s25RequireInvalid(t, schema, `{ echo(text: "a", suffix: "x") echo(text: "a", suffix: "y") }`)
		// 一个显式给出参数，一个依赖默认值：参数集合仍然不同。
		s25RequireInvalid(t, schema, `{ echo(text: "a") echo(text: "a", suffix: "!") }`)

		// 同一作用域下别名指向两个不同的字段。
		s25RequireInvalid(t, schema, `{ x: greeting x: echo(text: "a") }`)
		s25RequireInvalid(t, schema, `
			{ user { ...A ...B } }
			fragment A on S25User { y: name }
			fragment B on S25User { y: legacy }
		`)

		// 嵌套选择集深处的类型冲突：name 是 String，id 是 ID!。
		s25RequireInvalid(t, schema, `
			{
				p: user { z: name }
				p: user { z: id }
			}
		`)
		s25RequireInvalid(t, schema, `
			{ user { ...Outer } }
			fragment Outer on S25User { ...Deep1 ...Deep2 }
			fragment Deep1 on S25User { deep: name }
			fragment Deep2 on S25User { deep: id }
		`)
		// 互斥片段也无法解除 SameResponseShape 冲突。
		s25RequireInvalid(t, schema, `
			{
				nodes {
					... on S25User { shape: id }
					... on S25Robot { shape: serial }
				}
			}
		`)
	})
}

// ---------------------------------------------------------------------------
// §5.4 Arguments
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_Arguments(t *testing.T) {
	schema, _ := s25NewCoreSchema(t, nil)

	// §5.4.1 Argument Names:
	// "Every argument provided to a field or directive must be defined in the
	//  set of possible arguments of that field or directive."
	t.Run("5.4.1_ArgumentNames", func(t *testing.T) {
		s25RequireValid(t, schema, `{ echo(text: "a", suffix: "b") }`)
		s25RequireValid(t, schema, `{ greeting @skip(if: false) }`)

		s25RequireInvalid(t, schema, `{ echo(text: "a", notAnArgument: 1) }`)
		s25RequireInvalid(t, schema, `{ greeting @skip(if: false, notAnArgument: 1) }`)
	})

	// §5.4.2 Argument Uniqueness:
	// "Fields and directives treat arguments as a mapping of argument name to
	//  value. More than one argument with the same name in an argument set is
	//  ambiguous and invalid."
	t.Run("5.4.2_ArgumentUniqueness", func(t *testing.T) {
		s25RequireValid(t, schema, `{ echo(text: "a", suffix: "b") }`)
		s25RequireValid(t, schema, `{ greeting @skip(if: false) @include(if: true) }`)

		s25RequireInvalid(t, schema, `{ echo(text: "a", text: "b") }`)
		s25RequireInvalid(t, schema, `{ echo(text: "a", suffix: "b", suffix: "c") }`)
		s25RequireInvalid(t, schema, `{ greeting @skip(if: false, if: true) }`)
	})

	// §5.4.3 Required Arguments:
	// "For each argumentDefinition of the field/directive: let type be the
	//  expected type; if type is Non-Null and defaultValue does not exist,
	//  an argument must be provided and its value must not be the null literal."
	t.Run("5.4.3_RequiredArguments", func(t *testing.T) {
		s25RequireValid(t, schema, `{ echo(text: "a") }`)
		s25RequireValid(t, schema, `{ echo(text: "a", suffix: "b") }`)
		// 可空参数可以省略。
		s25RequireValid(t, schema, `{ echoNoDefault }`)
		s25RequireValid(t, schema, `{ echoFilter(filter: {text: "t"}) }`)

		// 缺少非空参数。
		s25RequireInvalid(t, schema, `{ echo(suffix: "b") }`)
		s25RequireInvalid(t, schema, `{ echo }`)
		s25RequireInvalid(t, schema, `{ echoFilter }`)
		// 非空参数被显式赋 null。
		s25RequireInvalid(t, schema, `{ echo(text: null) }`)
		s25RequireInvalid(t, schema, `{ echoFilter(filter: null) }`)
		// 指令的必填参数。
		s25RequireInvalid(t, schema, `{ greeting @skip }`)
		s25RequireInvalid(t, schema, `{ greeting @skip(if: null) }`)

		// 可空参数也可以显式传 null 字面量（§2.9.6 Null Value）。
		s25RequireValid(t, schema, `{ echoNoDefault(value: null) }`)
	})
}

// ---------------------------------------------------------------------------
// §5.5.1 Fragment Declarations
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_FragmentDeclarations(t *testing.T) {
	schema, _ := s25NewCoreSchema(t, nil)

	// §5.5.1.1 Fragment Name Uniqueness:
	// "For each fragment definition fragment in the document, fragment's name
	//  must be unique throughout the document."
	t.Run("5.5.1.1_FragmentNameUniqueness", func(t *testing.T) {
		s25RequireValid(t, schema, `
			{ user { ...A ...B } }
			fragment A on S25User { name }
			fragment B on S25User { id }
		`)
		s25RequireInvalid(t, schema, `
			{ user { ...A } }
			fragment A on S25User { name }
			fragment A on S25User { id }
		`)
	})

	// §5.5.1.2 Fragment Spread Type Existence:
	// "For each named spread namedSpread in the document, the target type of
	//  namedSpread must be defined in the schema."
	t.Run("5.5.1.2_FragmentSpreadTypeExistence", func(t *testing.T) {
		s25RequireValid(t, schema, `
			{ user { ...A } }
			fragment A on S25User { name }
		`)
		s25RequireValid(t, schema, `{ nodes { ... on S25Robot { serial } } }`)

		// 具名片段的目标类型不存在。
		s25RequireInvalid(t, schema, `
			{ user { ...A } }
			fragment A on S25NotDefined { name }
		`)
		// 内联片段的目标类型不存在。
		s25RequireInvalid(t, schema, `{ user { ... on S25NotDefined { name } } }`)
	})

	// §5.5.1.3 Fragments on Composite Types:
	// "Fragments may only be declared on unions, interfaces, and objects."
	t.Run("5.5.1.3_FragmentsOnCompositeTypes", func(t *testing.T) {
		// 对象类型。
		s25RequireValid(t, schema, `
			{ user { ...OnObject } }
			fragment OnObject on S25User { name }
		`)
		// 接口类型。
		s25RequireValid(t, schema, `
			{ nodes { ...OnInterface } }
			fragment OnInterface on S25Node { id }
		`)
		// union 类型。
		s25RequireValid(t, schema, `
			{ search { ...OnUnion } }
			fragment OnUnion on S25Search { __typename }
		`)
		// 内联片段同理。
		s25RequireValid(t, schema, `{ nodes { ... on S25User { name } } }`)

		// 标量上的片段。
		s25RequireInvalid(t, schema, `
			{ user { ...OnScalar } }
			fragment OnScalar on String { name }
		`)
		// 枚举上的片段。
		s25RequireInvalid(t, schema, `
			{ user { ...OnEnum } }
			fragment OnEnum on S25Mode { name }
		`)
		// 输入对象上的片段。
		s25RequireInvalid(t, schema, `
			{ user { ...OnInput } }
			fragment OnInput on S25Filter { text }
		`)
		// 内联片段落在标量上。
		s25RequireInvalid(t, schema, `{ user { ... on Boolean { name } } }`)
	})

	// §5.5.1.4 Fragments Must Be Used:
	// "For each fragment defined in the document, that fragment must be spread
	//  within an operation, either directly or transitively."
	t.Run("5.5.1.4_FragmentsMustBeUsed", func(t *testing.T) {
		s25RequireValid(t, schema, `
			{ user { ...Direct } }
			fragment Direct on S25User { name }
		`)
		// 传递使用也算使用。
		s25RequireValid(t, schema, `
			{ user { ...Outer } }
			fragment Outer on S25User { ...Inner }
			fragment Inner on S25User { name }
		`)

		s25RequireInvalid(t, schema, `
			{ greeting }
			fragment Unused on S25User { name }
		`)
		// 只被另一个同样未使用的片段引用，仍然是未使用。
		s25RequireInvalid(t, schema, `
			{ greeting }
			fragment UnusedOuter on S25User { ...UnusedInner }
			fragment UnusedInner on S25User { name }
		`)
	})
}

// ---------------------------------------------------------------------------
// §5.5.2 Fragment Spreads
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_FragmentSpreads(t *testing.T) {
	schema, _ := s25NewCoreSchema(t, nil)
	disjoint := s25valNewDisjointAbstractSchema(t)

	// §5.5.2.1 Fragment Spread Target Defined:
	// "For every namedSpread in the document, the target of namedSpread must
	//  be defined in the document by a fragment definition of the same name."
	t.Run("5.5.2.1_FragmentSpreadTargetDefined", func(t *testing.T) {
		s25RequireValid(t, schema, `
			{ user { ...Defined } }
			fragment Defined on S25User { name }
		`)
		s25RequireInvalid(t, schema, `{ user { ...NeverDefined } }`)
		s25RequireInvalid(t, schema, `
			{ user { ...Defined } }
			fragment Defined on S25User { ...AlsoNeverDefined }
		`)
	})

	// §5.5.2.3.1 Object Spreads In Object Scope:
	// "Let fragmentType be the type condition and parentType be the scoped
	//  type; fragmentType and parentType must be the same type."
	t.Run("5.5.2.3.1_ObjectSpreadsInObjectScope", func(t *testing.T) {
		// possible：同一个对象类型。
		s25RequireValid(t, schema, `{ user { ... on S25User { name } } }`)
		s25RequireValid(t, schema, `
			{ user { ...SameObject } }
			fragment SameObject on S25User { name }
		`)

		// impossible：不同的对象类型。
		s25RequireInvalid(t, schema, `{ user { ... on S25Robot { serial } } }`)
		s25RequireInvalid(t, schema, `
			{ user { ...OtherObject } }
			fragment OtherObject on S25Robot { serial }
		`)
	})

	// §5.5.2.3.2 Abstract Spreads in Object Scope:
	// "Let fragmentType be the (abstract) type condition and parentType the
	//  scoped object type; parentType must be in the set of possible types of
	//  fragmentType."
	t.Run("5.5.2.3.2_AbstractSpreadsInObjectScope", func(t *testing.T) {
		// possible：S25User 实现了 S25Node。
		s25RequireValid(t, schema, `{ user { ... on S25Node { id } } }`)
		// possible：S25User 是 S25Search 的成员。
		s25RequireValid(t, schema, `{ user { ... on S25Search { __typename } } }`)
		s25RequireValid(t, schema, `
			{ user { ...AbstractOnUser } }
			fragment AbstractOnUser on S25Node { id }
		`)

		// impossible：Query 既不实现 S25Node 也不是 S25Search 的成员。
		s25RequireInvalid(t, schema, `{ ... on S25Node { id } }`)
		s25RequireInvalid(t, schema, `{ ... on S25Search { __typename } }`)
		s25RequireInvalid(t, schema, `
			{ ...AbstractOnQuery }
			fragment AbstractOnQuery on S25Node { id }
		`)
	})

	// §5.5.2.3.3 Object Spreads In Abstract Scope:
	// "Let fragmentType be the object type condition and parentType the
	//  scoped abstract type; fragmentType must be in the set of possible types
	//  of parentType."
	t.Run("5.5.2.3.3_ObjectSpreadsInAbstractScope", func(t *testing.T) {
		// possible：S25User 实现 S25Node。
		s25RequireValid(t, schema, `{ nodes { ... on S25User { name } } }`)
		// possible：S25Robot 是 S25Search 的成员。
		s25RequireValid(t, schema, `{ search { ... on S25Robot { serial } } }`)
		s25RequireValid(t, schema, `
			{ nodes { ...ObjectInAbstract } }
			fragment ObjectInAbstract on S25Robot { serial }
		`)

		// impossible：Query 不在 S25Node / S25Search 的可能类型集合中。
		s25RequireInvalid(t, schema, `{ nodes { ... on Query { greeting } } }`)
		s25RequireInvalid(t, schema, `{ search { ... on Query { greeting } } }`)
		s25RequireInvalid(t, schema, `
			{ nodes { ...QueryInAbstract } }
			fragment QueryInAbstract on Query { greeting }
		`)
	})

	// §5.5.2.3.4 Abstract Spreads in Abstract Scope:
	// "Let fragmentType and parentType both be abstract; the intersection of
	//  their possible types must not be empty."
	t.Run("5.5.2.3.4_AbstractSpreadsInAbstractScope", func(t *testing.T) {
		// possible：S25Node 与 S25Search 的可能类型集合都是 {S25User, S25Robot}。
		s25RequireValid(t, schema, `{ nodes { ... on S25Search { __typename } } }`)
		s25RequireValid(t, schema, `{ search { ... on S25Node { id } } }`)
		s25RequireValid(t, schema, `
			{ nodes { ...UnionInInterface } }
			fragment UnionInInterface on S25Search { __typename }
		`)
		// possible：抽象类型落在自身上。
		s25RequireValid(t, disjoint, `{ alpha { ... on S25valAlpha { a } } }`)

		// impossible：S25valAlpha 与 S25valBeta 的可能类型集合不相交。
		s25RequireInvalid(t, disjoint, `{ alpha { ... on S25valBeta { b } } }`)
		s25RequireInvalid(t, disjoint, `{ beta { ... on S25valAlpha { a } } }`)
		s25RequireInvalid(t, disjoint, `
			{ alpha { ...BetaFragment } }
			fragment BetaFragment on S25valBeta { b }
		`)
	})
}

// ---------------------------------------------------------------------------
// §5.5.2.2 Fragment Spreads Must Not Form Cycles
//
// 环形片段可能让实现走进无限递归并让整个进程栈溢出，因此在子进程中执行。
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_FragmentCyclesAreRejected(t *testing.T) {
	if !s25Isolated(t, "fragment-cycle") {
		return
	}
	schema, _ := s25NewCoreSchema(t, nil)

	// §5.5.2.2 Fragment Spreads Must Not Form Cycles:
	// "The graph of fragment spreads must not form any cycles including
	//  spreading itself. Otherwise an operation could infinitely spread or
	//  infinitely execute on cycles in the underlying data."
	t.Run("5.5.2.2_FragmentSpreadsMustNotFormCycles", func(t *testing.T) {
		// VALID：无环的片段链。
		s25RequireValid(t, schema, `
			{ user { ...A } }
			fragment A on S25User { ...B }
			fragment B on S25User { ...C }
			fragment C on S25User { name }
		`)
		// VALID：同一个片段被多处引用但不构成环。
		s25RequireValid(t, schema, `
			{ user { ...A ...B } }
			fragment A on S25User { ...Leaf }
			fragment B on S25User { ...Leaf }
			fragment Leaf on S25User { name }
		`)

		// INVALID：自引用。
		s25RequireInvalid(t, schema, `
			{ user { ...Self } }
			fragment Self on S25User { name ...Self }
		`)
		// INVALID：2 元环。
		s25RequireInvalid(t, schema, `
			{ user { ...A } }
			fragment A on S25User { name ...B }
			fragment B on S25User { name ...A }
		`)
		// INVALID：3 元环。
		s25RequireInvalid(t, schema, `
			{ user { ...A } }
			fragment A on S25User { name ...B }
			fragment B on S25User { name ...C }
			fragment C on S25User { name ...A }
		`)
		// INVALID：环通过内联片段绕行。
		s25RequireInvalid(t, schema, `
			{ user { ...A } }
			fragment A on S25User { ... on S25User { ...B } }
			fragment B on S25User { name ...A }
		`)
	})
}

// ---------------------------------------------------------------------------
// §5.6 Values
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_Values(t *testing.T) {
	schema, _ := s25NewCoreSchema(t, nil)

	// §5.6.1 Values of Correct Type:
	// "Literal values must be compatible with the type expected in the
	//  position they are found."
	t.Run("5.6.1_ValuesOfCorrectType", func(t *testing.T) {
		s25RequireValid(t, schema, `{ echo(text: "a") }`)
		s25RequireValid(t, schema, `{ echoList(values: [1, 2, 3]) }`)
		// 负整数在 Int 位置上是合法的 IntValue。
		s25RequireValid(t, schema, `{ echoList(values: [-3]) }`)
		s25RequireValid(t, schema, `{ echoList(values: [0, -1, -2147483648]) }`)
		s25RequireValid(t, schema, `{ echoMode(mode: A) }`)
		s25RequireValid(t, schema, `{ echoMode(mode: DEPRECATED_C) }`)
		// 单值会被强制提升为列表。
		s25RequireValid(t, schema, `{ echoList(values: 1) }`)

		// 字符串出现在 Int 位置。
		s25RequireInvalid(t, schema, `{ echoList(values: ["1"]) }`)
		s25RequireInvalid(t, schema, `{ echoList(values: "1") }`)
		// 浮点字面量出现在 Int 位置。
		s25RequireInvalid(t, schema, `{ echoList(values: [1.5]) }`)
		s25RequireInvalid(t, schema, `{ echoList(values: [1.0]) }`)
		// 布尔出现在 Int 位置。
		s25RequireInvalid(t, schema, `{ echoList(values: [true]) }`)
		// 不存在的枚举值。
		s25RequireInvalid(t, schema, `{ echoMode(mode: NOT_A_MODE) }`)
		// 枚举位置上出现字符串字面量。
		s25RequireInvalid(t, schema, `{ echoMode(mode: "A") }`)
		// Int 出现在 String 位置。
		s25RequireInvalid(t, schema, `{ echo(text: 1) }`)
		// 列表元素为 null，而元素类型是 Int!。
		s25RequireInvalid(t, schema, `{ echoList(values: [1, null]) }`)
		// 输入对象位置上出现标量。
		s25RequireInvalid(t, schema, `{ echoFilter(filter: "nope") }`)

		// null 字面量在可空位置上是合法值（§2.9.6 Null Value）。
		s25RequireValid(t, schema, `{ echoNoDefault(value: null) }`)
	})

	// §5.6.2 Input Object Field Names:
	// "Every input field provided in an input object value must be defined in
	//  the set of possible fields of that input object's expected type."
	t.Run("5.6.2_InputObjectFieldNames", func(t *testing.T) {
		s25RequireValid(t, schema, `{ echoFilter(filter: {text: "a"}) }`)
		s25RequireValid(t, schema, `{ echoFilter(filter: {text: "a", count: 1, mode: B, tags: ["x"]}) }`)
		s25RequireValid(t, schema, `{ echoFilter(filter: {text: "a", nested: {flag: true}}) }`)

		s25RequireInvalid(t, schema, `{ echoFilter(filter: {text: "a", notAField: 1}) }`)
		// 嵌套输入对象里的未知字段。
		s25RequireInvalid(t, schema, `{ echoFilter(filter: {text: "a", nested: {notAField: 1}}) }`)
	})

	// §5.6.3 Input Object Field Uniqueness:
	// "Input objects must not contain more than one field of the same name,
	//  otherwise an ambiguity would exist which includes an ignored portion of
	//  syntax."
	t.Run("5.6.3_InputObjectFieldUniqueness", func(t *testing.T) {
		s25RequireValid(t, schema, `{ echoFilter(filter: {text: "a", count: 1}) }`)

		s25RequireInvalid(t, schema, `{ echoFilter(filter: {text: "a", text: "b"}) }`)
		s25RequireInvalid(t, schema, `{ echoFilter(filter: {text: "a", count: 1, count: 2}) }`)
		// 嵌套输入对象内部的重复字段。
		s25RequireInvalid(t, schema, `{ echoFilter(filter: {text: "a", nested: {flag: true, flag: false}}) }`)
	})

	// §5.6.4 Input Object Required Fields:
	// "For each Input Object Field field in fields: if field's type is
	//  Non-Null and does not have a default value, then it must be provided
	//  and its value must not be the null literal."
	t.Run("5.6.4_InputObjectRequiredFields", func(t *testing.T) {
		s25RequireValid(t, schema, `{ echoFilter(filter: {text: "a"}) }`)
		// 有默认值的可空字段可以省略。
		s25RequireValid(t, schema, `{ echoFilter(filter: {text: "a", count: 2}) }`)
		s25RequireValid(t, schema, `{ echoFilter(filter: {text: "a", nested: {values: [1]}}) }`)

		// 缺少非空输入字段 text。
		s25RequireInvalid(t, schema, `{ echoFilter(filter: {count: 1}) }`)
		s25RequireInvalid(t, schema, `{ echoFilter(filter: {}) }`)
		// 非空输入字段被显式赋 null。
		s25RequireInvalid(t, schema, `{ echoFilter(filter: {text: null}) }`)

		// 可空输入字段可以显式为 null（§2.9.6 Null Value）。
		s25RequireValid(t, schema, `{ echoFilter(filter: {text: "a", count: null}) }`)
	})
}

// ---------------------------------------------------------------------------
// §5.7 Directives
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_Directives(t *testing.T) {
	schema, _ := s25NewCoreSchema(t, nil)

	// §5.7.1 Directives Are Defined:
	// "For every directive in a document, the name of that directive must be
	//  defined by the schema."
	t.Run("5.7.1_DirectivesAreDefined", func(t *testing.T) {
		s25RequireValid(t, schema, `{ greeting @skip(if: false) }`)
		s25RequireValid(t, schema, `{ greeting @include(if: true) }`)

		s25RequireInvalid(t, schema, `{ greeting @nope }`)
		s25RequireInvalid(t, schema, `{ user @nope { name } }`)
		s25RequireInvalid(t, schema, `query Q @nope { greeting }`)
	})

	// §5.7.2 Directives Are In Valid Locations:
	// "For every directive in a document, the directive must be used in a
	//  location that the schema declares support for."
	// @skip / @include 的声明位置为 FIELD | FRAGMENT_SPREAD | INLINE_FRAGMENT。
	t.Run("5.7.2_DirectivesAreInValidLocations", func(t *testing.T) {
		// FIELD
		s25RequireValid(t, schema, `{ greeting @skip(if: false) }`)
		// FRAGMENT_SPREAD
		s25RequireValid(t, schema, `
			{ user { ...F @skip(if: false) } }
			fragment F on S25User { name }
		`)
		// INLINE_FRAGMENT
		s25RequireValid(t, schema, `{ user { ... on S25User @include(if: true) { name } } }`)

		// FRAGMENT_DEFINITION 不在 @skip 的允许位置里。
		s25RequireInvalid(t, schema, `
			{ user { ...F } }
			fragment F on S25User @skip(if: false) { name }
		`)
		// QUERY 不在 @skip 的允许位置里。
		s25RequireInvalid(t, schema, `query Q @skip(if: false) { greeting }`)
		// VARIABLE_DEFINITION 不在 @include 的允许位置里。
		s25RequireInvalid(t, schema, `
			query Q($v: String @include(if: true)) { echoNoDefault(value: $v) }
		`)
	})

	// §5.7.3 Directives Are Unique Per Location:
	// "For every location in the document for which Directives can apply, if
	//  any non-repeatable directives are applied, each one must be applied only
	//  once to that location."
	// @skip / @include 都不是 repeatable。
	t.Run("5.7.3_DirectivesAreUniquePerLocation", func(t *testing.T) {
		// 不同的非 repeatable 指令可以同时出现在一个位置。
		s25RequireValid(t, schema, `{ greeting @skip(if: false) @include(if: true) }`)
		// 同一个指令出现在两个不同的位置。
		s25RequireValid(t, schema, `
			{
				greeting @skip(if: false)
				user @skip(if: false) { name }
			}
		`)
		s25RequireValid(t, schema, `
			{ user { ...F @skip(if: false) } }
			fragment F on S25User { name @skip(if: false) }
		`)

		// 同一位置上重复应用非 repeatable 指令。
		s25RequireInvalid(t, schema, `{ greeting @skip(if: false) @skip(if: true) }`)
		s25RequireInvalid(t, schema, `{ greeting @include(if: true) @include(if: false) }`)
		s25RequireInvalid(t, schema, `{ user { ... on S25User @skip(if: false) @skip(if: true) { name } } }`)
		s25RequireInvalid(t, schema, `
			{ user { ...F @include(if: true) @include(if: false) } }
			fragment F on S25User { name }
		`)
	})
}

// ---------------------------------------------------------------------------
// §5.8 Variables
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_Variables(t *testing.T) {
	schema, _ := s25NewCoreSchema(t, nil)

	// §5.8.1 Variable Uniqueness:
	// "For every operation in the document, each variable's name must be
	//  unique within that operation."
	t.Run("5.8.1_VariableUniqueness", func(t *testing.T) {
		s25RequireValid(t, schema, `
			query Q($a: String, $b: String) {
				echo(text: "x", suffix: $a)
				echoNoDefault(value: $b)
			}
		`)
		// 不同操作之间可以重名。
		s25RequireValid(t, schema, `
			query A($v: String) { echoNoDefault(value: $v) }
			query B($v: String) { echoNoDefault(value: $v) }
		`)

		s25RequireInvalid(t, schema, `
			query Q($a: String, $a: String) { echoNoDefault(value: $a) }
		`)
		s25RequireInvalid(t, schema, `
			query Q($a: String, $a: Int) { echoNoDefault(value: $a) }
		`)
	})

	// §5.8.2 Variables Are Input Types:
	// "For every operation in the document, for every variable defined:
	//  variableType must be an input type."
	t.Run("5.8.2_VariablesAreInputTypes", func(t *testing.T) {
		s25RequireValid(t, schema, `
			query Q($a: String, $f: S25Filter!, $m: S25Mode, $l: [Int!]) {
				echoNoDefault(value: $a)
				echoFilter(filter: $f)
				echoMode(mode: $m)
				echoList(values: $l)
			}
		`)

		// 对象类型不是输入类型。
		s25RequireInvalid(t, schema, `query Q($u: S25User) { user { name } }`)
		// 接口类型不是输入类型。
		s25RequireInvalid(t, schema, `query Q($n: S25Node) { nodes { id } }`)
		// union 类型不是输入类型。
		s25RequireInvalid(t, schema, `query Q($s: S25Search) { search { __typename } }`)
		// 包装后的输出类型同样不是输入类型。
		s25RequireInvalid(t, schema, `query Q($u: [S25User!]!) { user { name } }`)
	})

	// §5.8.3 All Variable Uses Defined:
	// "For each operation in a document, each variable usage (including inside
	//  fragments transitively used by that operation) must be defined by that
	//  operation."
	t.Run("5.8.3_AllVariableUsesDefined", func(t *testing.T) {
		s25RequireValid(t, schema, `query Q($a: String) { echoNoDefault(value: $a) }`)
		// 片段内使用、操作上定义。
		s25RequireValid(t, schema, `
			query Q($a: String) { ...F }
			fragment F on Query { echoNoDefault(value: $a) }
		`)
		// 传递使用的片段。
		s25RequireValid(t, schema, `
			query Q($a: String) { ...Outer }
			fragment Outer on Query { ...Inner }
			fragment Inner on Query { echoNoDefault(value: $a) }
		`)

		// 直接使用未定义的变量。
		s25RequireInvalid(t, schema, `query Q { echoNoDefault(value: $a) }`)
		// 只在片段内使用、但操作没有定义。
		s25RequireInvalid(t, schema, `
			query Q { ...F }
			fragment F on Query { echoNoDefault(value: $a) }
		`)
		// 传递使用的片段里出现未定义变量。
		s25RequireInvalid(t, schema, `
			query Q($a: String) { ...Outer }
			fragment Outer on Query { ...Inner }
			fragment Inner on Query { echoNoDefault(value: $a) echoOptional(value: $b) }
		`)
		// 指令参数里的未定义变量。
		s25RequireInvalid(t, schema, `query Q { greeting @skip(if: $flag) }`)
	})

	// §5.8.4 All Variables Used:
	// "For every operation in the document, every variable defined by that
	//  operation must be used at least once, either directly or within a
	//  transitively included fragment."
	t.Run("5.8.4_AllVariablesUsed", func(t *testing.T) {
		s25RequireValid(t, schema, `query Q($a: String) { echoNoDefault(value: $a) }`)
		// 只在片段内使用，也算被使用。
		s25RequireValid(t, schema, `
			query Q($a: String) { ...F }
			fragment F on Query { echoNoDefault(value: $a) }
		`)
		// 只在指令参数里使用，也算被使用。
		s25RequireValid(t, schema, `query Q($flag: Boolean!) { greeting @skip(if: $flag) }`)

		s25RequireInvalid(t, schema, `query Q($a: String) { greeting }`)
		s25RequireInvalid(t, schema, `
			query Q($a: String, $unused: Int) { echoNoDefault(value: $a) }
		`)
		// 只被一个未被本操作引用的片段使用，不算被使用。
		s25RequireInvalid(t, schema, `
			query Q($a: String) { greeting ...Used }
			fragment Used on Query { greeting }
		`)
	})

	// §5.8.5 All Variable Usages Are Allowed:
	// "Variable usages must be compatible with the arguments they are passed
	//  to. Validation failures occur when variables are used in the context of
	//  types that are complete mismatches, or if a nullable type in a variable
	//  is passed to a non-null argument type."
	t.Run("5.8.5_AllVariableUsagesAreAllowed", func(t *testing.T) {
		// $a: Int! 用在 Int! 位置：允许。
		s25RequireValid(t, schema, `query Q($a: Int!) { echoList(values: [$a]) }`)
		// $a: Int = 1 用在 Int! 位置：变量有非 null 默认值，允许。
		s25RequireValid(t, schema, `query Q($a: Int = 1) { echoList(values: [$a]) }`)
		// $a: Int! 用在 Int 位置：允许。
		s25RequireValid(t, schema, `query Q($a: Int!) { echoFilter(filter: {text: "t", count: $a}) }`)
		// $a: String! 用在 String! 位置：允许。
		s25RequireValid(t, schema, `query Q($a: String!) { echo(text: $a) }`)
		// $a: [Int!] 用在 [Int!] 位置：允许。
		s25RequireValid(t, schema, `query Q($a: [Int!]) { echoList(values: $a) }`)
		// $a: [Int!]! 用在 [Int!] 位置：允许。
		s25RequireValid(t, schema, `query Q($a: [Int!]!) { echoList(values: $a) }`)

		// $a: Int 用在 Int! 位置：不允许。
		s25RequireInvalid(t, schema, `query Q($a: Int) { echoList(values: [$a]) }`)
		// $a: String 用在 String! 位置：不允许。
		s25RequireInvalid(t, schema, `query Q($a: String) { echo(text: $a) }`)
		// $a: [Int] 用在 [Int!] 位置：元素可空性不兼容，不允许。
		s25RequireInvalid(t, schema, `query Q($a: [Int]) { echoList(values: $a) }`)
		// $a: String 用在 Int! 位置：类型完全不匹配。
		s25RequireInvalid(t, schema, `query Q($a: String) { echoList(values: [$a]) }`)
		// $a: Int 用在 [Int!] 列表位置：列表位置不接受非列表变量。
		s25RequireInvalid(t, schema, `query Q($a: Int) { echoList(values: $a) }`)
		// $f: S25Filter 用在 S25Filter! 位置：不允许。
		s25RequireInvalid(t, schema, `query Q($f: S25Filter) { echoFilter(filter: $f) }`)
	})
}

// ---------------------------------------------------------------------------
// §7.1.3 + §5：校验失败的请求不得返回 data
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_ErrorsCarryLocationsAndOmitData(t *testing.T) {
	schema, _ := s25NewCoreSchema(t, nil)

	// §7.1.3 Data: "If an error was raised before execution begins, the data
	//  entry must not be present in the result."
	// §7.1.2 Errors: 每条 request error 必须携带 locations。
	cases := []struct {
		name  string
		query string
	}{
		{"unknown field", `{ notAFieldOnQuery }`},
		{"scalar with selection set", `{ greeting { nope } }`},
		{"missing required argument", `{ echo }`},
		{"unknown directive", `{ greeting @nope }`},
		{"undefined variable", `query Q { echoNoDefault(value: $a) }`},
		{"unused fragment", `{ greeting } fragment Unused on S25User { name }`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := s25Do(t, s25Request{Schema: schema, Query: testCase.query})
			if result == nil {
				t.Fatalf("result is nil")
			}
			if result.Data != nil {
				t.Fatalf("result.Data = %#v, want nil for a validation failure", result.Data)
			}
			if len(result.Errors) == 0 {
				t.Fatalf("expected at least one error, got none")
			}
			s25RequireAllErrorsHaveLocations(t, result)

			top := s25MarshalResult(t, result)
			if _, ok := top["data"]; ok {
				t.Fatalf("serialized result must not contain a %q key when the request failed before execution; got keys %v",
					"data", s25valTopLevelKeys(top))
			}
			if _, ok := top["errors"]; !ok {
				t.Fatalf("serialized result must contain an %q key; got keys %v", "errors", s25valTopLevelKeys(top))
			}
		})
	}
}

func s25valTopLevelKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

// ---------------------------------------------------------------------------
// §6.1.1：校验失败时不得执行任何 resolver
// ---------------------------------------------------------------------------

func TestSpec2025_Validation_DoesNotExecuteResolvers(t *testing.T) {
	// §6.1 Executing Requests: "If the operation is a query/mutation/subscription,
	//  the request is only executed after the document has been validated;
	//  if validation fails, the request is not executed."
	cases := []struct {
		name  string
		query string
	}{
		{"unknown field alongside a valid one", `{ greeting notAFieldOnQuery }`},
		{"missing required argument", `{ greeting echo }`},
		{"leaf selection on scalar", `{ greeting people { name { nope } } }`},
		{"fragment cycle free but unused", `{ greeting } fragment Unused on S25User { name }`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			counter := s25NewCounter()
			schema, _ := s25NewCoreSchema(t, counter)

			result := s25Do(t, s25Request{Schema: schema, Query: testCase.query})
			if result == nil {
				t.Fatalf("result is nil")
			}
			if len(result.Errors) == 0 {
				t.Fatalf("expected the request to be rejected by validation, got no errors")
			}
			if got := counter.Total(); got != 0 {
				t.Fatalf("resolver invocations = %d (%v), want 0 for a request rejected by validation",
					got, counter.Keys())
			}
		})
	}
}
