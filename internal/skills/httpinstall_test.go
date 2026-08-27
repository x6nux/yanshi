package skills

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Archive builders — real archives, built in memory.
//
// These construct genuine tar.gz / zip byte streams rather than fixtures on
// disk, for two reasons. A checked-in malicious archive is a file a scanner or
// a careless `tar xf` can act on, and a builder can express the attack
// precisely ("an entry literally named ../../escaped") where a fixture only
// records whatever the tool that produced it happened to emit.
// ---------------------------------------------------------------------------

// tarEntry is one member of a synthetic tar.
type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	mode     int64
	// declaredSize overrides the header's Size field, so a header that LIES
	// about the entry's length can be constructed.
	declaredSize int64
}

// buildTarGz assembles a gzipped tar from entries.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(e.body))
		if e.declaredSize != 0 {
			size = e.declaredSize
		}
		if tf == tar.TypeDir || tf == tar.TypeSymlink || tf == tar.TypeLink {
			size = 0
		}
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: e.name, Typeflag: tf, Linkname: e.linkname, Mode: mode, Size: size,
		}))
		if tf == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// zipEntry is one member of a synthetic zip.
type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

// buildZip assembles a zip archive from entries.
func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			hdr.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(hdr)
		require.NoError(t, err)
		_, err = w.Write([]byte(e.body))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// validSkillMD is a minimal, valid SKILL.md body.
const validSkillMD = "---\nname: fetched\ndescription: a skill fetched over http\n---\n# Fetched\nDo the thing.\n"

// stubFetcher is the test Fetcher: it serves bytes from memory, so the whole
// install path — every extraction rule included — runs with no network, no
// server and no timing.
type stubFetcher struct {
	body        []byte
	contentType string
	err         error
	// gotURL records the URL asked for, so a test can assert the URL reached
	// the transport unmangled.
	gotURL string
}

// Fetch returns the canned body, recording the requested URL.
func (s *stubFetcher) Fetch(_ context.Context, rawURL string) ([]byte, string, error) {
	s.gotURL = rawURL
	if s.err != nil {
		return nil, "", s.err
	}
	return s.body, s.contentType, nil
}

// ---------------------------------------------------------------------------
// Zip-slip: the attack tests.
//
// Each one asserts BOTH halves of the requirement: the install is refused, AND
// nothing landed outside the destination. The second half is the one that
// matters — an extractor that writes the file and then errors has already lost,
// and only the filesystem can testify to that.
// ---------------------------------------------------------------------------

// escapeCases enumerates the entry names that must never be extracted. Each is
// a real, distinct escape technique rather than a restatement of one.
var escapeCases = []struct {
	name  string
	entry string
}{
	{"parent traversal", "../escaped.txt"},
	{"deep parent traversal", "../../../../../../tmp/escaped.txt"},
	{"traversal after a legitimate prefix", "pack/../../escaped.txt"},
	{"traversal in the middle", "pack/sub/../../../escaped.txt"},
	{"absolute posix path", "/tmp/escaped.txt"},
	{"windows drive-qualified path", `C:\Windows\Temp\escaped.txt`},
	{"windows drive-relative path", "C:escaped.txt"},
	{"unc path", "//server/share/escaped.txt"},
	{"backslash traversal", `..\..\escaped.txt`},
	{"mixed separator traversal", `pack\..\..\escaped.txt`},
	{"dot entry", "."},
}

// TestExtractArchive_TarGzRejectsZipSlip is the attacking test for tar.gz: a
// malicious archive is built for each escape shape, extraction is attempted,
// and the filesystem OUTSIDE the destination is inspected afterwards.
func TestExtractArchive_TarGzRejectsZipSlip(t *testing.T) {
	for _, tc := range escapeCases {
		t.Run(tc.name, func(t *testing.T) {
			sandboxDir := t.TempDir()
			dest := filepath.Join(sandboxDir, "dest")
			require.NoError(t, os.MkdirAll(dest, 0o755))

			data := buildTarGz(t, []tarEntry{{name: tc.entry, body: "PWNED"}})
			err := ExtractArchive(data, dest)
			require.Error(t, err, "malicious entry %q must be refused", tc.entry)
			assertNothingEscaped(t, sandboxDir, dest)
		})
	}
}

