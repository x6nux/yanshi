package cli

// ApprovalPolicy is how a headless run answers the permission requests that
// reach it. It is a POLICY, not a mode: -mode decides what the SERVER resolves
// by itself (yolo/auto), while this decides what the CLIENT does with the
// questions the server deliberately left for a human.
//
// The distinction matters because the two knobs are read at different layers
// and an operator tuning one does not expect the other to move.
type ApprovalPolicy string

const (
	// ApprovalNever denies every request that reaches the client. This is the
	// default and the fail-closed answer: everything arriving here is something
	// a human was supposed to see (see headlessPermissionDecision).
	ApprovalNever ApprovalPolicy = "never"
	// ApprovalRequired one-shot allows tools that must ask before an
	// IRREVERSIBLE EXTERNAL effect (ApprovalRequired: a GitHub write, say) and
	// still denies force-prompt tools. It exists because those tools are
	// otherwise unreachable from every unattended surface — automation_*,
	// github_*, screenshot, acp_delegate — which makes a scripted workflow
	// impossible rather than merely gated.
	ApprovalRequired ApprovalPolicy = "required"
	// ApprovalAll one-shot allows anything that reaches the client, including
	// force-prompt tools such as task_cancel. It is the "I am watching the log"
	// setting.
	ApprovalAll ApprovalPolicy = "all"
)

// ParseApprovalPolicy maps a flag value onto a policy, rejecting typos. An
// unknown value must not silently behave like "never" or like "all": one of
// those two mistakes is a workflow that mysteriously stops and the other is an
// unattended agent approving things nobody intended.
func ParseApprovalPolicy(s string) (ApprovalPolicy, bool) {
	switch ApprovalPolicy(s) {
	case ApprovalNever, ApprovalRequired, ApprovalAll:
		return ApprovalPolicy(s), true
	}
	return "", false
}

// HeadlessPermissionDecision answers a permission_request when there is nobody
// to ask.
//
// The default answer is "deny", and that is the design rather than a
// placeholder. A permission_request reaching the client means the server has
// already declined to resolve it by itself, and every path that gets here is a
// question written for a human:
//
//   - ForcePrompt tools (task_cancel and friends) exist precisely so that a
//     prior approval cannot answer them.
//   - ApprovalRequired tools are the same rule for irreversible external
//     effects.
//   - OutOfScope deletions and unreadable payloads (Opaque) are the two tiers
//     the WS resolver deliberately refuses to auto-resolve even under yolo.
//   - Profile-policy denials that reach the client at all are ones no mode
//     claimed — under yolo and auto those are resolved server-side before a
//     frame is ever emitted.
//
// So a headless run is not "yolo but silent": -mode changes what the SERVER
// resolves, -approve changes what the CLIENT is willing to answer, and the
// floor underneath both is that nothing here is ever approved by accident.
//
// A policy grants ONE-SHOT allows only. It never sends "always_allow": an
// operator who asked for an unattended run did not ask for a permanent rule to
// be written into the approval store on the first call that needed one, and
// that rule would outlive the run.
func HeadlessPermissionDecision(ev StreamEvent, policy ApprovalPolicy) string {
	switch policy {
	case ApprovalAll:
		return "allow"
	case ApprovalRequired:
		if ev.ApprovalRequired && !ev.ForcePrompt {
			return "allow"
		}
	}
	return "deny"
}
