package lsp

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// enabledManager builds an enabled Manager (WorkRoot + a Dial-injected language)
// without spawning any real language server.
func enabledManager(t *testing.T) *Manager {
	t.Helper()
	return New(Config{
		WorkRoot:  t.TempDir(),
		Languages: map[string]LanguageServer{"go": {Command: "fake"}},
		Dial: func(string) (io.Reader, io.Writer, func() error, error) {
			return nil, nil, nil, errors.New("no fake server")
		},
	})
}

// TestCov_DidChange_NoLanguage covers the detectLanguage=="" no-op branch.
func TestCov_DidChange_NoLanguage(t *testing.T) {
	m := enabledManager(t)
	m.DidChange("readme.txt", "x") // .txt → no detected language → no-op
}

// TestCov_Diagnostics_NoLanguage covers the detectLanguage=="" nil branch.
func TestCov_Diagnostics_NoLanguage(t *testing.T) {
	m := enabledManager(t)
	assert.Nil(t, m.Diagnostics("readme.txt", time.Second))
}

// TestCov_ClientFor_UnconfiguredLanguage covers the "language not configured"
// os.ErrNotExist branch (before any spawn attempt).
func TestCov_ClientFor_UnconfiguredLanguage(t *testing.T) {
	m := enabledManager(t)
	_, err := m.clientFor("rust") // rust not in Languages
	assert.Error(t, err)
}

// TestCov_Client_CloseAlreadyClosed covers the idempotent double-Close branch.
func TestCov_Client_CloseAlreadyClosed(t *testing.T) {
	c := &Client{closed: true}
	assert.NoError(t, c.Close())
}