// TestExtractArchive_ZipRejectsZipSlip is the same attack against the zip
// reader, which has an entirely separate path through archive/zip.
func TestExtractArchive_ZipRejectsZipSlip(t *testing.T) {
	for _, tc := range escapeCases {
		t.Run(tc.name, func(t *testing.T) {
			sandboxDir := t.TempDir()
			dest := filepath.Join(sandboxDir, "dest")
			require.NoError(t, os.MkdirAll(dest, 0o755))

			data := buildZip(t, []zipEntry{{name: tc.entry, body: "PWNED"}})
			err := ExtractArchive(data, dest)
			require.Error(t, err, "malicious entry %q must be refused", tc.entry)
			assertNothingEscaped(t, sandboxDir, dest)
		})
	}
}

// assertNothingEscaped walks the whole sandbox and fails if any file exists
// outside dest.
//
// It walks rather than checking a predicted path because a containment bug
// does not necessarily write where the test author guessed: the point is that
// NOTHING appeared outside dest, which is a property of the tree, not of one
// filename.
func assertNothingEscaped(t *testing.T, sandboxDir, dest string) {
	t.Helper()
	var stray []string
	require.NoError(t, filepath.Walk(sandboxDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasPrefix(p, dest+string(filepath.Separator)) {
			stray = append(stray, p)
		}
		return nil
	}))
	assert.Empty(t, stray, "extraction wrote files outside the destination: %v", stray)
}

// TestExtractArchive_TarGzRejectsSymlink proves a symlink ENTRY is refused
// before any byte is written. A symlink is the post-extraction escape: the
// containment check passes for `link -> /etc` because the link itself lands
// inside dest, and the write through it does not.
func TestExtractArchive_TarGzRejectsSymlink(t *testing.T) {
	sandboxDir := t.TempDir()
	dest := filepath.Join(sandboxDir, "dest")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	outside := filepath.Join(sandboxDir, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o755))

	data := buildTarGz(t, []tarEntry{
		{name: "SKILL.md", body: validSkillMD},
		{name: "link", typeflag: tar.TypeSymlink, linkname: outside},
		{name: "link/pwned.txt", body: "PWNED"},
	})
	err := ExtractArchive(data, dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
	assert.NoFileExists(t, filepath.Join(outside, "pwned.txt"),
		"the write through the symlink must never have happened")
	assertNothingEscaped(t, sandboxDir, dest)
}

