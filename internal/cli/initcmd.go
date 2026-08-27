package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrConfigExists is returned by RunInit when the target config already exists
// and Force was not set.
//
// Refusing is the whole safety story of this command: a config file holds the
// operator's provider choices, their profile policy and their storage paths,
// and none of that is recoverable from the template. An init that overwrote
// silently would be a data-loss command wearing a setup command's name.
var ErrConfigExists = errors.New("cli: config already exists")

// InitOptions configures RunInit.
type InitOptions struct {
	// ConfigPath is the file to write. Empty means "config.yaml".
	ConfigPath string
	// ExamplePath is the template to copy. Empty resolves via
	// DefaultExamplePath.
	ExamplePath string
	// Force permits overwriting an existing config. The existing file is
	// backed up first, so even the explicit override cannot lose the original.
	Force bool
	// Env resolves an environment variable name to its value. Nil means
	// os.LookupEnv. Injected so tests do not have to mutate the process
	// environment.
	Env func(string) (string, bool)
}

// InitResult describes what RunInit produced.
type InitResult struct {
	// Path is the config file written.
	Path string
	// TemplateSource names where the template came from — a filesystem path,
	// or "built-in config.example.yaml" when the compiled-in copy was used.
	//
	// Reported because the two can differ in content: inside the source tree
	// the on-disk file wins and may be ahead of the binary. An operator
	// debugging "why does my config not have the key the docs mention" needs
	// to know which one they got.
	TemplateSource string
	// Backup is the path of the pre-existing config that was preserved, when
	// Force overwrote one. Empty otherwise.
	Backup string
	// ResolvedKeys lists the provider environment variables that were found
	// and left in place as ${VAR} references. Names only: the VALUES are
	// credentials and never appear in a result, a log line or the console.
	ResolvedKeys []string
	// MissingKeys lists the provider environment variables the template
	// references that are NOT set in this environment. The operator has to set
	// them before the config works, and saying so at init time is the whole
	// difference between a config that works and one that fails at first call.
	MissingKeys []string
}

// RunInit generates a config.yaml from config.example.yaml.
//
// It is deliberately non-interactive. The template is already the documented,
// commented, working configuration; a wizard that asked the same questions
// would produce a worse file (no comments, every default materialised as an
// explicit setting) and would be unusable from a provisioning script. What the
// operator actually needs at this moment is the file plus an honest list of
// which environment variables it still expects — which is what this returns.
//
// ${VAR} references are left AS references rather than expanded. Expanding
// them would bake a credential into a file on disk, which is precisely the
// thing config.example.yaml's ${VAR} convention exists to avoid.
func RunInit(opts InitOptions) (InitResult, error) {
	configPath := opts.ConfigPath
	if strings.TrimSpace(configPath) == "" {
		configPath = "config.yaml"
	}
	lookup := opts.Env
	if lookup == nil {
		lookup = os.LookupEnv
	}

	// The template comes from LoadExampleTemplate, not os.ReadFile, because
	// this command's entire audience is people who do NOT have a
	// config.example.yaml sitting next to them. Reading the working directory
	// made init succeed only inside the yanshi source tree — measured, not
	// theorised: a first run in a fresh directory exited 1 with
	// `read template "config.example.yaml": no such file or directory`.
	templateText, source, err := LoadExampleTemplate(configPath, opts.ExamplePath)
	if err != nil {
		return InitResult{}, err
	}
	template := []byte(templateText)

	result := InitResult{Path: configPath, TemplateSource: source}
	if _, statErr := os.Stat(configPath); statErr == nil {
		if !opts.Force {
			return InitResult{}, fmt.Errorf("%w: %s (use -force to overwrite; the "+
				"existing file is backed up first)", ErrConfigExists, configPath)
		}
		backup, berr := backupFile(configPath)
		if berr != nil {
			return InitResult{}, fmt.Errorf("back up existing %q: %w", configPath, berr)
		}
		result.Backup = backup
	}

	result.ResolvedKeys, result.MissingKeys = classifyEnvRefs(string(template), lookup)

	if dir := filepath.Dir(configPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return InitResult{}, err
		}
	}
	// 0600 rather than 0644: this file is where api keys land the moment the
	// operator replaces a ${VAR} with a literal, and doctor's permission
	// repair would otherwise flag a file yanshi itself just created.
	if err := os.WriteFile(configPath, template, 0o600); err != nil {
		return InitResult{}, err
	}
	return result, nil
}

// envRefPrefixes are the config keys whose ${VAR} references are credentials
// worth reporting on. Reporting on EVERY ${VAR} in the file would bury the two
// that matter under paths and ports.
var envRefPrefixes = []string{"api_key", "token", "client_secret", "passphrase"}

// classifyEnvRefs scans the template for `key: "${VAR}"` on a credential-ish
// key and splits the VAR names into (set, unset).
//
// Only names are returned, never values. The point of the report is "you still
// need to export OPENAI_API_KEY", and printing the value would defeat the
// ${VAR} indirection the template exists to provide.
func classifyEnvRefs(template string, lookup func(string) (string, bool)) (resolved, missing []string) {
	seenResolved := map[string]bool{}
	seenMissing := map[string]bool{}

	for _, line := range strings.Split(template, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // a commented-out block is not a reference the config makes
		}
		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "- "))
		if !isCredentialKey(key) {
			continue
		}
		for _, name := range envRefNames(value) {
			if _, ok := lookup(name); ok {
				seenResolved[name] = true
				continue
			}
			seenMissing[name] = true
		}
	}
	return sortedKeys(seenResolved), sortedKeys(seenMissing)
}

// isCredentialKey reports whether a YAML key names a credential field.
func isCredentialKey(key string) bool {
	for _, prefix := range envRefPrefixes {
		if key == prefix {
			return true
		}
	}
	return false
}

// envRefNames extracts every ${VAR} name from a YAML scalar.
func envRefNames(value string) []string {
	var out []string
	rest := value
	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			return out
		}
		rest = rest[start+2:]
		end := strings.Index(rest, "}")
		if end < 0 {
			return out
		}
		name := strings.TrimSpace(rest[:end])
		if name != "" {
			out = append(out, name)
		}
		rest = rest[end+1:]
	}
}

// sortedKeys returns a set's members in a stable order.
func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RenderInitResult writes the human-readable summary of an init run.
//
// The missing-variable list is the part worth printing: an operator who reads
// "wrote config.yaml" and nothing else discovers the unset key at their first
// prompt, with an error from the provider rather than from yanshi.
func RenderInitResult(w io.Writer, r InitResult) {
	if r.TemplateSource != "" {
		fmt.Fprintf(w, "wrote %s (from %s)\n", r.Path, r.TemplateSource)
	} else {
		fmt.Fprintf(w, "wrote %s\n", r.Path)
	}
	if r.Backup != "" {
		fmt.Fprintf(w, "previous config preserved at %s\n", r.Backup)
	}
	if len(r.ResolvedKeys) > 0 {
		fmt.Fprintf(w, "provider credentials found in the environment: %s\n",
			strings.Join(r.ResolvedKeys, ", "))
	}
	if len(r.MissingKeys) > 0 {
		fmt.Fprintf(w, "still to set before yanshi can call a provider: %s\n",
			strings.Join(r.MissingKeys, ", "))
		fmt.Fprintln(w, "  (export them, or replace the ${VAR} reference in the config)")
	}
	fmt.Fprintln(w, "next: `yanshi doctor` to verify, then `yanshi` to start")
}
