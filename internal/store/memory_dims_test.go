package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// C14: memories gain session / agent retrieval dimensions
// ---------------------------------------------------------------------------

// TestWriteMemoryScoped_RoundTrip: the dimensions are captured at WRITE time.
// A memory whose origin was never recorded can never be filtered later, however
// good the query side becomes — which is precisely the state C14 found.
func TestWriteMemoryScoped_RoundTrip(t *testing.T) {
	s, _ := openTempStore(t)
	id, err := s.WriteMemoryScoped("note", "worktree branch is feat/x",
		MemoryFilter{SessionID: "sess-1", AgentID: "reviewer"})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	got, err := s.RecallMemory(10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "sess-1", got[0].SessionID)
	assert.Equal(t, "reviewer", got[0].AgentID)
}

// TestWriteMemory_LeavesDimensionsEmpty: the unscoped writer records the empty
// string rather than a placeholder, because the empty string is what an
// unfiltered query matches and an invented id is not.
func TestWriteMemory_LeavesDimensionsEmpty(t *testing.T) {
	s, _ := openTempStore(t)
	_, err := s.WriteMemory("note", "no idea who wrote this")
	require.NoError(t, err)
	got, err := s.RecallMemory(10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].SessionID)
	assert.Empty(t, got[0].AgentID)
}

// seedDimensionedMemories writes one memory per (session, agent) combination
// plus one dimensionless legacy-shaped row.
func seedDimensionedMemories(t *testing.T, s *Store) {
	t.Helper()
	rows := []struct {
		content string
		dims    MemoryFilter
	}{
		{"alpha from s1/agentA", MemoryFilter{SessionID: "s1", AgentID: "agentA"}},
		{"alpha from s1/agentB", MemoryFilter{SessionID: "s1", AgentID: "agentB"}},
		{"alpha from s2/agentA", MemoryFilter{SessionID: "s2", AgentID: "agentA"}},
		{"alpha from nowhere", MemoryFilter{}},
	}
	for _, r := range rows {
		_, err := s.WriteMemoryScoped("note", r.content, r.dims)
		require.NoError(t, err)
	}
}

func TestRecallMemoryScoped(t *testing.T) {
	cases := []struct {
		name    string
		filter  MemoryFilter
		wantLen int
	}{
		// The DEFAULT stays cross-dimension. Sub-agents and the goalloop have
		// always shared one table; narrowing the default would make yesterday's
		// memories disappear today with no error to notice.
		{"zero filter sees everything", MemoryFilter{}, 4},
		{"by session", MemoryFilter{SessionID: "s1"}, 2},
		{"by agent", MemoryFilter{AgentID: "agentA"}, 2},
		{"by both", MemoryFilter{SessionID: "s1", AgentID: "agentA"}, 1},
		{"no such session", MemoryFilter{SessionID: "s9"}, 0},
		{"no such agent", MemoryFilter{AgentID: "ghost"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTempStore(t)
			seedDimensionedMemories(t, s)
			got, err := s.RecallMemoryScoped(0, tc.filter)
			require.NoError(t, err)
			assert.Len(t, got, tc.wantLen)
		})
	}
}

func TestSearchMemoryScoped(t *testing.T) {
	cases := []struct {
		name    string
		filter  MemoryFilter
		wantLen int
	}{
		{"zero filter sees everything", MemoryFilter{}, 4},
		{"by session", MemoryFilter{SessionID: "s1"}, 2},
		{"by agent", MemoryFilter{AgentID: "agentB"}, 1},
		{"by both", MemoryFilter{SessionID: "s2", AgentID: "agentA"}, 1},
		{"contradictory dimensions", MemoryFilter{SessionID: "s2", AgentID: "agentB"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTempStore(t)
			seedDimensionedMemories(t, s)
			got, err := s.SearchMemoryScoped("alpha", 0, tc.filter)
			require.NoError(t, err)
			assert.Len(t, got, tc.wantLen)
			for _, m := range got {
				if tc.filter.SessionID != "" {
					assert.Equal(t, tc.filter.SessionID, m.SessionID)
				}
				if tc.filter.AgentID != "" {
					assert.Equal(t, tc.filter.AgentID, m.AgentID)
				}
			}
		})
	}
}

// TestMemoryBackwardCompatibility: the pre-C14 entry points must keep behaving
// exactly as before, i.e. as cross-dimension queries. This is the assertion
// that would catch someone "helpfully" defaulting them to the current session.
func TestMemoryBackwardCompatibility(t *testing.T) {
	s, _ := openTempStore(t)
	seedDimensionedMemories(t, s)

	recalled, err := s.RecallMemory(0)
	require.NoError(t, err)
	assert.Len(t, recalled, 4)

	found, err := s.SearchMemory("alpha", 0)
	require.NoError(t, err)
	assert.Len(t, found, 4)
}

// TestMemoryFilterWhere pins the SQL fragment shape, including the alias, so a
// join-side query and a bare query cannot drift into referencing the wrong
// table.
//
// C13 added the clause
//
//	AND superseded_by = ''
//
// to every default fragment, including the zero filter's. That is the
// load-bearing part: the zero filter is what SearchMemory / RecallMemory pass,
// so if the clause were only emitted alongside a dimension, the unscoped
// reads — which are the defaults — would keep returning rows a distillation
// has already replaced, and every merge would make retrieval worse instead of
// better.
func TestMemoryFilterWhere(t *testing.T) {
	const cur = " AND superseded_by = ''"
	cases := []struct {
		name     string
		f        MemoryFilter
		alias    string
		wantSQL  string
		wantArgs int
	}{
		{"zero", MemoryFilter{}, "m.", " AND m.superseded_by = ''", 0},
		{"session only", MemoryFilter{SessionID: "s"}, "m.",
			" AND m.session_id = ? AND m.superseded_by = ''", 1},
		{"agent only", MemoryFilter{AgentID: "a"}, "", " AND agent_id = ?" + cur, 1},
		{"both", MemoryFilter{SessionID: "s", AgentID: "a"}, "m.",
			" AND m.session_id = ? AND m.agent_id = ? AND m.superseded_by = ''", 2},
		// IncludeSuperseded is the audit path: it must drop the clause and
		// nothing else, so the lineage stays readable under any dimension.
		{"include superseded", MemoryFilter{IncludeSuperseded: true}, "m.", "", 0},
		{"include superseded with dims", MemoryFilter{SessionID: "s", IncludeSuperseded: true}, "",
			" AND session_id = ?", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := tc.f.where(tc.alias)
			assert.Equal(t, tc.wantSQL, sql)
			assert.Len(t, args, tc.wantArgs)
		})
	}
}