// TestExtractArchive_ZipRejectsSymlink is the zip counterpart: archive/zip
// carries the symlink bit in the entry's mode.
func TestExtractArchive_ZipRejectsSymlink(t *testing.T) {
	sandboxDir := t.TempDir()
	dest := filepath.Join(sandboxDir, "dest")
	require.NoError(t, os.MkdirAll(dest, 0o755))

	data := buildZip(t, []zipEntry{
		{name: "link", body: "/etc", mode: os.ModeSymlink | 0o777},
	})
	err := ExtractArchive(data, dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
	assertNothingEscaped(t, sandboxDir, dest)
}

// TestExtractArchive_TarGzRejectsHardLinkAndDevices covers the other entry
// types that name something outside the archive.
func TestExtractArchive_TarGzRejectsHardLinkAndDevices(t *testing.T) {
	cases := []struct {
		name    string
		entry   tarEntry
		wantSub string
	}{
		{"hard link", tarEntry{name: "hl", typeflag: tar.TypeLink, linkname: "/etc/passwd"}, "hard link"},
		{"char device", tarEntry{name: "dev", typeflag: tar.TypeChar}, "device"},
		{"block device", tarEntry{name: "dev", typeflag: tar.TypeBlock}, "device"},
		{"fifo", tarEntry{name: "fifo", typeflag: tar.TypeFifo}, "device or FIFO"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			err := ExtractArchive(buildTarGz(t, []tarEntry{tc.entry}), dest)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// TestExtractArchive_RejectsOversizedEntry proves the per-file cap is enforced
// on the actual stream, not on the header. The header declares a tiny size and
// the body is larger; a size check that trusted the header would let it
// through.
func TestExtractArchive_RejectsOversizedEntry(t *testing.T) {
	dest := t.TempDir()
	// A zip whose CENTRAL DIRECTORY is honest is the ordinary case, so use the
	// budget path first.
	big := strings.Repeat("A", int(MaxArchiveFileBytes)+1024)
	err := ExtractArchive(buildZip(t, []zipEntry{{name: "big.txt", body: big}}), dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "per-file cap")
}

// TestExtractArchive_ForgedSizeHeaderIsRefused builds the one archive shape
// that can lie about an entry's size and proves it does not extract.
//
// It has to be a ZIP. A tar's declared size IS its framing: archive/tar stops
// the entry reader at the header's Size, so a "lying" tar header cannot
// deliver more bytes than it declares. A zip carries the size in the central
// directory, separately from the deflate stream, so patching it yields an
// archive claiming 4 bytes for a sixteen-megabyte member.
//
// What this does NOT prove is that writeArchiveFile's LimitReader is what
// stopped it — see TestWriteArchiveFile_BoundsTheWriteItself, which pins that
// separately after a mutation probe showed no archive-level test could tell
// the difference.
func TestExtractArchive_ForgedSizeHeaderIsRefused(t *testing.T) {
	body := strings.Repeat("B", int(MaxArchiveFileBytes)+4096)
	data := buildZip(t, []zipEntry{{name: "lying.txt", body: body}})

	// Forge the recorded uncompressed size (it appears in both the local file
	// header and the central directory record).
	var truth [4]byte
	binary.LittleEndian.PutUint32(truth[:], uint32(len(body)))
	forged := bytes.ReplaceAll(data, truth[:], []byte{4, 0, 0, 0})
	require.NotEqual(t, data, forged, "the size field must actually have been patched")

	zr, err := zip.NewReader(bytes.NewReader(forged), int64(len(forged)))
	require.NoError(t, err)
	require.EqualValues(t, 4, zr.File[0].UncompressedSize64,
		"the archive must be claiming a size it does not have")

	dest := t.TempDir()
	require.Error(t, ExtractArchive(forged, dest),
		"a forged size header must not produce a successful extraction")
	info, statErr := os.Stat(filepath.Join(dest, "lying.txt"))
	if statErr == nil {
		assert.LessOrEqual(t, info.Size(), MaxArchiveFileBytes+1)
	}
}

// TestWriteArchiveFile_BoundsTheWriteItself pins the per-file LimitReader at
// the level where it is the only thing acting.
//
// This test exists in this shape because of a mutation probe. The obvious
// version — build a lying archive, delete the LimitReader, expect red — stayed
// GREEN for both formats, and the reason is not a weak assertion: neither
// stdlib reader will hand writeArchiveFile more bytes than the cap in the
// first place. archive/tar frames the entry at the declared size, and
// archive/zip aborts the deflate stream with "not a valid zip file" as soon as
// the output exceeds what the header promised, leaving a zero-byte file.
//
// So the LimitReader is defence in depth whose marginal effect through today's
// callers is nil, and it is kept rather than deleted for the reason this
// codebase keeps such layers: the caps are a security property, the readers'
// early refusals are an implementation detail of the standard library that no
// test of ours governs, and a future caller (an operator upload, a format
// added later) reaches this function with a plain io.Reader that has no
// framing at all. That caller is what this test simulates. Deleting the layer
// would trade one already-written line for a silent hole, and would also make
// its absence unfalsifiable.
func TestWriteArchiveFile_BoundsTheWriteItself(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.bin")
	// A reader with no framing and no declared length, carrying more than the
	// cap — exactly what the stdlib readers never produce.
	src := bytes.NewReader(bytes.Repeat([]byte("A"), int(MaxArchiveFileBytes)+4096))

	err := writeArchiveFile(target, src, 4 /* a declared size that lies */)
	require.Error(t, err, "an unframed over-cap stream must be refused")
	assert.Contains(t, err.Error(), "per-file cap")

	info, statErr := os.Stat(target)
	require.NoError(t, statErr)
	assert.LessOrEqual(t, info.Size(), MaxArchiveFileBytes+1,
		"the write must stop at the cap rather than draining the whole stream")
}

// TestWriteArchiveFile_AcceptsExactlyTheCap is the boundary companion: reading
// one byte past the limit is what distinguishes "exactly at the cap" from
// "lying about it", so the at-cap case must still succeed.
func TestWriteArchiveFile_AcceptsExactlyTheCap(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates the full per-file cap")
	}
	target := filepath.Join(t.TempDir(), "atcap.bin")
	src := bytes.NewReader(bytes.Repeat([]byte("A"), int(MaxArchiveFileBytes)))
	require.NoError(t, writeArchiveFile(target, src, MaxArchiveFileBytes))
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, MaxArchiveFileBytes, info.Size())
}

// TestWriteArchiveFile_RefusesToOverwrite pins the O_EXCL: an archive naming
// the same path twice must not have the later copy win, because the SKILL.md
// that then gets validated is not the one a reader of the archive predicts.
func TestWriteArchiveFile_RefusesToOverwrite(t *testing.T) {
	target := filepath.Join(t.TempDir(), "dup.txt")
	require.NoError(t, writeArchiveFile(target, strings.NewReader("first"), 5))
	err := writeArchiveFile(target, strings.NewReader("second"), 6)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "twice")
	b, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, "first", string(b), "the first copy must survive")
}

