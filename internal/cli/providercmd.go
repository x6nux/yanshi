package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/x6nux/yanshi/internal/secrets"
)

// ErrProviderExists is returned when the config already declares a provider of
// the requested name and the caller did not ask to replace it.
//
// Refusing is the point. A provider block carries the operator's model choice,
// base URL and generation parameters; overwriting silently because the name
// matched would discard all of it to change one API key.
var ErrProviderExists = errors.New("cli: provider already exists")

// ProviderKinds are the adapter kinds the config accepts, in the order the
// prompt offers them.
//
// The list is here rather than derived from internal/llm/eino because
// internal/cli must not depend on the LLM layer to run a wizard. It is short
// and changes rarely; ProviderKindsAreValid keeps it honest against the config
// validator, which is the component that actually rejects a bad value.
var ProviderKinds = []string{"openai", "openai-responses", "anthropic"}

// ProviderAddOptions configures RunProviderAdd.
type ProviderAddOptions struct {
	// ConfigPath is the config file to amend. Empty means "config.yaml".
	ConfigPath string
	// Name, Kind, Model, BaseURL and APIKey come from flags. Any left empty is
	// prompted for when In is a terminal-backed reader.
	Name    string
	Kind    string
	Model   string
	BaseURL string
	APIKey  string
	// ContextWindow is optional; 0 leaves the key out so the built-in model
	// catalog resolves it.
	ContextWindow int
	// Replace permits overwriting an existing provider of the same name. The
	// file is backed up first, exactly as `yanshi init -force` does.
	Replace bool
	// In supplies interactive answers. nil disables prompting, so every value
	// must arrive as a flag — which is what makes this usable from a
	// provisioning script.
	In io.Reader
	// Out receives the prompts. nil discards them.
	Out io.Writer
	// Secrets stores the API key. Required: this command exists specifically
	// so a key does NOT end up in config.yaml, and a nil manager would quietly
	// undo that.
	Secrets *secrets.Manager
}

// ProviderAddResult describes what RunProviderAdd changed.
type ProviderAddResult struct {
	// Path is the config file written; Backup names the preserved original
	// when Replace overwrote a provider.
	Path   string
	Backup string
	// Name is the provider added or replaced.
	Name string
	// SecretRef is the `secret://…` reference written into the config in place
	// of the key. The KEY ITSELF is never in this struct: a result is printed,
	// logged and returned from a function, and every one of those is a place a
	// credential must not be.
	SecretRef string
	// Replaced reports that an existing provider of this name was overwritten.
	Replaced bool
	// RestartRequired is always true today and is a FIELD rather than a
	// constant so the day llm.providers becomes reloadable, one place changes.
	// See internal/cli/daemon.go's nonReloadableSections: provider clients and
	// their context windows are built at boot, so a running daemon cannot
	// adopt a new one.
	RestartRequired bool
}

// ProviderSecretService is the credential-backend namespace for provider API
// keys. The account within it is the provider name, so one key per provider is
// representable and replacing one leaves the others alone.
const ProviderSecretService = "yanshi-provider"

// ProviderSecretRef renders the config-side reference for a provider's key.
//
// The config stores a REFERENCE, not the key. That is the entire reason this
// command exists rather than the operator editing YAML: config.yaml gets copied
// into dotfile repositories, attached to bug reports and read by every process
// on the machine, and a provider key in it is a credential in all three places.
func ProviderSecretRef(name string) string {
	return "secret://" + ProviderSecretService + "/" + name
}

