package orchestrator

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/imagestore"
	"github.com/x6nux/yanshi/internal/proto"
)

// validPNGb64 encodes a 1x1 PNG as base64 (a real, decodable image).
func validPNGb64(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func newImageStore() *imagestore.Store {
	return imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
}

// TestCov_AppendPlaceholders_DecodeError covers the base64-decode-fail branch
// and the resulting "no valid images → history unchanged" branch.
func TestCov_AppendPlaceholders_DecodeError(t *testing.T) {
	o := &Orchestrator{imageStore: newImageStore()}
	history := []*schema.Message{schema.UserMessage("hi")}
	result := o.appendPlaceholders(history, []proto.ImageAttach{
		{Source: "t", Fmt: "png", DataB64: "!!!not-base64!!!"},
	})
	assert.Equal(t, history, result, "decode failure → no placeholder → unchanged")
}

// TestCov_AppendPlaceholders_PutError covers the imageStore.Put-error branch
// (unsupported format → Put rejects → no placeholder).
func TestCov_AppendPlaceholders_PutError(t *testing.T) {
	o := &Orchestrator{imageStore: newImageStore()}
	history := []*schema.Message{schema.UserMessage("hi")}
	// Valid base64 but an unsupported image format → Put fails.
	result := o.appendPlaceholders(history, []proto.ImageAttach{
		{Source: "t", Fmt: "bmp", DataB64: base64.StdEncoding.EncodeToString([]byte("bytes"))},
	})
	assert.Equal(t, history, result, "Put failure → no placeholder → unchanged")
}

// TestCov_AppendPlaceholders_AppendToLastUser covers the success path where the
// placeholder is appended to an existing trailing user message.
func TestCov_AppendPlaceholders_AppendToLastUser(t *testing.T) {
	o := &Orchestrator{imageStore: newImageStore()}
	history := []*schema.Message{schema.UserMessage("hello")}
	result := o.appendPlaceholders(history, []proto.ImageAttach{
		{Source: "t", Fmt: "png", DataB64: validPNGb64(t)},
	})
	require.Len(t, result, 1)
	assert.Contains(t, result[0].Content, "hello")
	assert.Contains(t, result[0].Content, "img-", "placeholder appended to the last user message")
}

// TestCov_AppendPlaceholders_NewUserMsg covers the success path where there is
// no trailing user message, so a new user message carrying the placeholder is
// appended.
func TestCov_AppendPlaceholders_NewUserMsg(t *testing.T) {
	o := &Orchestrator{imageStore: newImageStore()}
	result := o.appendPlaceholders(nil, []proto.ImageAttach{
		{Source: "t", Fmt: "png", DataB64: validPNGb64(t)},
	})
	require.Len(t, result, 1)
	assert.Contains(t, result[0].Content, "img-", "placeholder added as a new user message")
}
