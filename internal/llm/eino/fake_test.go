package eino

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFakeModel_Generate(t *testing.T) {
	m := NewFakeModel([]string{"hello"}, nil)
	out, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	assert.Equal(t, "hello", out.Content)
}

func TestFakeModel_Stream(t *testing.T) {
	m := NewFakeModel([]string{"abc"}, nil)
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	defer sr.Close()
	var got string
	for {
		msg, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		got += msg.Content
	}
	assert.Equal(t, "abc", got)
}

func TestFakeModel_Error(t *testing.T) {
	m := NewFakeModel(nil, errors.New("boom"))
	_, err := m.Generate(context.Background(), nil)
	require.Error(t, err)
}

func TestFakeModelRecordsImagePartsAndDescribes(t *testing.T) {
	m := NewFakeModel(nil, nil)
	m.Vision = true
	m.RecordImages = true
	b64 := "iVBORw0KGgo=" // placeholder base64; vision mode only cares about part count
	url := "data:image/png;base64," + b64
	msg := &schema.Message{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeText, Text: "describe this"},
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{MIMEType: "image/png", URL: &url},
		}},
	}}
	resp, err := m.Generate(context.Background(), []*schema.Message{msg})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if m.LastImageCount != 1 {
		t.Fatalf("LastImageCount = %d want 1", m.LastImageCount)
	}
	if !strings.Contains(resp.Content, "fake-vision") || !strings.Contains(resp.Content, "1") {
		t.Fatalf("vision reply = %q want a deterministic fake-vision(1 image) string", resp.Content)
	}
}

func TestFakeModelImageCountZeroForTextOnly(t *testing.T) {
	m := NewFakeModel([]string{"plain"}, nil)
	m.Vision = true
	m.RecordImages = true
	m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("just text"),
	})
	if m.LastImageCount != 0 {
		t.Fatalf("LastImageCount = %d want 0 for text-only input", m.LastImageCount)
	}
}

func TestFakeModel_NewWithMessages(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, Content: "pre-built"},
		{Role: schema.Assistant, Content: "second"},
	}
	m := NewFakeModelWithMessages(msgs, nil)
	got, err := m.Generate(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "pre-built" {
		t.Fatalf("expected 'pre-built', got %q", got.Content)
	}

	got, err = m.Generate(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "second" {
		t.Fatalf("expected 'second', got %q", got.Content)
	}
}

func TestFakeModel_EchoMode(t *testing.T) {
	m := NewFakeModel(nil, nil)
	m.Echo = true
	got, err := m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("hello"),
		schema.UserMessage("world"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Content, "hello") || !strings.Contains(got.Content, "world") {
		t.Fatalf("echo should contain input content, got %q", got.Content)
	}
}

func TestFakeModel_RepeatMode(t *testing.T) {
	m := NewFakeModel([]string{"first", "second"}, nil)
	m.Repeat = true
	got, err := m.Generate(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "first" {
		t.Fatalf("expected 'first', got %q", got.Content)
	}

	got, err = m.Generate(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "first" {
		t.Fatalf("Repeat mode must always return responses[0], got %q", got.Content)
	}
}

func TestFakeModel_StreamError(t *testing.T) {
	m := NewFakeModel(nil, errors.New("stream boom"))
	_, err := m.Stream(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error from Stream")
	}
}
