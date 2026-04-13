// Package exec 负责执行 SelectionPlan，是三层架构的第二层（执行层）。
// 职责：按照计划中的依赖关系，将字段分批（batch）并发或串行执行，
// 将结果写入 RunFrame，最终由 response 层组装成 GraphQL 响应。
package exec

import (
	"fmt"
	"sync"

	"github.com/graphql-go/graphql/sgraph/plan"
)

// RunFrame 是单次 GraphQL 请求的执行帧，存储所有中间结果和错误信息。
//
// 设计原则：
//   - slot 在执行前由 SelectionPlan.TotalSlots 静态预分配，避免运行时扩容；
//   - 每个 slot 只写入一次（WriteSlot 会检查重复写入）；
//   - errors 列表记录所有字段错误，支持 partial data（部分成功）语义。
type RunFrame struct {
	// slots 存储每个 SlotID 对应字段的执行结果，长度由 TotalSlots 决定
	slots []interface{}

	// slotWritten 标记对应 slot 是否已经写入，防止重复写入
	slotWritten []bool

	// errors 记录执行过程中发生的所有字段错误（支持 partial data）
	errors []FieldError

	// variables 是本次 GraphQL 请求的变量表（来自请求输入）
	variables map[string]interface{}

	// mu 保护 errors 列表的并发写入安全
	mu sync.Mutex
}

// FieldError 表示单个字段执行时发生的错误，包含路径信息（对应 GraphQL error 格式）
type FieldError struct {
	// Path 是发生错误的字段路径，如 ["users", "0", "name"]
	Path []string

	// Err 是 resolver 返回的原始错误
	Err error

	// Message 是对外展示的错误消息
	Message string
}

// NewRunFrame 创建一个新的执行帧并预分配 slot 空间。
//   - totalSlots: 来自 SelectionPlan.TotalSlots
//   - variables:  本次请求的 GraphQL 变量表
func NewRunFrame(totalSlots int, variables map[string]interface{}) *RunFrame {
	if variables == nil {
		variables = map[string]interface{}{}
	}
	return &RunFrame{
		slots:       make([]interface{}, totalSlots),
		slotWritten: make([]bool, totalSlots),
		variables:   variables,
	}
}

// WriteSlot 将 resolver 的执行结果写入指定 slot。
// 若该 slot 已被写入过，返回错误（避免覆盖、保证幂等）。
func (f *RunFrame) WriteSlot(id plan.SlotID, value interface{}) error {
	idx := int(id)
	if idx < 0 || idx >= len(f.slots) {
		return fmt.Errorf("runframe: slot %d 越界（总计 %d 个）", id, len(f.slots))
	}
	if f.slotWritten[idx] {
		return fmt.Errorf("runframe: slot %d 已经写入，禁止重复写入", id)
	}
	f.slots[idx] = value
	f.slotWritten[idx] = true
	return nil
}

// ReadSlot 从 RunFrame 中读取指定 slot 的值。
// 若 slot 尚未写入，返回 nil（可能是上游字段出错或返回 null）。
func (f *RunFrame) ReadSlot(id plan.SlotID) interface{} {
	idx := int(id)
	if idx < 0 || idx >= len(f.slots) {
		return nil
	}
	return f.slots[idx]
}

// GetVariable 按名称从请求变量表中取值。
// 若变量不存在，返回 nil。
func (f *RunFrame) GetVariable(name string) interface{} {
	return f.variables[name]
}

// AddError 向执行帧的错误列表追加一条字段错误（线程安全）。
func (f *RunFrame) AddError(path []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, FieldError{
		Path:    path,
		Err:     err,
		Message: err.Error(),
	})
}

// Errors 返回所有字段错误的只读切片（供 response 层使用）
func (f *RunFrame) Errors() []FieldError {
	return f.errors
}

// Slots 返回 slot 切片（供 response 层从中读取字段执行结果）
func (f *RunFrame) Slots() []interface{} {
	return f.slots
}

// totalSlots 返回 frame 的 slot 总数（供 IteratorStep 创建子帧使用）
func (f *RunFrame) totalSlots() int {
	return len(f.slots)
}

// copyTo 将当前帧所有已写入的 slot 复制到目标帧。
// 用于 IteratorStep 创建每个列表元素的子帧时保留父帧的上下文数据。
func (f *RunFrame) copyTo(dst *RunFrame) {
	for i, written := range f.slotWritten {
		if written {
			dst.slots[i] = f.slots[i]
			dst.slotWritten[i] = true
		}
	}
}
