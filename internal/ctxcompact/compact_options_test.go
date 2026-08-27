// internal/ctxcompact/compact_options_test.go
package ctxcompact

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// optionsHistory is a history large enough to clear both entry points' gates
// (over threshold, more than keepRecent*2+1 messages) with a tool result that
// carries a credential — the shape C11 exists for.
func optionsHistory(secret string) []*schema.Message {
	return []*schema.Message{
		{Role: schema.User, Content: "read the deploy config"},
		{Role: schema.Assistant, Content: strings.Repeat("scanning files ", 100)},
		{Role: schema.Tool, ToolCallID: "c1", Content: "DEPLOY_TOKEN=" + secret + "\n" + strings.Repeat("log line ", 100)},
		{Role: schema.Assistant, Content: strings.Repeat("more analysis ", 100)},
		{Role: schema.User, Content: "and then?"},
		{Role: schema.Assistant, Content: "done"},
	}
}

// TestEntryPointsCarryOptionsToTheModel is the anti-"written but never called"
// test for the whole task.
//
// Every other test in this package reaches Run or RunSummary DIRECTLY, which is
// exactly how a feature can be fully proven and still never execute in
// production: MaybeCompact and ForceCompact are what the WS and SSE transports
// actually call, and until they grew an Options parameter there was no way for
// a Redactor to arrive. A test suite that never crosses those two functions
// would have reported C11 as complete while the redactor sat unreachable.
//
// The assertion inspects what the summarizer WAS HANDED, not what came back:
// a summarizer that ignores its input yields a clean summary whether or not the
// secret leaked, so only the recorded call inputs are evidence.
func TestEntryPointsCarryOptionsToTheModel(t *testing.T) {
	const secret = "deploy-key-778899aabb"

	entries := []struct {
		name string
		call func(msgs []*schema.Message, m model.BaseChatModel, opts Options) (bool, error)
	}{
		{
			name: "MaybeCompactWithOptions",
			call: func(msgs []*schema.Message, m model.BaseChatModel, opts Options) (bool, error) {
				// threshold 0.5 against a 2000-token window: the fixture is far
				// over it, so the pre-turn gate opens.
				_, _, _, did := MaybeCompactWithOptions(context.Background(), msgs,
					0.5, 2000, 1, m, nil, opts)
				return did, nil
			},
		},
		{
			name: "ForceCompactWithOptions",
			call: func(msgs []*schema.Message, m model.BaseChatModel, opts Options) (bool, error) {
				_, _, _, did := ForceCompactWithOptions(context.Background(), msgs,
					2000, 1, m, nil, opts)
				return did, nil
			},
		},
	}

	for _, entry := range entries {
		t.Run(entry.name+" passes the Redactor through", func(t *testing.T) {
			rs := &recordingSummarizer{Return: realSummary}
			did, err := entry.call(optionsHistory(secret), rs, Options{Redactor: newTestRedactor(secret)})
			require.NoError(t, err)
			require.True(t, did, "compaction must actually run, or nothing was proven")

			calls := append(rs.GenerateCalls, rs.StreamCalls...)
			require.NotEmpty(t, calls, "the summarizer must have been called")
			for i, call := range calls {
				for j, m := range call {
					require.NotNil(t, m)
					assert.NotContains(t, m.Content, secret,
						"%s: call[%d] msg[%d] carried the secret to the summary model — "+
							"the Options.Redactor is not reaching Run", entry.name, i, j)
				}
			}
		})

		t.Run(entry.name+" passes the quality gate through", func(t *testing.T) {
			// An acknowledgement must not be allowed to replace the history via
			// these entry points either. did=false is how that surfaces here.
			rs := &recordingSummarizer{Return: "好的，我已经总结完毕。"}
			did, err := entry.call(optionsHistory(secret), rs, Options{})
			require.NoError(t, err)
			assert.False(t, did,
				"%s reported a successful compaction for an acknowledgement: the history "+
					"would have been replaced by that sentence", entry.name)
		})

		t.Run(entry.name+" still compacts a real summary", func(t *testing.T) {
			// The contrast case: without it, an entry point wired to return
			// did=false unconditionally would pass the assertion above.
			rs := &recordingSummarizer{Return: realSummary}
			did, err := entry.call(optionsHistory(secret), rs, Options{})
			require.NoError(t, err)
			assert.True(t, did, "%s must still compact when the summary is good", entry.name)
		})

		t.Run(entry.name+" honours DisableQualityGate", func(t *testing.T) {
			rs := &recordingSummarizer{Return: "好的"}
			did, err := entry.call(optionsHistory(secret), rs, Options{DisableQualityGate: true})
			require.NoError(t, err)
			assert.True(t, did, "%s ignored the explicit opt-out", entry.name)
		})
	}
}

// TestOptions_ZeroValueReproducesHistoricalRunOpts pins the compatibility
// promise on Options: adopting the new entry points must not change behaviour
// until a field is actually set. The two literals the old code carried inline
// (ModelWindow, ChunkThreshold 0.9) are asserted explicitly because they are
// what every existing caller depends on.
func TestOptions_ZeroValueReproducesHistoricalRunOpts(t *testing.T) {
	got := Options{}.runOpts(8000)

	assert.Equal(t, 8000, got.ModelWindow)
	assert.Equal(t, 0.9, got.ChunkThreshold, "the historical chunk threshold must be preserved")
	assert.Nil(t, got.Redactor, "no redaction unless one is supplied")
	assert.False(t, got.DisableQualityGate)
	assert.Equal(t, DefaultQualityPolicy, got.qualityPolicy(),
		"an unconfigured caller still gets the default floors")
}

// TestOptions_FieldsReachRunOpts guards against a field being added to Options
// and then not copied in runOpts — a one-line omission that leaves the setting
// silently inert, which is the same defect class this whole file exists for.
func TestOptions_FieldsReachRunOpts(t *testing.T) {
	red := newTestRedactor("a-secret-value-x")
	custom := QualityPolicy{MinChars: 7, MinCompressionDenominator: 3}
	got := Options{
		Redactor:      red,
		OutputReserve: 1234,
		Quality:       custom,
	}.runOpts(100000)

	assert.Same(t, red, got.Redactor)
	assert.Equal(t, 1234, got.OutputReserve)
	assert.Equal(t, custom, got.Quality)
	assert.Equal(t, custom, got.qualityPolicy(), "an explicit policy must win over the default")
}
