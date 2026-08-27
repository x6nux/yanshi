package cli

// liverun_t8_test.go — installing a skill over real HTTP, including the
// archives an attacker would serve.
//
// internal/skills already tests ExtractArchive against malicious entries, and
// those tests are good. What they cannot cover is the composition: the
// production Fetcher, a real TLS server, the staging directory, the
// symlink re-check that runs after extraction, and the publish. A guard that
// works in the extractor but is bypassed because the install path unpacks
// somewhere else first would leave every one of those tests green.
//
// So this serves the archives over a real TLS listener, installs through the
// production entry point with the DEFAULT fetcher (nil — the same code path a
// user gets), and then checks the filesystem OUTSIDE the target directory. An
// error return is not sufficient evidence: the write may have landed before the
// error was produced, and the whole point of a containment check is where the
// bytes ended up.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/skills"
)

// tarEntry describes one member of a synthesised tar archive.
//
// Archives are built in memory rather than checked in: a malicious archive on
// disk is a file that scanners quarantine and that a reader cannot inspect
// without unpacking it, and the shape being tested is far clearer as code.
type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	size     int64 // when non-zero, overrides len(body) in the header
}

// makeTarGz renders entries into a gzipped tar.
func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		size := e.size
		if size == 0 && tf == tar.TypeReg {
			size = int64(len(e.body))
		}
		hdr := &tar.Header{
			Name: e.name, Mode: 0o644, Size: size,
			Typeflag: tf, Linkname: e.linkname,
		}
		if tf == tar.TypeDir {
			hdr.Mode = 0o755
			hdr.Size = 0
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if tf == tar.TypeReg && e.body != "" {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// makeZip renders entries into a zip archive.
func makeZip(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.typeflag == tar.TypeSymlink {
			hdr.SetMode(os.ModeSymlink | 0o777)
		} else {
			hdr.SetMode(0o644)
		}
		w, err := zw.CreateHeader(hdr)
		require.NoError(t, err)
		body := e.body
		if e.typeflag == tar.TypeSymlink {
			body = e.linkname
		}
		_, err = w.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// validSkillMD is a minimal well-formed SKILL.md.
const validSkillMD = `---
name: probe-skill
description: A skill served over HTTP for the T8 acceptance run.
---

# probe-skill

Body text.
`

// serveArchive starts a real TLS server handing out body, and returns its URL
// plus an http.Client configured to trust it.
//
// TLS rather than plaintext because the installer refuses non-https sources —
// which is itself a property worth exercising on the real path rather than
// working around with an injected fetcher.
func serveArchive(t *testing.T, name string, body []byte) (string, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/" + name, srv.Client()
}

// withDefaultTransport points http.DefaultClient at the test server's CA for
// the duration of the test, so the PRODUCTION fetcher (which uses
// http.DefaultClient) can reach it. Restored afterwards.
func withDefaultTransport(t *testing.T, c *http.Client) {
	t.Helper()
	saved := http.DefaultClient.Transport
	http.DefaultClient.Transport = c.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = saved })
	// Sanity: the server must really be TLS, or this test proves nothing about
	// the https-only rule.
	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, tr.TLSClientConfig, "the test server must be TLS")
}

// TestLiveRun_T8InstallsAGoodSkillOverRealHTTPS is the positive control. Without
// it, every refusal below could be an installer that rejects everything.
func TestLiveRun_T8InstallsAGoodSkillOverRealHTTPS(t *testing.T) {
	archive := makeTarGz(t, []tarEntry{
		{name: "probe-skill/", typeflag: tar.TypeDir},
		{name: "probe-skill/SKILL.md", body: validSkillMD},
		{name: "probe-skill/helper.txt", body: "just data\n"},
	})
	url, client := serveArchive(t, "probe-skill.tar.gz", archive)
	withDefaultTransport(t, client)

	dst := t.TempDir()
	// nil fetcher = the production one. Passing a stub here would test the stub.
	got, err := skills.InstallFromURL(url, dst, nil)
	require.NoError(t, err, "a well-formed skill archive must install")
	// InstallFromURL returns the pack NAME, not a path: resolve it against the
	// destination the way a caller listing the skill root would.
	installed := filepath.Join(dst, got)
	t.Logf("installed %q -> %s", got, installed)

	body, err := os.ReadFile(filepath.Join(installed, "SKILL.md"))
	require.NoError(t, err)
	if !strings.Contains(string(body), "probe-skill") {
		t.Errorf("installed SKILL.md does not look like the served one: %.100s", body)
	}
	if _, err := os.Stat(filepath.Join(installed, "helper.txt")); err != nil {
		t.Errorf("the archive's data file did not survive the install: %v", err)
	}
}

