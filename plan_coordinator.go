package graphql

import (
	"errors"
	"fmt"

	"github.com/graphql-go/graphql/language/ast"
)

func coordinateBatches(blueprint *SGraphExecutionPlan) ([]*BatchPlan, error) {
	if blueprint == nil {
		return nil, errors.New("blueprint cannot be nil")
	}
	operationDef, operationDefOk := blueprint.schemaResolveInfo.operation.(*ast.OperationDefinition)
	if !operationDefOk {
		return nil, errors.New("operation definition is not an operationDefinition")
	}
	if operationDef.Operation != ast.OperationTypeQuery {
		return nil, fmt.Errorf("not supported operation type: %s", operationDef.Operation)
	}

	roots := blueprint.roots
	if len(roots) == 0 {
		if operationDef.Name == nil {
			return nil, errors.New("no roots found")
		}
		return nil, fmt.Errorf("no roots found for %s", operationDef.Name.Value)
	}

	fieldById := make(map[uint32]*FieldPlan)
	//key是fieldId，根据fieldId查对应Step
	stepByFieldId := make(map[uint32]Step)
	//元素是fieldId
	fieldIdsByStepOrder := make([]uint32, 0)

	var actualMaxFieldId uint32
	for _, root := range roots {
		if root == nil {
			return nil, errors.New("root cannot be nil")
		}

		maxFieldId, err := appendBatches(root, nil, true, fieldById, stepByFieldId, &fieldIdsByStepOrder)
		if err != nil {
			return nil, err
		}
		if maxFieldId > actualMaxFieldId {
			actualMaxFieldId = maxFieldId
		}
	}

	//检查field实际最大fieldId和blueprint记录的最大fieldId是否一致
	if blueprint.maxFieldId != actualMaxFieldId {
		return nil, fmt.Errorf("plan max field id %d does not match %d", actualMaxFieldId, blueprint.maxFieldId)
	}

	//DAG元信息
	//记录当前fieldId的依赖节点数量
	indegree := make(map[uint32]uint32, len(fieldIdsByStepOrder))
	//记录当前fieldId的被依赖节点id
	dependents := make(map[uint32][]uint32, len(fieldIdsByStepOrder))
	//记录当前fieldId的依赖节点id的set，去重
	dependencySets := make(map[uint32]map[uint32]struct{}, len(fieldIdsByStepOrder))

	for _, fieldId := range fieldIdsByStepOrder {
		indegree[fieldId] = 0
	}

	for _, consumerId := range fieldIdsByStepOrder {
		fieldPlan := fieldById[consumerId]

		for _, paramPlans := range [3][]*ParamPlan{
			fieldPlan.paramPlans,
			fieldPlan.bulkParamPlans,
			fieldPlan.directiveParamPlans,
		} {
			for _, pp := range paramPlans {
				if pp == nil {
					continue
				}

				switch pp.paramType {
				case PARAM_TYPE_ENUM_CONST, PARAM_TYPE_ENUM_INPUT, PARAM_TYPE_ENUM_VAR_TEMPLATE:
					continue
				case PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE, PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW:
					dependentFieldId := pp.dependentFieldId
					//参数不能依赖于附属的field自身
					if dependentFieldId == consumerId {
						return nil, fmt.Errorf("field %d cannot depend on itself", consumerId)
					}

					//参数依赖fieldId必须存在
					if _, exists := fieldById[dependentFieldId]; !exists {
						return nil, fmt.Errorf("field %d depends on unknown field", consumerId)
					}

					//fieldId对应的step必须存在
					if _, exists := stepByFieldId[dependentFieldId]; !exists {
						return nil, fmt.Errorf("field %d depends on field %d which does not produce a FieldResponse", consumerId, dependentFieldId)
					}

					if dependencySets[consumerId] == nil {
						dependencySets[consumerId] = make(map[uint32]struct{})
					}
					if _, exists := dependencySets[consumerId][dependentFieldId]; exists {
						//多个参数允许读取同一个producer，只建立一条边
						continue
					}

					dependencySets[consumerId][dependentFieldId] = struct{}{}
					indegree[consumerId]++
					dependents[dependentFieldId] = append(dependents[dependentFieldId], consumerId)
				default:
					return nil, fmt.Errorf("field %d contains unknown param type %d", consumerId, pp.paramType)
				}
			}
		}
	}

	// 使用拓扑排序计算最长依赖层级
	levels := make(map[uint32]uint32, len(fieldIdsByStepOrder))
	ready := make([]uint32, 0, len(fieldIdsByStepOrder))

	for _, fieldId := range fieldIdsByStepOrder {
		if indegree[fieldId] == 0 {
			ready = append(ready, fieldId)
		}
	}

	processed := 0
	for readIndex := 0; readIndex < len(ready); readIndex++ {
		producerId := ready[readIndex]
		processed++

		for _, consumerId := range dependents[producerId] {
			nextLevel := levels[producerId] + 1
			if nextLevel > levels[consumerId] {
				levels[consumerId] = nextLevel
			}

			indegree[consumerId]--
			if indegree[consumerId] == 0 {
				ready = append(ready, consumerId)
			}
		}
	}

	if processed != len(fieldIdsByStepOrder) {
		cyclicFieldIds := make([]uint32, 0)
		for _, fieldId := range fieldIdsByStepOrder {
			if indegree[fieldId] != 0 {
				cyclicFieldIds = append(cyclicFieldIds, fieldId)
			}
		}
		return nil, fmt.Errorf("field dependency cycle detected:%v", cyclicFieldIds)
	}

	//按照原FieldPlan DFS顺序写入同层batch，保持编排结果稳定
	batches := make([]*BatchPlan, 0)
	for _, fieldId := range fieldIdsByStepOrder {
		targetBatchId := levels[fieldId]
		batches = ensureBatch(batches, targetBatchId, blueprint.schemaResolveInfo.operation)
		batches[targetBatchId].steps = append(batches[targetBatchId].steps, stepByFieldId[fieldId])
	}
	return batches, nil
}