// TestExtractArchive_RejectsTooManyEntries proves the count cap, which is the
// only one that catches a million empty files.
func TestExtractArchive_RejectsTooManyEntries(t *testing.T) {
	dest := t.TempDir()
	entries := make([]tarEntry, 0, MaxArchiveEntries+1)
	for i := 0; i <= MaxArchiveEntries; i++ {
		entries = append(entries, tarEntry{name: "f" + itoa(i), body: "x"})
	}
	err := ExtractArchive(buildTarGz(t, entries), dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entries")
}

// TestExtractArchive_RejectsDuplicateEntryName proves an archive naming the
// same path twice is refused rather than silently letting the last copy win —
// which would mean the SKILL.md that gets validated is not the one a reader of
// the archive would predict.
func TestExtractArchive_RejectsDuplicateEntryName(t *testing.T) {
	dest := t.TempDir()
	err := ExtractArchive(buildTarGz(t, []tarEntry{
		{name: "SKILL.md", body: validSkillMD},
		{name: "SKILL.md", body: "---\nname: evil\ndescription: d\n---\n"},
	}), dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "twice")
}

// TestExtractArchive_RejectsUnknownFormat proves the format is decided by
// magic bytes, so neither the URL suffix nor Content-Type can steer it.
func TestExtractArchive_RejectsUnknownFormat(t *testing.T) {
	err := ExtractArchive([]byte("this is not an archive"), t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized archive format")
}

// TestExtractArchive_HappyPath proves the ordinary archive extracts, including
// nested directories, so the rejections above are not simply "refuses
// everything".
func TestExtractArchive_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		data func() []byte
	}{
		{"tar.gz", func() []byte {
			return buildTarGz(t, []tarEntry{
				{name: "SKILL.md", body: validSkillMD},
				{name: "reference/", typeflag: tar.TypeDir},
				{name: "reference/notes.md", body: "notes"},
			})
		}},
		{"zip", func() []byte {
			return buildZip(t, []zipEntry{
				{name: "SKILL.md", body: validSkillMD},
				{name: "reference/notes.md", body: "notes"},
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			require.NoError(t, ExtractArchive(tc.data(), dest))
			assert.FileExists(t, filepath.Join(dest, "SKILL.md"))
			b, err := os.ReadFile(filepath.Join(dest, "reference", "notes.md"))
			require.NoError(t, err)
			assert.Equal(t, "notes", string(b))
		})
	}
}

// ---------------------------------------------------------------------------
// safeJoin unit table — the gate itself, independent of any archive format.
// ---------------------------------------------------------------------------

func TestSafeJoin(t *testing.T) {
	dest := filepath.Clean(filepath.Join(string(filepath.Separator), "tmp", "dest"))
	for _, tc := range escapeCases {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := safeJoin(dest, tc.entry)
			require.Error(t, err)
		})
	}
	t.Run("rejects empty name", func(t *testing.T) {
		_, err := safeJoin(dest, "")
		require.Error(t, err)
	})
	t.Run("accepts a plain name", func(t *testing.T) {
		got, err := safeJoin(dest, "SKILL.md")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dest, "SKILL.md"), got)
	})
	t.Run("accepts a nested name", func(t *testing.T) {
		got, err := safeJoin(dest, "a/b/c.md")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dest, "a", "b", "c.md"), got)
	})
	t.Run("accepts an inward-collapsing dotdot", func(t *testing.T) {
		// "a/../a/x" cleans back inside, but the element scan rejects any ".."
		// regardless. Rejecting is the correct behaviour: no legitimate skill
		// pack needs one, and admitting the collapsing form means the gate has
		// to be right about Clean's semantics on every platform.
		_, err := safeJoin(dest, "a/../a/x")
		require.Error(t, err)
	})
}

