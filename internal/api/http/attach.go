// internal/api/http/attach.go
//
// Server-side resolution of @path attachments (UX3).
//
// The client sends paths; the server reads the bytes. That split is the whole
// design: a client-supplied blob has already escaped every filesystem boundary
// by the time the server sees it, and the server cannot tell an attachment the
// user picked from one an injected prompt fabricated. A path can be checked.
//
// This is arbitrary file reading triggered by user TEXT, executed BEFORE the
// turn starts and therefore outside any tool context — there is no permission
// callback to escalate to. Every check here is fail-closed for that reason,
// including the ones that would be a prompt anywhere else.

package http

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/pathjail"
	"github.com/x6nux/yanshi/internal/proto"
)

// maxAttachBytes caps a single attachment at 64 KiB.
//
// Roughly 16k tokens — an eighth of a common 128k window. Anything larger
// belongs in fs_read, which can page through it; inlining it means one message
// can consume the compaction budget before the model has said anything.
const maxAttachBytes = 64 << 10

// maxTurnAttachBytes caps the whole turn at 256 KiB, because the per-file cap
// alone is trivially defeated by attaching five files.
const maxTurnAttachBytes = 256 << 10

// resolveAttachments reads every allowed attachment and returns the text block
// to prepend to the user's message, plus a human-readable line for each ref it
// refused.
//
// Refusals are RETURNED, not silently dropped: a user who typed @secrets.env
// and got a normal-looking answer would have no way to know the file never
// reached the model, and would read the answer as if it had.
func resolveAttachments(workRoot string, prof guard.PermissionProfile, refs []proto.AttachRef) (string, []string) {
	if len(refs) == 0 {
		return "", nil
	}
	g := guard.New()
	var b strings.Builder
	var refused []string
	total := 0

	for _, ref := range refs {
		clean, note := checkAttachment(g, prof, workRoot, ref.Path)
		if note != "" {
			refused = append(refused, note)
			continue
		}
		info, err := os.Stat(clean)
		if err != nil {
			refused = append(refused, fmt.Sprintf("%s: cannot read (%v)", ref.Path, err))
			continue
		}
		if info.IsDir() {
			refused = append(refused, fmt.Sprintf("%s: is a directory; attach a file, or "+
				"ask for fs_list", ref.Path))
			continue
		}
		if info.Size() > maxAttachBytes {
			refused = append(refused, fmt.Sprintf(
				"%s: %d bytes exceeds the %d-byte attachment limit; ask the model to "+
					"fs_read it in chunks", ref.Path, info.Size(), maxAttachBytes))
			continue
		}
		if total+int(info.Size()) > maxTurnAttachBytes {
			refused = append(refused, fmt.Sprintf(
				"%s: skipped, this turn's attachments already total %d bytes (limit %d); "+
					"ask the model to fs_read it", ref.Path, total, maxTurnAttachBytes))
			continue
		}
		data, err := os.ReadFile(clean)
		if err != nil {
			refused = append(refused, fmt.Sprintf("%s: cannot read (%v)", ref.Path, err))
			continue
		}
		total += len(data)
		rel, relErr := filepath.Rel(workRoot, clean)
		if relErr != nil {
			rel = clean
		}
		fmt.Fprintf(&b, "<attachment path=%q>\n%s\n</attachment>\n\n", rel, data)
	}
	return b.String(), refused
}

// checkAttachment runs the three fail-closed checks and returns the resolved
// absolute path, or a refusal note.
//
// The order matters: the jail runs first because guard's FS globs are matched
// against a path, and matching a glob against an unresolved "../.." is how a
// jail gets bypassed by a rule that looks correct.
func checkAttachment(g *guard.Guard, prof guard.PermissionProfile, workRoot, raw string) (string, string) {
	if strings.TrimSpace(raw) == "" {
		return "", "empty attachment path"
	}
	// 1. Root jail. pathjail.WithinRootAbs is the canonical implementation —
	// it evaluates symlinks, compares Windows drive letters with EqualFold and
	// re-checks case. A hand-written prefix test here would be a second,
	// weaker jail, and the weaker one is the one that gets bypassed.
	clean, err := pathjail.WithinRootAbs(workRoot, raw)
	if err != nil {
		return "", fmt.Sprintf("%s: outside the work root (%v)", raw, err)
	}
	// 2. The same guard profile a real fs_read would face. An attachment is
	// not a lesser operation than the tool; it just skips the model.
	//
	// The guard sees the path expressed under the CONFIGURED root, not the
	// symlink-resolved one. pathjail resolves symlinks — it has to, that is
	// how it proves containment — and on macOS a work root under /var resolves
	// to /private/var, which matches none of the globs an operator wrote
	// against the path they actually configured. Checking the resolved form
	// would refuse every legitimate attachment on that platform, and the
	// symptom ("not permitted by the active profile") points at the profile
	// rather than at the resolution.
	//
	// This is not a hole: rel is derived from a path pathjail already proved
	// is inside the root, so the string handed to the guard describes the same
	// file, in the namespace whose globs are being matched.
	forGuard := clean
	if rel, relErr := filepath.Rel(resolvedRoot(workRoot), clean); relErr == nil {
		forGuard = filepath.Join(workRoot, rel)
	}
	dec := g.Check(prof, guard.Action{
		Tool: "fs_read",
		FS:   guard.FSWant{Op: "read", Paths: []string{forGuard}},
	})
	// 3. ONLY Allow passes. Prompt normally means "ask the user", and there is
	// nobody to ask: this runs before the turn, outside any tool context, with
	// no permission callback installed. Treating Prompt as permission would
	// read a file the profile did not allow, on a path the user can trigger
	// with plain text.
	if dec.Verdict != guard.Allow {
		return "", fmt.Sprintf("%s: not permitted by the active profile (%v)", raw, dec.Verdict)
	}
	return clean, ""
}

// resolvedRoot is the work root with symlinks evaluated, matching what
// pathjail.WithinRootAbs returns. Failure falls back to the raw root: the
// caller only uses this to compute a relative path, and a wrong relative path
// makes the guard check STRICTER, never looser.
func resolvedRoot(workRoot string) string {
	if r, err := filepath.EvalSymlinks(workRoot); err == nil {
		return r
	}
	return workRoot
}

// attachmentPreamble assembles the block prepended to the user's message.
// Refusals are included so the model — and, through it, the user — learns that
// a named file did not arrive, instead of answering as though it had.
func attachmentPreamble(body string, refused []string) string {
	if body == "" && len(refused) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(body)
	if len(refused) > 0 {
		b.WriteString("<attachments-refused>\n")
		for _, r := range refused {
			b.WriteString("- " + r + "\n")
		}
		b.WriteString("</attachments-refused>\n\n")
	}
	return b.String()
}
