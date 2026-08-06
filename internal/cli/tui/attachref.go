package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/x6nux/yanshi/internal/proto"
)

// attachTokenRe matches an @path token in the prompt.
//
// The left boundary is start-of-string or whitespace, so an email address and
// a Go struct tag do not become attachments. The path may not contain
// whitespace: a quoting syntax would be a second parser to keep in sync with
// the completion popup, and a user with spaces in a filename can still attach
// it from the popup, which inserts a path this regexp accepts.
var attachTokenRe = regexp.MustCompile(`(^|\s)@([^\s@]+)`)

// extractAttachRefs finds the @path references in a prompt.
//
// It only returns paths that EXIST as files under root. The alternative —
// forwarding every @token and letting the server refuse — turns "@ the middle
// of a sentence" and "@username" into a refusal block in the model's context
// on a large share of ordinary messages. The server still re-checks every ref
// it receives: this filter is for noise, not for safety, and the resolver's
// jail and guard checks do not depend on it.
//
// The @token is deliberately LEFT in the text. The model should see what the
// user actually asked; the file contents are prepended by the server, so the
// reference and its content sit next to each other.
func extractAttachRefs(text, root string) []proto.AttachRef {
	if root == "" || !strings.Contains(text, "@") {
		return nil
	}
	var refs []proto.AttachRef
	seen := map[string]bool{}
	for _, m := range attachTokenRe.FindAllStringSubmatch(text, -1) {
		raw := strings.TrimRight(m[2], ".,;:!?)")
		if raw == "" || seen[raw] {
			continue
		}
		abs := raw
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, raw)
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue
		}
		seen[raw] = true
		refs = append(refs, proto.AttachRef{Path: abs})
	}
	return refs
}
