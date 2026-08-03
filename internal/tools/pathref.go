package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/proto"
)

// pathRefPattern matches an "@ref" token that starts the string or follows
// whitespace. The leading group is required because Go's regexp has no
// lookbehind and an unanchored "@" would also fire inside e-mail addresses and
// package paths ("user@host.png", "a@b").
var pathRefPattern = regexp.MustCompile(`(^|\s)@([^\s]+)`)

// maxPathRefImages caps how many attachments one message may expand to, and
// maxPathRefImageBytes caps each one. Both exist because the text that drives
// this expansion is model-influenceable (a sub-agent prompt is model output),
// so "@a.png @b.png @c.png …" is a context-window exhaustion vector, not just a
// user convenience. The byte cap matches imagestore's own per-image default so
// an attachment that passes here is not silently dropped by the store on the
// non-multimodal placeholder path.
const (
	maxPathRefImages     = 8
	maxPathRefImageBytes = 10 << 20 // 10 MiB
)

// pathRefGuardTool is the guard tool name @path expansion authorizes as. It is
// deliberately fs_read rather than a name of its own: expanding "@a.png" IS a
// filesystem read performed on the caller's behalf, so it must consume the same
// FS read permission a profile already grants (or withholds) for fs_read. A
// private tool name would have created a read path that every existing profile
// silently allows, i.e. a privilege escape hatch shaped like a feature.
const pathRefGuardTool = "fs_read"

// PathRefRejection records one @path reference that named an image but could
// not be attached, with the reason. Rejections are surfaced (rather than
// silently swallowed) so the caller can decide whether to tell the user; the
// reference itself is always left verbatim in the text, so a rejection never
// loses information the model needs.
type PathRefRejection struct {
	Ref    string
	Reason string
}

// PathRefResult is the outcome of expanding @path references in one message.
// Text is the input with every successfully attached reference expanded (the
// "@" is dropped, the path stays, so the model still knows which file the
// attachment came from). Every other token — including rejected ones — is
// byte-identical to the input.
type PathRefResult struct {
	Text     string
	Images   []proto.ImageAttach
	Rejected []PathRefRejection
}

// ResolveImagePathRefs is Tier G entry B: it turns "@path" references inside a
// user (or sub-agent) message into real image attachments.
//
// # Why this needs the vision tool's root-jail
//
// The text handed to this function is not trustworthy input. It is a turn's
// trailing user message, and on the sub-agent path that message is composed
// from MODEL OUTPUT. A model that can write "@../../../etc/shadow.png" into a
// delegated prompt would, without a jail, have discovered an arbitrary file
// read that bypasses every FS-guarded tool — the bytes would arrive in the
// prompt as an image part with no tool call ever being made, so neither the
// tool allowlist nor the audit log would show anything.
//
// It therefore goes through exactly the same two doors image_describe's path
// ref uses, in the same order:
//
//  1. withinRootAbs (the pathjail canonical kernel) — resolves symlinks, so a
//     symlink planted inside the work root cannot point out of it. This check
//     is NOT reimplemented here; a prefix comparison would miss symlinks.
//  2. Authorize with an FS read action — the profile still decides, so a
//     read-restricted profile blocks @path just as it blocks fs_read.
//
// # Why non-image extensions are left alone
//
// "@" is ordinary prose ("@notes.md", "@someone", "@v1.2"). Only a Tier G image
// extension (IsImagePath) opts a token into resolution at all; everything else
// is passed through untouched and, critically, never touches the filesystem —
// so the jail is not a source of spurious errors for text that was never a file
// reference to begin with. A non-image token is not an error, it is text.
//
// ctx must carry the work root and the permission profile (the orchestrator's
// withTurnContext binds both). A missing work root rejects every image ref:
// without a root there is nothing to jail against, and failing open here would
// be the exact escape the jail exists to prevent.
func ResolveImagePathRefs(ctx context.Context, text string) PathRefResult {
	matches := pathRefPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return PathRefResult{Text: text}
	}
	root := WorkRootFromContext(ctx)

	res := PathRefResult{}
	seen := make(map[string]bool, len(matches))
	var out strings.Builder
	last := 0
	expanded := false
	for _, m := range matches {
		refStart, refEnd := m[4], m[5]
		ref := text[refStart:refEnd]
		if !IsImagePath(ref) {
			continue // ordinary prose: leave the token exactly as written
		}
		img, reason := resolvePathRefImage(ctx, root, ref, len(res.Images), seen)
		if reason != "" {
			res.Rejected = append(res.Rejected, PathRefRejection{Ref: ref, Reason: reason})
			continue
		}
		if img != nil {
			res.Images = append(res.Images, *img)
		}
		// Resolved (or a duplicate of an already-attached file): keep everything
		// up to the "@", then the path without it.
		out.WriteString(text[last : refStart-1])
		out.WriteString(ref)
		last = refEnd
		expanded = true
	}
	if !expanded {
		return PathRefResult{Text: text, Rejected: res.Rejected}
	}
	out.WriteString(text[last:])
	res.Text = out.String()
	return res
}

// resolvePathRefImage runs one reference through the jail, the FS guard and the
// size cap. It returns (nil, "") when the reference resolves to a file already
// attached in this message, (nil, reason) on rejection, and (img, "") on
// success.
func resolvePathRefImage(ctx context.Context, root, ref string, attached int, seen map[string]bool) (*proto.ImageAttach, string) {
	if root == "" {
		return nil, "no work root bound: @path image references cannot be resolved"
	}
	if attached >= maxPathRefImages {
		return nil, "too many @path image references in one message"
	}
	candidate := ref
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, ref)
	}
	// Door 1: the canonical root-jail (symlink-resolving). Never replace this
	// with a string prefix test — see the doc comment on ResolveImagePathRefs.
	absPath, err := withinRootAbs(root, candidate)
	if err != nil {
		return nil, "path escapes the work root or does not exist: " + err.Error()
	}
	// Door 2: the FS guard, with the profile bound in ctx.
	argsJSON, _ := json.Marshal(map[string]string{"path": ref})
	if err := Authorize(ctx, guard.Action{
		Tool: pathRefGuardTool,
		FS:   guard.FSWant{Op: "read", Paths: []string{absPath}},
	}, string(argsJSON)); err != nil {
		return nil, "permission denied: " + err.Error()
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, "cannot stat: " + err.Error()
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		return nil, "not a regular file"
	}
	if info.Size() > maxPathRefImageBytes {
		return nil, "image exceeds the per-image size limit"
	}
	if seen[absPath] {
		return nil, ""
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, "cannot read: " + err.Error()
	}
	seen[absPath] = true
	// W/H are left zero on purpose: they are display dims for the
	// non-multimodal placeholder, and imagestore.Put derives them from the
	// bytes it decodes. Guessing them here would add a second decode.
	return &proto.ImageAttach{
		Source:  ref,
		Fmt:     detectFmt(ref),
		DataB64: base64.StdEncoding.EncodeToString(data),
	}, ""
}
