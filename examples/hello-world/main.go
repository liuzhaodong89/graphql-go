package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/graphql-go/graphql/plan"
	"log"

	"github.com/graphql-go/graphql"
)

func main() {
	// Schema
	fields := graphql.Fields{
		"hello": &graphql.Field{
			Type: graphql.String,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return "world", nil
			},
		},
		"test": &graphql.Field{
			Type: graphql.String,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return "yes", nil
			},
		},
		"very": &graphql.Field{
			Type: graphql.String,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return "good", nil
			},
		},
	}
	rootQuery := graphql.ObjectConfig{Name: "RootQuery", Fields: fields}
	schemaConfig := graphql.SchemaConfig{Query: graphql.NewObject(rootQuery)}
	schema, err := graphql.NewSchema(schemaConfig)
	if err != nil {
		log.Fatalf("failed to create new schema, error: %v", err)
	}

	// Query
	query := `
		{
			hello,
			test,
			very
		}
	`
	params := graphql.Params{Schema: schema, RequestString: query}
	r := graphql.Do(params)
	if len(r.Errors) > 0 {
		log.Fatalf("failed to execute graphql operation, errors: %+v", r.Errors)
	}
	rJSON, _ := json.Marshal(r)
	fmt.Printf("%s \n", rJSON) // {“data”:{“hello”:”world”}}

	r1 := graphql.Do(params)
	if len(r1.Errors) > 0 {
		log.Fatalf("failed to execute graphql operation, errors: %+v", r1.Errors)
	}
	rJSON1, _ := json.Marshal(r1)
	fmt.Printf("%s \n", rJSON1)

	testQuery := `
	{
		product(id:1){
			productid,
			title,
			skus{
				skuid,
				skuquantity,
				skuprice,
				locationname
			}
			store{
				storeid,
				recommendations{
					productid,
					title,
				}
			}
		}
	}`
	if len(testQuery) > 0 {
		//TODO初始化root_node
		rootNode := plan.RootNode{}
		rootNode.SetId(0)
		//TODO初始化product_node和对应的paramAccessNode
		productPANode := plan.ParamAccessNode{}
		productPANode.SetId(1)
		productPANode.AddSourceParamKeys([]string{"id"})
		productPANode.AddTargetParamTypes([]int{plan.PARAM_TYPE_INT})

		var rootNodeRef plan.Node = &rootNode
		productPANode.AddDependencies([]*plan.Node{&rootNodeRef})

		pdtFunc := func(paramVals interface{}, ctx *context.Context) (interface{}, error) {
			if paramVals == nil {
				return nil, errors.New("paramVals is nil")
			}
			realParam := paramVals.(interface{})
			if realParam != 1 {
				return nil, nil
			}
			resultMap := make(map[string]interface{})
			resultMap["productid"] = 1
			resultMap["title"] = "testTitle"
			resultMap["storeid"] = 123
			return resultMap, nil
		}
		productNode := plan.ResolverNode{}
		productNode.SetId(2)
		productNode.SetResoloverFunc(pdtFunc)

		var productPANodeRef plan.Node = &productPANode
		productNode.AddDependencies([]*plan.Node{&productPANodeRef})
		//TODO初始化skus_node和对应的ParamAccessNode
		skusParamAccessNode := plan.ParamAccessNode{}
		skusParamAccessNode.SetId(3)
		skusParamAccessNode.AddSourceParamKeys([]string{"id"})
		skusParamAccessNode.AddDependencies([]*plan.Node{&rootNodeRef})
		skusParamAccessNode.AddTargetParamTypes([]int{plan.PARAM_TYPE_INT})

		skusFunc := func(paramVals interface{}, ctx *context.Context) (interface{}, error) {
			return nil, nil
		}
		skusNode := plan.ResolverNode{}
		skusNode.SetId(4)
		skusNode.SetResoloverFunc(skusFunc)

		var skusPANodeRef plan.Node = &skusParamAccessNode
		skusNode.AddDependencies([]*plan.Node{&skusPANodeRef})
		//TODO初始化location_node和对应的ParamAccessNode
		locationParamAccessNode := plan.ParamAccessNode{}
		locationParamAccessNode.SetId(5)
		locationParamAccessNode.AddSourceParamKeys([]string{"skuid"})
		locationParamAccessNode.AddTargetParamTypes([]int{plan.PARAM_TYPE_INT})

		var skuNodeRef plan.Node = &locationParamAccessNode
		locationParamAccessNode.AddDependencies([]*plan.Node{&skuNodeRef})

		loactionFunc := func(paramVals interface{}, ctx *context.Context) (interface{}, error) {
			return nil, nil
		}
		locationNode := plan.ResolverNode{}
		locationNode.SetId(6)
		locationNode.SetResoloverFunc(loactionFunc)

		var locationPANodeRef plan.Node = &locationNode
		locationNode.AddDependencies([]*plan.Node{&locationPANodeRef})
		//TODO初始化store_node
		storeParamAccessNode := plan.ParamAccessNode{}
		storeParamAccessNode.SetId(7)
		storeParamAccessNode.AddSourceParamKeys([]string{"storeid"})
		storeParamAccessNode.AddTargetParamTypes([]int{plan.PARAM_TYPE_INT})

		var productNodeRef plan.Node = &productNode
		storeParamAccessNode.AddDependencies([]*plan.Node{&productNodeRef})

		storeFunc := func(paramVals interface{}, ctx *context.Context) (interface{}, error) {
			return nil, nil
		}
		storeNode := plan.ResolverNode{}
		storeNode.SetId(8)
		storeNode.SetResoloverFunc(storeFunc)

		var storePANodeRef plan.Node = &storeParamAccessNode
		storeNode.AddDependencies([]*plan.Node{&storePANodeRef})
		//TODO初始化recommendations对应的product_node
		recParamAccessNode := plan.ParamAccessNode{}
		recParamAccessNode.SetId(9)
		recParamAccessNode.AddSourceParamKeys([]string{"storeid"})
		var storeNodeRef plan.Node = &storeNode
		recParamAccessNode.AddDependencies([]*plan.Node{&storeNodeRef})

		recFunc := func(paramVals interface{}, ctx *context.Context) (interface{}, error) {
			return nil, nil
		}
		recNode := plan.ResolverNode{}
		recNode.SetId(10)
		recNode.SetResoloverFunc(recFunc)

		var recPANodeRef plan.Node = &recParamAccessNode
		recNode.AddDependencies([]*plan.Node{&recPANodeRef})

		rundata := plan.RunData{}
		rundata.SetOriginalParams(map[string]interface{}{
			"id": 1,
		})

		//TODO组装step
		productStep := plan.NormalStep{}
		productStep.SetId(1)
		productStep.SetParamAccessNode(&productPANode)
		productStep.SetResolverNode(&productNode)

		skusStep := plan.NormalStep{}
		skusStep.SetId(2)
		skusStep.SetParamAccessNode(&skusParamAccessNode)
		skusStep.SetResolverNode(&skusNode)

		locationStep := plan.NormalStep{}
		locationStep.SetId(3)
		locationStep.SetParamAccessNode(&locationParamAccessNode)
		locationStep.SetResolverNode(&locationNode)

		storeStep := plan.NormalStep{}
		storeStep.SetId(4)
		storeStep.SetParamAccessNode(&storeParamAccessNode)
		storeStep.SetResolverNode(&storeNode)

		recommendationsStep := plan.NormalStep{}
		recommendationsStep.SetId(5)
		recommendationsStep.SetParamAccessNode(&recParamAccessNode)
		recommendationsStep.SetResolverNode(&recNode)
		//TODO组装batch
		firstBatch := plan.Batch{}
		firstBatch.SetId(0)

		var productStepRef plan.Step = &productStep
		firstBatch.AddStep(&productStepRef)
		var skuStepRef plan.Step = &skusStep
		firstBatch.AddStep(&skuStepRef)

		secondBatch := plan.Batch{}
		secondBatch.SetId(1)

		var storeStepRef plan.Step = &storeStep
		secondBatch.AddStep(&storeStepRef)
		var locationStepRef plan.Step = &locationStep
		secondBatch.AddStep(&locationStepRef)

		thirdBatch := plan.Batch{}
		thirdBatch.SetId(2)

		var recommendationsStepRef plan.Step = &recommendationsStep
		thirdBatch.AddStep(&recommendationsStepRef)

		firstBatch.SetChild(&secondBatch)
		secondBatch.SetChild(&thirdBatch)

		runData := plan.RunData{}
		runData.SetOriginalParams(map[string]interface{}{
			"id": 1,
		})
		ctx := context.Background()

		firstBatch.Execute(&runData, &ctx)
	}
}
