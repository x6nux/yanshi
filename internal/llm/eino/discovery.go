package eino

// W-C-03/05/06/15: local model-runtime discovery — Ollama, LM Studio, a disk
// cache for both, and a real (not config-trusted) image-support probe.
//
// This is a DIFFERENT "discovery" than discover.go's M9 preflight.
// discover.go asks one question about a cloud or gateway provider that is
// already configured with a model id: "does the endpoint's catalogue
// contain this exact name?" — advisory, startup-only, one shot. This file
// (and its discovery_ollama.go/discovery_lmstudio.go/discovery_cache.go
// siblings) answers a different question about a LOCAL runtime that has no
// fixed model list at all: "what does it have right now, is it even
// running, and does what it has actually accept images?" — callable at any
// time, not just startup, because the answer changes as an operator runs
// `ollama pull` or loads a different model in LM Studio's UI.
//
// RELATIONSHIP TO THE MODEL CATALOG (ADR-0024, ADR-0025): models.yaml is a
// //go:embed file — compiled into the binary, structurally immutable at
// runtime. Nothing in these four files writes to it, overrides it, or even
// imports modelcatalog.go/contextwindow.go/pricing.go. Discovery answers
// "what EXISTS on this machine right now" (an inventory question the static
// table cannot answer — an operator can pull any of thousands of Ollama
// tags); the catalog answers "what do we already believe about a model id
// we were told to use" (a capability question). A caller that wants to feed
// a discovered fact (LM Studio's max_context_length, a probed ImageSupport)
// into the resolution ladder does so through the EXISTING override path —
// ProviderConfig.ContextWindow / ProviderConfig.Multimodal, both
// operator-reviewed config edits — not an automatic write discovery
// performs itself. See ADR-0025 for the full reasoning and the rejected
// alternatives (auto-write into models.yaml, auto-write into
// ProviderConfig, a fourth override tier).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"time"
)

// DiscoveredModel is one model a local runtime reported as available,
// normalized across Ollama's and LM Studio's different wire shapes so
// callers (the cache, a future model picker) can treat both the same way.
type DiscoveredModel struct {
	// ID is the string a caller would put in ProviderConfig.Model — Ollama's
	// "name" (e.g. "llama3:latest") or LM Studio's "id".
	ID string `json:"id"`
	// ContextWindow is the runtime's reported window, 0 when it did not say.
	// Ollama's /api/tags never reports one; LM Studio's /api/v0/models does.
	ContextWindow int `json:"context_window,omitempty"`
	// Loaded is true when the runtime reports this model as currently
	// resident in memory (LM Studio's "state":"loaded"). Ollama's /api/tags
	// has no equivalent concept — every entry it returns is a model pulled
	// to disk, not necessarily loaded — so this is always false for Ollama
	// results; callers must not read a false here as "confirmed not loaded"
	// for that runtime, only as "this runtime doesn't report loaded state".
	Loaded bool `json:"loaded,omitempty"`
	// DeclaredMultimodal is the RUNTIME's own claim, not a probe (see
	// ImageSupport / ProbeImageSupport below for the measured version): LM
	// Studio reports "type":"vlm" for vision models. Ollama's /api/tags has
	// no comparable field, so this is always false for Ollama results.
	DeclaredMultimodal bool `json:"declared_multimodal,omitempty"`
}

// ProbeResult reports whether a local runtime answered a reachability
// check.
//
// Endpoints records which individual health signals were checked and each
// one's outcome — Ollama's Probe checks two ("root", "api_tags") so a daemon
// that answers the root ping but fails /api/tags can be reported as "up but
// broken" instead of collapsing to the same false Available a fully-down
// daemon would produce. See OllamaClient.Probe / LMStudioClient.Probe.
type ProbeResult struct {
	Available bool            `json:"available"`
	Detail    string          `json:"detail"`
	Endpoints map[string]bool `json:"endpoints"`
}