// TestLiveRun_T8MaliciousArchivesAreRefusedAndWriteNothingOutside serves each
// attack shape over real HTTPS and, after the install fails, verifies that
// nothing was created outside the destination.
//
// The canary is a real file in a real directory next to the target, holding
// known bytes. A traversal that "fails" after overwriting it is a successful
// attack with a bad error message, and only reading the canary back can tell
// the two apart.
func TestLiveRun_T8MaliciousArchivesAreRefusedAndWriteNothingOutside(t *testing.T) {
	cases := []struct {
		name    string
		archive func(t *testing.T, canary string) []byte
	}{
		{
			name: "tar.gz zip-slip via ../",
			archive: func(t *testing.T, canary string) []byte {
				return makeTarGz(t, []tarEntry{
					{name: "probe-skill/SKILL.md", body: validSkillMD},
					{name: "probe-skill/../../" + filepath.Base(canary), body: "OWNED BY THE ARCHIVE\n"},
				})
			},
		},
		{
			name: "tar.gz absolute path",
			archive: func(t *testing.T, canary string) []byte {
				return makeTarGz(t, []tarEntry{
					{name: "probe-skill/SKILL.md", body: validSkillMD},
					{name: canary, body: "OWNED BY THE ARCHIVE\n"},
				})
			},
		},
		{
			name: "tar.gz symlink entry pointing outside",
			archive: func(t *testing.T, canary string) []byte {
				return makeTarGz(t, []tarEntry{
					{name: "probe-skill/SKILL.md", body: validSkillMD},
					{name: "probe-skill/escape", typeflag: tar.TypeSymlink, linkname: filepath.Dir(canary)},
				})
			},
		},
		{
			name: "zip zip-slip via ../",
			archive: func(t *testing.T, canary string) []byte {
				return makeZip(t, []tarEntry{
					{name: "probe-skill/SKILL.md", body: validSkillMD},
					{name: "probe-skill/../../" + filepath.Base(canary), body: "OWNED BY THE ARCHIVE\n"},
				})
			},
		},
		{
			name: "zip symlink entry pointing outside",
			archive: func(t *testing.T, canary string) []byte {
				return makeZip(t, []tarEntry{
					{name: "probe-skill/SKILL.md", body: validSkillMD},
					{name: "probe-skill/escape", typeflag: tar.TypeSymlink, linkname: filepath.Dir(canary)},
				})
			},
		},
		{
			name: "tar.gz member over the per-entry size cap",
			archive: func(t *testing.T, canary string) []byte {
				// Real bytes, just past skills.MaxArchiveFileBytes. A declared
				// size with no body behind it is a malformed tar that the
				// writer itself rejects, so it would test the test; the number
				// is read from the package so a changed cap cannot silently
				// turn this into an ordinary small file. It compresses to a few
				// KiB on the wire.
				big := strings.Repeat("A", int(skills.MaxArchiveFileBytes)+1024)
				return makeTarGz(t, []tarEntry{
					{name: "probe-skill/SKILL.md", body: validSkillMD},
					{name: "probe-skill/huge.bin", body: big},
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			outside := filepath.Join(base, "outside")
			require.NoError(t, os.MkdirAll(outside, 0o755))
			canary := filepath.Join(outside, "canary.txt")
			const sacred = "PRE-EXISTING FILE OUTSIDE THE SKILL ROOT\n"
			require.NoError(t, os.WriteFile(canary, []byte(sacred), 0o644))

			dst := filepath.Join(base, "skills")
			require.NoError(t, os.MkdirAll(dst, 0o755))

			url, client := serveArchive(t, "evil.tar.gz", tc.archive(t, canary))
			withDefaultTransport(t, client)

			got, err := skills.InstallFromURL(url, dst, nil)
			t.Logf("install -> path=%q err=%v", got, err)
			if err == nil {
				t.Errorf("a malicious archive installed successfully to %s", got)
			}

			// THE assertion: the file outside the destination is untouched.
			back, rerr := os.ReadFile(canary)
			require.NoError(t, rerr, "the canary file itself must still exist")
			if string(back) != sacred {
				t.Errorf("the archive wrote OUTSIDE the destination.\n  path: %s\n  want: %q\n  got:  %q",
					canary, sacred, back)
			}
			// And nothing new appeared beside it.
			entries, rerr := os.ReadDir(outside)
			require.NoError(t, rerr)
			if len(entries) != 1 {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("the archive created %d entries outside the destination: %v", len(entries)-1, names)
			}
		})
	}
}

// TestLiveRun_T8PlaintextTransportIsRefused checks the rule on the production
// path rather than inside one Fetcher implementation.
//
// The archive served here is perfectly valid, so a success would mean the
// transport rule — not the archive validation — is what failed.
func TestLiveRun_T8PlaintextTransportIsRefused(t *testing.T) {
	archive := makeTarGz(t, []tarEntry{
		{name: "probe-skill/SKILL.md", body: validSkillMD},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	dst := t.TempDir()
	_, err := skills.InstallFromURL(srv.URL+"/skill.tar.gz", dst, nil)
	t.Logf("plaintext install -> %v", err)
	if err == nil {
		t.Fatalf("a skill archive was installed over plaintext http")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("the refusal does not mention the transport rule, so an operator "+
			"cannot tell why it failed: %v", err)
	}
	entries, rerr := os.ReadDir(dst)
	require.NoError(t, rerr)
	if len(entries) != 0 {
		t.Errorf("a refused plaintext install still left %d entries in the destination", len(entries))
	}
}
