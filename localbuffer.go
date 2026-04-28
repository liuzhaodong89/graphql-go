package graphql

import (
	"sync"

	lmap "github.com/liuzhaodong89/lockfree-collection/map"
)

var initMutex sync.Mutex

var LocalASTbuffer *lmap.Lmap[string, any]
var LocalFinishFuncBuffer *lmap.Lmap[string, any]

func init() {
	if LocalASTbuffer == nil {
		initMutex.Lock()
		defer initMutex.Unlock()
		if LocalASTbuffer == nil {
			LocalASTbuffer = lmap.New[string, any]()
		}
		if LocalFinishFuncBuffer == nil {
			LocalFinishFuncBuffer = lmap.New[string, any]()
		}
	}
}
