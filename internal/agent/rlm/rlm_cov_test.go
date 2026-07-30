package rlm

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// nilMsgModel wraps FakeModel (so it still satisfies model.BaseChatModel via the
// embedded Stream) but returns a nil message with no error, exercising Runner's
// "model returned nil message" branch.
type nilMsgModel struct{ *einollm.FakeModel }

func (m *nilMsgModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return nil, nil
}

// TestCov_Run_EmptyPrompts covers the no-prompts early return.
func TestCov_Run_EmptyPrompts(t *testing.T) {
	r := Runner{Model: einollm.NewFakeModel([]string{"x"}, nil)}
	res, err := r.Run(context.Background(), nil)
	assert.NoError(t, err)
	assert.Empty(t, res)
}

// TestCov_Run_NilMessage covers the "model returned nil message" error branch.
func TestCov_Run_NilMessage(t *testing.T) {
	r := Runner{Model: &nilMsgModel{FakeModel: einollm.NewFakeModel(nil, nil)}}
	res, err := r.Run(context.Background(), []string{"prompt"})
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Contains(t, res[0].Error, "nil message")
}
