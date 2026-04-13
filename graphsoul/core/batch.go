package core

import (
	"context"
	"sync"
	"sync/atomic"
)

type BatchResult struct {
	interrupt bool
}

func (br *BatchResult) IsInterrupt() bool {
	return br.interrupt
}

type Batch struct {
	concurrent bool
	steps      []Step
}

func (b *Batch) Execute(rundata *Rundata, ctx context.Context) *BatchResult {
	if b.concurrent && len(b.steps) > 1 {
		//并发执行step
		semaphore := atomic.Uint32{}
		wg := sync.WaitGroup{}
		wg.Add(len(b.steps))

		for _, step := range b.steps {
			go func() {
				fieldErr := step.Execute(rundata, ctx)
				if fieldErr != nil && fieldErr.fieldType == FIELD_ERROR_TYPE_TREE {
					semaphore.Add(1)
				}
				wg.Done()
			}()
		}

		wg.Wait()
		//判断是否有需要中断整个流程的错误
		if semaphore.Load() >= 1 {
			return &BatchResult{interrupt: true}
		}
	} else {
		//串行执行step
		for _, step := range b.steps {
			fieldErr := step.Execute(rundata, ctx)
			if fieldErr != nil && fieldErr.fieldType == FIELD_ERROR_TYPE_TREE {
				//判断是否有需要中断整个流程的错误
				return &BatchResult{interrupt: true}
			}
		}
	}
	return nil
}
