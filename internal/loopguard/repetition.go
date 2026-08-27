package loopguard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"sync"
)

// hashArgsLimit bounds how many bytes of a tool call's arguments feed the
// hash. Copied from QwenPaw's DoomLoopGate._hash_args (_MAX_HASH_INPUT =
// 2048): enough to distinguish "fs_read of a.go" from "fs_read of b.go"
// without pulling a whole file's contents through the hasher on every call.
const hashArgsLimit = 2048

// HashArgs reduces a tool call's raw argument blob to a short, stable
// fingerprint suitable for ToolCall.ArgsHash.
//
// The arguments are hashed rather than compared because that is what makes
// "same tool, different arguments" cheap AND correct at the same time. The
// distinction matters concretely: fs_read on one file at offset 0 and the same
// file at offset 4000 are legitimate sequential progress through a large file,
// and a repetition detector keyed only on the tool name would flag reading a
// long file as a doom loop. Because offset is part of the argument blob, the
// two calls hash differently and the window sees them as distinct.
//
// The bytes are truncated first (see hashArgsLimit), so two calls that agree
// on their first 2 KiB and differ afterwards collide. That is a deliberate
// trade from the reference: the alternative is hashing megabytes of file
// content on every fs_write, and a false "you are repeating yourself" nudge
// costs one message while the alternative costs the turn's latency budget.
func HashArgs(argsJSON string) string {
	if len(argsJSON) > hashArgsLimit {
		argsJSON = argsJSON[:hashArgsLimit]
	}
	sum := sha256.Sum256([]byte(argsJSON))
	return hex.EncodeToString(sum[:])[:16]
}

// RepetitionStage is one escalation step in RepetitionGate.
//
// After is the number of consecutive repetition hits at which the stage
// activates; the highest matching stage wins. Stop selects ActionStop,
// otherwise the stage produces ActionModifyPrompt carrying Prompt.
type RepetitionStage struct {
	// After is the consecutive-hit count at which this stage activates.
	After int
	// Stop makes this stage end the turn instead of nudging the model.
	Stop bool
	// Prompt is the injected warning (nudge stages) or the stop reason (stop
	// stages).
	Prompt string
}

// Default repetition detection parameters, copied verbatim from QwenPaw's
// DoomLoopConfig (src/qwenpaw/config/config.py): a 3-call sliding window and a
// similarity threshold of 1.0, i.e. "every call in the window is identical".
//
// The threshold is 1.0 and not something looser because the similarity formula
// (see repetitionSimilarity) is over exact signatures, so any value below 1.0
// starts flagging windows that contain genuinely different work. QwenPaw ships
// it configurable and defaults it to 1.0; the same choice is made here.
const (
	// DefaultRepetitionWindow is the sliding window size (QwenPaw:
	// DoomLoopConfig.window_size).
	DefaultRepetitionWindow = 3
	// DefaultRepetitionThreshold is the similarity value at or above which a
	// window counts as repetition (QwenPaw:
	// DoomLoopConfig.similarity_threshold).
	DefaultRepetitionThreshold = 1.0
	// DefaultRepetitionWarnAfter is the consecutive-hit count for the nudge
	// stage (QwenPaw: stages[0].after).
	DefaultRepetitionWarnAfter = 3
	// DefaultRepetitionStopAfter is the consecutive-hit count for the stop
	// stage (QwenPaw: stages[1].after).
	DefaultRepetitionStopAfter = 4
)

// defaultRepetitionWarning and defaultRepetitionStopReason are the reference's
// stage texts (QwenPaw DoomLoopConfig.stages), reworded only to name the tool
// and count that actually triggered — the reference's static string tells the
// model it is repeating without telling it what.
const (
	defaultRepetitionWarning = "[WARNING] Repetitive pattern detected: you have already called %s with these exact arguments %d times in a row without making progress. Do not call it again the same way. Either change the arguments, use a different tool, or tell the user what is blocking you."
	defaultRepetitionStop    = "repetition: %s was called with identical arguments %d times in a row without progress"
)

// DefaultRepetitionStages returns the two-stage escalation QwenPaw ships:
// warn at 3 consecutive hits, stop at 4.
func DefaultRepetitionStages() []RepetitionStage {
	return []RepetitionStage{
		{After: DefaultRepetitionWarnAfter},
		{After: DefaultRepetitionStopAfter, Stop: true},
	}
}

// RepetitionGate detects a model stuck calling the same tool with the same
// arguments, and escalates through configured stages.
//
// # What "consecutive hits" counts
//
// The reference seeds the counter with the window size on the first detection
// and increments by one per subsequent detecting iteration
// (DoomLoopGate.check). That is copied here, and it is why the default stages
// read 3 and 4: the first detection reports 3 (the window is full of identical
// calls, so three identical calls have happened), and one more identical call
// after the warning reports 4 and stops. Any non-repeating window resets the
// counter to zero, so a model that takes the nudge is not stopped for a later,
// unrelated repetition.
type RepetitionGate struct {
	window    int
	threshold float64
	stages    []RepetitionStage

	mu      sync.Mutex
	history []ToolCall
	hits    int
}

