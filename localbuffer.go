package graphql

import (
	lmap "github.com/liuzhaodong89/lockfree-collection/map"
	"sync"
)

var initMutex sync.Mutex

var LocalASTbuffer *lmap.Lmap
var LocalFinishFuncBuffer *lmap.Lmap

func init() {
	if LocalASTbuffer == nil {
		initMutex.Lock()
		defer initMutex.Unlock()
		if LocalASTbuffer == nil {
			LocalASTbuffer = lmap.New()
		}
		if LocalFinishFuncBuffer == nil {
			LocalFinishFuncBuffer = lmap.New()
		}
	}
}
