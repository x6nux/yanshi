package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
)

// VisionRunner is the subset of model.BaseChatModel the image_describe tool
// needs to call the auxiliary multimodal model.
type VisionRunner interface {
	Generate(ctx context.Context, messages []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

// VisionUsageFunc records one auxiliary model call's token usage. Nil-safe.
type VisionUsageFunc func(prompt, completion, total int)

// imageDescribeArgs is the tool's JSON args shape.
type imageDescribeArgs struct {
	ImageRef string `json:"image_ref"`
	Question string `json:"question"`
}

// imageDescribeState holds the collaborators the run closure captures.
type imageDescribeState struct {
	aux   VisionRunner
	store *imagestore.Store
	root  string
	usage VisionUsageFunc
}

const defaultVisionQuestion = "请描述这张图片的内容"

// NewImageDescribeTool builds the image_describe tool as a *GuardedTool.
func NewImageDescribeTool(aux VisionRunner, store *imagestore.Store, root string, usage VisionUsageFunc) Tool {
	t := &imageDescribeState{aux: aux, store: store, root: root, usage: usage}
	return NewGuardedTool(
		"image_describe", "Image", "Describe an image via the auxiliary multimodal model. image_ref is an image id (img-N) or a file path; question is optional.",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"image_ref": {Type: schema.String, Required: true, Desc: "image id (img-N) or file path"},
			"question":  {Type: schema.String, Desc: "optional question (default: describe the image)"},
		}),
		SyncStream(t.run),
	)
}

func (t *imageDescribeState) run(ctx context.Context, argsJSON string) (string, error) {
	var args imageDescribeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errorResult("invalid args: " + err.Error()), nil
	}
	if t.aux == nil {
		return errorResult("主模型非多模态且未配置 multimodal: true 的 provider；请在 config 里加一个 multimodal provider 作 vision 辅助"), nil
	}
	question := strings.TrimSpace(args.Question)
	if question == "" {
		question = defaultVisionQuestion
	}
	// Trimmed because the model reads the ref out of a placeholder that reads
	// "[image:img-1 | attach | 1x1 png]" — the id is followed by a space before
	// the separator, and copying "img-1 " out of it is the natural mistake.
	// Untrimmed, that lookup misses and the answer is "not found in store" for
	// an image that is right there.
	imgBytes, fmtName, err := t.resolveRef(ctx, strings.TrimSpace(args.ImageRef), argsJSON)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	msg := buildVisionMessage(imgBytes, fmtName, question)
	resp, err := t.aux.Generate(ctx, []*schema.Message{msg})
	if err != nil {
		return errorResult("辅助模型调用失败：" + err.Error()), nil
	}
	t.recordUsage(ctx, resp)
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return errorResult("辅助模型未返回描述"), nil
	}
	return resp.Content, nil
}

// resolveRef parses image_ref: img-N goes through store; otherwise treated as
// a path (guard FS check + read bytes). ctx must carry the injected profile.
func (t *imageDescribeState) resolveRef(ctx context.Context, ref, argsJSON string) ([]byte, string, error) {
	if strings.HasPrefix(ref, "img-") {
		e, ok := t.store.Get(ref)
		if !ok {
			return nil, "", fmt.Errorf("image %q not found in store", ref)
		}
		return e.Bytes, e.Fmt, nil
	}
	if t.root == "" {
		return nil, "", fmt.Errorf("path refs require a work root; use an image id (img-N) instead")
	}
	// Path jail: use withinRootAbs (pathjail canonical kernel) so symlink escape
	// is blocked (filepath.EvalSymlinks + volume + Windows case). The previous
	// strings.HasPrefix check did NOT resolve symlinks, so a symlink inside the
	// work root pointing outside let image_describe read arbitrary files.
	absPath, err := withinRootAbs(t.root, filepath.Join(t.root, ref))
	if err != nil {
		return nil, "", fmt.Errorf("path %q escapes the work root: %w", ref, err)
	}
	// FS guard check: image_describe path refs must pass FS guard (read op).
	if err := Authorize(ctx, guard.Action{
		Tool: "image_describe",
		FS:   guard.FSWant{Op: "read", Paths: []string{absPath}},
	}, argsJSON); err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, "", err
	}
	return data, detectFmt(ref), nil
}

func (t *imageDescribeState) recordUsage(ctx context.Context, resp *schema.Message) {
	if resp == nil || resp.ResponseMeta == nil || resp.ResponseMeta.Usage == nil {
		return
	}
	u := resp.ResponseMeta.Usage
	if t.usage != nil {
		t.usage(int(u.PromptTokens), int(u.CompletionTokens), int(u.TotalTokens))
	}
	// The auxiliary model's tokens are spent on the caller's behalf and have to
	// land in the caller's ledger, which is what the turn's usage sink is.
	//
	// VisionUsageFunc above accumulates them too, but that accumulator has no
	// reader: it is a process-wide counter, while cost is tracked per session,
	// so it could never have reached /cost. Sub-agent spend already travels
	// this way (see the sink call in runSubAgentTurn) for the same reason —
	// work delegated to another model is still the caller's bill.
	if sink := UsageSinkFrom(ctx); sink != nil {
		sink(registry.Usage{
			PromptTokens:     int64(u.PromptTokens),
			CompletionTokens: int64(u.CompletionTokens),
			TotalTokens:      int64(u.TotalTokens),
			ModelCalls:       1,
		})
	}
}

// buildVisionMessage assembles a single user message with [image part + question].
func buildVisionMessage(imgBytes []byte, fmtName, question string) *schema.Message {
	mime := mimeForFmt(fmtName)
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(imgBytes)
	return &schema.Message{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{MIMEType: mime, URL: &dataURL},
		}},
		{Type: schema.ChatMessagePartTypeText, Text: question},
	}}
}

func mimeForFmt(f string) string {
	switch f {
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func detectFmt(path string) string {
	low := strings.ToLower(path)
	switch {
	case strings.HasSuffix(low, ".png"):
		return "png"
	case strings.HasSuffix(low, ".gif"):
		return "gif"
	case strings.HasSuffix(low, ".webp"):
		return "webp"
	default:
		return "jpeg"
	}
}

// IsImagePath reports whether path's extension is one of the Tier G image
// formats. Shared by the @path TUI entry (entry B), fs_read/web_fetch (entry C),
// and image_describe's path ref.
func IsImagePath(path string) bool {
	low := strings.ToLower(path)
	switch {
	case strings.HasSuffix(low, ".png"),
		strings.HasSuffix(low, ".jpg"),
		strings.HasSuffix(low, ".jpeg"),
		strings.HasSuffix(low, ".gif"),
		strings.HasSuffix(low, ".webp"):
		return true
	}
	return false
}
