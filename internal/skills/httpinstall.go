// internal/skills/httpinstall.go
//
// HTTP direct install: fetch one skill pack as a tar.gz or zip archive from a
// URL, unpack it into a staging dir under every containment rule the git path
// already enforces, run the full ValidateSkillDir, and publish by rename.
//
// It exists because the git path is not merely "the other way to install" —
// it requires a `git` binary on PATH and hard-codes github.com in the URL it
// builds (see realClone). An air-gapped or mirrored deployment has neither.
// The registry those sites do have is an HTTP file server, so that is the
// second registry this file adds.
//
// The unpacker is the security-critical half. An archive is attacker-supplied
// data that names its own destination paths, so it gets the full treatment:
//
//   - Zip-slip: every entry path is resolved against the destination and
//     rejected unless it lands INSIDE it. Absolute paths, "..", and Windows
//     drive letters are all rejected by that one check plus an explicit
//     pre-filter, because the resolve-and-compare alone behaves differently on
//     Windows for absolute entries (filepath.Join does not override the base).
//   - Symlinks: rejected outright rather than resolved. A symlink is how an
//     archive escapes AFTER extraction — the containment check passes for
//     `link -> /etc`, and the write to `link/passwd` happens through it. The
//     git path bans symlinks for the same reason (rejectSymlinks); this one
//     bans them one layer earlier, before any byte is written.
//   - Hard links and devices: rejected. Same argument, different mechanism.
//   - Decompression bombs: total bytes, per-file bytes and entry count are all
//     capped, and the caps are enforced on the DECOMPRESSED stream (via
//     io.LimitReader per entry) rather than on the download size, because a
//     10 KiB gzip expands to gigabytes.
//   - Transport: HTTPS only by default. An http:// URL is a plaintext channel
//     delivering code the model will be told to run.
package skills

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Archive extraction limits. Each cap closes a different shape of bomb:
// MaxArchiveBytes bounds the whole pack, MaxArchiveFileBytes bounds one entry
// inside it (so a single huge member cannot use the whole budget), and
// MaxArchiveEntries bounds the count (so a million empty files cannot exhaust
// inodes while staying far under the byte caps).
const (
	// MaxArchiveBytes caps the total DECOMPRESSED size of a skill archive.
	MaxArchiveBytes int64 = 64 << 20 // 64 MiB
	// MaxArchiveFileBytes caps the decompressed size of any single entry.
	MaxArchiveFileBytes int64 = 16 << 20 // 16 MiB
	// MaxArchiveEntries caps the number of members in a skill archive.
	MaxArchiveEntries = 4096
	// MaxDownloadBytes caps the COMPRESSED bytes read from the network. It is
	// separate from MaxArchiveBytes because the download is bounded before
	// anything is decompressed: a server that streams forever must be cut off
	// without first being unpacked.
	MaxDownloadBytes int64 = 64 << 20 // 64 MiB
)

// httpFetchTimeout bounds the whole download, not one read. A per-read
// deadline lets a server that dribbles one byte per second hold the install
// open indefinitely while never technically stalling.
const httpFetchTimeout = 60 * time.Second

// Fetcher is the HTTP-download abstraction, the direct counterpart of
// CloneImpl. Production uses realFetch (net/http with a size cap and an
// HTTPS requirement); tests inject a stub serving bytes from memory, so the
// install path — including every extraction rule — is exercised without a
// network or a live server.
type Fetcher interface {
	// Fetch retrieves rawURL and returns the response body bytes and the
	// content type the server reported (used only as a hint; the archive
	// format is decided by sniffing the bytes).
	Fetch(ctx context.Context, rawURL string) (body []byte, contentType string, err error)
}

// realFetch is the production Fetcher: HTTPS-only, size-capped, single GET.
type realFetch struct{}