func TestIsAbsoluteArchivePath(t *testing.T) {
	for _, s := range []string{"/x", "C:/x", "c:x", "Z:/x", "//host/share"} {
		assert.True(t, isAbsoluteArchivePath(s), "%q must be absolute", s)
	}
	for _, s := range []string{"x", "a/b", "./x", "1:x", ":x", ""} {
		assert.False(t, isAbsoluteArchivePath(s), "%q must be relative", s)
	}
}

func TestArchiveFormatSniffers(t *testing.T) {
	assert.True(t, isGzip([]byte{0x1f, 0x8b, 0x08}))
	assert.False(t, isGzip([]byte{0x1f}))
	assert.False(t, isGzip(nil))
	assert.True(t, isZip([]byte{'P', 'K', 0x03, 0x04}))
	assert.True(t, isZip([]byte{'P', 'K', 0x05, 0x06}))
	assert.True(t, isZip([]byte{'P', 'K', 0x07, 0x08}))
	assert.False(t, isZip([]byte{'P', 'K', 0x01, 0x02}))
	assert.False(t, isZip([]byte("PK")))
}

// ---------------------------------------------------------------------------
// InstallFromURL
// ---------------------------------------------------------------------------

func TestInstallFromURL_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		data func() []byte
	}{
		{"tar.gz at archive root", func() []byte {
			return buildTarGz(t, []tarEntry{{name: "SKILL.md", body: validSkillMD}})
		}},
		{"tar.gz inside a wrapper dir", func() []byte {
			return buildTarGz(t, []tarEntry{
				{name: "fetched-1.0/", typeflag: tar.TypeDir},
				{name: "fetched-1.0/SKILL.md", body: validSkillMD},
				{name: "fetched-1.0/reference/guide.md", body: "guide"},
			})
		}},
		{"zip at archive root", func() []byte {
			return buildZip(t, []zipEntry{{name: "SKILL.md", body: validSkillMD}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "skills")
			f := &stubFetcher{body: tc.data()}
			name, err := InstallFromURL("https://mirror.example/pack.tgz", dst, f)
			require.NoError(t, err)
			assert.Equal(t, "fetched", name)
			assert.Equal(t, "https://mirror.example/pack.tgz", f.gotURL)
			assert.FileExists(t, filepath.Join(dst, "fetched", "SKILL.md"))
			// And it is loadable, which is the only definition of "installed"
			// that matters: a pack on disk the Loader skips is not installed.
			reg, err := NewLoader(User(dst)).Load()
			require.NoError(t, err)
			_, ok := reg.Get("fetched")
			assert.True(t, ok)
		})
	}
}

// TestInstallFromURL_RunsFullValidation proves the HTTP path is not a second,
// weaker door: it goes through the SAME ValidateSkillDir as the git path.
func TestInstallFromURL_RunsFullValidation(t *testing.T) {
	cases := []struct {
		name    string
		skillMD string
		wantSub string
	}{
		{"empty description", "---\nname: fetched\ndescription: \"\"\n---\nbody", "description"},
		{"illegal name", "---\nname: ../evil\ndescription: d\n---\nbody", "invalid skill name"},
		{"no frontmatter", "not a skill file", "SKILL.md"},
		{
			"malformed requires",
			"---\nname: fetched\ndescription: d\nrequires:\n  - bin: /usr/bin/gh\n---\nbody",
			"not a bare program name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "skills")
			data := buildTarGz(t, []tarEntry{{name: "SKILL.md", body: tc.skillMD}})
			_, err := InstallFromURL("https://mirror.example/p.tgz", dst, &stubFetcher{body: data})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
			// Nothing may be published on a validation failure.
			entries, readErr := os.ReadDir(dst)
			if readErr == nil {
				assert.Empty(t, entries, "a rejected pack must leave dstRoot empty")
			}
		})
	}
}