// RepetitionConfig configures a RepetitionGate. The zero value selects every
// documented default, so `NewRepetitionGate(RepetitionConfig{})` is the
// QwenPaw-equivalent gate.
type RepetitionConfig struct {
	// Window is the sliding window size. Values below 2 are raised to 2: a
	// window of one call is trivially "all identical" and would stop every
	// turn on its first tool call. The reference clamps identically
	// (max(2, window_size)).
	Window int
	// Threshold is the similarity value at or above which the window counts as
	// repetition. <= 0 selects DefaultRepetitionThreshold.
	Threshold float64
	// Stages is the escalation ladder. Empty selects DefaultRepetitionStages.
	Stages []RepetitionStage
}

// NewRepetitionGate builds a RepetitionGate from cfg.
func NewRepetitionGate(cfg RepetitionConfig) *RepetitionGate {
	window := cfg.Window
	if window <= 0 {
		window = DefaultRepetitionWindow
	}
	if window < 2 {
		window = 2
	}
	threshold := cfg.Threshold
	if threshold <= 0 {
		threshold = DefaultRepetitionThreshold
	}
	stages := cfg.Stages
	if len(stages) == 0 {
		stages = DefaultRepetitionStages()
	} else {
		stages = append([]RepetitionStage(nil), stages...)
	}
	sort.SliceStable(stages, func(i, j int) bool { return stages[i].After < stages[j].After })
	return &RepetitionGate{window: window, threshold: threshold, stages: stages}
}

// Name implements Gate.
func (g *RepetitionGate) Name() string { return "repetition" }

// Priority implements Gate. 5, matching QwenPaw's DoomLoopGate.priority: this
// runs before the budget gates so a looping model is told to change approach
// before it is told it ran out of budget.
func (g *RepetitionGate) Priority() int { return 5 }

// Check implements Gate. It appends the iteration's calls to the sliding
// window, then evaluates the window.
func (g *RepetitionGate) Check(obs Observation) Result {
	g.mu.Lock()
	defer g.mu.Unlock()

	for _, c := range obs.Calls {
		g.history = append(g.history, c)
	}
	// Bound the retained history at twice the window, as the reference does
	// (deque maxlen = window_size * 2). Only the last `window` entries are ever
	// read; the slack exists so a burst of parallel calls in one iteration does
	// not reallocate on every append.
	if max := g.window * 2; len(g.history) > max {
		g.history = append(g.history[:0], g.history[len(g.history)-max:]...)
	}

	if !g.detect() {
		g.hits = 0
		return Result{}
	}
	if g.hits == 0 {
		// First detection: the window being full of identical calls means
		// `window` identical calls have already happened. Seeding with the
		// window size (rather than 1) is what makes the stage numbers read as
		// "after N identical calls".
		g.hits = g.window
	} else {
		g.hits++
	}

	stage, ok := g.activeStage()
	if !ok {
		return Result{}
	}
	last := g.history[len(g.history)-1]
	if stage.Stop {
		reason := stage.Prompt
		if reason == "" {
			reason = fmt.Sprintf(defaultRepetitionStop, last.Name, g.hits)
		}
		return Result{Action: ActionStop, Reason: reason}
	}
	text := stage.Prompt
	if text == "" {
		text = fmt.Sprintf(defaultRepetitionWarning, last.Name, g.hits)
	}
	return Result{
		Action:       ActionModifyPrompt,
		Reason:       "repetitive tool calls detected (" + strconv.Itoa(g.hits) + " in a row)",
		Continuation: text,
	}
}

// activeStage returns the highest stage whose After threshold the current hit
// count has reached.
func (g *RepetitionGate) activeStage() (RepetitionStage, bool) {
	for i := len(g.stages) - 1; i >= 0; i-- {
		if g.hits >= g.stages[i].After {
			return g.stages[i], true
		}
	}
	return RepetitionStage{}, false
}

// detect reports whether the trailing window is repetitive.
func (g *RepetitionGate) detect() bool {
	if len(g.history) < g.window {
		return false
	}
	return repetitionSimilarity(g.history[len(g.history)-g.window:]) >= g.threshold
}

// repetitionSimilarity scores how repetitive a window of calls is, using
// QwenPaw's formula (DoomLoopGate._compute_similarity):
//
//	1 - (unique - 1) / (total - 1)
//
// which is 1.0 when every signature is identical, 0.0 when all are distinct,
// and interpolates linearly between. Windows shorter than 2 score 0.0 —
// the formula divides by total-1, and a single call is not evidence of
// anything.
func repetitionSimilarity(window []ToolCall) float64 {
	if len(window) <= 1 {
		return 0
	}
	seen := make(map[string]struct{}, len(window))
	for _, c := range window {
		seen[c.Name+":"+c.ArgsHash] = struct{}{}
	}
	return 1 - float64(len(seen)-1)/float64(len(window)-1)
}
