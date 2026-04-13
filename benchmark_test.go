package graphql

import (
	"log"
	"testing"
)

func BenchmarkGQLParse(b *testing.B) {
	// Schema
	fields := Fields{
		"hello": &Field{
			Type: String,
			Resolve: func(p ResolveParams) (interface{}, error) {
				return "world", nil
			},
		},
	}
	rootQuery := ObjectConfig{Name: "RootQuery", Fields: fields}
	schemaConfig := SchemaConfig{Query: NewObject(rootQuery)}
	schema, err := NewSchema(schemaConfig)
	if err != nil {
		log.Fatalf("failed to create new schema, error: %v", err)
	}

	// Query
	query := `
		{
			hello
		}
	`
	params := Params{Schema: schema, RequestString: query}
	for i := 0; i < b.N; i++ {
		r := Do(params)
		if len(r.Errors) > 0 {
			log.Fatalf("failed to execute graphql operation, errors: %+v", r.Errors)
		}
	}
}