// RunProviderAdd adds (or replaces) a provider in config.yaml, storing the API
// key in the secrets backend and writing only a reference into the file.
//
// Editing YAML in place rather than regenerating it is deliberate: the shipped
// config.example.yaml is heavily commented, and a round-trip through a Go
// struct would erase every comment the operator has been reading. yaml.Node
// preserves them.
func RunProviderAdd(opts ProviderAddOptions) (ProviderAddResult, error) {
	configPath := opts.ConfigPath
	if strings.TrimSpace(configPath) == "" {
		configPath = "config.yaml"
	}
	if opts.Secrets == nil || opts.Secrets.Store() == nil {
		return ProviderAddResult{}, fmt.Errorf(
			"no secrets backend is configured, so the API key cannot be stored outside config.yaml; " +
				"set secrets.backend in the config first")
	}
	spec, err := collectProviderSpec(opts)
	if err != nil {
		return ProviderAddResult{}, err
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return ProviderAddResult{}, fmt.Errorf("read %s: %w (run `yanshi init` first)", configPath, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return ProviderAddResult{}, fmt.Errorf("parse %s: %w", configPath, err)
	}

	replaced, err := upsertProvider(&doc, spec)
	if err != nil {
		return ProviderAddResult{}, err
	}
	if replaced && !opts.Replace {
		return ProviderAddResult{}, fmt.Errorf("%w: %s (use -replace to overwrite; the "+
			"existing config is backed up first)", ErrProviderExists, spec.Name)
	}

	result := ProviderAddResult{
		Path: configPath, Name: spec.Name, SecretRef: ProviderSecretRef(spec.Name),
		Replaced: replaced, RestartRequired: true,
	}
	// The secret is written BEFORE the config. A config that names a secret
	// which does not exist fails on the first call with "secret not found"; a
	// secret with no config entry is inert. The first is a broken deployment,
	// the second is garbage — so the ordering puts the recoverable failure
	// first if the process dies between the two writes.
	if err := opts.Secrets.Set(ProviderSecretService, spec.Name, spec.APIKey); err != nil {
		return ProviderAddResult{}, fmt.Errorf("store the API key: %w", err)
	}
	if r := opts.Secrets.Redactor(); r != nil {
		r.Register(spec.APIKey)
	}

	backup, err := writeProviderConfig(configPath, &doc)
	if err != nil {
		return ProviderAddResult{}, err
	}
	result.Backup = backup
	return result, nil
}

// providerSpec is the validated set of answers.
type providerSpec struct {
	Name          string
	Kind          string
	Model         string
	BaseURL       string
	APIKey        string
	ContextWindow int
}

// collectProviderSpec fills the spec from flags, prompting for whatever is
// missing, and validates the result.
//
// Validation happens AFTER prompting rather than per-answer so a scripted
// invocation with a bad flag gets the same message an interactive one does,
// and so a wizard cannot leave the operator half-way through a config they
// will be told is invalid at the end anyway.
func collectProviderSpec(opts ProviderAddOptions) (providerSpec, error) {
	spec := providerSpec{
		Name: strings.TrimSpace(opts.Name), Kind: strings.TrimSpace(opts.Kind),
		Model: strings.TrimSpace(opts.Model), BaseURL: strings.TrimSpace(opts.BaseURL),
		APIKey: opts.APIKey, ContextWindow: opts.ContextWindow,
	}
	p := newProviderPrompter(opts.In, opts.Out)
	var err error
	if spec.Name == "" {
		if spec.Name, err = p.ask("provider name (e.g. openai, my-gateway)", ""); err != nil {
			return spec, err
		}
	}
	if spec.Kind == "" {
		if spec.Kind, err = p.ask("adapter kind ("+strings.Join(ProviderKinds, " / ")+")",
			ProviderKinds[0]); err != nil {
			return spec, err
		}
	}
	if spec.Model == "" {
		if spec.Model, err = p.ask("model id (e.g. gpt-4o)", ""); err != nil {
			return spec, err
		}
	}
	if spec.BaseURL == "" && p.interactive() {
		// OPTIONAL, so it is only prompted for when there is someone to ask.
		// An empty base URL means the adapter's own default endpoint, which is
		// what a first-party provider wants — so a scripted caller that omits
		// the flag must get that default rather than "base URL is required;
		// pass it as a flag", which is how this first shipped and which made
		// the whole command unusable without a terminal.
		if spec.BaseURL, err = p.ask("base URL (blank for the provider default)", ""); err != nil {
			return spec, err
		}
	}
	if spec.APIKey == "" {
		if spec.APIKey, err = p.askSecret("API key"); err != nil {
			return spec, err
		}
	}
	return spec, validateProviderSpec(spec)
}

// validateProviderSpec rejects the answers that produce a provider which cannot
// work, naming the field rather than failing later at first call.
func validateProviderSpec(spec providerSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("provider name is required")
	}
	if strings.ContainsAny(spec.Name, " \t/:") {
		// The name becomes both a YAML key and the account under which the key
		// is stored; a slash or colon in it silently reshapes the secret ref.
		return fmt.Errorf("provider name %q may not contain spaces, slashes or colons", spec.Name)
	}
	if spec.Model == "" {
		return fmt.Errorf("model id is required")
	}
	if !providerKindIsKnown(spec.Kind) {
		return fmt.Errorf("unknown adapter kind %q; expected one of: %s",
			spec.Kind, strings.Join(ProviderKinds, ", "))
	}
	if strings.TrimSpace(spec.APIKey) == "" {
		return fmt.Errorf("API key is required")
	}
	if len(spec.APIKey) < secrets.MinSecretLength {
		// Below this length the Redactor drops the value, so the key would
		// appear verbatim in logs and stored messages. Saying so now beats
		// discovering it in a transcript.
		return fmt.Errorf("the API key is only %d characters; yanshi cannot redact anything shorter "+
			"than %d, so it would appear verbatim in logs", len(spec.APIKey), secrets.MinSecretLength)
	}
	if spec.ContextWindow < 0 {
		return fmt.Errorf("context window must not be negative")
	}
	return nil
}

