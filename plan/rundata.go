package plan

import (
	"errors"
	"fmt"

	lmap "github.com/liuzhaodong89/lockfree-collection/map"
)

type RunData struct {
	originalParams *lmap.Lmap
	nodeResults    *lmap.Lmap
	nodeErrors     *lmap.Lmap
}

func (r *RunData) GetOriginalParams() *lmap.Lmap {
	return r.originalParams
}

func (r *RunData) SetOriginalParams(params map[string]interface{}) {
	if r.originalParams == nil {
		r.originalParams = lmap.New()
	}
	for k, v := range params {
		r.originalParams.Set(k, v)
	}
}

func (r *RunData) GetOriginalParamValueByName(name string) (interface{}, error) {
	if r.originalParams == nil {
		return nil, fmt.Errorf("no original param found: %s because params is nil", name)
	}
	val, exist := r.originalParams.Get(name)
	if !exist {
		return nil, fmt.Errorf("no original param found: %s", name)
	}
	return val, nil
}

func (r *RunData) GetNodeResultByNodeId(nodeId uint32) (interface{}, error) {
	if r.nodeResults == nil {
		return nil, fmt.Errorf("no node results found for node: %d because results is nil", nodeId)
	}
	val, exist := r.nodeResults.Get(nodeId)
	if !exist {
		return nil, fmt.Errorf("no node results found for node: %d", nodeId)
	}
	return val, nil
}

func (r *RunData) GetNodeErrorByNodeId(nodeId uint32) error {
	if r.nodeErrors == nil {
		return nil
	}
	val, exist := r.nodeErrors.Get(nodeId)
	if !exist {
		return nil
	}
	if err, ok := val.(error); ok {
		return err
	}
	return nil
}

func (r *RunData) SetNodeResultByNodeId(nodeId uint32, result interface{}) error {
	if r.nodeResults == nil {
		r.nodeResults = lmap.New()
	}
	temp, _ := r.nodeResults.Get(nodeId)
	if temp == nil {
		r.nodeResults.Set(nodeId, result)
		return nil
	} else {
		return errors.New("result is already set")
	}
}

func (r *RunData) SetNodeErrorByNodeId(nodeId uint32, err error) error {
	if r.nodeErrors == nil {
		r.nodeErrors = lmap.New()
	}
	temp, _ := r.nodeErrors.Get(nodeId)
	if temp == nil {
		r.nodeErrors.Set(nodeId, err)
		return nil
	} else {
		return errors.New("error is already set")
	}
}
