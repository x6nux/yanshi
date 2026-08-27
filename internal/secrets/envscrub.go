package secrets

import (
	"regexp"
	"sort"
	"strings"
)

// EnvScrubPolicy is a caller's declaration of which environment variables a
// child process legitimately needs, on top of the default "strip every
// credential" posture.
//
// The zero value is the secure default: nothing is allowed through, so every
// variable ScrubEnv recognises as a credential is removed. That direction is
// deliberate — a launcher that forgets to build a policy gets the strict
// behaviour, not the permissive one.
//
// Allow is matched against variable NAMES only, never values. A caller that
// knows `gh` needs GH_TOKEN says so by name; it cannot say "let anything that
// looks like a GitHub token through", because that is the exact rule an
// attacker-supplied value would satisfy.
type EnvScrubPolicy struct {
	// Allow lists variable names that survive the scrub even when they match a
	// credential rule. Matching is case-insensitive and treats '-' as '_', the
	// same normalisation IsCredentialEnvName uses, so callers do not have to
	// guess the host's spelling.
	Allow []string
}

// EnvScrubResult carries the scrubbed environment plus the names that were
// removed.
//
// DroppedNames exists so the removal is observable. A credential that silently
// vanishes from a child's environment produces the single most confusing
// failure mode this feature can cause — `gh` reporting "not logged in" on a
// machine where the operator is demonstrably logged in — and an operator with
// no log line has nothing to search for. Only NAMES are recorded; the values
// are what this whole file exists to keep out of places they can be read.
type EnvScrubResult struct {
	// Env is the surviving environment, in the input's order.
	Env []string
	// DroppedNames is the sorted, de-duplicated set of removed variable names.
	DroppedNames []string
}

// SplitEnvEntry splits one "NAME=value" environment entry.
//
// ok is false for an entry with no '=' at all, in which case name is the whole
// entry and value is empty. Callers that scrub must pass such entries through
// untouched rather than guessing: an entry that is not a well-formed
// assignment carries no value to leak.
func SplitEnvEntry(entry string) (name, value string, ok bool) {
	i := strings.IndexByte(entry, '=')
	if i < 0 {
		return entry, "", false
	}
	return entry[:i], entry[i+1:], true
}

// ScrubEnv removes credential-bearing entries from env.
//
// An entry is removed when its NAME matches IsCredentialEnvName or its VALUE
// matches LooksLikeCredentialValue, unless the name appears in policy.Allow.
// Everything else — PATH, HOME, LANG, GOMODCACHE, TERM, the managed proxy
// variables — passes through untouched, because a child that cannot resolve
// its own interpreter is not contained, it is broken (see
// shell.childLaunchPosture for the same lesson learned the hard way).
//
// Both directions are needed and neither subsumes the other:
//
//   - Name-only would miss DATABASE_URL=postgres://user:hunter2@db/app, whose
//     name is innocent and whose value is a password in transit.
//   - Value-only would miss an empty-but-present GH_TOKEN and, more
//     importantly, every credential shape no vendor regex here anticipates —
//     the name is the part a human chose to be descriptive.
//
// The input slice is never mutated; the result is a fresh slice.
func ScrubEnv(env []string, policy EnvScrubPolicy) EnvScrubResult {
	allow := normalizedAllowSet(policy.Allow)
	kept := make([]string, 0, len(env))
	droppedSet := make(map[string]struct{})
	for _, entry := range env {
		name, value, ok := SplitEnvEntry(entry)
		if !ok {
			kept = append(kept, entry)
			continue
		}
		if _, allowed := allow[normalizeEnvName(name)]; allowed {
			kept = append(kept, entry)
			continue
		}
		if IsCredentialEnvName(name) || (!isStructuralEnvName(name) && LooksLikeCredentialValue(value)) {
			droppedSet[name] = struct{}{}
			continue
		}
		kept = append(kept, entry)
	}
	dropped := make([]string, 0, len(droppedSet))
	for name := range droppedSet {
		dropped = append(dropped, name)
	}
	sort.Strings(dropped)
	return EnvScrubResult{Env: kept, DroppedNames: dropped}
}

// normalizedAllowSet turns a policy's Allow list into a lookup set under the
// same normalisation used for detection.
func normalizedAllowSet(allow []string) map[string]struct{} {
	if len(allow) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(allow))
	for _, name := range allow {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		set[normalizeEnvName(name)] = struct{}{}
	}
	return set
}

// normalizeEnvName upper-cases a variable name and folds '-' to '_'.
//
// Both halves are load-bearing. Windows compares environment variable names
// case-insensitively, so a policy written as "gh_token" must match a host
// entry spelled "GH_TOKEN"; and callers that copy a name out of an HTTP header
// ("X-API-Key") would otherwise produce a rule that can never fire.
func normalizeEnvName(name string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(name), "-", "_"))
}

