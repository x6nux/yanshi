package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/lockfile"
)

// SchedulePath is the operator endpoint for scheduled tasks. It sits next to
// ControlPath on the same loopback server and behind the same auth middleware.
//
// It exists rather than having the CLI open the SQLite store directly because
// the scheduler is ALREADY mutating that state from inside the daemon.
// automation.Manager serialises its read-modify-write with an in-process
// mutex, which a second process does not participate in: a `pause` issued from
// a shell while a tick is enqueueing would be a lost update, and the symptom
// (an automation that stays active, or a run that vanishes) looks exactly like
// a scheduler bug. One writer, reached over a socket, has no such window.
const SchedulePath = "/api/v1/schedule"

// ScheduleOp names a scheduled-task operation.
type ScheduleOp string

const (
	// ScheduleList returns every automation with its next fire time.
	ScheduleList ScheduleOp = "list"
	// ScheduleShow returns one automation together with its run history.
	ScheduleShow ScheduleOp = "show"
	// SchedulePause deactivates an automation without deleting it.
	SchedulePause ScheduleOp = "pause"
	// ScheduleResume reactivates a paused automation and recomputes its next
	// fire time from now.
	ScheduleResume ScheduleOp = "resume"
	// ScheduleRunNow enqueues an automation immediately without disturbing its
	// schedule.
	ScheduleRunNow ScheduleOp = "run-now"
	// ScheduleDelete removes an automation and its run history.
	ScheduleDelete ScheduleOp = "delete"
)

// ScheduleOps returns every operation name in a stable order, for usage text
// and for validation.
func ScheduleOps() []ScheduleOp {
	return []ScheduleOp{
		ScheduleList, ScheduleShow, SchedulePause,
		ScheduleResume, ScheduleRunNow, ScheduleDelete,
	}
}

// mutatingScheduleOps are the operations that change state. They are named so
// the handler can require an explicit id for each: a "pause everything" that
// results from a forgotten argument is not a mistake an operator can undo.
var mutatingScheduleOps = map[ScheduleOp]bool{
	SchedulePause:  true,
	ScheduleResume: true,
	ScheduleRunNow: true,
	ScheduleDelete: true,
}

// ScheduleRequest is the POST body of a schedule operation.
type ScheduleRequest struct {
	Op ScheduleOp `json:"op"`
	// ID selects the automation. Required for every op except list.
	ID string `json:"id,omitempty"`
}

