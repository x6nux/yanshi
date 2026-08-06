package approval

import (
	"testing"
	"time"
)

// TestEveryMatchOutcomeIsAudited pins "每次命中可审计" as a property of Match
// rather than of the audit bus.
//
// The bus had tests (Publish, Subscribe, drop-on-full) and the miss path had
// one, but both hit paths -- session and persistent -- ran with a nil sink, so
// nothing asserted that a successful authorization is recorded at all. An
// approval that silently grants is the one an operator most needs to find
// afterwards: it is the entry that explains why a tool ran without asking.
//
// The table walks every outcome Match can produce, because "每次" is the load
// bearing word. A test that only covered the persistent hit would still pass
// if the session branch stopped auditing.
func TestEveryMatchOutcomeIsAudited(t *testing.T) {
	cases := []struct {
		name     string
		ttl      TTL
		session  string
		wantKind []string // kinds expected, in order
	}{
		{"persistent hit", TTLPersistent, "other-session", []string{"hit"}},
		{"session hit", TTLSession, "s", []string{"hit"}},
		{"once hit is consumed", TTLOnce, "s", []string{"hit", "consume"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var kinds []string
			var ruleIDs []string
			m, err := New(&fakeKV{}, "proc-1", func(e AuditEvent) {
				kinds = append(kinds, e.Kind)
				ruleIDs = append(ruleIDs, e.RuleID)
			})
			if err != nil {
				t.Fatal(err)
			}
			scope := Scope{Tool: "shell_run", Program: "go"}
			rule := Rule{ID: "r1", Action: "shell_run", Scope: scope, TTL: tc.ttl, Source: SourceUser}
			if tc.ttl != TTLOnce {
				rule.ExpiresAt = time.Now().Add(time.Hour)
			}
			if err := m.Record("s", rule); err != nil {
				t.Fatal(err)
			}
			kinds, ruleIDs = nil, nil // drop whatever Record itself emitted

			hit, got := m.Match(tc.session, scope, time.Now())
			if !hit || got == nil {
				t.Fatalf("expected a hit, got hit=%v rule=%v", hit, got)
			}
			if len(kinds) != len(tc.wantKind) {
				t.Fatalf("audit kinds = %v, want %v", kinds, tc.wantKind)
			}
			for i := range tc.wantKind {
				if kinds[i] != tc.wantKind[i] {
					t.Fatalf("audit kinds = %v, want %v", kinds, tc.wantKind)
				}
			}
			// The rule id is what makes an audit line actionable: without it
			// the operator knows something was allowed but not by which rule.
			for _, id := range ruleIDs {
				if id != "r1" {
					t.Fatalf("audit event carries rule id %q, want r1", id)
				}
			}
		})
	}

	t.Run("miss is audited too", func(t *testing.T) {
		var kinds []string
		m, err := New(&fakeKV{}, "proc-1", func(e AuditEvent) { kinds = append(kinds, e.Kind) })
		if err != nil {
			t.Fatal(err)
		}
		if hit, _ := m.Match("s", Scope{Tool: "shell_run"}, time.Now()); hit {
			t.Fatal("expected a miss")
		}
		if len(kinds) != 1 || kinds[0] != "miss" {
			t.Fatalf("audit kinds = %v, want [miss]", kinds)
		}
	})
}
