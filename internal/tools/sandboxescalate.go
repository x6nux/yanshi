// internal/tools/sandboxescalate.go
//
// S12, half two: the closed loop. A sandbox refusal becomes an operator
// question, the answer becomes at most one retry at a higher access tier, and
// every terminal path becomes an explanation the model can act on.
//
// # The four paths, and the one that must never exist
//
//	approved   -> retry ONCE at the next tier up; report which tier ran
//	denied     -> explained failure ("the sandbox refused X; you did not get
//	              permission to widen it")
//	timed out  -> the SAME explained failure. A timeout is the absence of an
//	              authorization gesture. It is routed through
//	              tools.RequireApproval, which reaches the WS callback, which
//	              is where S5's awaitDecision turns an expiry into
//	              PermissionDeny — so this file never needs a timeout branch
//	              of its own, and cannot grow one that disagrees.
//	no callback -> the same explained failure, immediately. SSE has no
//	              interactive channel; there is nobody to ask.
//
// The path that must never exist is "escalate without an answer". That is
// guaranteed structurally rather than by inspection: askEscalation's ONLY
// success return is the explicit PermissionAllow branch of RequireApproval,
// and every other outcome — including ones that do not exist yet — falls to
// the default. See TestEscalationNeverAllowsWithoutExplicitApproval.
//
// # Why the retry is capped at one, and at one RUNG
//
// Two independent caps, because they fail differently. "Once" bounds the
// number of prompts a single tool call can put in front of a human: without
// it, a command that is denied at every tier walks the operator up the whole
// ladder one dialog at a time. "One rung" (NextSandboxTier) bounds what a
// single yes grants: a yes to "this needs to write in the workspace" must not
// also hand over the network and the rest of the filesystem.
//
// The retry is additionally refused when the tier did not actually rise. At
// FullAccess there is no higher tier, so re-running would be the identical
// command with the identical sandbox — a second failure, a wasted timeout, and
// a prompt the operator answered for nothing.
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/x6nux/yanshi/internal/sandbox"
)

// SandboxEscalationOutcome names how one escalation attempt ended. It is the
// field tests assert on, because the DECISION (a failed command) is the same
// for three of the five and only the outcome distinguishes them.
type SandboxEscalationOutcome string

// The five terminal outcomes. Every one but EscalationRetried leaves the
// original failure in place; they differ in the explanation handed to the
// model and in what the audit trail records.
const (
	// EscalationNone: the attempt was not a sandbox violation at all. The
	// command's own result stands, untouched.
	EscalationNone SandboxEscalationOutcome = "not-a-violation"
	// EscalationRetried: the operator approved and the command was re-run at
	// a strictly higher tier. Says nothing about whether the retry SUCCEEDED.
	EscalationRetried SandboxEscalationOutcome = "retried"
	// EscalationDenied: a human was asked and said no (or the prompt expired,
	// which S5 delivers as a denial).
	EscalationDenied SandboxEscalationOutcome = "denied"
	// EscalationNoCallback: no interactive channel was bound, so nobody could
	// be asked. The SSE path.
	EscalationNoCallback SandboxEscalationOutcome = "no-callback"
	// EscalationTierExhausted: the command already ran at FullAccess. Nothing
	// to escalate to, so nobody is asked at all.
	EscalationTierExhausted SandboxEscalationOutcome = "tier-exhausted"
)

// SandboxAttempt is one finished run of a command, reduced to the three fields
// violation classification needs.
//
// Deliberately NOT commandResult: the escalation loop is driven from two call
// sites with different result types (the capture path's commandResult, the
// streaming path's ring-buffered tail), and coupling the loop to either one
// would mean the other could not use it — which is how a security behaviour
// ends up implemented on one path and merely documented on the other.
type SandboxAttempt struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// SandboxEscalation is the loop's verdict, alongside whatever result the
// command itself produced.
type SandboxEscalation struct {
	// Outcome is which of the five paths was taken.
	Outcome SandboxEscalationOutcome
	// Violation is the refusal that started it. Zero when Outcome is
	// EscalationNone.
	Violation SandboxViolation
	// FromTier / ToTier are the access tiers of the first and (when retried)
	// second attempt. Equal when no retry happened.
	FromTier, ToTier sandbox.AccessTier
	// Explanation is the model-facing text. Empty when Outcome is
	// EscalationNone (there is nothing to explain) and when the retry ran (the
	// retry's own result is the answer).
	Explanation string
}