// credentialEnvWords are the '_'-delimited words that make a variable name a
// credential regardless of which vendor prefixed it. This is what covers the
// *_API_KEY / *_TOKEN / *_SECRET / *_PASSWORD / *_CREDENTIAL / *_PRIVATE_KEY
// families in one rule instead of one entry per product.
//
// Matching on whole WORDS rather than substrings is the difference between a
// usable rule and an unusable one: substring "KEY" also matches KEYBOARD and
// substring "PWD" matches PWD, the working-directory variable every shell
// exports. PWD is therefore absent here on purpose, and PASSWD is present.
//
// AUTH is included with its eyes open. It strips SSH_AUTH_SOCK, which is not a
// credential itself but a socket that hands out signatures from every key the
// operator's agent holds — the textbook "reach out of the project" channel the
// guard's auto-approval prompt already names. A caller that genuinely needs
// agent forwarding declares it in EnvScrubPolicy.Allow; the default is off.
var credentialEnvWords = map[string]struct{}{
	"KEY": {}, "APIKEY": {}, "TOKEN": {}, "SECRET": {}, "SECRETS": {},
	"PASSWORD": {}, "PASSWD": {}, "PASSPHRASE": {},
	"CREDENTIAL": {}, "CREDENTIALS": {}, "AUTH": {}, "COOKIE": {},
	"PRIVATEKEY": {}, "SIGNINGKEY": {}, "SESSIONKEY": {},
}

// credentialEnvSuffixes catch the same families when the author ran the words
// together (GITHUBTOKEN, MYAPIKEY), which the word split above cannot see.
var credentialEnvSuffixes = []string{
	"APIKEY", "TOKEN", "SECRET", "PASSWORD", "PASSPHRASE",
	"CREDENTIAL", "CREDENTIALS", "PRIVATEKEY", "ACCESSKEY", "SECRETKEY",
}

// credentialEnvPrefixes are vendor namespaces whose whole space is treated as
// credential material.
//
// This is intentionally broader than "only the key variable": AWS_ also covers
// AWS_SESSION_TOKEN and AWS_PROFILE, and OPENAI_ also covers OPENAI_ORG_ID.
// Handing a subprocess the account identifier without the key is not a leak,
// but it is also not something any child launched here needs, and enumerating
// exactly which member of a vendor namespace is sensitive is a list that goes
// stale every time the vendor ships a variable. Callers that need one back name
// it in EnvScrubPolicy.Allow.
var credentialEnvPrefixes = []string{
	"AWS_", "AZURE_", "GCP_", "GOOGLE_",
	"OPENAI_", "ANTHROPIC_", "GEMINI_", "DASHSCOPE_", "DEEPSEEK_", "MOONSHOT_",
	"GROQ_", "MISTRAL_", "COHERE_", "TOGETHER_", "REPLICATE_", "HUGGINGFACE_", "HF_",
	"GITHUB_", "GITLAB_", "GH_", "GLPAT_",
	"NPM_", "PYPI_", "TWINE_", "CARGO_REGISTRY_",
	"SLACK_", "DISCORD_", "TELEGRAM_", "TWILIO_", "STRIPE_",
	"DINGTALK_", "FEISHU_", "LARK_",
	"VAULT_", "CLOUDFLARE_", "DIGITALOCEAN_", "VERCEL_", "NETLIFY_",
}

// credentialEnvNames are exact names that none of the shape rules above would
// catch.
var credentialEnvNames = map[string]struct{}{
	"NETRC": {}, "GIT_ASKPASS": {}, "SSH_ASKPASS": {},
	"GNUPGHOME": {}, "DOCKER_CONFIG": {}, "KUBECONFIG": {},
	"GOOGLE_APPLICATION_CREDENTIALS": {},
}

// structuralEnvNames are variables whose removal breaks the child outright.
// They are exempt from VALUE-based detection only; a structural name that also
// matched a credential NAME rule would still be dropped (none of them do).
//
// This trades a theoretical leak for a certain breakage, the same way
// MinSecretLength does. A PATH whose value happens to satisfy a vendor regex
// would leave the child unable to resolve any program at all, and the operator
// would see "command not found" with nothing pointing at this file. The leak
// side is close to nothing: nobody transports a credential in PATH by accident,
// and a caller who does it on purpose has already put it in the child's argv.
var structuralEnvNames = map[string]struct{}{
	"PATH": {}, "PATHEXT": {}, "HOME": {}, "USERPROFILE": {}, "PWD": {},
	"TMPDIR": {}, "TEMP": {}, "TMP": {}, "SHELL": {}, "COMSPEC": {},
	"LANG": {}, "LC_ALL": {}, "LC_CTYPE": {}, "TERM": {}, "TZ": {},
	"USER": {}, "USERNAME": {}, "LOGNAME": {},
	"SYSTEMROOT": {}, "WINDIR": {}, "SYSTEMDRIVE": {}, "APPDATA": {}, "LOCALAPPDATA": {},
	"GOPATH": {}, "GOROOT": {}, "GOMODCACHE": {}, "GOCACHE": {},
}

