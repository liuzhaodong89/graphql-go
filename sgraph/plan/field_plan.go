// Package build 负责将 GraphQL 字段定义编译成可执行计划（三层架构第一层）。
// 职责：静态分析字段依赖、分配 SlotID、生成 FieldPlan 树和 SelectionPlan。
package plan

import "context"

// SlotID 是每个字段执行结果在 RunFrame 中的唯一编号。
// 由 Planner 在编译期静态分配，执行期按编号读写，避免运行时动态查找。
type SlotID int

// NoSlot 表示没有父结果的特殊 slot 编号（用于根字段的 SourceSlot）
const NoSlot SlotID = -1

// FieldPlan 表示单个 GraphQL field 的完整执行计划。
// 对应一次完整的 "field execution = source + args + resolve + complete + children"。
type FieldPlan struct {
	// ResponseKey 是写入响应 JSON 的键名（有 alias 用 alias，否则用 FieldName）
	ResponseKey string

	// FieldName 是 Schema 中定义的原始字段名
	FieldName string

	// Path 是该字段在响应中的完整路径，如 ["user", "address", "city"]
	Path []string

	// ResultSlot 是本字段 resolver 执行结果存入 RunFrame 的 slot 编号
	ResultSlot SlotID

	// SourceSlot 是父字段结果所在的 slot 编号；NoSlot(-1) 表示根节点，source 传 nil
	SourceSlot SlotID

	// ArgPlans 描述如何组装该字段的 arguments（每个 argument 一个 ArgPlan）
	ArgPlans []*ArgPlan

	// Resolver 是实际执行取数逻辑的函数，由用户在 Schema 定义时注入
	Resolver ResolverFunc

	// TypeKind 表示该字段的返回类型种类，决定 complete 阶段的行为
	TypeKind TypeKind

	// NonNull 为 true 时，若 resolver 返回 nil/error，需向上冒泡 null 错误
	NonNull bool

	// Children 是 KindObject 类型字段的子字段计划列表
	Children []*FieldPlan

	// ListItemSlot 仅 KindList 时使用，表示每轮迭代中当前列表元素的临时存放 slot
	ListItemSlot SlotID

	// ListItemChildren 是 KindList 时列表元素对象的子字段计划列表
	ListItemChildren []*FieldPlan
}

// ArgPlan 描述如何为单个 field argument 求值。
type ArgPlan struct {
	// Name 是 argument 的名称（对应 Schema 中定义的参数名）
	Name string

	// Kind 决定从哪里以何种方式取值
	Kind ArgKind

	// ConstValue 当 Kind == ArgKindConst 时直接作为参数值使用
	ConstValue interface{}

	// VariableName 当 Kind == ArgKindVariable 时，从请求变量表中按此名称取值
	VariableName string

	// SourceSlot 当 Kind == ArgKindFromSlot 时，从 RunFrame 的指定 slot 读取父结果
	SourceSlot SlotID

	// FieldPath 当 Kind == ArgKindFromSlot 时，从 slot 结果中按字段路径提取子值
	// 例如 ["id"] 表示取 slot 结果的 .id 字段；为空则整体传入
	FieldPath []string
}

// ArgKind 表示 argument 的求值方式
type ArgKind int

const (
	// ArgKindConst 直接使用 ConstValue 中的常量值（字面量参数）
	ArgKindConst ArgKind = iota
	// ArgKindVariable 从 GraphQL variables 中按 VariableName 取值
	ArgKindVariable
	// ArgKindFromSlot 从已执行字段的结果 slot 中提取字段作为参数（父子依赖传参）
	ArgKindFromSlot
)

// TypeKind 表示 GraphQL 字段返回类型的种类，决定 complete 阶段如何处理结果
type TypeKind int

const (
	// KindScalar 标量类型（String、Int、Boolean、Float、ID 等），直接序列化
	KindScalar TypeKind = iota
	// KindObject 对象类型，resolver 返回 map，需继续执行子 selection set
	KindObject
	// KindList 列表类型，resolver 返回 []interface{}，对每个元素执行子 selection set
	KindList
	// KindEnum 枚举类型，序列化时取字符串名称
	KindEnum
)

// ResolverFunc 是 resolver 函数的标准签名。
//   - source: 父字段的执行结果（根字段传 nil）
//   - args:   已组装好的 argument map（key 为 argument 名称）
//   - ctx:    请求级 context，用于传递认证信息、取消信号、DataLoader 等
//
// 返回字段原始数据（尚未 complete）和可选错误。
type ResolverFunc func(source interface{}, args map[string]interface{}, ctx context.Context) (interface{}, error)

// SelectionPlan 是整个 GraphQL operation 的顶层执行计划。
// 由 Planner.Build 生成，交给 exec 层的 Executor 执行。
type SelectionPlan struct {
	// OperationType 区分 query / mutation / subscription，影响并发策略
	OperationType OperationType

	// RootFields 是操作顶层字段的执行计划列表
	RootFields []*FieldPlan

	// TotalSlots 是本次执行需要分配的 slot 总数，由 Planner 在编译期计算
	TotalSlots int
}

// OperationType 表示 GraphQL operation 的类型
type OperationType int

const (
	// OperationQuery 查询操作，根字段可以并发执行
	OperationQuery OperationType = iota
	// OperationMutation 变更操作，根字段必须按顺序串行执行
	OperationMutation
)

// FieldDef 是用户注册字段时的输入定义（简化了 AST 解析过程，直接 API 注册）。
type FieldDef struct {
	// ResponseKey 响应中使用的键名（alias 或 field name）
	ResponseKey string

	// FieldName Schema 中的原始字段名
	FieldName string

	// Resolver 用户提供的取数函数
	Resolver ResolverFunc

	// TypeKind 返回类型种类
	TypeKind TypeKind

	// NonNull 是否为非空字段
	NonNull bool

	// Args argument 定义列表
	Args []*ArgPlan

	// Children 子字段定义，仅 KindObject 和 KindList 有效
	Children []*FieldDef
}