// Fetch downloads rawURL over HTTPS, refusing plaintext transport and bodies
// larger than MaxDownloadBytes.
func (realFetch) Fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	if err := validateHTTPSURL(rawURL); err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, httpFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("skills: build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("skills: fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("skills: fetch %s: HTTP %d", rawURL, resp.StatusCode)
	}
	// Read one byte past the cap so an exactly-at-limit body is accepted and a
	// larger one is detected without buffering all of it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxDownloadBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("skills: read body: %w", err)
	}
	if int64(len(body)) > MaxDownloadBytes {
		return nil, "", fmt.Errorf("skills: archive exceeds the %d MiB download cap",
			MaxDownloadBytes>>20)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// validateHTTPSURL rejects any URL that is not an absolute https:// URL with a
// host.
//
// Plaintext http:// is refused rather than upgraded. The payload is code the
// skill will instruct the model to run, so an on-path attacker who can rewrite
// the response owns the agent; silently upgrading to https would also mask a
// mirror that genuinely only serves plaintext, which its operator needs to
// know. A deployment whose mirror is plaintext-only supplies its own Fetcher
// rather than getting a flag here: the refusal is the default this package
// ships, and relaxing it should be a visible decision in the composition root.
func validateHTTPSURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("skills: invalid URL %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("skills: refusing scheme %q: skill archives must be fetched "+
			"over https (the archive is code the model will be told to run)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("skills: URL %q has no host", rawURL)
	}
	return nil
}

// InstallAny is the single entry point that routes an install source to the
// registry that can serve it: an "https://…" URL goes to InstallFromURL, and
// anything else goes to Install (the github: git path).
//
// The dispatch is on the SOURCE STRING rather than on a caller-chosen flag so
// there is one place where "which registries exist" is answered. A second
// registry added as a second WS verb would give the TUI, the WS handler and
// the docs three chances to disagree about what is installable, and the first
// two would be discovered by a user typing a URL into the verb that cannot
// take one.
//
// Both backends may be nil; each path substitutes its production
// implementation, exactly as Install already does for CloneImpl.
func InstallAny(src, dstRoot string, clone CloneImpl, fetch Fetcher) (string, error) {
	return InstallAnyWithOptions(src, dstRoot, clone, fetch, InstallOptions{})
}

// InstallAnyWithOptions is InstallAny with the acquisition-time options made
// explicit. Both backends apply the SAME S7 content scan; routing by URL scheme
// must not also route around a security gate, which is the failure mode that
// having two install paths invites.
func InstallAnyWithOptions(src, dstRoot string, clone CloneImpl, fetch Fetcher, opts InstallOptions) (string, error) {
	if isHTTPSource(src) {
		return InstallFromURLWithOptions(src, dstRoot, fetch, opts)
	}
	return InstallWithOptions(src, dstRoot, clone, opts)
}

// isHTTPSource reports whether src names an HTTP(S) URL rather than a
// github:owner/repo source.
//
// http:// is matched here even though validateHTTPSURL then refuses it. The
// alternative — letting a plaintext URL fall through to the git path — makes
// the user's error report "unsupported source (only github: supported)",
// which describes neither the problem nor the fix. Routing it to the
// scheme check produces the message that names the actual objection.
func isHTTPSource(src string) bool {
	lower := strings.ToLower(src)
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")
}

// InstallFromURL downloads a single skill pack from rawURL, unpacks it into a
// staging directory, validates it with the SAME ValidateSkillDir the git path
// and the /skill validate verb use, and publishes it by atomic rename into
// dstRoot. Returns the installed skill's name.
//
// fetch may be nil — production passes nil to use realFetch; tests pass a
// stub. This mirrors Install's CloneImpl parameter exactly, for the same
// reason: the install logic and every extraction rule must be testable without
// a network.
//
// Security invariants, in the order they are applied:
//
//  1. HTTPS-only transport, size-capped download (realFetch).
//  2. Format sniffed from the bytes, not from the URL suffix or the server's
//     Content-Type — both are attacker-influenced.
//  3. Extraction into a tempdir OUTSIDE the loader-scanned root, under the
//     zip-slip / symlink / size / count rules described at the top of this
//     file.
//  4. A second, independent symlink sweep of what landed on disk
//     (rejectSymlinks), so a future extractor bug cannot publish one.
//  5. Full ValidateSkillDir: frontmatter, name, description, requires shape,
//     and directory-name agreement.
//  6. Remote .trusted / .disabled / .scan-override markers deleted — a remote
//     pack does not get to assert its own trust or scan-approved state.
//  7. S7 content scan of the body and every shipped script; blocking findings
//     refuse the install unless the caller passed AllowUnsafe.
//  8. Rename into a path proven to be inside dstRoot, refusing to overwrite.
func InstallFromURL(rawURL, dstRoot string, fetch Fetcher) (string, error) {
	return InstallFromURLWithOptions(rawURL, dstRoot, fetch, InstallOptions{})
}

