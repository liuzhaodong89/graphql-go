package build

import (
	"strconv"
	"strings"
)

type SGraphPlan struct {
	originalInputs map[string]any
	roots          []*FieldPlan
}

func (s *SGraphPlan) GetRoots() []*FieldPlan {
	return s.roots
}

func (s *SGraphPlan) GetOriginalInputs() map[string]any {
	return s.originalInputs
}

func (s *SGraphPlan) GetCacheKey() string {
	var sb strings.Builder
	for _, root := range s.roots {
		s.loopCreateCacheKey(root, &sb)
	}
	return sb.String()
}

func (s *SGraphPlan) loopCreateCacheKey(fp *FieldPlan, keyBuilder *strings.Builder) {
	if fp == nil {
		return
	}
	keyBuilder.WriteString(strconv.FormatUint(uint64(fp.GetFieldId()), 10))
	keyBuilder.WriteString("-")
	for _, child := range fp.GetChildrenFields() {
		s.loopCreateCacheKey(child, keyBuilder)
	}
}

// MaxFieldId 遍历整棵字段树，返回最大的 fieldId。
// 用于 NewRundata 预分配 slice 容量（下标 = fieldId）。
func (s *SGraphPlan) MaxFieldId() uint32 {
	var max uint32
	for _, root := range s.roots {
		walkMaxFieldId(root, &max)
	}
	return max
}

func walkMaxFieldId(fp *FieldPlan, max *uint32) {
	if fp == nil {
		return
	}
	if fp.fieldId > *max {
		*max = fp.fieldId
	}
	for _, child := range fp.childrenFields {
		walkMaxFieldId(child, max)
	}
}