func isStructuralEnvName(name string) bool {
	_, ok := structuralEnvNames[normalizeEnvName(name)]
	return ok
}

// IsCredentialEnvName reports whether an environment variable's NAME marks it
// as credential material. Matching is case-insensitive and '-' folds to '_'.
//
// Structural names (PATH, HOME, LANG, …) are never credentials by name, so the
// check is ordered to answer that first: a future word added to
// credentialEnvWords cannot accidentally take PATH out from under every child
// process.
func IsCredentialEnvName(name string) bool {
	norm := normalizeEnvName(name)
	if norm == "" {
		return false
	}
	if _, ok := structuralEnvNames[norm]; ok {
		return false
	}
	if _, ok := credentialEnvNames[norm]; ok {
		return true
	}
	for _, prefix := range credentialEnvPrefixes {
		if strings.HasPrefix(norm, prefix) {
			return true
		}
	}
	for _, word := range strings.Split(norm, "_") {
		if _, ok := credentialEnvWords[word]; ok {
			return true
		}
	}
	for _, suffix := range credentialEnvSuffixes {
		if strings.HasSuffix(norm, suffix) {
			return true
		}
	}
	return false
}

// credentialValuePatterns are anchored, full-value shapes for credentials that
// arrive under an innocent name.
//
// Anchoring both ends is what makes value scanning safe enough to run over
// every variable: an unanchored "sk-" search would strip any PATH that happens
// to contain a directory called sk-tools, and a stripped PATH is a child that
// cannot start. Each pattern therefore describes a COMPLETE value, with the
// length the issuing vendor actually mints.
//
// The shapes come from the same vendor list the log redactor scans for
// (internal/observe/log). They are deliberately NOT shared code: that one runs
// a cheap lowercase prefix test over every attribute of every log line, where a
// false positive costs one redacted log field. Here a false positive removes a
// variable from a running program, so the test must be strict rather than
// cheap, and the two tables answer different questions about the same vendors.
var credentialValuePatterns = []*regexp.Regexp{
	// AWS access key / temporary credentials identifiers.
	regexp.MustCompile(`^(?:AKIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASIA)[A-Z0-9]{16}$`),
	// OpenAI / Anthropic and anything else minting sk- keys.
	regexp.MustCompile(`^sk-[A-Za-z0-9_\-]{16,}$`),
	// Stripe restricted / live / test keys.
	regexp.MustCompile(`^(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}$`),
	// Google API keys.
	regexp.MustCompile(`^AIza[A-Za-z0-9_\-]{35}$`),
	// GitHub classic, OAuth, user-to-server, refresh and fine-grained tokens.
	regexp.MustCompile(`^gh[pousr]_[A-Za-z0-9]{36,}$`),
	regexp.MustCompile(`^github_pat_[A-Za-z0-9_]{20,}$`),
	// GitLab personal access tokens.
	regexp.MustCompile(`^glpat-[A-Za-z0-9_\-]{16,}$`),
	// Slack bot / user / app tokens.
	regexp.MustCompile(`^xox[baprse]-[A-Za-z0-9\-]{10,}$`),
	// JWTs: header.payload with at least the separator for a signature.
	regexp.MustCompile(`^eyJ[A-Za-z0-9_\-]+\.eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]*$`),
	// An Authorization header value carried in an env var.
	regexp.MustCompile(`^(?i:bearer|basic) [A-Za-z0-9_\-.=+/]{16,}$`),
	// Connection strings with an inline password.
	regexp.MustCompile(`^(?i:mongodb|mysql|postgres|postgresql|redis|rediss|amqp|amqps)(?:\+srv)?://[^\s:/@]+:[^\s@/]+@\S+$`),
}

// pemPrivateKeyHeader opens every PEM private-key block, whatever the algorithm
// label in the middle is. Matched as a prefix rather than a full-value regex
// because a PEM block is multi-line and its body length varies by key size.
const pemPrivateKeyHeader = "-----BEGIN"

// LooksLikeCredentialValue reports whether a value has the shape of a
// credential, independent of the variable name carrying it.
//
// This is the half that catches DATABASE_URL, MY_SETTING and every other
// innocent name someone parked a token in. It answers false for the empty
// string: an exported-but-unset variable carries nothing to leak, and dropping
// it would remove a variable whose presence the child may legitimately test.
func LooksLikeCredentialValue(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, pemPrivateKeyHeader) && strings.Contains(trimmed, "PRIVATE KEY") {
		return true
	}
	for _, re := range credentialValuePatterns {
		if re.MatchString(trimmed) {
			return true
		}
	}
	return false
}
