package build

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
