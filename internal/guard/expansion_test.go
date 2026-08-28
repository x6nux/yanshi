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
		// FIELD SPLITTING. `${IFS}` expands to X and the resulting word is then
		// SPLIT on IFS, so the shell sees `a` and `b` — two words, not the one
		// word `aXb` this row used to assert. The old expectation was the reason
		// `IFS=,; X=rm,-rf,/; $X` graded as a program called `rm,-rf,/` and
		// reached Allow while /bin/sh deleted the root.
		{in: `IFS=X; a${IFS}b`, want: `IFS=X; a b`, changed: true},
		{in: `IFS=,; X=rm,-rf,/; $X`, want: `IFS=,; X=rm,-rf,/; rm -rf /`, changed: true},
		{in: `X=rm,-rf,/; IFS=,; $X`, want: `X=rm,-rf,/; IFS=,; rm -rf /`, changed: true},

		// Positional parameters the string itself set.
		{in: `set -- /; rm -rf $1`, want: `set -- /; rm -rf /`, changed: true},
		{in: `set -- /; rm -rf ${1}`, want: `set -- /; rm -rf /`, changed: true},
		{in: `readonly X=rm; $X -rf /`, want: `readonly X=rm; rm -rf /`, changed: true},

		// A VALUE CARRYING A CONTROL OPERATOR IS DATA. POSIX does not re-scan an
		// expansion's result for operators, so the `;` here is a word `echo`
		// prints. Emitting it bare turned this into a structural, unappealable
		// HardDeny for a command that deletes nothing; the single quotes are what
		// keep splitControlSegments from cutting the command in half.
		{in: `X='; rm -rf /'; echo $X`, want: `X='; rm -rf /'; echo ';' rm -rf /`, changed: true},
		{in: `X=' && rm -rf /'; echo $X`, want: `X=' && rm -rf /'; echo  '&&' rm -rf /`, changed: true},

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