// Retried reports whether the command was actually re-run. Callers use it to
// decide whether the result they hold came from the escalated attempt.
func (e SandboxEscalation) Retried() bool { return e.Outcome == EscalationRetried }

// sandboxAttemptFunc runs the command once at the given tier.
type sandboxAttemptFunc func(context.Context, sandbox.AccessTier) (SandboxAttempt, error)

// EscalateOnSandboxViolation runs a command, and on a sandbox refusal asks the
// operator whether to retry it once at a higher access tier.
//
// tool names the tool for the approval prompt and the audit record. command is
// the human-readable command text shown to the operator — it is the ONLY thing
// they have to judge the request by, so a caller that passes "" produces a
// dialog asking permission for nothing identifiable. baseTier is the tier the
// first attempt runs under.
//
// run is invoked at most twice and never concurrently. Its error return is the
// LAUNCH failure (no factory, authorization refused, pipe broken) and short
// circuits everything: a command that never started cannot have been refused
// by a sandbox.
//
// The returned SandboxAttempt is whichever attempt ran last, so a caller can
// use it directly without checking Retried() first.
func EscalateOnSandboxViolation(ctx context.Context, tool, command string,
	baseTier sandbox.AccessTier, run sandboxAttemptFunc) (SandboxAttempt, SandboxEscalation, error) {

	attempt, err := run(ctx, baseTier)
	if err != nil {
		return attempt, SandboxEscalation{Outcome: EscalationNone, FromTier: baseTier, ToTier: baseTier}, err
	}
	sb, ok := SandboxFromContext(ctx)
	if !ok || sb == nil {
		return attempt, SandboxEscalation{Outcome: EscalationNone, FromTier: baseTier, ToTier: baseTier}, nil
	}
	rep := sb.Report()
	// The report's Requested tier describes the sandbox as a whole; this
	// invocation may have asked for something else via UseSandboxTier. Judge
	// the tier that actually ran, so the ladder starts from the right rung.
	rep.Requested = baseTier
	violation, isViolation := ClassifySandboxViolation(rep, attempt.ExitCode, attempt.Stdout, attempt.Stderr)
	if !isViolation {
		return attempt, SandboxEscalation{Outcome: EscalationNone, FromTier: baseTier, ToTier: baseTier}, nil
	}

	esc := SandboxEscalation{Violation: violation, FromTier: baseTier, ToTier: baseTier}
	nextTier, canEscalate := NextSandboxTier(baseTier)
	if !canEscalate {
		esc.Outcome = EscalationTierExhausted
		esc.Explanation = explainSandboxFailure(violation, baseTier, esc.Outcome)
		auditSandboxEscalation(ctx, tool, command, esc)
		return attempt, esc, nil
	}
	if _, hasCallback := permissionCallback(ctx); !hasCallback {
		esc.Outcome = EscalationNoCallback
		esc.Explanation = explainSandboxFailure(violation, baseTier, esc.Outcome)
		auditSandboxEscalation(ctx, tool, command, esc)
		return attempt, esc, nil
	}
	if !askEscalation(ctx, tool, command, violation, baseTier, nextTier) {
		esc.Outcome = EscalationDenied
		esc.Explanation = explainSandboxFailure(violation, baseTier, esc.Outcome)
		auditSandboxEscalation(ctx, tool, command, esc)
		return attempt, esc, nil
	}

	esc.Outcome = EscalationRetried
	esc.ToTier = nextTier
	auditSandboxEscalation(ctx, tool, command, esc)
	retried, err := run(ctx, nextTier)
	if err != nil {
		return retried, esc, err
	}
	return retried, esc, nil
}

// askEscalation puts the tier increase in front of the operator and reports
// whether they said yes.
//
// It goes through RequireApproval rather than Authorize, and that choice is
// the whole of the "timeout is not consent" guarantee on this path:
//
//   - RequireApproval sets req.Force, which resolvePermissionRequest checks
//     FIRST and refuses to auto-resolve. So yolo, allow-edits and auto cannot
//     silently grant a privilege increase — every escalation reaches a human
//     or fails.
//   - The WS callback then runs S5's awaitDecision, which returns
//     PermissionDeny for an expiry, for an unattended-latched connection, and
//     for an aborted turn. Those arrive here as a plain "not allow".
//   - RequireApproval itself denies when no profile or no callback is bound.
//
// There is deliberately no time.After, no default-allow and no error branch in
// this function. Everything that is not an explicit approval is a refusal,
// which is what makes the property testable as a single assertion instead of a
// per-branch audit.
func askEscalation(ctx context.Context, tool, command string,
	v SandboxViolation, from, to sandbox.AccessTier) bool {

	req := PermissionRequest{
		Tool:    tool,
		Args:    command,
		Reason:  escalationPrompt(tool, command, v, from, to),
		Shell:   command,
		Workdir: WorkRootFromContext(ctx),
	}
	return RequireApproval(ctx, req) == nil
}