// providerKindIsKnown reports whether kind is in ProviderKinds.
func providerKindIsKnown(kind string) bool {
	for _, k := range ProviderKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// providerPrompter reads answers from a reader, writing prompts to a writer.
// A nil reader makes every ask fail with a message naming the flag to pass,
// which is what turns "this wizard needs a terminal" into a scriptable error.
type providerPrompter struct {
	in  *bufio.Reader
	out io.Writer
}

func newProviderPrompter(in io.Reader, out io.Writer) *providerPrompter {
	p := &providerPrompter{out: out}
	if out == nil {
		p.out = io.Discard
	}
	if in != nil {
		p.in = bufio.NewReader(in)
	}
	return p
}

// interactive reports whether there is a reader to prompt against. Optional
// fields consult it so a scripted caller gets the documented default instead of
// a demand for a flag it deliberately left off.
func (p *providerPrompter) interactive() bool { return p.in != nil }

// ask prompts for one value, returning def when the answer is blank.
func (p *providerPrompter) ask(label, def string) (string, error) {
	if p.in == nil {
		return "", fmt.Errorf("%s is required; pass it as a flag (no interactive input is available)", label)
	}
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", label)
	}
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

// askSecret prompts for a credential.
//
// It does NOT disable terminal echo, and that is a deliberate limitation rather
// than an oversight: turning echo off requires the input to be the process's
// own terminal, while this reader is injected precisely so the command stays
// testable and scriptable. The honest handling is to warn that the value will
// be visible and to recommend the flag or the environment for anyone who cares.
func (p *providerPrompter) askSecret(label string) (string, error) {
	if p.in == nil {
		return "", fmt.Errorf("%s is required; pass -api-key (no interactive input is available)", label)
	}
	fmt.Fprintf(p.out, "%s (typed characters are visible; use -api-key to avoid the echo): ", label)
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	return strings.TrimSpace(line), nil
}

// writeProviderConfig re-serialises the document, backing up the original and
// writing atomically through a temp file plus rename.
//
// The backup is unconditional rather than only-on-replace. This command rewrites
// a hand-maintained file; even a pure append is a rewrite, and a YAML
// round-trip that lost something must be recoverable.
func writeProviderConfig(path string, doc *yaml.Node) (string, error) {
	backup, err := backupFile(path)
	if err != nil {
		return "", fmt.Errorf("back up %s: %w", path, err)
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("encode %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".provider-*.yaml")
	if err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	// 0600 for the same reason `yanshi init` uses it: this file is one edit
	// away from holding a literal credential.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return backup, nil
}

// RenderProviderAddResult prints the outcome, in the style of
// RenderInitResult: what changed, and what the operator must do next.
func RenderProviderAddResult(w io.Writer, r ProviderAddResult) {
	verb := "added"
	if r.Replaced {
		verb = "replaced"
	}
	fmt.Fprintf(w, "%s provider %q in %s\n", verb, r.Name, r.Path)
	if r.Backup != "" {
		fmt.Fprintf(w, "previous config preserved at %s\n", r.Backup)
	}
	fmt.Fprintf(w, "the API key is in the secrets backend; the config holds only %s\n", r.SecretRef)
	if r.RestartRequired {
		// Not a hedge: llm.providers is on the non-reloadable list because the
		// clients and their context windows are built at boot. `daemon reload`
		// would report it REJECTED, so saying "restart" here is the accurate
		// instruction rather than the cautious one.
		fmt.Fprintln(w, "restart the backend for it to take effect "+
			"(`yanshi daemon stop`, then start it again — providers are bound at boot and cannot be reloaded)")
	}
}

// sortedProviderNames lists the provider names declared in a parsed config, for
// the `provider list` view.
func sortedProviderNames(doc *yaml.Node) []string {
	seq := providerSequence(doc, false)
	if seq == nil {
		return nil
	}
	var out []string
	for _, entry := range seq.Content {
		if name := mappingValue(entry, "name"); name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// providerContextWindowNode renders the optional context_window scalar, or nil
// when it should be omitted so the built-in catalog resolves the window.
func providerContextWindowNode(n int) *yaml.Node {
	if n <= 0 {
		return nil
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(n)}
}

// ListProviders returns the provider names declared in the config at path.
//
// It reads the YAML directly rather than going through config.Load because
// Load applies defaults, expands ${VAR} references and validates every other
// section — none of which this needs, and all of which turn "list what is
// declared" into "refuse to answer because the compaction block is malformed".
func ListProviders(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		path = "config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return sortedProviderNames(&doc), nil
}
