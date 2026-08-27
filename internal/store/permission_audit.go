// internal/store/permission_audit.go
//
// S6: the durable sink for permission decisions.
//
// internal/tools.auditPermission has always built a structured record for every
// permission verdict — tool, decision, source, reason code — and has always
// sent it to exactly one place: slog. That answers "what is happening right
// now" and nothing at all about last night. Under yolo or auto, where decisions
// are made without a human in the loop, "which rm did the agent approve and
// why" was unanswerable the moment the terminal buffer rolled over.
//
// The record was never the missing piece; the sink was.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// maxDigestBytes truncates the command/path digest stored on an audit row.
// The digest is a diagnostic aid, not evidence: an operator reading the trail
// needs enough to recognise the action, and an unbounded field turns the audit
// table into a second copy of every tool argument ever passed.
const maxDigestBytes = 512

// PermissionAudit is one persisted permission decision.
//
// CmdDigest is a REDACTED, truncated summary of the command or paths involved.
// It is the only field that can contain caller-influenced text, which is why
// AppendPermissionAudit runs it through the store's redactor before it reaches
// SQLite — tool arguments carry API keys, tokens and connection strings often
// enough that "the audit table" and "the credential dump" would otherwise be
// the same table.
type PermissionAudit struct {
	ID         int64
	TS         int64
	SessionID  string
	AgentID    string
	Tool       string
	Decision   string
	Source     string
	ReasonCode string
	CmdDigest  string
}

// AppendPermissionAudit persists one permission decision.
//
// Tool is required — a row that cannot name the tool it judged is not an audit
// record. Everything else may be empty (a decision made on the SSE path has no
// session, a structural refusal has no source).
//
// Deliberately NOT inside the caller's transaction and deliberately not fatal
// to the caller: the audit trail records what the guard decided, and a full
// disk must not become a way to make the guard itself fail. Callers log the
// error and proceed. TS is set here from the wall clock so two records written
// by the same turn cannot disagree about when the turn happened.
func (s *Store) AppendPermissionAudit(rec PermissionAudit) error {
	if rec.Tool == "" {
		return fmt.Errorf("store: permission audit: empty tool")
	}
	ts := rec.TS
	if ts == 0 {
		ts = time.Now().Unix()
	}
	digest := s.redact(rec.CmdDigest)
	if len(digest) > maxDigestBytes {
		digest = digest[:maxDigestBytes]
	}
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO permission_audit
			   (ts, session_id, agent_id, tool, decision, source, reason_code, cmd_digest)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ts, rec.SessionID, rec.AgentID, rec.Tool, rec.Decision,
			rec.Source, rec.ReasonCode, digest,
		)
		return err
	})
}

// PermissionAuditQuery filters the audit trail. The zero value returns the most
// recent DefaultAuditPageSize records across all sessions.
//
// Since/Until are Unix seconds and inclusive/exclusive respectively; zero means
// unbounded on that end. Results are newest-first, because the question that
// brings anyone to an audit trail is almost always about the recent past.
type PermissionAuditQuery struct {
	SessionID string
	AgentID   string
	Tool      string
	Decision  string
	Since     int64
	Until     int64
	Limit     int
}

// DefaultAuditPageSize bounds an unspecified PermissionAuditQuery.Limit.
const DefaultAuditPageSize = 100

// MaxAuditPageSize bounds an over-specified PermissionAuditQuery.Limit.
const MaxAuditPageSize = 1000

// QueryPermissionAudit returns audit records matching q, newest first.
func (s *Store) QueryPermissionAudit(q PermissionAuditQuery) ([]PermissionAudit, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultAuditPageSize
	}
	if limit > MaxAuditPageSize {
		limit = MaxAuditPageSize
	}
	var sb strings.Builder
	sb.WriteString(`SELECT id, ts, session_id, agent_id, tool, decision, source, reason_code, cmd_digest
	                FROM permission_audit WHERE 1=1`)
	var args []any
	eq := func(col, val string) {
		if val != "" {
			sb.WriteString(" AND " + col + " = ?")
			args = append(args, val)
		}
	}
	eq("session_id", q.SessionID)
	eq("agent_id", q.AgentID)
	eq("tool", q.Tool)
	eq("decision", q.Decision)
	if q.Since > 0 {
		sb.WriteString(" AND ts >= ?")
		args = append(args, q.Since)
	}
	if q.Until > 0 {
		sb.WriteString(" AND ts < ?")
		args = append(args, q.Until)
	}
	sb.WriteString(" ORDER BY ts DESC, id DESC LIMIT ?")
	args = append(args, limit)

	rows, err := s.DB.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PermissionAudit
	for rows.Next() {
		var r PermissionAudit
		if err := rows.Scan(&r.ID, &r.TS, &r.SessionID, &r.AgentID, &r.Tool,
			&r.Decision, &r.Source, &r.ReasonCode, &r.CmdDigest); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
