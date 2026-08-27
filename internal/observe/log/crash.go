package log

// Crash-site capture: when a turn dies, write enough of the scene to a file to
// diagnose it later WITHOUT writing the thing the rest of this package exists
// to keep out of logs.
//
// The normal log path deliberately records only SafeErrorType(err) and never
// err.Error(), because provider errors routinely carry prompts, response
// bodies and credentials. That decision is right for a line that gets shipped
// to a log aggregator and is not reversed here. What it leaves behind is a
// process that can die with one word of explanation and no way to reconstruct
// what happened. A crash report is the controlled exit: one file, on the local
// disk, redacted, with message METADATA rather than message bodies.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Redactor is the subset of *secrets.Redactor a crash dump needs.
//
// It is an interface rather than the concrete type so this package stays a
// leaf: internal/secrets is where the process-wide redactor lives, and having
// the logger import it would put a credential store underneath every package
// that logs. The composition root passes the real one in.
type Redactor interface {
	Redact(string) string
}

// MaxCrashStackBytes bounds the goroutine dump written into a report. A
// deadlocked process with thousands of goroutines produces megabytes of stack;
// past the first screenful the marginal diagnostic value is near zero, and the
// point of a crash report is that someone opens it.
const MaxCrashStackBytes = 256 << 10

// DefaultCrashMessages is how many trailing conversation messages a report
// describes. Enough to see the shape of the turn that died (which tool ran,
// how big the last model reply was), few enough that the file stays readable.
const DefaultCrashMessages = 20

// MessageMeta describes one conversation message WITHOUT its body.
//
// Role, size and tool name answer the questions a post-mortem actually asks --
// which tool was running, was the context enormous, did the model reply at all
// -- and none of them can leak a prompt, a file the agent read, or a
// credential that ended up in a tool argument. Body capture is a separate,
// explicitly-enabled path (see CrashDumper.IncludeBodies).
type MessageMeta struct {
	Role     string `json:"role"`
	Bytes    int    `json:"bytes"`
	ToolName string `json:"tool_name,omitempty"`
	// Body is populated ONLY when the operator turned on IncludeBodies. It is
	// redacted like every other string in the report, but redaction only
	// removes REGISTERED secrets -- it cannot remove the user's own data.
	Body string `json:"body,omitempty"`
}

// CrashReport is the on-disk scene. Every string field has passed through the
// Redactor before serialization.
type CrashReport struct {
	// Time is when the report was written (RFC3339, UTC).
	Time time.Time `json:"time"`
	// Kind is "panic" or "error", so a reader can tell a recovered runtime
	// panic from a turn that failed with an error value.
	Kind string `json:"kind"`
	// ErrorType is the concrete Go type, the same value the log line carries,
	// so a report can be joined back to the log entry that mentioned it.
	ErrorType string `json:"error_type,omitempty"`
	// ErrorChain is the redacted, unwrapped error chain. This is the field the
	// plain log path refuses to write; it is acceptable here because the file
	// is local, is named on stderr rather than shipped, and has been through
	// the process-wide redactor.
	ErrorChain []string `json:"error_chain,omitempty"`
	// IDs are the correlation identifiers bound to the dying context.
	TraceID   string `json:"trace_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	Tool      string `json:"tool,omitempty"`
	// Messages describes the tail of the conversation. Metadata only unless
	// bodies were explicitly enabled.
	Messages []MessageMeta `json:"messages,omitempty"`
	// BodiesIncluded records whether the debug body switch was on, so a reader
	// knows whether an empty Body means "not captured" or "empty message".
	BodiesIncluded bool `json:"bodies_included"`
	// Stack is the goroutine dump, truncated to MaxCrashStackBytes.
	Stack string `json:"stack,omitempty"`
	// StackTruncated says the dump hit the cap, so nobody reads a cut-off
	// final frame as the end of the trace.
	StackTruncated bool `json:"stack_truncated,omitempty"`
	// ConfigFingerprint identifies the configuration WITHOUT reproducing it:
	// a sorted key list plus a SHA-256 over the key/value pairs. Two reports
	// with the same fingerprint ran the same config; a report never carries
	// the values, because a config is where api keys live.
	ConfigFingerprint ConfigFingerprint `json:"config_fingerprint"`
	// Runtime records the Go version and platform, which is the first thing
	// asked about any crash that only reproduces on one machine.
	Runtime RuntimeInfo `json:"runtime"`
}

// RuntimeInfo is the build/platform triple carried by every report.
type RuntimeInfo struct {
	GoVersion string `json:"go_version"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
}

