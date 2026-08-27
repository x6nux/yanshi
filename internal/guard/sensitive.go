package guard

import (
	"fmt"
	"path/filepath"
	"strings"
)

// sensitive.go adds a built-in, profile-independent denylist for credential
// paths, checked BEFORE the profile's own FS globs.
//
// The hole it closes is concrete and shipped: config.example.yaml's `coding`
// profile carries `fs: { read: ["**"], write: ["**"] }`, and "**" matches
// ~/.ssh/id_rsa exactly as happily as it matches a source file. Every operator
// who copies that example gets a profile whose FS dimension returns Allow for
// the user's private keys, AWS credentials, and browser login database. No
// prompt, no record — the model just reads them, and the contents then travel
// to the provider on the next request.
//
// TIER CHOICE — this is a PROMPT, not a HardDeny, and it is not structural.
//
// The tier was picked against the two failure modes, not against how scary the
// paths sound:
//
//   - A HardDeny (either tier) would be unappealable in the default mode and
//     would break legitimate work with no path forward: "read ~/.gitconfig to
//     find my git user" and "run the deploy script that needs ~/.aws/config"
//     are ordinary requests. An unappealable gate on a path the user is
//     entitled to read teaches operators to widen the profile globally, which
//     is strictly worse than one prompt.
//   - A silent Allow (today) is the actual incident.
//
// Prompt is the tier that fits: the user sees the exact path, and default /
// allow-edits surface it interactively. It is deliberately NOT a structural
// HardDeny, so CLAUDE.md's closed enumeration of the five structural classes
// stays true and needs no edit — a claim that is worth more intact than this
// gate would gain by joining it.
//
// EXPLICIT-GRANT ESCAPE HATCH. A profile that names the path LITERALLY (no
// wildcard characters in the pattern) still gets Allow. That distinction is the
// whole design: "**" is a statement about the project tree that happens to also
// cover the home directory, while `fs.read: ["~/.aws/config"]` is an operator
// deliberately typing out a credential path. The first is an accident, the
// second is a decision, and only the second should bypass the gate. See
// profileGrantsExplicitly.

// sensitivePathSuffixes is the built-in credential denylist, ported from
// QwenPaw's DEFAULT_SANDBOX_DENY_PATHS (governance/policy.py) with the Windows
// equivalents its POSIX-only list omits.
//
// Entries are home-RELATIVE and stored without the "~/" prefix: matching is
// done on the path suffix after the home directory, so the same table serves
// /Users/me (macOS), /home/me (Linux) and C:/Users/me (Windows).
//
// The comment on each group records WHY the path is a credential, because the
// list is only maintainable if the next person can tell whether a proposed
// addition belongs. A path earns a place here when reading it yields a secret
// that grants access to something OUTSIDE this project.
var sensitivePathSuffixes = []string{
	// SSH private keys and known-hosts/config (host inventory is itself recon).
	".ssh",
	// AWS access keys / session tokens.
	".aws",
	// GPG private keyring.
	".gnupg",
	// Kubernetes cluster credentials and client certs.
	".kube",
	// Google Cloud OAuth refresh tokens.
	".config/gcloud",
	// Azure CLI credentials.
	".azure",
	// Docker registry auth (base64 registry passwords).
	".docker/config.json",
	// Generic dotenv in the home directory — secrets by convention.
	".env",
	// Claude Code config and memory (may carry API keys).
	".claude",
	// macOS Keychain database.
	"Library/Keychains",
	// Browser-stored credentials.
	"Library/Application Support/Google/Chrome/Default/Login Data",
	"Library/Application Support/Firefox/Profiles",
	// Git credential store and config (tokens are routinely embedded in URLs).
	".git-credentials",
	".gitconfig",
	// Terraform CLI credentials (registry tokens).
	".terraformrc",
	// HashiCorp Vault token.
	".vault-token",
	// npm / yarn registry auth tokens.
	".npmrc",
	".yarnrc",
	// PyPI upload tokens.
	".pypirc",
	// Nix config (may carry substituter tokens).
	".config/nix",
	// netrc: cleartext login credentials for arbitrary hosts.
	".netrc",

	// ── Windows equivalents. QwenPaw's list is POSIX-only; on Windows these
	// are where the same secrets actually live, so a POSIX-only port would
	// leave the gate inert on that platform. Paths are home-relative in their
	// forward-slash spelling — normalizePath has already folded backslashes.
	"AppData/Roaming/gcloud",
	"AppData/Roaming/gh",
	"AppData/Roaming/Docker",
	"AppData/Roaming/Microsoft/Crypto",      // DPAPI / private key containers
	"AppData/Roaming/Microsoft/Protect",     // DPAPI master keys
	"AppData/Local/Google/Chrome/User Data", // Chrome Login Data on Windows
	"AppData/Local/Microsoft/Credentials",   // Windows Credential Manager
	"AppData/Local/Microsoft/Edge/User Data",
	".aws/credentials",
	".azure/accessTokens.json",
}

