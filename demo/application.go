package main

import (
	"fmt"

	graphql "github.com/graphql-go/graphql"
)

type application struct {
	schema graphql.Schema
}

func newApplication() (*application, error) {
	auditDirective := newAuditDirective()

	schema, err := buildSchema(auditDirective)
	if err != nil {
		return nil, fmt.Errorf("build schema: %w", err)
	}

	directiveRegistry, paramRegistry, err := buildRegistries()
	if err != nil {
		return nil, err
	}

	// Engine 创建时会复制并冻结两个 Registry。创建完成后，Schema、resolver、
	// compiler 和 handler 必须保持只读，同一个 Engine 才能安全地并发复用。
	engine, err := graphql.NewSGraphEngine(
		&schema,
		directiveRegistry,
		paramRegistry,
	)
	if err != nil {
		return nil, fmt.Errorf("create sgraph engine: %w", err)
	}
	if err := graphql.RegisterSGraphEngine(engine); err != nil {
		return nil, fmt.Errorf("register sgraph engine: %w", err)
	}

	return &application{
		schema: schema,
	}, nil
}

func (app *application) execute() *graphql.Result {
	return graphql.Do(graphql.Params{
		Schema:        app.schema,
		RequestString: registryDemoQuery,
		OperationName: "RegistryDemo",
	})
}
