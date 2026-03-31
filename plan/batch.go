package plan

import (
	"context"
	"sync"
)

type Batch struct {
	id    uint32
	child *Batch
	steps []*Step
}

func (b *Batch) GetId() uint32 {
	return b.id
}

func (b *Batch) SetId(id uint32) {
	b.id = id
}

func (b *Batch) GetChild() *Batch {
	return b.child
}

func (b *Batch) SetChild(child *Batch) {
	b.child = child
}

func (b *Batch) GetSteps() []*Step {
	return b.steps
}

func (b *Batch) AddStep(step *Step) {
	if b.steps == nil {
		b.steps = make([]*Step, 0)
	}
	b.steps = append(b.steps, step)
}

func (b *Batch) Execute(rundata *RunData, ctx *context.Context) (interface{}, error) {
	if b.steps != nil {
		wg := sync.WaitGroup{}
		wg.Add(len(b.steps))

		for _, step := range b.steps {
			go func() {
				exeRes, exeErr := (*step).Execute(rundata, ctx)
				if exeErr != nil {

				}
				if exeRes != nil {

				}
				wg.Done()
			}()
		}
		wg.Wait()
	}
	return nil, nil
}
