// Package response 负责将 RunFrame 中的执行结果组装成 GraphQL 响应（三层架构第三层）。
// 职责：
//  1. 按照 FieldPlan 树结构和 ResponseKey，从 slot 数组中读取字段结果；
//  2. 递归组装 data 对象（处理对象嵌套和列表）；
//  3. 将 FieldError 列表转换为 GraphQL errors 数组格式；
//  4. 支持 partial data：data 和 errors 可同时非空。
package response

// GraphQLError 表示 GraphQL 响应中 errors 数组的单个错误对象。
// 遵循 GraphQL 规范 https://spec.graphql.org/October2021/#sec-Errors
type GraphQLError struct {
	// Message 是对外展示的错误描述（必填）
	Message string `json:"message"`

	// Path 是发生错误的字段路径（如 ["users", 0, "name"]），nil 表示非字段级错误
	Path []interface{} `json:"path,omitempty"`

	// Extensions 是可选扩展信息（如错误码、调试信息等）
	Extensions map[string]interface{} `json:"extensions,omitempty"`
}

// Result 是 GraphQL 执行的最终响应，对应标准的 { data, errors } 结构。
type Result struct {
	// Data 是执行成功的字段数据，若顶层所有字段均失败则为 nil
	Data map[string]interface{} `json:"data"`

	// Errors 是执行过程中收集的所有字段错误列表（partial data 时 data 和 errors 同时非空）
	Errors []GraphQLError `json:"errors,omitempty"`
}

// FieldErrorInfo 是从 exec 层传入的字段错误信息。
// 使用独立结构体而非直接依赖 exec.FieldError，避免 response 和 exec 包循环依赖。
type FieldErrorInfo struct {
	// Path 发生错误的字段路径
	Path []string

	// Message 对外展示的错误消息
	Message string

	// Err 原始错误对象（用于日志等内部用途）
	Err error
}