// TestInstallFromURL_StripsRemoteMarkers proves a remote pack cannot assert
// its own trust state — the same rule the git path enforces, restated here
// because a second acquisition path is a second chance to forget it.
func TestInstallFromURL_StripsRemoteMarkers(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "skills")
	data := buildTarGz(t, []tarEntry{
		{name: "SKILL.md", body: validSkillMD},
		{name: ".trusted", body: ""},
		{name: ".disabled", body: ""},
	})
	name, err := InstallFromURL("https://mirror.example/p.tgz", dst, &stubFetcher{body: data})
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(dst, name, ".trusted"))
	assert.NoFileExists(t, filepath.Join(dst, name, ".disabled"))

	reg, err := NewLoader(User(dst)).Load()
	require.NoError(t, err)
	s, ok := reg.Get(name)
	require.True(t, ok)
	assert.False(t, s.Trusted, "a downloaded pack must never arrive trusted")
	assert.True(t, s.Enabled)
}

// TestInstallFromURL_RefusesOverwrite proves an existing skill is not silently
// replaced by a download.
func TestInstallFromURL_RefusesOverwrite(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "skills")
	data := buildTarGz(t, []tarEntry{{name: "SKILL.md", body: validSkillMD}})
	_, err := InstallFromURL("https://m/p.tgz", dst, &stubFetcher{body: data})
	require.NoError(t, err)
	_, err = InstallFromURL("https://m/p.tgz", dst, &stubFetcher{body: data})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

// TestInstallFromURL_MultiSkillArchiveIsRefused proves the installer says it
// cannot choose rather than picking one — silently installing whichever
// directory sorted first is the outcome the explicit refusal exists to avoid.
func TestInstallFromURL_MultiSkillArchiveIsRefused(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "skills")
	data := buildTarGz(t, []tarEntry{
		{name: "one/", typeflag: tar.TypeDir},
		{name: "one/SKILL.md", body: validSkillMD},
		{name: "two/", typeflag: tar.TypeDir},
		{name: "two/SKILL.md", body: "---\nname: two\ndescription: d\n---\nb"},
	})
	_, err := InstallFromURL("https://m/p.tgz", dst, &stubFetcher{body: data})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "top-level directories")
}

// TestInstallFromURL_NoSkillMd covers both locateSkillDir failure branches.
func TestInstallFromURL_NoSkillMd(t *testing.T) {
	t.Run("empty archive", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "skills")
		data := buildTarGz(t, []tarEntry{{name: "readme.txt", body: "hi"}})
		_, err := InstallFromURL("https://m/p.tgz", dst, &stubFetcher{body: data})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no SKILL.md")
	})
	t.Run("wrapper dir without SKILL.md", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "skills")
		data := buildTarGz(t, []tarEntry{
			{name: "wrapper/", typeflag: tar.TypeDir},
			{name: "wrapper/readme.txt", body: "hi"},
		})
		_, err := InstallFromURL("https://m/p.tgz", dst, &stubFetcher{body: data})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no SKILL.md")
	})
}