// localHTTPClient returns an *http.Client whose transport never routes
// through an operator-configured HTTP(S) proxy.
//
// Every other client in this package (provider.go, anthropic.go,
// responses.go) uses http.DefaultTransport, whose Proxy defaults to
// http.ProxyFromEnvironment — correct for reaching a cloud provider, wrong
// here. Ollama and LM Studio are reached over 127.0.0.1 specifically
// because they are local; routing that traffic through a corporate proxy is
// never correct, and when the proxy cannot reach back to loopback it turns
// "the daemon is not running" and "the proxy is unreachable" into the same
// opaque timeout, defeating the "report truthfully when unavailable"
// acceptance bullet (W-C-03/W-C-05). internal/netpolicy/proxy.go's own
// transport makes the identical Proxy: nil choice for the same reason.
func localHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   timeout,
	}
}

// DefaultDiscoveryHTTPTimeout bounds one local-runtime request (a probe, a
// listing, or the non-streaming half of a load). It is longer than
// discover.go's DefaultDiscoveryTimeout (5s, tuned for a cloud gateway's
// control plane) because a local machine under load — the exact situation
// an operator is probing FROM — can take longer to answer a loopback
// request than a remote SaaS API answers a WAN one.
const DefaultDiscoveryHTTPTimeout = 10 * time.Second

// MultimodalSource marks whether an ImageSupport verdict came from
// configuration/metadata or from an actual probe — W-C-15's "标记来源是文档
// 还是实测" acceptance bullet. This is the whole reason ImageSupport carries
// a Source field instead of being a plain bool: a caller comparing two
// verdicts must never be able to treat "the vendor's docs (or the runtime's
// own self-reported metadata) say so" and "we sent an image and watched the
// model describe it" as the same kind of evidence.
type MultimodalSource string

const (
	// SourceDocumented means the claim came from static configuration or a
	// runtime's own self-reported metadata (ProviderConfig.Multimodal, LM
	// Studio's "type":"vlm") — asserted, never exercised.
	SourceDocumented MultimodalSource = "documented"
	// SourceProbed means yanshi sent a real image to the endpoint and
	// evaluated the response.
	SourceProbed MultimodalSource = "probed"
)

// ImageSupport is one verdict about whether a model accepts image input,
// tagged with where the verdict came from.
type ImageSupport struct {
	Supported bool             `json:"supported"`
	Source    MultimodalSource `json:"source"`
	// Detail explains a false verdict (the probe's rejection/failure
	// reason) or names the origin of a documented claim.
	Detail string `json:"detail"`
}

// DocumentedImageSupport builds an ImageSupport for a claim that was never
// exercised. origin names where the claim came from (e.g.
// "ProviderConfig.Multimodal", "LM Studio type=vlm") so a report mixing
// sources can explain why a documented verdict should carry less weight
// than a probed one.
func DocumentedImageSupport(supported bool, origin string) ImageSupport {
	return ImageSupport{Supported: supported, Source: SourceDocumented, Detail: origin}
}

// probeImagePNG returns a freshly PNG-encoded 1x1 solid-red pixel, built
// with the standard library rather than embedded as a hand-written base64
// literal — image/png's encoder is the one authority on "this is a valid
// PNG" that a byte string typed from memory is not.
func probeImagePNG() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0xff, G: 0x00, B: 0x00, A: 0xff})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("eino: discovery: encode probe image: %w", err)
	}
	return buf.Bytes(), nil
}

// chatProbeRequest/chatProbeResponse mirror just the fields ProbeImageSupport
// needs from the OpenAI-compatible chat-completions wire shape that both
// Ollama's and LM Studio's /v1 surfaces serve.
type chatProbeRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []chatProbeMessage `json:"messages"`
}

type chatProbeMessage struct {
	Role    string             `json:"role"`
	Content []chatProbeContent `json:"content"`
}

type chatProbeContent struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	ImageURL *chatProbeImgURL `json:"image_url,omitempty"`
}

type chatProbeImgURL struct {
	URL string `json:"url"`
}

type chatProbeResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// probeImagePrompt asks for a one-word answer so ProbeImageSupport's
// classification doesn't have to parse prose, and offers an explicit
// negative token (NO_VISION) so a text-only model that DOES follow
// instructions has a truthful answer available instead of being forced to
// guess a color.
const probeImagePrompt = "Look at the attached image and reply with exactly one word: RED if the image is solid red, or NO_VISION if you cannot see images at all. Do not explain."

// ProbeImageSupport sends a real, freshly-encoded solid-red 1x1 PNG to an
// OpenAI-compatible chat-completions endpoint and classifies whether the
// model actually saw it — not merely whether the HTTP call succeeded. This
// is the "实测" (measured) half of W-C-15, paired with DocumentedImageSupport
// for the "documentation" half; ImageSupport.Source tells a caller which one
// produced a given verdict.
//
// A 200 response alone is weak evidence: some local serving stacks accept an
// image_url content part structurally and answer from the text portion of
// the prompt alone, silently ignoring the image. Asking the model to name
// the color and checking the answer catches that silent-ignore case; asking
// it to say NO_VISION explicitly gives an honest exit to a backend that
// rejects image content on a non-vision model with a clear inline message
// instead of a wrong guess. A response that is neither is reported as
// unsupported with a detail explaining the ambiguity, rather than guessed
// either way — see the default case below.
//
// client may be nil, in which case a loopback-only client (localHTTPClient)
// with DefaultDiscoveryHTTPTimeout is used; pass a client explicitly when
// probing something other than a bare loopback address. apiKey may be empty
// (most local runtimes require none).
func ProbeImageSupport(ctx context.Context, client *http.Client, chatCompletionsURL, apiKey, model string) ImageSupport {
	imgPNG, err := probeImagePNG()
	if err != nil {
		return ImageSupport{Source: SourceProbed, Detail: err.Error()}
	}
	imgB64 := base64.StdEncoding.EncodeToString(imgPNG)
	reqBody := chatProbeRequest{
		Model:     model,
		MaxTokens: 10,
		Messages: []chatProbeMessage{{
			Role: "user",
			Content: []chatProbeContent{
				{Type: "text", Text: probeImagePrompt},
				{Type: "image_url", ImageURL: &chatProbeImgURL{URL: "data:image/png;base64," + imgB64}},
			},
		}},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return ImageSupport{Source: SourceProbed, Detail: fmt.Sprintf("encode probe request: %v", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatCompletionsURL, bytes.NewReader(payload))
	if err != nil {
		return ImageSupport{Source: SourceProbed, Detail: fmt.Sprintf("build probe request: %v", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if client == nil {
		client = localHTTPClient(DefaultDiscoveryHTTPTimeout)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ImageSupport{Source: SourceProbed, Detail: fmt.Sprintf("probe request failed: %v", err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return ImageSupport{Source: SourceProbed, Detail: fmt.Sprintf("read probe response: %v", err)}
	}
	var decoded chatProbeResponse
	_ = json.Unmarshal(body, &decoded) // best-effort: a non-JSON body leaves decoded zero-valued, handled below.
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(body))
		if decoded.Error != nil && decoded.Error.Message != "" {
			detail = decoded.Error.Message
		}
		return ImageSupport{Source: SourceProbed, Detail: fmt.Sprintf("endpoint rejected the image (HTTP %d): %s", resp.StatusCode, detail)}
	}
	if len(decoded.Choices) == 0 {
		return ImageSupport{Source: SourceProbed, Detail: "endpoint returned no completion choices"}
	}
	answer := strings.ToUpper(strings.TrimSpace(decoded.Choices[0].Message.Content))
	switch {
	case strings.Contains(answer, "NO_VISION"):
		return ImageSupport{Source: SourceProbed, Detail: "model reported it cannot see images"}
	case strings.Contains(answer, "RED"):
		return ImageSupport{Supported: true, Source: SourceProbed, Detail: "model correctly named the probe image's color"}
	default:
		return ImageSupport{Source: SourceProbed, Detail: fmt.Sprintf("model responded but did not identify the probe image (got %q) — the endpoint likely accepted the image structurally without processing it", answer)}
	}
}
