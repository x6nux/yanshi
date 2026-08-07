package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// serverRouteRe pulls the path out of s.HandleFunc("METHOD /api/...", …).
var serverRouteRe = regexp.MustCompile(`HandleFunc\("(?:[A-Z]+ )?(/[^"]*)"`)

// sdkQuotedPathRe finds an /api/… path inside a string literal — double
// quotes, single quotes, or a backtick template. A path in a COMMENT is not
// matched, which is deliberate: a file has to be able to explain that an
// endpoint was removed without that explanation counting as a reference.
var sdkQuotedPathRe = regexp.MustCompile("[\"'`]([^\"'`\\s]*/api/v1/[^\"'`\\s]*)[\"'`]")

// pathParamRe normalises the two spellings of a path parameter so
// /api/v1/sessions/{id} on the server matches ${id} interpolation in the SDKs.
var pathParamRe = regexp.MustCompile(`\{[^}]*\}|\$\{[^}]*\}`)

func normalisePath(p string) string {
	p = pathParamRe.ReplaceAllString(p, "{}")
	return strings.TrimSuffix(p, "/")
}

// TestSDKsOnlyReferenceRegisteredEndpoints keeps the SDKs from advertising a
// route the server does not serve.
//
// The TS client's ws transport connected to /api/v1/threads/{id}/stream for
// months. The server never registered it — an audit measured a plain 404 —
// and the branch had no test, because every caller injected a fake socket, so
// nothing ever tried the real URL. The Python client raised on the same
// option, which at least said so out loud, but the two SDKs disagreed about
// what "not supported" looked like.
//
// Nothing structural could catch that: GOV5 reasons about Go tool names, GOV9
// about Go symbols, and neither reads TypeScript or Python. This does, by
// comparing the paths the SDKs put in string literals against the routes
// internal/api/http actually registers.
//
// Paths in comments are ignored on purpose — see sdkQuotedPathRe.
//
// ledger: D2/V15#1 start/resume/run/stream/cancel 可用
func TestSDKsOnlyReferenceRegisteredEndpoints(t *testing.T) {
	root := mustModuleRoot()

	registered := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(root, "internal", "api"),
		func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".go" {
				return err
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for _, m := range serverRouteRe.FindAllStringSubmatch(string(src), -1) {
				registered[normalisePath(m[1])] = true
			}
			return nil
		})
	require.NoError(t, err)
	require.NotEmpty(t, registered,
		"no server routes were parsed, so this test would pass for any SDK at all")

	var offenders []string
	for _, dir := range []string{
		filepath.Join(root, "sdk", "ts", "src"),
		filepath.Join(root, "sdk", "python", "src"),
	} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			ext := filepath.Ext(path)
			if ext != ".ts" && ext != ".py" {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range sdkQuotedPathRe.FindAllStringSubmatch(string(src), -1) {
				// Trim any prefix the literal carried (template heads etc.).
				p := m[1]
				if i := strings.Index(p, "/api/v1/"); i > 0 {
					p = p[i:]
				}
				if registered[normalisePath(p)] {
					continue
				}
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+": "+p)
			}
			return nil
		})
		require.NoError(t, err)
	}
	sort.Strings(offenders)

	require.Emptyf(t, offenders,
		"these SDK sources reference endpoints internal/api/http does not register, so a "+
			"caller using them gets a 404 rather than an unsupported-feature error:\n  %s",
		strings.Join(offenders, "\n  "))
}