// sensitiveAbsolutePaths are credential stores that do NOT live under the
// user's home directory and therefore cannot be expressed as a home-relative
// suffix. They are matched as absolute prefixes.
//
// These are separate from the machine-wide system trees the destructive gate
// guards: this list is about READING secrets, not about mass deletion, so the
// membership test is "does this file contain a key" rather than "is this a
// system root".
var sensitiveAbsolutePaths = []string{
	"/etc/shadow",        // password hashes
	"/etc/gshadow",       // group password hashes
	"/etc/sudoers",       // privilege grants
	"/etc/sudoers.d",     // ditto
	"/etc/ssh",           // host keys
	"/root/.ssh",         // root's keys, reachable when the agent runs elevated
	"/var/lib/docker",    // container secrets/volumes
	"/proc/self/environ", // the process's own env, i.e. every API key
}

// IsSensitivePath reports whether p names a credential store covered by the
// built-in denylist, and returns the matched entry so the denial can name it.
//
// Matching is by path PREFIX at a segment boundary: "~/.ssh" covers
// "~/.ssh/id_rsa" and "~/.ssh" itself, but not a project directory that merely
// starts with the same characters (".sshconfig"). A bare prefix test would
// produce exactly that false positive, and false positives on a gate that
// prompts are how a gate gets switched off.
//
// workdir participates only so that a RELATIVE path can be resolved at all. A
// credential path that resolves INSIDE the working directory is still matched:
// a project that keeps a .git-credentials file in its own tree holds a real
// token, and "it is in the repo" is not evidence that reading it is intended.
func IsSensitivePath(p, workdir string) (string, bool) {
	norm, ok := normalizePath(p, workdir)
	if !ok {
		return "", false
	}
	folded := foldForDeny(norm)

	if home := homeDir(); home != "" {
		if h, ok := normalizePath(home, ""); ok {
			for _, suffix := range sensitivePathSuffixes {
				full := foldForDeny(strings.TrimSuffix(h, "/") + "/" + suffix)
				if pathHasPrefixSegment(folded, full) {
					return "~/" + suffix, true
				}
			}
		}
	}
	for _, abs := range sensitiveAbsolutePaths {
		if pathHasPrefixSegment(folded, foldForDeny(abs)) {
			return abs, true
		}
	}
	return "", false
}

// pathHasPrefixSegment reports whether p equals prefix or lives underneath it,
// comparing at a path-segment boundary. Both arguments must already be
// normalized and folded.
func pathHasPrefixSegment(p, prefix string) bool {
	if p == prefix {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(prefix, "/")+"/")
}

// profileGrantsExplicitly reports whether one of the profile's FS patterns
// names path LITERALLY — no wildcard metacharacter anywhere in the pattern.
//
// This is the escape hatch that keeps the built-in denylist from being a
// capability the operator cannot grant. The test is on the PATTERN, not on
// whether it happens to match: "**" matches ~/.ssh/id_rsa but says nothing
// about it, while `~/.aws/config` says exactly one thing. Requiring the
// literal spelling means granting a credential path is an edit somebody has to
// make on purpose and a reviewer can see in a diff.
//
// A pattern containing any of * ? [ ] is treated as a wildcard and does NOT
// grant. That includes narrow-looking ones like "~/.ssh/*": a glob over a key
// directory is still a blanket grant over every key in it, which is the shape
// this gate exists to catch.
func profileGrantsExplicitly(patterns []string, path, workdir string) bool {
	target, ok := normalizePath(path, workdir)
	if !ok {
		return false
	}
	for _, pat := range patterns {
		if strings.ContainsAny(pat, "*?[]") {
			continue
		}
		lit, ok := normalizePath(pat, workdir)
		if !ok {
			continue
		}
		if foldForDeny(lit) == foldForDeny(target) {
			return true
		}
		// A literal DIRECTORY grant covers its contents: naming "~/.aws" is
		// an operator statement about that whole credential store, and
		// requiring a separate line per file inside it would push operators
		// back to the wildcard that this gate refuses.
		if pathHasPrefixSegment(foldForDeny(target), foldForDeny(lit)) {
			return true
		}
	}
	return false
}

// checkSensitiveFS is the built-in credential gate, run inside checkFS BEFORE
// the profile's own globs are consulted. See the file header for the tier
// rationale (Prompt, with a literal-grant escape hatch).
func (g *Guard) checkSensitiveFS(p PermissionProfile, a Action) Decision {
	if len(a.FS.Paths) == 0 {
		return allow()
	}
	patterns := p.FS.Read
	if a.FS.Op != "read" {
		patterns = p.FS.Write
	}
	for _, raw := range a.FS.Paths {
		entry, hit := IsSensitivePath(raw, a.Workdir)
		if !hit {
			continue
		}
		if profileGrantsExplicitly(patterns, raw, a.Workdir) {
			continue
		}
		return prompt(fmt.Sprintf(
			"path %q is a built-in sensitive credential location (%s); grant it literally in the profile to allow without asking",
			filepath.ToSlash(raw), entry))
	}
	return allow()
}