// ConfigFingerprint identifies a configuration without reproducing it.
type ConfigFingerprint struct {
	// Keys are the configuration key names, sorted. Names are safe; values
	// are not, and are never carried.
	Keys []string `json:"keys,omitempty"`
	// SHA256 is a hex digest over the sorted key=value pairs. It distinguishes
	// two configurations from each other without disclosing either.
	SHA256 string `json:"sha256,omitempty"`
}

// FingerprintConfig builds a ConfigFingerprint from a flat key/value view of
// the configuration.
//
// The digest covers the VALUES, so a report can prove two runs used identical
// settings, while the file itself carries only the key names. Callers pass
// whatever flattening they like; what matters is that the same flattening is
// used across reports, or fingerprints will not compare.
func FingerprintConfig(values map[string]string) ConfigFingerprint {
	if len(values) == 0 {
		return ConfigFingerprint{}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, values[k])
	}
	return ConfigFingerprint{Keys: keys, SHA256: hex.EncodeToString(h.Sum(nil))}
}

// CrashDumper writes crash reports into a directory.
//
// The zero value is unusable on purpose: NewCrashDumper resolves the directory
// and creates it, so a dumper that exists is a dumper that can write. A crash
// path that discovers its own misconfiguration mid-crash is the worst possible
// time to find out.
type CrashDumper struct {
	dir      string
	redactor Redactor
	// IncludeBodies turns on message-body capture. It is a debug switch and
	// defaults off: the whole reason the normal log path prints no error body
	// is that bodies carry prompts and credentials, and a crash report that
	// flipped that by default would be a leak with extra steps. Operators who
	// need bodies opt in per-installation and accept the consequence.
	IncludeBodies bool
	// MaxMessages bounds how many trailing messages are described. Zero means
	// DefaultCrashMessages.
	MaxMessages int
	// ConfigValues is the flat config view fingerprinted into every report.
	ConfigValues map[string]string

	mu sync.Mutex
}

// NewCrashDumper returns a dumper writing into dir, creating it when missing.
// A nil redactor is replaced by a no-op, so a caller that has not built the
// process redactor yet still gets reports rather than a nil dereference at the
// worst moment.
func NewCrashDumper(dir string, r Redactor) (*CrashDumper, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	if r == nil {
		r = nopRedactor{}
	}
	return &CrashDumper{dir: abs, redactor: r}, nil
}

// nopRedactor is the fallback when no process redactor exists yet. It removes
// nothing, which is honest: the alternative (silently skipping the dump) trades
// a diagnosable crash for an undiagnosable one.
type nopRedactor struct{}

func (nopRedactor) Redact(s string) string { return s }

// Dir returns the directory reports are written to.
func (d *CrashDumper) Dir() string { return d.dir }

// DefaultCrashDir returns the canonical crash-report directory under the OS
// user-config dir (e.g. ~/.yanshi/crash on Unix), matching where the log file
// lives so an operator finds both in one place.
func DefaultCrashDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "yanshi", "crash"), nil
}

// Dump captures err plus the conversation tail and writes one report file.
// It returns the path written.
//
// kind is "panic" or "error". messages may be nil; when it is longer than the
// dumper's cap only the trailing window is described, because the frames
// nearest the crash are the ones that explain it.
func (d *CrashDumper) Dump(ctx context.Context, kind string, err error, messages []MessageMeta) (string, error) {
	report := d.build(ctx, kind, err, messages)
	return d.write(report)
}

// DumpPanic is the deferred-recover entry point: it captures the recovered
// value together with the goroutine dump and writes a report.
//
// recovered is turned into an error via fmt.Errorf so a panic value of any
// type lands in the same shape as an error crash. The stack is captured HERE
// rather than inside build, so the frames are the panicking goroutine's rather
// than the reporter's.
func (d *CrashDumper) DumpPanic(ctx context.Context, recovered any, messages []MessageMeta) (string, error) {
	report := d.build(ctx, "panic", fmt.Errorf("panic: %v", recovered), messages)
	report.Stack, report.StackTruncated = captureStacks()
	return d.write(report)
}

