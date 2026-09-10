package main

import graphql "github.com/graphql-go/graphql"

// newAuditDirective 创建 GraphQL Schema 中的指令定义，用于原生 parse/validate。
// 指令是否参与 SGraph 运行期执行由 DirectiveRegistry 单独决定。
func newAuditDirective() *graphql.Directive {
	return graphql.NewDirective(graphql.DirectiveConfig{
		Name:        "audit",
		Description: "Records audit metadata for a selected field.",
		Locations: []string{
			graphql.DirectiveLocationField,
		},
		Args: graphql.FieldConfigArgument{
			"tag": &graphql.ArgumentConfig{
				Type: graphql.NewNonNull(graphql.String),
			},
		},
	})
}
