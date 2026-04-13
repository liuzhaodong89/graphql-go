package build

type SGraphPlan struct {
	originalInputs map[string]any
	roots          []*FieldPlan
}