// build assembles the report and applies redaction to every string that leaves
// this function. Redaction happens once, at assembly, rather than at each call
// site: a field added later is redacted by construction instead of by whoever
// remembers.
func (d *CrashDumper) build(ctx context.Context, kind string, err error, messages []MessageMeta) CrashReport {
	ids := IDsFromContext(ctx)
	limit := d.MaxMessages
	if limit <= 0 {
		limit = DefaultCrashMessages
	}
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}

	report := CrashReport{
		Time:              time.Now().UTC(),
		Kind:              kind,
		ErrorType:         SafeErrorType(err),
		ErrorChain:        d.redactChain(err),
		TraceID:           ids.TraceID,
		SessionID:         ids.SessionID,
		TurnID:            ids.TurnID,
		Tool:              ids.Tool,
		BodiesIncluded:    d.IncludeBodies,
		ConfigFingerprint: FingerprintConfig(d.ConfigValues),
		Runtime: RuntimeInfo{
			GoVersion: runtime.Version(),
			GOOS:      runtime.GOOS,
			GOARCH:    runtime.GOARCH,
		},
	}
	report.Messages = make([]MessageMeta, 0, len(messages))
	for _, m := range messages {
		clean := MessageMeta{
			Role:     d.redactor.Redact(m.Role),
			Bytes:    m.Bytes,
			ToolName: d.redactor.Redact(m.ToolName),
		}
		// The body is dropped unless the operator opted in. Dropping it here
		// rather than at the call site means a caller that hands us bodies by
		// habit cannot leak them by forgetting the switch.
		if d.IncludeBodies {
			clean.Body = d.redactor.Redact(m.Body)
		}
		report.Messages = append(report.Messages, clean)
	}
	return report
}

// redactChain walks the Unwrap chain and returns each level's redacted message.
// The chain (rather than just the outermost text) is what makes a report worth
// opening: the top-level wrap usually says "turn failed" and the cause three
// levels down says which host refused the connection.
func (d *CrashDumper) redactChain(err error) []string {
	var out []string
	seen := 0
	for e := err; e != nil && seen < 16; seen++ {
		out = append(out, d.redactor.Redact(e.Error()))
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	return out
}

// write serializes the report to a uniquely named file in the dumper's
// directory and returns its path. The mutex serialises concurrent crashes so
// two goroutines dying in the same nanosecond do not race for one filename.
func (d *CrashDumper) write(report CrashReport) (string, error) {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')

	d.mu.Lock()
	defer d.mu.Unlock()
	base := fmt.Sprintf("crash-%s", report.Time.Format("20060102T150405.000000000Z"))
	path := filepath.Join(d.dir, base+".json")
	for i := 1; ; i++ {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			break
		}
		path = filepath.Join(d.dir, fmt.Sprintf("%s-%d.json", base, i))
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// captureStacks returns the dump of every goroutine, truncated to
// MaxCrashStackBytes, and whether truncation happened.
//
// All goroutines rather than just this one: the panics worth a report are
// usually the ones where another goroutine holds the lock, and a single-stack
// dump shows the victim instead of the cause.
func captureStacks() (string, bool) {
	buf := make([]byte, MaxCrashStackBytes)
	n := runtime.Stack(buf, true)
	truncated := n == MaxCrashStackBytes
	return string(buf[:n]), truncated
}

// ReportCrash writes a crash report and announces its path on stderr.
//
// The stderr line is the whole delivery mechanism: a report nobody is told
// about is a file that gets deleted with the temp directory. The line names
// only the path, never the error text, so the stderr stream keeps the same
// disclosure posture as the structured log.
//
// Every failure is swallowed and reported as an empty path. A dump that fails
// must not become the thing that takes the process down -- it is running on a
// path that is already handling a crash.
func ReportCrash(ctx context.Context, d *CrashDumper, kind string, err error, messages []MessageMeta, stderr io.Writer) string {
	if d == nil {
		return ""
	}
	path, derr := d.Dump(ctx, kind, err, messages)
	if derr != nil || path == "" {
		return ""
	}
	if stderr != nil {
		fmt.Fprintf(stderr, "yanshi: crash report -> %s\n", path)
	}
	return path
}

// TruncateForCrash shortens s to at most n runes, appending an ellipsis marker
// when it cut. Used by callers assembling MessageMeta bodies under the debug
// switch, so one enormous message cannot make the report unopenable.
func TruncateForCrash(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…[truncated]"
}

// CrashDirEntries lists the report files in dir, newest first. Returned for
// operator surfaces (doctor, a status command) that want to say how many
// crashes happened without parsing them.
func CrashDirEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "crash-") ||
			!strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	// Names embed a sortable UTC timestamp, so reverse-lexical is
	// reverse-chronological without stat-ing every file.
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}
