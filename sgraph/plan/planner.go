package plan

import "fmt"

// Planner 负责将用户注册的字段定义树（FieldDef 树）编译成 SelectionPlan。
//
// 编译期职责：
//  1. 递归遍历 FieldDef 树，为每个字段分配唯一 SlotID；
//  2. 建立父子 slot 之间的依赖关系（SourceSlot）；
//  3. 处理 list 类型字段，为 listItemSlot 额外分配一个 slot；
//  4. 计算并记录总 slot 数（TotalSlots），供 exec 层预分配 RunFrame。
type Planner struct {
	// nextSlot 是下一个可分配的 SlotID，每次调用 allocSlot() 后自增
	nextSlot int
}

// NewPlanner 创建并返回一个新的 Planner 实例
func NewPlanner() *Planner {
	return &Planner{}
}

// allocSlot 分配一个新的 slot 编号并返回，内部计数器自增
func (p *Planner) allocSlot() SlotID {
	id := p.nextSlot
	p.nextSlot++
	return SlotID(id)
}

// Build 将一组根字段定义编译为完整的 SelectionPlan。
//   - operationType: 指定操作类型（OperationQuery / OperationMutation）
//   - rootDefs:      根字段定义列表（对应 GraphQL 顶层 selection set）
//
// 若定义有误（如缺少 Resolver），返回错误。
func (p *Planner) Build(operationType OperationType, rootDefs []*FieldDef) (*SelectionPlan, error) {
	if len(rootDefs) == 0 {
		return nil, fmt.Errorf("build: rootDefs 不能为空")
	}

	rootFields := make([]*FieldPlan, 0, len(rootDefs))
	for _, def := range rootDefs {
		// 根字段的 SourceSlot = NoSlot，表示没有父结果，resolver 的 source 参数为 nil
		fp, err := p.compileField(def, NoSlot, nil)
		if err != nil {
			return nil, err
		}
		rootFields = append(rootFields, fp)
	}

	return &SelectionPlan{
		OperationType: operationType,
		RootFields:    rootFields,
		TotalSlots:    p.nextSlot,
	}, nil
}

// compileField 递归地将一个 FieldDef 编译为 FieldPlan。
//   - def:        当前字段定义
//   - sourceSlot: 父字段结果所在的 slot（根字段传 NoSlot）
//   - parentPath: 父字段的响应路径前缀
func (p *Planner) compileField(def *FieldDef, sourceSlot SlotID, parentPath []string) (*FieldPlan, error) {
	if def.Resolver == nil {
		return nil, fmt.Errorf("build: 字段 %q 缺少 Resolver", def.FieldName)
	}

	// 为当前字段分配结果 slot
	resultSlot := p.allocSlot()

	// 构建当前字段的完整响应路径
	path := make([]string, len(parentPath)+1)
	copy(path, parentPath)
	path[len(parentPath)] = def.ResponseKey

	fp := &FieldPlan{
		ResponseKey: def.ResponseKey,
		FieldName:   def.FieldName,
		Path:        path,
		ResultSlot:  resultSlot,
		SourceSlot:  sourceSlot,
		ArgPlans:    def.Args,
		Resolver:    def.Resolver,
		TypeKind:    def.TypeKind,
		NonNull:     def.NonNull,
	}

	// 递归编译子字段
	if len(def.Children) > 0 {
		switch def.TypeKind {
		case KindObject:
			// 对象类型：子字段以当前 resultSlot 作为 SourceSlot（父结果即子字段的 source）
			fp.Children = make([]*FieldPlan, 0, len(def.Children))
			for _, child := range def.Children {
				childPlan, err := p.compileField(child, resultSlot, path)
				if err != nil {
					return nil, err
				}
				fp.Children = append(fp.Children, childPlan)
			}

		case KindList:
			// 列表类型：额外分配一个 listItemSlot，每轮迭代中暂存当前列表元素
			fp.ListItemSlot = p.allocSlot()
			fp.ListItemChildren = make([]*FieldPlan, 0, len(def.Children))
			for _, child := range def.Children {
				// 列表子字段的 source 来自 ListItemSlot（当前迭代的单个元素）
				childPlan, err := p.compileField(child, fp.ListItemSlot, path)
				if err != nil {
					return nil, err
				}
				fp.ListItemChildren = append(fp.ListItemChildren, childPlan)
			}
		}
	}

	return fp, nil
}
