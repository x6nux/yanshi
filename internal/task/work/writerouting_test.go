package work

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// bareWrite matches a write issued directly on the Store's *sql.DB, bypassing
// the injected WriteTxer and therefore the process-wide writeMu.
var bareWrite = regexp.MustCompile(`s\.db\.(Exec|ExecContext|BeginTx)\(`)

// TestAllWritesRouteThroughTheWriteTxer turns the package header's central
// claim into something that can fail.
//
// The header asserted "all write paths route through the injected WriteTxer"
// while wt() had zero callers and every one of the twelve write sites went
// straight to s.db. A prose claim about concurrency is exactly the kind that
// rots invisibly: the unrouted write is a latent SQLITE_BUSY under load, and
// the sentence saying otherwise is a reader who stops looking for it.
//
// Reads are untouched — writeMu serialises writers only, and routing reads
// through a transaction would cost contention for nothing.
func TestAllWritesRouteThroughTheWriteTxer(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	var offenders []string
	for i, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue // the header describes the rule; it is not a violation of it
		}
		if bareWrite.MatchString(line) {
			offenders = append(offenders, strings.TrimSpace(line)+"  (store.go:"+itoa(i+1)+")")
		}
	}
	// The regex above only rejects writes on s.db. A helper returning an
	// unlocked WriteTxer satisfies it while defeating the point entirely --
	// measured W3 review round 16 by swapping s.wt() for exactly that. The
	// invariant that actually holds is narrower and checkable: store.go
	// constructs unlockedWriteTxer exactly once, inside wt(), which is the
	// nil-Wt fallback FromDB installs for tests. A second construction is the
	// only way to reach an unlocked writer without a single s.db in sight.
	if n := strings.Count(string(src), "unlockedWriteTxer{"); n != 1 {
		offenders = append(offenders,
			"store.go constructs unlockedWriteTxer "+itoa(n)+" times, want exactly 1 (inside wt()); "+
				"an extra construction bypasses the shared writeMu without touching s.db")
	}

	if len(offenders) > 0 {
		t.Errorf("%d write(s) bypass the WriteTxer and so bypass the process-wide "+
			"writeMu — route them through s.wt().WriteTx:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