// InstallFromURLWithOptions is InstallFromURL with the acquisition-time options
// made explicit. See InstallFromURL for the ordered security invariants.
func InstallFromURLWithOptions(rawURL, dstRoot string, fetch Fetcher, opts InstallOptions) (string, error) {
	// The transport check runs HERE, before the Fetcher is consulted, and
	// realFetch checks again for direct callers.
	//
	// It used to live only inside realFetch, and that was a hole with a test
	// to prove it: the Fetcher parameter exists so tests can serve bytes from
	// memory, so putting a security policy inside one implementation of it
	// meant any injected Fetcher silently opted out of the policy. A seam
	// whose stated purpose is testability must not also be the place a
	// transport rule is decided — otherwise the rule holds for exactly the
	// configuration nobody is worried about.
	if err := validateHTTPSURL(rawURL); err != nil {
		return "", err
	}
	if fetch == nil {
		fetch = realFetch{}
	}
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		return "", fmt.Errorf("skills: mkdir dstRoot: %w", err)
	}
	rootAbs, err := filepath.Abs(dstRoot)
	if err != nil {
		return "", fmt.Errorf("skills: abs dstRoot: %w", err)
	}

	body, _, err := fetch.Fetch(context.Background(), rawURL)
	if err != nil {
		return "", err
	}

	// Staging sits beside dstRoot (not inside it) so the loader never scans a
	// half-written pack, and so staging and target share a filesystem — which
	// is what keeps the publish a rename rather than a copy.
	staging, err := os.MkdirTemp(filepath.Dir(rootAbs), ".yanshi-skill-http-")
	if err != nil {
		return "", fmt.Errorf("skills: mkstaging: %w", err)
	}
	defer os.RemoveAll(staging)

	// Unpack into a CHILD of staging rather than into staging itself. The pack
	// is later renamed to its declared name as a sibling of the unpack dir,
	// and when the archive carries SKILL.md at its root that sibling would
	// otherwise be a child of the very directory being renamed — which is
	// EINVAL, not a subtle bug, and it made the whole root-level-archive shape
	// fail while the wrapper-dir shape worked.
	unpack := filepath.Join(staging, "unpack")
	if err := os.MkdirAll(unpack, 0o755); err != nil {
		return "", fmt.Errorf("skills: mkdir unpack: %w", err)
	}
	if err := ExtractArchive(body, unpack); err != nil {
		return "", err
	}

	// The pack may be at the archive root or inside a single wrapper directory
	// (what `tar czf` of a directory, and every GitHub release tarball,
	// produces).
	skillDir, err := locateSkillDir(unpack)
	if err != nil {
		return "", err
	}

	// Independent of the extractor's own symlink refusal. Two checks of one
	// rule is the shape used everywhere a containment failure is unrecoverable:
	// the extractor rejects before writing, this rejects what actually exists.
	if err := rejectSymlinks(skillDir); err != nil {
		return "", err
	}

	// Delete remote trust assertions BEFORE validating, so a pack cannot claim
	// to be trusted even transiently on disk. The .scan-override marker is
	// purged by scanStagedPack below, for the same reason.
	for _, marker := range []string{".trusted", ".disabled"} {
		if err := os.Remove(filepath.Join(skillDir, marker)); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("skills: remove remote %s: %w", marker, err)
		}
	}

	// ValidateSkillDir compares the DIRECTORY name against the frontmatter
	// name, and the staging dir is named by MkdirTemp, so rename to the
	// declared name first and validate in place. The rename target stays
	// inside staging, so a hostile name cannot reach outside it: validName has
	// already refused separators and dots by the time this runs.
	fm, _, err := readFrontmatter(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return "", fmt.Errorf("skills: read SKILL.md: %w", err)
	}
	if !validName(fm.Name) {
		return "", fmt.Errorf("skills: invalid skill name %q", fm.Name)
	}
	named := filepath.Join(staging, fm.Name)
	if named != skillDir {
		if err := os.Rename(skillDir, named); err != nil {
			return "", fmt.Errorf("skills: stage under declared name: %w", err)
		}
		skillDir = named
	}
	if err := ValidateSkillDir(skillDir); err != nil {
		return "", err
	}

	// S7 content scan, identical to the git path's. It runs on the staged pack
	// so a refused download never reaches the loader-scanned root.
	if err := scanStagedPack(skillDir, opts.AllowUnsafe); err != nil {
		return "", err
	}

	dstAbs, err := filepath.Abs(filepath.Join(dstRoot, fm.Name))
	if err != nil {
		return "", fmt.Errorf("skills: abs dst: %w", err)
	}
	if !isWithin(dstAbs, rootAbs) {
		return "", fmt.Errorf("skills: dst %q escapes dstRoot %q", dstAbs, rootAbs)
	}
	if _, err := os.Stat(dstAbs); err == nil {
		return "", fmt.Errorf("skills: target %q already exists (use /skill uninstall first)", dstAbs)
	}
	if err := os.Rename(skillDir, dstAbs); err != nil {
		return "", fmt.Errorf("skills: rename to final: %w", err)
	}
	return fm.Name, nil
}

