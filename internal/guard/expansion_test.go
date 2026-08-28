package guard

import "testing"

// TestExpandKnownParametersResolvesOnlyWhatTheStringDefines pins the ONE rule
// that keeps this reading from being a regression, plus the three claims
// expansion.go's header makes about scope.
//
// The corpus in bypasscorpus_test.go asserts the end-to-end verdicts; this
// asserts the rule those verdicts rest on, because "resolve nothing whose value
// is absent from the string" is invisible in a verdict — a reader that resolved
// everything to the empty string would turn most of the same rows red and this
// is where it would be caught.
func TestExpandKnownParametersResolvesOnlyWhatTheStringDefines(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		changed  bool
	}{
		// Values the string carries.
		{in: `rm${IFS}-rf${IFS}/`, want: `rm -rf /`, changed: true},
		{in: `${x:-rm} -rf /`, want: `rm -rf /`, changed: true},
		{in: `${x-rm} -rf /`, want: `rm -rf /`, changed: true},
		{in: `rm -rf "${x:-/}"`, want: `rm -rf "/"`, changed: true},
		{in: `X=rm; $X -rf /`, want: `X=rm; rm -rf /`, changed: true},
		{in: `export X=rm; ${X} -rf /`, want: `export X=rm; rm -rf /`, changed: true},
		{in: `set -- rm -rf /; "$@"`, want: `set -- rm -rf /; rm -rf /`, changed: true},
		{in: `IFS=X; a${IFS}b`, want: `IFS=X; aXb`, changed: true},

		// Values the string does NOT carry. Leaving them alone is what stops
		// `rm -rf $BUILD_DIR` from collapsing into a bare, structurally refused
		// `rm -rf`; blanking them here is the regression this row exists for.
		{in: `rm -rf $BUILD_DIR`, want: `rm -rf $BUILD_DIR`, changed: false},
		{in: `rm -rf ${BUILD_DIR}`, want: `rm -rf ${BUILD_DIR}`, changed: false},
		{in: `rm -rf ${x:+/}`, want: `rm -rf ${x:+/}`, changed: false},
		{in: `rm -rf $1`, want: `rm -rf $1`, changed: false},
		{in: `rm -rf $(echo /)`, want: `rm -rf $(echo /)`, changed: false},
		{in: `rm -rf $'\x2f'`, want: `rm -rf $'\x2f'`, changed: false},

		// Single quotes suppress expansion in every POSIX shell.
		{in: `echo '${x:-rm}'`, want: `echo '${x:-rm}'`, changed: false},

		// Definitions are collected LEFT TO RIGHT: a value assigned after a use
		// does not reach back to it, which is what the shell does.
		{in: `rm -rf $X; X=/`, want: `rm -rf $X; X=/`, changed: false},

		// A KEY=value that is not a leading assignment is an operand.
		{in: `grep FOO=1 file; echo $FOO`, want: `grep FOO=1 file; echo $FOO`, changed: false},
	} {
		got, changed := expandKnownParameters(tc.in)
		if got != tc.want || changed != tc.changed {
			t.Errorf("expandKnownParameters(%q) = (%q, %v), want (%q, %v)",
				tc.in, got, changed, tc.want, tc.changed)
		}
	}
}

// TestElideExpansionsRestoresACredentialSegment pins the opposite decision the
// credential denylist takes about an unresolved expansion, and the reason the
// two are allowed to disagree: this reading is only ever consulted for a PATH,
// where an extra match costs one prompt.
func TestElideExpansionsRestoresACredentialSegment(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		changed  bool
	}{
		{in: `~/.s${x}sh/authorized_keys`, want: `~/.ssh/authorized_keys`, changed: true},
		{in: `~/.ssh${x}/authorized_keys`, want: `~/.ssh/authorized_keys`, changed: true},
		{in: `~/.s$xsh/authorized_keys`, want: `~/.s/authorized_keys`, changed: true},
		{in: `~/.ssh/authorized_keys`, want: `~/.ssh/authorized_keys`, changed: false},
	} {
		got, changed := elideExpansions(tc.in)
		if got != tc.want || changed != tc.changed {
			t.Errorf("elideExpansions(%q) = (%q, %v), want (%q, %v)",
				tc.in, got, changed, tc.want, tc.changed)
		}
	}
}
