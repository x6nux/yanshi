package lsp

import (
	"strings"
	"testing"
)

// TestLanguageServerEnvStripsCredentials pins what a language server sees.
//
// spawnLocked left cmd.Env nil, which in Go means the child inherits the whole
// parent environment. gopls, tsserver, pyright and clangd therefore received
// every provider API key and cloud credential yanshi holds — none of which a
// program that reads source files and answers questions about them has any use
// for.
//
// The second half is not decoration: a scrub that removed PATH would satisfy
// every leak assertion while making the server unable to resolve its own
// helpers. "Contained" and "broken" are different outcomes.
func TestLanguageServerEnvStripsCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-lspprobe-00000000000000000")
	t.Setenv("DATABASE_URL", "postgres://u:lspprobepassword@db/app")
	t.Setenv("YANSHI_LSP_ORDINARY", "lspprobe-ordinary")

	got := strings.Join(languageServerEnv(), "\n")

	for _, leaked := range []string{"sk-ant-lspprobe-00000000000000000", "lspprobepassword"} {
		if strings.Contains(got, leaked) {
			t.Errorf("a language server would receive %q", leaked)
		}
	}
	if !strings.Contains(got, "lspprobe-ordinary") {
		t.Error("an ordinary variable was dropped: the scrub is truncating rather than filtering")
	}
	if !strings.Contains(got, "PATH=") {
		t.Error("PATH was removed, which breaks the server rather than containing it")
	}
}