// locateSkillDir finds the directory holding SKILL.md: either the extraction
// root itself, or a single wrapper directory inside it.
//
// The wrapper case is not an edge case — it is what `tar czf pack.tgz mypack`
// and every GitHub release tarball produce. Descending exactly one level (and
// only when there is exactly one directory and no SKILL.md above it) keeps the
// rule stateable: it never has to guess between two candidate packs, so an
// archive carrying several skills is refused with a message saying so rather
// than silently installing whichever one sorted first.
func locateSkillDir(root string) (string, error) {
	if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err == nil {
		return root, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("skills: read archive root: %w", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	switch len(dirs) {
	case 0:
		return "", errors.New("skills: archive contains no SKILL.md at its root and no subdirectory to look in")
	case 1:
		cand := filepath.Join(root, dirs[0])
		if _, err := os.Stat(filepath.Join(cand, "SKILL.md")); err != nil {
			return "", fmt.Errorf("skills: archive contains no SKILL.md (looked in the root and in %q)", dirs[0])
		}
		return cand, nil
	default:
		return "", fmt.Errorf("skills: archive holds %d top-level directories; "+
			"this installer takes ONE skill pack per URL, so it will not guess which", len(dirs))
	}
}

// ExtractArchive unpacks a zip or gzipped-tar archive into dest, enforcing
// every containment and size rule this package requires. The format is decided
// by sniffing data's magic bytes.
//
// Exported so a caller holding archive bytes from somewhere other than HTTP
// (a local file, an operator upload) gets the same rules rather than writing a
// second extractor — the copy of a security rule that drifts is always the one
// that is not the gate.
func ExtractArchive(data []byte, dest string) error {
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("skills: abs dest: %w", err)
	}
	switch {
	case isGzip(data):
		return extractTarGz(data, destAbs)
	case isZip(data):
		return extractZip(data, destAbs)
	default:
		return errors.New("skills: unrecognized archive format (expected .tar.gz or .zip); " +
			"the format is read from the file's magic bytes, not from the URL or Content-Type")
	}
}

// isGzip reports whether data starts with the gzip magic number.
func isGzip(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

// isZip reports whether data starts with a PKZIP local-file / empty-archive
// signature.
func isZip(data []byte) bool {
	if len(data) < 4 || data[0] != 'P' || data[1] != 'K' {
		return false
	}
	// 0304 = local file header, 0506 = end of central directory (empty zip),
	// 0708 = spanned archive marker.
	return (data[2] == 0x03 && data[3] == 0x04) ||
		(data[2] == 0x05 && data[3] == 0x06) ||
		(data[2] == 0x07 && data[3] == 0x08)
}

// safeJoin resolves one archive-declared entry path against dest and returns
// the absolute destination, refusing anything that would land outside it.
//
// This is THE zip-slip gate, and it is deliberately three checks rather than
// one, because the single resolve-and-compare that is usually presented as
// sufficient is not:
//
//   - An absolute entry path ("/etc/cron.d/x", "C:\\windows\\x") must be
//     rejected explicitly. filepath.Join does not let an absolute second
//     argument override the base on POSIX, but the Windows drive-letter form
//     is not absolute by POSIX rules and vice versa, so the host-independent
//     isAbsoluteArchivePath check runs first.
//   - Backslashes are normalized to forward slashes before splitting.
//     A zip written on Windows may carry "..\\..\\x" as a single POSIX path
//     element, which path.Clean leaves entirely intact.
//   - Only then does the resolved path get compared against dest, which
//     catches every remaining "../" form including the ones Clean collapses.
func safeJoin(dest, entryName string) (string, error) {
	if entryName == "" {
		return "", errors.New("skills: archive entry with empty name")
	}
	name := strings.ReplaceAll(entryName, "\\", "/")
	if isAbsoluteArchivePath(name) {
		return "", fmt.Errorf("skills: archive entry %q is an absolute path", entryName)
	}
	for _, el := range strings.Split(name, "/") {
		if el == ".." {
			return "", fmt.Errorf("skills: archive entry %q escapes the destination (contains %q)", entryName, "..")
		}
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == "/" {
		return "", fmt.Errorf("skills: archive entry %q resolves to the destination itself", entryName)
	}
	target := filepath.Join(dest, filepath.FromSlash(cleaned))
	if !isWithin(target, dest) {
		return "", fmt.Errorf("skills: archive entry %q escapes the destination", entryName)
	}
	return target, nil
}

// isAbsoluteArchivePath reports whether a slash-normalized archive entry name
// is absolute under EITHER POSIX or Windows conventions.
//
// Host-independent on purpose: an archive built on one OS is routinely
// extracted on another, and a check that only understands the host's own
// convention is exactly the gap an attacker picks the other convention for.
func isAbsoluteArchivePath(name string) bool {
	if strings.HasPrefix(name, "/") {
		return true
	}
	// Drive-qualified ("C:/x") and drive-relative ("C:x") forms both name a
	// location outside dest on Windows.
	if len(name) >= 2 && name[1] == ':' {
		c := name[0]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return true
		}
	}
	// UNC share.
	return strings.HasPrefix(name, "//")
}

// archiveBudget tracks the cumulative decompressed size and entry count of one
// extraction, so the caps are enforced across entries rather than per entry.
type archiveBudget struct {
	bytes   int64
	entries int
}

// admit charges one entry against the budget, returning an error when a cap is
// exceeded.
func (b *archiveBudget) admit(name string, size int64) error {
	b.entries++
	if b.entries > MaxArchiveEntries {
		return fmt.Errorf("skills: archive holds more than %d entries", MaxArchiveEntries)
	}
	if size > MaxArchiveFileBytes {
		return fmt.Errorf("skills: archive entry %q is larger than the %d MiB per-file cap",
			name, MaxArchiveFileBytes>>20)
	}
	b.bytes += size
	if b.bytes > MaxArchiveBytes {
		return fmt.Errorf("skills: archive expands beyond the %d MiB total cap",
			MaxArchiveBytes>>20)
	}
	return nil
}

// extractTarGz unpacks a gzipped tar into dest.
func extractTarGz(data []byte, dest string) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("skills: gzip: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var budget archiveBudget
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("skills: tar: %w", err)
		}
		// Reject by TYPE before resolving the path. A symlink or hard link
		// entry is refused whatever it points at: the destructive one points
		// outside dest, and distinguishing it from a benign one requires
		// resolving a target that later entries can still change.
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			return fmt.Errorf("skills: archive entry %q is a symlink; skill packs must not contain symlinks "+
				"(a symlink is how an extracted pack escapes its directory afterwards)", hdr.Name)
		case tar.TypeLink:
			return fmt.Errorf("skills: archive entry %q is a hard link; skill packs must not contain hard links", hdr.Name)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return fmt.Errorf("skills: archive entry %q is a device or FIFO node", hdr.Name)
		case tar.TypeDir, tar.TypeReg:
			// handled below
		default:
			return fmt.Errorf("skills: archive entry %q has unsupported type %q", hdr.Name, string(hdr.Typeflag))
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("skills: mkdir %q: %w", hdr.Name, err)
			}
			continue
		}
		if err := budget.admit(hdr.Name, hdr.Size); err != nil {
			return err
		}
		if err := writeArchiveFile(target, tr, hdr.Size); err != nil {
			return err
		}
	}
	return nil
}

