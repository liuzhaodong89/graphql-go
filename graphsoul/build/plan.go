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