// ScheduleItem is one automation as an operator sees it.
//
// The prompt is deliberately absent. It is model input -- the same category
// the structured logger redacts wholesale -- and an operator listing their
// schedules does not need it to decide what to pause. `show` carries a bounded
// preview instead, which is enough to tell two automations apart.
type ScheduleItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
	// Schedule is the human-readable recurrence ("cron 0 9 * * 1-5", "every
	// 300s").
	Schedule  string     `json:"schedule"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	// PromptPreview is a bounded excerpt, present only on show.
	PromptPreview string `json:"prompt_preview,omitempty"`
}

// ScheduleRun is one execution record as an operator sees it.
type ScheduleRun struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"`
	ScheduledFor time.Time  `json:"scheduled_for"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	Error        string     `json:"error,omitempty"`
}

// ScheduleResponse is the reply to a schedule operation.
type ScheduleResponse struct {
	OK    bool           `json:"ok"`
	Items []ScheduleItem `json:"items,omitempty"`
	Runs  []ScheduleRun  `json:"runs,omitempty"`
	// Message is a human-readable summary of what a mutating op did.
	Message string `json:"message,omitempty"`
	// Error carries a bounded failure description. Manager errors are
	// domain-level ("automation %q not found") and safe to forward; anything
	// that is not is replaced by a generic string before it lands here.
	Error string `json:"error,omitempty"`
}

// PromptPreviewRunes bounds the prompt excerpt carried by `show`. Enough to
// tell two automations apart, short enough that it is an identifier rather than
// a copy of the model input.
const PromptPreviewRunes = 120

// ScheduleManager is the subset of *automation.Manager the endpoint needs.
// Declared as an interface so the handler is testable without a SQLite store
// and a queue adapter behind it.
type ScheduleManager interface {
	List() ([]automation.Automation, error)
	Read(id string) (automation.Automation, []automation.Run, error)
	Pause(id string) error
	Resume(id string) error
	Delete(id string) error
	RunNow(ctx context.Context, id string) (automation.Run, error)
}

// compile-time proof that the production manager satisfies the narrow view.
var _ ScheduleManager = (*automation.Manager)(nil)

// NewScheduleHandler returns the HTTP handler for SchedulePath.
//
// POST-only for the same reason ControlPath is: `delete` must have no
// accidental invocation path, and splitting reads onto GET would mean two
// request shapes for one endpoint.
func NewScheduleHandler(mgr ScheduleManager) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeSchedule(w, http.StatusMethodNotAllowed, ScheduleResponse{
				Error: "the schedule endpoint accepts POST only",
			})
			return
		}
		if mgr == nil {
			// A build without the automation subsystem says so. Reporting an
			// empty list instead would tell an operator their schedules are
			// gone, which is a different and much more alarming fact.
			writeSchedule(w, http.StatusNotImplemented, ScheduleResponse{
				Error: "this daemon was assembled without the automation scheduler",
			})
			return
		}
		var req ScheduleRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeSchedule(w, http.StatusBadRequest, ScheduleResponse{
				Error: "malformed schedule request",
			})
			return
		}
		if mutatingScheduleOps[req.Op] && strings.TrimSpace(req.ID) == "" {
			writeSchedule(w, http.StatusBadRequest, ScheduleResponse{
				Error: fmt.Sprintf("%q requires an automation id", req.Op),
			})
			return
		}
		status, resp := runScheduleOp(r.Context(), mgr, req)
		writeSchedule(w, status, resp)
	}
}

// runScheduleOp dispatches one operation and maps its outcome to a status.
func runScheduleOp(ctx context.Context, mgr ScheduleManager, req ScheduleRequest) (int, ScheduleResponse) {
	switch req.Op {
	case ScheduleList:
		items, err := mgr.List()
		if err != nil {
			return http.StatusInternalServerError, ScheduleResponse{Error: err.Error()}
		}
		return http.StatusOK, ScheduleResponse{OK: true, Items: toScheduleItems(items, false)}

	case ScheduleShow:
		if strings.TrimSpace(req.ID) == "" {
			return http.StatusBadRequest, ScheduleResponse{Error: `"show" requires an automation id`}
		}
		item, runs, err := mgr.Read(req.ID)
		if err != nil {
			return http.StatusNotFound, ScheduleResponse{Error: err.Error()}
		}
		return http.StatusOK, ScheduleResponse{
			OK:    true,
			Items: toScheduleItems([]automation.Automation{item}, true),
			Runs:  toScheduleRuns(runs),
		}

	case SchedulePause:
		if err := mgr.Pause(req.ID); err != nil {
			return http.StatusNotFound, ScheduleResponse{Error: err.Error()}
		}
		return http.StatusOK, ScheduleResponse{OK: true,
			Message: fmt.Sprintf("paused %s (it will not fire until resumed)", req.ID)}

	case ScheduleResume:
		if err := mgr.Resume(req.ID); err != nil {
			return http.StatusNotFound, ScheduleResponse{Error: err.Error()}
		}
		// The next fire time is recomputed from now by Update, so report it:
		// an operator resuming a long-paused automation needs to know it is
		// not about to fire for every slot it missed.
		item, _, rerr := mgr.Read(req.ID)
		resp := ScheduleResponse{OK: true, Message: fmt.Sprintf("resumed %s", req.ID)}
		if rerr == nil {
			resp.Items = toScheduleItems([]automation.Automation{item}, false)
		}
		return http.StatusOK, resp

	case ScheduleRunNow:
		run, err := mgr.RunNow(ctx, req.ID)
		if err != nil {
			return http.StatusInternalServerError, ScheduleResponse{
				Error: err.Error(),
				Runs:  toScheduleRuns([]automation.Run{run}),
			}
		}
		return http.StatusOK, ScheduleResponse{OK: true,
			Message: fmt.Sprintf("queued %s as run %s (the schedule is unchanged)", req.ID, run.ID),
			Runs:    toScheduleRuns([]automation.Run{run})}

	case ScheduleDelete:
		if err := mgr.Delete(req.ID); err != nil {
			return http.StatusNotFound, ScheduleResponse{Error: err.Error()}
		}
		return http.StatusOK, ScheduleResponse{OK: true,
			Message: fmt.Sprintf("deleted %s and its run history", req.ID)}

	default:
		return http.StatusBadRequest, ScheduleResponse{
			Error: fmt.Sprintf("unknown schedule op %q (want one of: %s)",
				req.Op, joinOps(ScheduleOps())),
		}
	}
}

// joinOps renders the op list for a usage message.
func joinOps(ops []ScheduleOp) string {
	parts := make([]string, len(ops))
	for i, o := range ops {
		parts[i] = string(o)
	}
	return strings.Join(parts, ", ")
}

// toScheduleItems projects domain automations onto the operator view, sorted by
// next fire time so the answer to "what runs next" is the first line.
//
// withPrompt is false for list: a schedule listing is a table an operator scans,
// and model input does not belong in it (see ScheduleItem).
func toScheduleItems(items []automation.Automation, withPrompt bool) []ScheduleItem {
	out := make([]ScheduleItem, 0, len(items))
	for _, item := range items {
		view := ScheduleItem{
			ID:        item.ID,
			Name:      item.Name,
			Active:    item.Active,
			Schedule:  describeSchedule(item.Schedule),
			NextRunAt: item.NextRunAt,
			LastRunAt: item.LastRunAt,
		}
		if withPrompt {
			view.PromptPreview = previewPrompt(item.Prompt)
		}
		out = append(out, view)
	}
	sort.SliceStable(out, func(i, j int) bool {
		// A paused automation has no next fire time; those sort last, because
		// the question a listing answers is "what is about to happen".
		switch {
		case out[i].NextRunAt == nil && out[j].NextRunAt == nil:
			return out[i].ID < out[j].ID
		case out[i].NextRunAt == nil:
			return false
		case out[j].NextRunAt == nil:
			return true
		}
		return out[i].NextRunAt.Before(*out[j].NextRunAt)
	})
	return out
}

// describeSchedule renders a Schedule as something an operator can read back.
func describeSchedule(s automation.Schedule) string {
	switch s.Kind {
	case "cron":
		return "cron " + s.Cron
	case "interval":
		return fmt.Sprintf("every %ds", s.IntervalSec)
	case "":
		return "(none)"
	default:
		return s.Kind
	}
}

// previewPrompt bounds the prompt excerpt and collapses whitespace so one
// automation occupies one line.
func previewPrompt(prompt string) string {
	flat := strings.Join(strings.Fields(prompt), " ")
	runes := []rune(flat)
	if len(runes) <= PromptPreviewRunes {
		return flat
	}
	return string(runes[:PromptPreviewRunes]) + "…"
}

// toScheduleRuns projects run records onto the operator view, newest first.
func toScheduleRuns(runs []automation.Run) []ScheduleRun {
	out := make([]ScheduleRun, 0, len(runs))
	for _, run := range runs {
		if run.ID == "" {
			continue // a zero Run from a failed RunNow carries nothing to show
		}
		out = append(out, ScheduleRun{
			ID:           run.ID,
			Status:       run.Status,
			ScheduledFor: run.ScheduledFor,
			EndedAt:      run.EndedAt,
			Error:        run.Error,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ScheduledFor.After(out[j].ScheduledFor)
	})
	return out
}

// writeSchedule serialises a schedule reply.
func writeSchedule(w http.ResponseWriter, status int, resp ScheduleResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// ErrUnknownScheduleOp is returned by RunSchedule for a name that is not an
// operation.
var ErrUnknownScheduleOp = errors.New("cli: unknown schedule operation")

// RunSchedule issues one schedule operation against the project's daemon.
//
// It requires a live daemon and says so plainly when there is none. Falling
// back to reading the store directly would be worse than failing: the listing
// would be correct only until the scheduler's next tick, and an operator would
// have no way to tell a stale answer from a current one.
func RunSchedule(ctx context.Context, root string, req ScheduleRequest) (ScheduleResponse, error) {
	if !isKnownScheduleOp(req.Op) {
		return ScheduleResponse{}, fmt.Errorf("%w: %q (want one of: %s)",
			ErrUnknownScheduleOp, req.Op, joinOps(ScheduleOps()))
	}
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	lf, err := lockfile.Read(root)
	if err != nil {
		return ScheduleResponse{}, fmt.Errorf(
			"%w: scheduled tasks live in the running daemon; start one with `yanshi serve`", ErrNoDaemon)
	}
	if !lf.Alive() {
		return ScheduleResponse{}, fmt.Errorf(
			"%w (lockfile names pid %d, which is not running)", ErrNoDaemon, lf.PID)
	}
	return postSchedule(ctx, "http://"+lf.Addr, req)
}

// isKnownScheduleOp reports whether op is one of the defined operations.
func isKnownScheduleOp(op ScheduleOp) bool {
	for _, known := range ScheduleOps() {
		if known == op {
			return true
		}
	}
	return false
}

// postSchedule issues the schedule POST and decodes the reply.
func postSchedule(ctx context.Context, baseURL string, req ScheduleRequest) (ScheduleResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return ScheduleResponse{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(callCtx, "POST", baseURL+SchedulePath,
		strings.NewReader(string(body)))
	if err != nil {
		return ScheduleResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return ScheduleResponse{}, fmt.Errorf("schedule call to %s: %w", baseURL, err)
	}
	defer resp.Body.Close()

	if isMissingRoute(resp) {
		return ScheduleResponse{}, fmt.Errorf(
			"%w: the running daemon has no schedule endpoint (it predates `yanshi schedule`; "+
				"restart it with the current binary)", ErrNoDaemon)
	}
	var out ScheduleResponse
	if derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); derr != nil {
		return ScheduleResponse{}, fmt.Errorf("decode schedule reply: %w", derr)
	}
	if !out.OK {
		detail := out.Error
		if detail == "" {
			detail = resp.Status
		}
		return out, fmt.Errorf("schedule %s failed: %s", req.Op, detail)
	}
	return out, nil
}

// isMissingRoute distinguishes "this daemon has no such endpoint" from "the
// endpoint answered 404 about a specific automation".
//
// Both are HTTP 404, and telling them apart matters: the first means the
// operator is talking to an older binary and should restart it, the second
// means they typed an id that does not exist. Status alone cannot separate
// them, so the discriminator is the content type -- ServeMux's built-in
// not-found writes text/plain, while every reply this package produces is
// JSON. Reading the body first and falling back to the status keeps a future
// JSON-emitting 404 middleware from being misread as an old daemon.
func isMissingRoute(resp *http.Response) bool {
	if resp.StatusCode != http.StatusNotFound {
		return false
	}
	return !strings.Contains(resp.Header.Get("Content-Type"), "application/json")
}

// RenderScheduleResponse writes the human-readable result of a schedule call.
//
// The listing shows NEXT FIRE TIME as both an absolute stamp and a relative
// offset. The relative form is the one an operator actually reads ("in 4m"),
// and the absolute one is the one they can correlate with a log line; printing
// only one of the two makes the other a mental arithmetic exercise.
func RenderScheduleResponse(w io.Writer, resp ScheduleResponse) {
	if resp.Message != "" {
		fmt.Fprintln(w, resp.Message)
	}
	if resp.Error != "" {
		fmt.Fprintln(w, "error:", resp.Error)
	}
	if len(resp.Items) > 0 {
		renderScheduleItems(w, resp.Items)
	}
	if len(resp.Runs) > 0 {
		renderScheduleRuns(w, resp.Runs)
	}
	if len(resp.Items) == 0 && len(resp.Runs) == 0 && resp.Message == "" && resp.Error == "" {
		fmt.Fprintln(w, "no scheduled tasks")
	}
}

// renderScheduleItems writes the automation table.
func renderScheduleItems(w io.Writer, items []ScheduleItem) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tSCHEDULE\tNEXT RUN")
	for _, item := range items {
		state := "paused"
		if item.Active {
			state = "active"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			item.ID, item.Name, state, item.Schedule, formatNextRun(item.NextRunAt))
	}
	_ = tw.Flush()
	for _, item := range items {
		if item.PromptPreview != "" {
			fmt.Fprintf(w, "\nprompt: %s\n", item.PromptPreview)
		}
	}
}

// renderScheduleRuns writes the run-history table.
func renderScheduleRuns(w io.Writer, runs []ScheduleRun) {
	fmt.Fprintln(w, "\nruns:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTATUS\tSCHEDULED FOR\tERROR")
	for _, run := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			run.ID, run.Status, run.ScheduledFor.Format(time.RFC3339), run.Error)
	}
	_ = tw.Flush()
}

// formatNextRun renders a fire time as "<RFC3339> (in 4m0s)", or "-" when the
// automation is paused and has none.
func formatNextRun(at *time.Time) string {
	if at == nil {
		return "-"
	}
	delta := time.Until(*at).Round(time.Second)
	if delta < 0 {
		// A next-run time in the past means the tick has not caught up yet.
		// Saying "overdue" rather than printing a negative duration is the
		// difference between an operator seeing a scheduler that is behind and
		// one that looks broken.
		return fmt.Sprintf("%s (overdue by %s)", at.Format(time.RFC3339), (-delta).String())
	}
	return fmt.Sprintf("%s (in %s)", at.Format(time.RFC3339), delta)
}
