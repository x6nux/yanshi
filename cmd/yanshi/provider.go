// `yanshi provider` (O5) and `yanshi acp` (O11).
//
// They live in their own file rather than ops.go for the plain GOV2 reason
// (ops.go and main.go are both close to the 1000-pure-line cap) and because
// neither talks to an already-running daemon, which is the shape ops.go's
// header claims for its three.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/x6nux/yanshi/internal/acpserver"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/secrets"
)

// runProvider implements `yanshi provider <add|list>`.
func runProvider(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprint(stdout, providerUsage)
		return exitOK
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: yanshi provider <add|list> [flags]")
		return exitUsage
	}
	switch args[0] {
	case "add":
		return providerAdd(args[1:], stdin, stdout, stderr)
	case "list":
		return providerList(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown provider subcommand %q (want add or list)\n", args[0])
		return exitUsage
	}
}

// providerAdd parses the flags and runs the wizard.
//
// Every prompted value is also a flag, so the same command works from a
// provisioning script with no terminal. -api-key is offered even though the
// wizard can prompt for it, because the prompt cannot disable terminal echo
// (the reader is injected, not the process's own tty) and a flag at least lets
// a caller keep the key out of the visible session.
func providerAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "config file to amend")
	name := fs.String("name", "", "provider name (prompted when omitted)")
	kind := fs.String("kind", "", "adapter kind: "+strings.Join(cli.ProviderKinds, " | "))
	modelID := fs.String("model", "", "model id, e.g. gpt-4o")
	baseURL := fs.String("base-url", "", "API base URL (blank for the provider default)")
	apiKey := fs.String("api-key", "", "API key; stored in the secrets backend, never in the config")
	window := fs.Int("context-window", 0, "token window override (0 lets the built-in catalog decide)")
	replace := fs.Bool("replace", false, "overwrite an existing provider of the same name")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi provider add: %v\n", err)
		return exitErr
	}
	smgr, err := secrets.NewManager(secrets.Config{
		Backend:       cfg.Secrets.Backend,
		FilePath:      cfg.Secrets.FilePath,
		PassphraseEnv: cfg.Secrets.PassphraseEnv,
		Stderr:        stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "yanshi provider add: secrets: %v\n", err)
		return exitErr
	}
	defer smgr.Close()

	// ProviderAddOptions.In documents "nil disables prompting", and
	// collectProviderSpec's interactive() consults it so an OPTIONAL field
	// falls back to its documented default instead of demanding an answer.
	// That whole path was unreachable: this call site passed os.Stdin
	// unconditionally, so interactive() was always true. Measured — a fully
	// flagged, non-TTY `provider add -name … -kind … -model … -api-key …`
	// stopped at `base URL (blank for the provider default):` and died on EOF,
	// which makes the command unusable from any provisioning script.
	//
	// A pipe or heredoc is still honoured: the reader is dropped only when
	// stdin is the process's own os.Stdin AND that is not a terminal. An
	// injected reader (tests) is never dropped — someone bothering to inject
	// one is itself the answer to "is anybody there".
	promptIn := stdin
	if f, ok := stdin.(*os.File); ok && f == os.Stdin && !cli.StdinIsTerminal(f) {
		promptIn = nil
	}

	result, err := cli.RunProviderAdd(cli.ProviderAddOptions{
		ConfigPath: *configPath, Name: *name, Kind: *kind, Model: *modelID,
		BaseURL: *baseURL, APIKey: *apiKey, ContextWindow: *window,
		Replace: *replace, In: promptIn, Out: stdout, Secrets: smgr,
	})
	if err != nil {
		// The error is rendered through a SafeLogger rather than Fprintf: the
		// only string in scope that could carry a credential is one the user
		// just typed, and a wizard that echoes the key back in its own error
		// message would defeat the point of the command.
		secrets.NewSafeLogger(stderr, smgr.Redactor()).Printf("yanshi provider add: %v", err)
		if isProviderUsageError(err) {
			return exitUsage
		}
		return exitErr
	}
	cli.RenderProviderAddResult(stdout, result)
	return exitOK
}

// isProviderUsageError reports whether err is the operator's mistake (a
// refusal to clobber) rather than a runtime failure, so the exit code says
// which. A refusal is exit 2 because the operator has a decision to make;
// a failed write is exit 1 because nothing they type changes it.
func isProviderUsageError(err error) bool {
	return errors.Is(err, cli.ErrProviderExists)
}

// providerList prints the provider names the config declares.
func providerList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("provider list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "config file to read")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	names, err := cli.ListProviders(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi provider list: %v\n", err)
		return exitErr
	}
	if *asJSON {
		// An object with a named key, not a bare array: an array cannot grow a
		// field later without breaking every consumer.
		return emitJSON(stdout, stderr, providerJSON{Providers: names})
	}
	if len(names) == 0 {
		fmt.Fprintln(stdout, "no providers configured (see `yanshi provider add`)")
		return exitOK
	}
	for _, n := range names {
		fmt.Fprintln(stdout, n)
	}
	return exitOK
}

