package http

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

// visionPNGB64 is a minimal 1x1 red PNG, base64-encoded — the payload a real
// client would paste. Kept honest (a decodable PNG) rather than an arbitrary
// blob so the non-multimodal placeholder path would also work on it.
func visionPNGB64(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// newVisionServer builds a Server whose model registry holds exactly one
// multimodal model ("vision-model"), backed by a FakeModel in Vision mode: the
// assistant reply it streams back is literally "fake-vision(N image…)", where N
// is the number of image parts that reached the model.
//
// That reply is the assertion surface for the image fan-out on every transport:
// it travels back over the same stream the test already reads, so no test
// goroutine has to touch the fake's recorded state (which would be an
// unsynchronized read under -race). N == 1 can only happen if the transport
// filled BOTH TurnOpts.Images (the attachment) and TurnOpts.ModelID (without a
// model id the orchestrator sees a non-multimodal model and swaps the image for
// a text placeholder, i.e. zero image parts).
func newVisionServer(t *testing.T) (*Server, map[string]model.BaseChatModel, *orchestrator.Orchestrator) {
	t.Helper()
	fm := einollm.NewFakeModel(nil, nil)
	fm.Vision = true
	o, err := orchestrator.New(orchestrator.Config{
		Model:         fm,
		MultimodalMap: map[string]bool{"vision-model": true},
	})
	require.NoError(t, err)
	return New(Config{Token: "t"}), map[string]model.BaseChatModel{"vision-model": fm}, o
}

// TestChatWS_UserMessageImagesReachTheModel proves the WS turn loop forwards a
// user_message frame's attachments to the orchestrator. Before this task the
// handler read only cf.Text: cf.Images existed on the wire, was decoded, and was
// then silently dropped, so a pasted screenshot never reached the model at all.
//
// ledger: G/VISION-TOOL#1 五入口各自可产生图像附件
func TestChatWS_UserMessageImagesReachTheModel(t *testing.T) {
	s, models, o := newVisionServer(t)
	s.ChatWS(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessageWithImages("what is this?",
		[]proto.ImageAttach{{Source: "paste", Fmt: "png", W: 1, H: 1, DataB64: visionPNGB64(t)}})))

	var reply strings.Builder
	for {
		f := readFrame(t, c)
		if f.Type == "agent_chunk" {
			reply.WriteString(f.Text)
		}
		if f.Type == "done" {
			break
		}
	}
	assert.Contains(t, reply.String(), "fake-vision(1 image)",
		"the WS turn must carry cf.Images into TurnOpts — got %q", reply.String())
}

// TestChat_SSE_ImagesReachTheModel proves the SSE transport forwards image
// attachments. SSE was the path both the spec and the audit missed: its request
// struct had no image field at all, so a POST carrying images was accepted
// without even a parse error and the attachments vanished.
//
// ledger: G/VISION-TOOL#1 五入口各自可产生图像附件
func TestChat_SSE_ImagesReachTheModel(t *testing.T) {
	s, models, o := newVisionServer(t)
	s.Chat(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"message":"what is this?","images":[{"source":"paste","fmt":"png","w":1,"h":1,"dataB64":"` +
		visionPNGB64(t) + `"}]}`
	req, err := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(out), "fake-vision(1 image)",
		"the SSE turn must carry request images into TurnOpts — got %q", string(out))
}