// extractZip unpacks a zip archive into dest.
func extractZip(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("skills: zip: %w", err)
	}
	var budget archiveBudget
	for _, f := range zr.File {
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("skills: archive entry %q is a symlink; skill packs must not contain symlinks "+
				"(a symlink is how an extracted pack escapes its directory afterwards)", f.Name)
		}
		if mode&os.ModeDevice != 0 || mode&os.ModeNamedPipe != 0 || mode&os.ModeSocket != 0 {
			return fmt.Errorf("skills: archive entry %q is a device, FIFO or socket node", f.Name)
		}
		target, err := safeJoin(dest, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("skills: mkdir %q: %w", f.Name, err)
			}
			continue
		}
		// UncompressedSize64 is declared by the archive, so it is a hint an
		// attacker controls, not a measurement. It is charged to the budget
		// for early rejection, and writeArchiveFile independently truncates
		// the actual stream at the cap — a lying header cannot buy more bytes
		// than the cap allows.
		if err := budget.admit(f.Name, int64(f.UncompressedSize64)); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("skills: open archive entry %q: %w", f.Name, err)
		}
		err = writeArchiveFile(target, rc, int64(f.UncompressedSize64))
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeArchiveFile creates target's parent, then copies at most
// MaxArchiveFileBytes from src into it, failing if the stream carries more.
//
// The cap is re-applied HERE rather than trusted from the declared size,
// because the declared size is attacker-supplied in both formats. Measured
// honestly, this layer's marginal effect through TODAY's two callers is nil:
// archive/tar frames each entry at the header's Size so it cannot over-deliver
// at all, and archive/zip aborts the deflate stream with "not a valid zip
// file" the moment the output exceeds what the central directory promised. A
// mutation probe deleting the LimitReader left every archive-level test green
// for exactly that reason.
//
// It is kept, and tested at this level instead
// (httpinstall_test.go::TestWriteArchiveFile_BoundsTheWriteItself), because
// those early refusals are an implementation detail of the standard library
// that no test in this repository governs, and because this function takes a
// plain io.Reader: a future caller — an operator upload, a format added later
// — arrives with no framing whatsoever, and for that caller this is the only
// bound there is. Deleting it would trade one already-written line for a
// silent hole and would make its own absence unfalsifiable.
//
// Reading one byte past the limit is what distinguishes "exactly at the cap"
// from "lying about it".
//
// The file is created with O_EXCL so a second entry naming the same path
// cannot overwrite the first: an archive carrying `SKILL.md` twice would
// otherwise pass validation on the copy that landed last, which is not the one
// a reader of the archive would predict.
func writeArchiveFile(target string, src io.Reader, declared int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("skills: mkdir parent of %q: %w", target, err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("skills: archive names %q twice", filepath.Base(target))
		}
		return fmt.Errorf("skills: create %q: %w", target, err)
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(src, MaxArchiveFileBytes+1))
	if err != nil {
		return fmt.Errorf("skills: write %q: %w", target, err)
	}
	if n > MaxArchiveFileBytes {
		return fmt.Errorf("skills: archive entry %q exceeds the %d MiB per-file cap "+
			"(declared %d bytes)", filepath.Base(target), MaxArchiveFileBytes>>20, declared)
	}
	return nil
}
