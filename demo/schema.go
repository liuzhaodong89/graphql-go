package main

import (
	"fmt"

	graphql "github.com/graphql-go/graphql"
)

func buildSchema(auditDirective *graphql.Directive) (graphql.Schema, error) {
	userType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DemoUser",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
			},
			"name": &graphql.Field{
				Type: graphql.NewNonNull(graphql.String),
			},
		},
	})

	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"seedUser": &graphql.Field{
				Type: graphql.NewNonNull(userType),
				Resolve: func(params graphql.ResolveParams) (any, error) {
					return map[string]any{
						"id":   "U-100",
						"name": "seed-user",
					}, nil
				},
			},
			"userByID": &graphql.Field{
				Type: graphql.NewNonNull(userType),
				Args: graphql.FieldConfigArgument{
					"id": &graphql.ArgumentConfig{
						Type: graphql.NewNonNull(graphql.ID),
					},
				},
				Resolve: func(params graphql.ResolveParams) (any, error) {
					id, exists := params.Args["id"]
					if !exists {
						return nil, fmt.Errorf("userByID resolver did not receive id")
					}

					idText := fmt.Sprint(id)
					return map[string]any{
						"id":   idText,
						"name": "user-" + idText,
					}, nil
				},
			},
		},
	})

	// SchemaConfig.Directives 非空时不会自动补充标准指令，因此必须先复制
	// SpecifiedDirectives，再追加自定义指令，保留 @skip/@include 等既有能力。
	directives := append([]*graphql.Directive(nil), graphql.SpecifiedDirectives...)
	directives = append(directives, auditDirective)

	return graphql.NewSchema(graphql.SchemaConfig{
		Query:      queryType,
		Directives: directives,
	})
}