// uint32返回的是当前节点下所有子节点的最大fieldId值，error是构建batch过程中的错误
func appendBatches(fieldPlan *FieldPlan, parentFieldPlan *FieldPlan, isRoot bool, fieldById map[uint32]*FieldPlan, stepById map[uint32]Step, stepOrder *[]uint32) (uint32, error) {
	if fieldPlan == nil {
		return 0, nil
	}

	if fieldPlan.fieldId == 0 {
		return 0, fmt.Errorf("field id must be greater than 0")
	}

	if _, exists := fieldById[fieldPlan.fieldId]; exists {
		return 0, fmt.Errorf("field id already defined: %d", fieldPlan.fieldId)
	}

	if !isRoot {
		//非根节点时，父亲节点不能为空
		if parentFieldPlan == nil {
			return 0, fmt.Errorf("field %d has no parent", fieldPlan.fieldId)
		}
		if fieldPlan.parentFieldId != parentFieldPlan.fieldId {
			return 0, fmt.Errorf("parent field id must match field id: %d", fieldPlan.fieldId)
		}
	}

	fieldById[fieldPlan.fieldId] = fieldPlan
	resolverFunc := fieldPlan.resolverFunc
	bulkResolverFunc := fieldPlan.bulkResolverFunc
	var step Step

	if isRoot {
		if resolverFunc == nil {
			return 0, fmt.Errorf("field %d has no resolver function", fieldPlan.fieldId)
		}
		if bulkResolverFunc != nil {
			return 0, fmt.Errorf("root field %d should not use bulk resolver", fieldPlan.fieldId)
		}
		step = &SingleCallStep{
			fieldPlan: fieldPlan,
		}
	} else {
		parentWrapperTypeInfo := parentFieldPlan.fieldWrapperTypeInfo
		if !parentWrapperTypeInfo.isList && bulkResolverFunc != nil {
			return 0, fmt.Errorf("field %d should not use bulk resolver", fieldPlan.fieldId)
		}

		if parentWrapperTypeInfo.isList {
			//如果父节点的结果是List，本节点包装成IterationCallStep
			if resolverFunc != nil || bulkResolverFunc != nil {
				step = &IterationCallStep{
					fieldPlan: fieldPlan,
				}
			}
		} else if resolverFunc != nil {
			//如果父节点的结果不是List，本节点包装成SingleCallStep
			step = &SingleCallStep{
				fieldPlan: fieldPlan,
			}
		}

		if bulkResolverFunc != nil && fieldPlan.fieldTypeScope != nil && fieldPlan.fieldTypeScope.dynamicTypeResolver != nil {
			hasExplicitParentDependency := false
			for _, paramPlan := range fieldPlan.bulkParamPlans {
				if paramPlan == nil || paramPlan.dependentFieldId != parentFieldPlan.fieldId {
					continue
				}
				if paramPlan.paramType == PARAM_TYPE_ENUM_FIELD_RESPONSE_ATTRIBUTE || paramPlan.paramType == PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW {
					hasExplicitParentDependency = true
					break
				}
			}
			if !hasExplicitParentDependency {
				return 0, fmt.Errorf("bulk resolver field %d under an abstract parent requires an explicit FIELD_RESPONSE dependency on parent field %d", fieldPlan.fieldId, parentFieldPlan.fieldId)
			}
		}

		needsParentFullResponse := step != nil && resolverFunc != nil && bulkResolverFunc == nil && (parentWrapperTypeInfo.isList || fieldPlan.fieldTypeScope.dynamicTypeResolver != nil)
		if needsParentFullResponse {
			hasParentFullResponse := false

			for _, paramPlan := range fieldPlan.paramPlans {
				if paramPlan == nil {
					continue
				}
				if paramPlan.paramType == PARAM_TYPE_ENUM_FIELD_RESPONSE_RAW && paramPlan.dependentFieldId == parentFieldPlan.fieldId {
					hasParentFullResponse = true
					break
				}
			}

			if !hasParentFullResponse {
				return 0, fmt.Errorf("field %d requires full result dependency on parent field", fieldPlan.fieldId)
			}
		}
	}

	isIntrospectionResultField := fieldPlan.parentType != nil && isIntrospectionCoordinate(fieldPlan.parentType.Name(), fieldPlan.fieldName)
	if step == nil && !isIntrospectionResultField {
		// 无resolver字段的普通参数和批量参数没有执行者
		for _, paramPlan := range fieldPlan.paramPlans {
			if paramPlan != nil {
				return 0, fmt.Errorf("field %d has param plan but no resolver", fieldPlan.fieldId)
			}
		}
		for _, bulkParamPlan := range fieldPlan.bulkParamPlans {
			if bulkParamPlan != nil {
				return 0, fmt.Errorf("field %d has bulk param plan but no bulk resolver", fieldPlan.fieldId)
			}
		}
	} else if step != nil {
		stepById[fieldPlan.fieldId] = step
		*stepOrder = append(*stepOrder, fieldPlan.fieldId)
	}

	maxFieldId := fieldPlan.fieldId
	for _, child := range fieldPlan.childrenFields {
		chlidMaxFieldId, err := appendBatches(child, fieldPlan, false, fieldById, stepById, stepOrder)
		if err != nil {
			return 0, err
		}
		if chlidMaxFieldId > maxFieldId {
			maxFieldId = chlidMaxFieldId
		}
	}
	return maxFieldId, nil
}

func ensureBatch(batches []*BatchPlan, targetId uint32, opDef ast.Definition) []*BatchPlan {
	for {
		if len(batches) <= int(targetId) {
			operationDef, operationDefOk := opDef.(*ast.OperationDefinition)
			if !operationDefOk {
				break
			}
			concurrent := operationDef.Operation == ast.OperationTypeQuery
			batches = append(batches, &BatchPlan{batchId: uint32(len(batches)), concurrent: concurrent})
		} else {
			break
		}
	}
	return batches
}