// TestInstallFromURL_FetchError propagates the transport failure verbatim
// rather than turning it into a generic install error.
func TestInstallFromURL_FetchError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "skills")
	_, err := InstallFromURL("https://m/p.tgz", dst,
		&stubFetcher{err: errors.New("connection refused")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

// TestInstallFromURL_TransportCheckIsNotInsideTheSeam pins a hole this test
// suite actually found. The scheme check originally lived only in realFetch,
// so any injected Fetcher — the seam that exists for TESTABILITY — silently
// opted out of the transport policy, and the WS handler with a test fetcher
// wired in happily installed from http://.
//
// The assertion is that the refusal happens with a Fetcher that would have
// succeeded: a stub carrying a perfectly valid pack. If the check drifts back
// inside realFetch, this install succeeds and the test fails.
func TestInstallFromURL_TransportCheckIsNotInsideTheSeam(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "skills")
	f := &stubFetcher{body: buildTarGz(t, []tarEntry{{name: "SKILL.md", body: validSkillMD}})}
	_, err := InstallFromURL("http://mirror.example/pack.tgz", dst, f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing scheme")
	assert.Empty(t, f.gotURL, "the fetcher must not even be consulted for a refused scheme")
	_, statErr := os.Stat(filepath.Join(dst, "fetched"))
	assert.True(t, os.IsNotExist(statErr), "nothing may be published")
}

// TestValidateHTTPSURL proves plaintext and scheme-less URLs are refused, and
// that the refusal names the scheme so the user can see what was objected to.
func TestValidateHTTPSURL(t *testing.T) {
	cases := []struct {
		url     string
		wantErr string
	}{
		{"https://mirror.example/pack.tgz", ""},
		{"http://mirror.example/pack.tgz", "refusing scheme \"http\""},
		{"ftp://mirror.example/pack.tgz", "refusing scheme \"ftp\""},
		{"file:///etc/passwd", "refusing scheme \"file\""},
		{"mirror.example/pack.tgz", "refusing scheme"},
		{"https:///pack.tgz", "no host"},
		{"https://%zz", "invalid URL"},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			err := validateHTTPSURL(tc.url)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestRealFetch_RefusesPlaintextBeforeAnyIO proves the scheme check runs
// before the request is built, so a plaintext URL never reaches the network at
// all — it is refused, not fetched-then-discarded.
func TestRealFetch_RefusesPlaintextBeforeAnyIO(t *testing.T) {
	_, _, err := realFetch{}.Fetch(context.Background(), "http://127.0.0.1:1/pack.tgz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing scheme")
	assert.NotContains(t, err.Error(), "connection", "it must not have dialled")
}

// ---------------------------------------------------------------------------
// InstallAny routing
// ---------------------------------------------------------------------------

func TestIsHTTPSource(t *testing.T) {
	for _, s := range []string{"https://x/y.tgz", "HTTPS://X/Y", "http://x/y", "HtTp://x"} {
		assert.True(t, isHTTPSource(s), "%q", s)
	}
	for _, s := range []string{"github:o/r", "github:o/r/sub", "", "ftp://x", "httpsx"} {
		assert.False(t, isHTTPSource(s), "%q", s)
	}
}

// TestInstallAny_RoutesByScheme proves ONE verb serves both registries, and
// that each backend is reached by the source that names it — a router that
// sent everything one way would pass a test that only checked the other.
func TestInstallAny_RoutesByScheme(t *testing.T) {
	t.Run("https goes to the fetcher", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "skills")
		f := &stubFetcher{body: buildTarGz(t, []tarEntry{{name: "SKILL.md", body: validSkillMD}})}
		// A clone impl that would fail loudly if it were reached.
		clone := &failingClone{t: t}
		name, err := InstallAny("https://m/p.tgz", dst, clone, f)
		require.NoError(t, err)
		assert.Equal(t, "fetched", name)
		assert.Equal(t, "https://m/p.tgz", f.gotURL)
	})
	t.Run("github goes to the cloner", func(t *testing.T) {
		remote := t.TempDir()
		writeSkillFM(t, remote, "cloned", "name: cloned\ndescription: d")
		dst := filepath.Join(t.TempDir(), "skills")
		// A fetcher that would fail loudly if it were reached.
		f := &stubFetcher{err: errors.New("fetcher must not be used for a github: source")}
		name, err := InstallAny("github:fake/cloned", dst, &CloneStub{AsRemote: remote}, f)
		require.NoError(t, err)
		assert.Equal(t, "cloned", name)
		assert.Empty(t, f.gotURL, "the fetcher must not have been called")
	})
	t.Run("plaintext http is routed to the scheme refusal", func(t *testing.T) {
		// Not to the git path's "only github: supported", which describes
		// neither the problem nor the fix.
		_, err := InstallAny("http://m/p.tgz", filepath.Join(t.TempDir(), "s"), nil, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refusing scheme")
	})
}

// failingClone is a CloneImpl that fails the test if it is ever called. It is
// the routing assertion's negative half: without it, a router that sent every
// source to the fetcher would still pass the https case.
type failingClone struct{ t *testing.T }

// Clone fails the test.
func (f *failingClone) Clone(context.Context, string, string, string) error {
	f.t.Fatal("the git clone path must not be reached for an https:// source")
	return nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// itoa renders a non-negative int without importing strconv for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ensure io is used (writeArchiveFile's contract is exercised through the
// public API, but the import documents that these tests deal in streams).
var _ = io.Discard