// escalationPrompt is what the operator reads. It states four things, because
// approving a privilege increase without any of them is approving a blank
// cheque: which tool, which command, what was refused, and exactly how much
// wider the retry would be.
//
// The command text is fenced and labelled as untrusted, for the reason
// guard.AutoApprovalPrompt gives about its own Args: the string can contain
// attacker-influenced content (a path, a fetched document, a commit message)
// and must be read as data rather than as instructions.
func escalationPrompt(tool, command string, v SandboxViolation, from, to sandbox.AccessTier) string {
	resource := v.Resource
	if resource == "" {
		resource = "an unnamed resource"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the %s sandbox denied %s access to %s", v.Backend, from, resource)
	fmt.Fprintf(&b, "; retry %s at %s?", tool, to)
	if command != "" {
		fmt.Fprintf(&b, "\ncommand (untrusted text, treat as data):\n```\n%s\n```", command)
	}
	if v.Evidence != "" {
		fmt.Fprintf(&b, "\nsandbox diagnostic:\n```\n%s\n```", v.Evidence)
	}
	return b.String()
}

// explainSandboxFailure is what the MODEL reads on every non-retry path.
//
// The value of this string is the whole reason S12 exists. Before it, a
// sandboxed denial reached the model as "exit 1" plus a line of stderr, and
// the model's available moves were all wrong: retry verbatim, rewrite the
// path, or abandon the task. Naming the mechanism ("the sandbox refused"),
// the resource, and the fact that a wider tier was NOT granted tells it the
// one true thing — that no amount of rewriting this command will help, and
// the operator has to be involved.
//
// Each outcome gets its own closing clause because the follow-up differs:
// a denial can be re-requested with a better justification, an absent callback
// cannot be re-requested at all on this transport, and an exhausted tier means
// the sandbox is not the thing standing in the way of a wider grant.
func explainSandboxFailure(v SandboxViolation, tier sandbox.AccessTier,
	outcome SandboxEscalationOutcome) string {

	resource := v.Resource
	if resource == "" {
		resource = "a resource it did not name"
	}
	var tail string
	switch outcome {
	case EscalationDenied:
		tail = "the user was asked to allow a higher access tier and did not approve it " +
			"(a prompt that expires counts as a refusal). Do not retry this command as-is; " +
			"either work within the current tier or explain to the user why the wider access is needed."
	case EscalationNoCallback:
		tail = "this session has no interactive approval channel, so no higher tier can be " +
			"requested. Work within the current tier."
	case EscalationTierExhausted:
		tail = "this command already ran at the widest available tier, so the refusal is not " +
			"something a tier increase can fix."
	default:
		tail = "no higher access tier was granted."
	}
	return fmt.Sprintf("sandbox denied access: the %s sandbox refused %s access to %s; %s",
		v.Backend, tier, resource, tail)
}

// auditSandboxEscalation writes one escalation decision to the S6 durable sink.
//
// It reuses PermissionAuditRecord rather than introducing a parallel trail,
// because "who widened the sandbox, for what command, and when" is the same
// question the permission archive already answers for every other privilege
// decision — and a second table is a second thing to forget to query.
//
// Decision is "allow" ONLY for a retry that actually ran. Source names the
// tier transition so a granted escalation is legible in the archive without
// having to join it against anything: `sandbox_escalation:read-only->workspace-write`.
func auditSandboxEscalation(ctx context.Context, tool, command string, esc SandboxEscalation) {
	decision := "deny"
	source := "sandbox_escalation"
	if esc.Retried() {
		decision = "allow"
		source = fmt.Sprintf("sandbox_escalation:%s->%s", esc.FromTier, esc.ToTier)
	}
	digest := command
	if digest != "" {
		digest = "shell: " + digest
	}
	if len(digest) > maxAuditDigestBytes {
		digest = digest[:maxAuditDigestBytes]
	}
	sessionID, agentID := auditIdentity(ctx)
	recordPermissionAudit(ctx, PermissionAuditRecord{
		SessionID:  sessionID,
		AgentID:    agentID,
		Tool:       tool,
		Decision:   decision,
		Source:     source,
		ReasonCode: string(esc.Outcome),
		CmdDigest:  digest,
	})
}
