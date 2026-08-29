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

// executedOnWriteSuffixes and executedOnWriteAbsolutePaths are the WRITE
// direction's own denylist. They exist because the two tables above are not one.
//
// # One table was answering two questions
//
// sensitivePathSuffixes states its membership rule on itself: "a path earns a
// place here when READING it yields a secret". checkFS consulted it for reads
// AND writes, so the write direction had no membership rule of its own — and the
// family it should have been protecting was entirely outside the table.
// Measured, all Allow under a profile with `write: ["**"]`:
//
//	tee -a ~/.bashrc          cp /tmp/x ~/.bashrc      tee -a ~/.zshrc
//	tee -a ~/.profile         tee -a /etc/cron.d/zz    tee -a ~/.local/bin/ls
//	tee -a /usr/local/bin/ls  tee -a ~/.config/fish/config.fish
//
// while `tee -a ~/.gitconfig` prompted. ~/.bashrc executes on the NEXT
// interactive shell, unconditionally; ~/.gitconfig executes only if git reaches
// a key that names a program. THE ONE THAT ALWAYS RUNS WAS OUTSIDE THE TABLE AND
// THE CONDITIONAL ONE WAS INSIDE IT, because the question the table asks is
// about reading.
//
// # The two rules, kept apart on purpose
//
//	READ:  reading this yields a secret that grants access to something outside
//	       this project.                                   (sensitive* above)
//	WRITE: writing this makes something EXECUTE LATER that nobody in this
//	       session will read — the write IS the execution, deferred. (here)
//
// The read tables still apply to writes as well, and that overlap is deliberate
// rather than sloppy: overwriting ~/.ssh/authorized_keys is not a secret leak
// but it is exactly as bad, and a path can honestly satisfy both rules
// (~/.gitconfig, /etc/sudoers). What the split fixes is the direction where one
// rule was silently standing in for the other.
//
// # What is deliberately NOT here
//
//   - /etc/passwd and /etc/group. Writing them creates an ACCOUNT, which is a
//     different harm with a different rule, and admitting it would restart the
//     defect this split exists to fix: one table, two questions.
//   - .git/hooks. It is the purest instance of the rule — a shell script git
//     runs on the next commit — and it is PROJECT-relative, so neither a
//     home-relative suffix nor an absolute prefix can name it. Recorded here
//     rather than omitted silently.
//
// Failure direction, same as the read table's: a path missing from here costs a
// silent write, a path wrongly in it costs one prompt. So the list errs toward
// inclusion.
var executedOnWriteSuffixes = []string{
	// Shell startup files. Every one of these is sourced by the next
	// interactive or login shell, with no further condition.
	".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".profile",
	".zshrc", ".zshenv", ".zprofile", ".zlogin", ".zlogout",
	".kshrc", ".cshrc", ".tcshrc",
	".config/fish/config.fish", ".config/fish/conf.d",
	// Directories on the default PATH of a normal login: a file dropped here
	// replaces a command the user is about to type.
	".local/bin", "bin",
	// Per-user autostart and service units.
	".config/autostart",
	".config/systemd/user",
	"Library/LaunchAgents",
	// Windows: the per-user Startup folder runs everything in it at logon.
	"AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup",
}

// executedOnWriteAbsolutePaths is the machine-wide half of the same rule.
var executedOnWriteAbsolutePaths = []string{
	// System-wide shell startup.
	"/etc/profile", "/etc/profile.d", "/etc/bash.bashrc", "/etc/bashrc",
	"/etc/zshrc", "/etc/zsh", "/etc/environment",
	// Scheduled execution.
	"/etc/crontab", "/etc/cron.d",
	"/etc/cron.hourly", "/etc/cron.daily", "/etc/cron.weekly", "/etc/cron.monthly",
	"/var/spool/cron",
	// Service definitions and boot scripts.
	"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system",
	"/etc/init.d", "/etc/rc.local",
	"/Library/LaunchDaemons", "/Library/LaunchAgents", "/Library/StartupItems",
	// The dynamic loader runs these inside every process that starts.
	"/etc/ld.so.preload", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	// Directories on the default PATH.
	"/bin", "/sbin", "/usr/bin", "/usr/sbin", "/usr/local/bin", "/usr/local/sbin",
}

// IsExecutedOnWritePath reports whether writing p schedules an execution nobody
// in this session will read. See executedOnWriteSuffixes for the rule and for
// why it is separate from IsSensitivePath's.
func IsExecutedOnWritePath(p, workdir string) (string, bool) {
	return denylistHit(p, workdir, executedOnWriteSuffixes, executedOnWriteAbsolutePaths)
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
// A path carrying an unresolved parameter expansion gets a SECOND reading with
// the expansion elided, because the table above matches on literal directory
// segments and an expansion spliced into one breaks the match without changing
// where the write lands. Measured: `echo k > ~/.s${x}sh/authorized_keys` planted
// a key with no prompt, as did `~/.ssh${x}/authorized_keys`. Blanking is the
// right reading HERE and the wrong one for the deletion gate — see
// elideExpansions for why the two dimensions take opposite decisions.
func IsSensitivePath(p, workdir string) (string, bool) {
	return denylistHit(p, workdir, sensitivePathSuffixes, sensitiveAbsolutePaths)
}

// denylistHit answers "does p fall under one of these home-relative suffixes or
// absolute prefixes", including the second reading with expansions elided.
//
// It is shared by both denylists rather than copied, because the matching is the
// part that has been wrong before (segment boundaries, case folding, the elided
// re-read) and the tables are the part that differs. A second copy would be a
// second place for `~/.s${x}sh/authorized_keys` to stop matching.
func denylistHit(p, workdir string, suffixes, absolutes []string) (string, bool) {
	if entry, hit := denylistHitOnce(p, workdir, suffixes, absolutes); hit {
		return entry, true
	}
	if elided, changed := elideExpansions(p); changed {
		return denylistHitOnce(elided, workdir, suffixes, absolutes)
	}
	return "", false
}

// denylistHitOnce is denylistHit for ONE spelling of the path.
func denylistHitOnce(p, workdir string, suffixes, absolutes []string) (string, bool) {
	norm, ok := normalizePath(p, workdir)
	if !ok {
		return "", false
	}
	folded := foldForDeny(norm)

	if home := homeDir(); home != "" {
		if h, ok := normalizePath(home, ""); ok {
			for _, suffix := range suffixes {
				full := foldForDeny(strings.TrimSuffix(h, "/") + "/" + suffix)
				if pathHasPrefixSegment(folded, full) {
					return "~/" + suffix, true
				}
			}
		}
	}
	for _, abs := range absolutes {
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
	writing := a.FS.Op != "read"
	for _, raw := range a.FS.Paths {
		entry, hit := IsSensitivePath(raw, a.Workdir)
		what := "sensitive credential location"
		if !hit && writing {
			// The write direction's own rule. Consulted ONLY for writes: reading
			// ~/.bashrc or listing /usr/bin is ordinary, and the harm this table
			// names does not exist in that direction. See its header.
			entry, hit = IsExecutedOnWritePath(raw, a.Workdir)
			what = "location whose contents are executed later without being read"
		}
		if !hit {
			continue
		}
		if profileGrantsExplicitly(patterns, raw, a.Workdir) {
			continue
		}
		return prompt(fmt.Sprintf(
			"path %q is a built-in %s (%s); grant it literally in the profile to allow without asking",
			filepath.ToSlash(raw), what, entry))
	}
	return allow()
}
