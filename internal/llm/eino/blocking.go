package eino

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// BlockingModel is a test double whose Generate/Stream calls block until Block
// is closed or the call's context is cancelled. It exists to make mid-turn
// cancellation deterministic in tests: a turn using BlockingModel cannot
// finish on its own, so the only way it ends is a real context cancel.
//
// Started is closed the first time the model is called, letting a test wait
// until the turn is genuinely in flight before sending a cancel frame — this
// removes the "cancel arrives before the model starts" race.
//
// When Block is closed the model returns Response; when the context fires it
// returns ctx.Err().
type BlockingModel struct {
	// Block leaves every call blocked until closed.
	Block chan struct{}
	// Started is closed on the first model call.
	Started chan struct{}
	// Response is returned once Block is closed.
	Response *schema.Message

	once sync.Once
}

// NewBlockingModel returns a BlockingModel that returns the given response
// once Block is closed, or ctx.Err() if the context fires first.
func NewBlockingModel(response string) *BlockingModel {
	return &BlockingModel{
		Block:    make(chan struct{}),
		Started:  make(chan struct{}),
		Response: schema.AssistantMessage(response, nil),
	}
}

// wait blocks until Block is closed or ctx is cancelled. It also signals
// Started on the first call.
func (m *BlockingModel) wait(ctx context.Context) (*schema.Message, error) {
	m.once.Do(func() { close(m.Started) })
	select {
	case <-m.Block:
		return m.Response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Generate blocks until Block is closed or ctx is cancelled (model.BaseChatModel).
func (m *BlockingModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return m.wait(ctx)
}

// Stream blocks until Block is closed or ctx is cancelled (model.BaseChatModel).
func (m *BlockingModel) Stream(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.wait(ctx)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray[*schema.Message]([]*schema.Message{msg}), nil
}

var _ model.BaseChatModel = (*BlockingModel)(nil)