// runACPServer implements `yanshi acp`: speak the Agent Client Protocol on
// stdio as the AGENT side, so a host such as Zed can drive yanshi's own
// orchestrator.
//
// Diagnostics go to stderr and stdout carries nothing but protocol frames —
// the same contract `yanshi app` established, and for the same reason: the
// host parses stdout line by line and one stray log line desynchronises it.
func runACPServer(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "config.yaml", "path to configuration file")
	fakeModel := fs.Bool("fake-model", false, "use a deterministic fake model (no API keys needed)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "yanshi acp: unexpected positional argument")
		return exitUsage
	}

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: *configPath, FakeModel: *fakeModel})
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi acp: %v\n", err)
		return exitErr
	}
	defer app.Shutdown(context.Background())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := acpserver.New(app.AgentAPI, os.Stderr)
	if err := srv.Serve(ctx, stdin, stdout); err != nil {
		fmt.Fprintf(os.Stderr, "yanshi acp: %v\n", err)
		return exitErr
	}
	return exitOK
}

// providerUsage is the `yanshi provider -h` text. A literal for the same
// reason daemonUsage is: the verb is positional and the flags differ per verb.
const providerUsage = `Usage: yanshi provider <add|list> [flags]

Manage the LLM providers declared in config.yaml. The API key goes into the
secrets backend and the config gets only a secret:// reference, so the file
stays safe to copy, commit and attach to a bug report.

Verbs:
  add    Add (or, with -replace, overwrite) one provider. Any value omitted
         from the flags is prompted for; with no terminal every value must be
         a flag, so the same command works from a provisioning script.
  list   Print the provider names the config declares.

Flags (add):
  -config string          config file to amend (default config.yaml)
  -name string            provider name
  -kind string            adapter kind: openai | openai-responses | anthropic
  -model string           model id, e.g. gpt-4o
  -base-url string        API base URL (blank for the provider default)
  -api-key string         API key; stored in the secrets backend, never in the config
  -context-window int     token window override (0 lets the built-in catalog decide)
  -replace                overwrite an existing provider of the same name

Flags (list):
  -config string          config file to read (default config.yaml)
  -json                   emit machine-readable JSON

Providers are bound at boot, so a new one needs a backend restart;
"yanshi daemon reload" reports the section as refused rather than pretending
otherwise.
`

// providerJSON is the shape `provider list -json` emits. Declared so the
// output has a stable documented type rather than being whatever a []string
// marshals to.
type providerJSON struct {
	Providers []string `json:"providers"`
}

// runMCPAuth implements `yanshi auth mcp-login <server>` and
// `yanshi auth mcp-logout <server>`.
//
// It builds a Manager from the config purely to reach the endpoints and the
// token store, and does NOT start the servers: a login must work when the
// server is unreachable — that is frequently WHY the operator is logging in.
func runMCPAuth(
	ctx context.Context,
	sub string,
	cfg *config.Config,
	args []string,
	smgr *secrets.Manager,
	stdout io.Writer,
	safeLog *secrets.SafeLogger,
) int {
	if len(args) == 0 {
		safeLog.Printf("usage: yanshi auth %s <server>", sub)
		return exitUsage
	}
	server := args[0]

	store, err := mcp.NewSecretsTokenStore(smgr)
	if err != nil {
		safeLog.Printf("auth %s: %v", sub, err)
		return exitErr
	}
	mgr := mcp.NewManager(mcpServerConfigs(cfg))
	mgr.SetTokenStore(store)

	if sub == "mcp-logout" {
		if err := cli.RunMCPLogout(server, store, stdout); err != nil {
			safeLog.Printf("auth mcp-logout: %v", err)
			return exitErr
		}
		return exitOK
	}
	if err := cli.RunMCPLogin(ctx, cli.MCPLoginOptions{
		Server: server, Manager: mgr, Out: stdout,
	}); err != nil {
		safeLog.Printf("auth mcp-login: %v", err)
		return exitErr
	}
	return exitOK
}

// mcpServerConfigs projects config.MCP.Servers onto the mcp package's shape.
//
// It is a second, narrower copy of what bootstrap.buildMCPManager does, and
// deliberately so: this path must NOT start the servers or the health loop,
// because a login has to work while the server is unreachable — which is often
// exactly why the operator is logging in. Sharing bootstrap's builder would
// mean spawning every stdio server and waiting on every HTTP handshake to read
// two URLs out of the config.
func mcpServerConfigs(cfg *config.Config) map[string]*mcp.ServerConfig {
	out := make(map[string]*mcp.ServerConfig, len(cfg.MCP.Servers))
	for name, sc := range cfg.MCP.Servers {
		transport := mcp.TransportStdio
		if sc.Transport == "http" {
			transport = mcp.TransportHTTP
		}
		entry := &mcp.ServerConfig{
			Name: name, Enabled: sc.Enabled, Transport: transport,
			Command: sc.Command, Args: sc.Args, Env: sc.Env,
			URL: sc.URL, Bearer: sc.Bearer, Reconnect: sc.Reconnect,
		}
		// The oauth block config.MCPServerConfig now carries. Both this path
		// and bootstrap.buildMCPManager must project it: `auth mcp-login`
		// reads authorization_url/client_id from THIS copy, while the running
		// agent reads token_url from bootstrap's. Projecting in only one place
		// yields a login that succeeds and a server that never uses the token.
		if sc.OAuth != nil {
			entry.OAuth = &mcp.OAuthConfig{
				TokenURL:         sc.OAuth.TokenURL,
				AuthorizationURL: sc.OAuth.AuthorizationURL,
				ClientID:         sc.OAuth.ClientID,
				ClientSecret:     sc.OAuth.ClientSecret,
				Scopes:           sc.OAuth.Scopes,
				Grant:            sc.OAuth.Grant,
			}
		}
		out[name] = entry
	}
	return out
}
