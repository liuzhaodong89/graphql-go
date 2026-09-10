package main

import (
	"fmt"

	graphql "github.com/graphql-go/graphql"
)

func buildRegistries() (*graphql.DirectiveRegistry, *graphql.ParamRegistry, error) {
	directiveRegistry := graphql.NewDirectiveRegistry()

	// audit 只保留名称、位置和参数计划，不执行 ShouldExecute、
	// BeforeResolve 或 AfterResolve。
	if err := directiveRegistry.RegisterMetadataOnly("audit"); err != nil {
		return nil, nil, fmt.Errorf("register audit directive metadata: %w", err)
	}

	paramRegistry := graphql.NewParamRegistry()
	err := paramRegistry.RegisterQuery(graphql.QueryParamConfig{
		DocumentBody:  registryDemoQuery,
		OperationName: "RegistryDemo",
		FieldParams: []graphql.FieldParamBinding{
			{
				Target: graphql.FieldParamTarget{
					// ResponsePath 使用 Query 中最终的 responseName，因此这里使用 alias profile。
					ResponsePath: []string{"profile"},

					// ParentTypeName 和 FieldName 使用 Schema 坐标，不能填写 alias。
					ParentTypeName: "Query",
					FieldName:      "userByID",
					ParamName:      "id",
				},
				Source: graphql.ParamSource{
					Kind: graphql.ParamSourceFieldResponse,
					FieldResponse: &graphql.FieldResponseParamSource{
						// source 字段的 ResponsePath 同样使用 Query 中的 alias seed。
						ResponsePath:   []string{"seed"},
						ParentTypeName: "Query",
						FieldName:      "seedUser",

						// ResultPath 从 seedUser resolver 的原始结果中读取 id。
						ResultPath: []string{"id"},
					},
				},
			},
		},
		DirectiveParams: []graphql.DirectiveParamBinding{
			{
				Target: graphql.DirectiveParamTarget{
					Location:       graphql.DirectiveLocationField,
					DirectiveName:  "audit",
					ParamName:      "tag",
					ResponsePath:   []string{"profile"},
					ParentTypeName: "Query",
					FieldName:      "userByID",
				},
				Source: graphql.ParamSource{
					Kind:       graphql.ParamSourceConst,
					ConstValue: "registry-tag",
				},
			},
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("register query parameter metadata: %w", err)
	}

	return directiveRegistry, paramRegistry, nil
}
