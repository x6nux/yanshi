//go:build windows

package shell

import (
	"context"
	"os/exec"
	"syscall"
	"testing"

	"github.com/x6nux/yanshi/internal/sandbox"
)

// This file tests the carrier that gets a Windows restricted token from
// sandbox.Prepare to the spawn.
//
// It is small and it exists because the gap it closes was real and silent.
// childLaunchPosture.prepare hands the backend a STAND-IN exec.Cmd and copies
// four fields back — Program, Dir, Env and Args. A restricted token is none of
// those: it arrives on SysProcAttr, which was not copied. So a backend that set
// it was writing into a value that was discarded, every child ran under the
// unrestricted token, and Report() went on claiming os-isolated. Nothing
// errored and nothing looked wrong.
//
// sandbox/poststart.go documents exactly this shape for CreationFlags, which is
// still open. That is why these assertions are about the PLUMBING and use a
// fake token value: they need no Windows privilege, no real token, and no
// particular host state, so they give a definite verdict on the windows CI leg
// rather than skipping the way a test needing a real sandbox might.

// fakeTokenValue is a non-zero handle-shaped number.
//
// It is never handed to the kernel — every assertion here stops at the struct
// field — so it does not have to be a real token. Using a real one would make
// this test depend on CreateRestrictedToken working, which is the OTHER half's
// job and would turn a plumbing regression into a skip on any host where token
// creation failed.
const fakeTokenValue = uintptr(0xBEEF0001)

// tokenSandbox is a sandbox.Sandbox whose Prepare does the one thing the
// Windows backend does: put a token on the command.
//
// Deliberately not spySandbox from procfactory_test.go: that one asserts the
// argv and env mutations survive, and adding a token to it would make one fake
// carry two unrelated contracts. This one exists so that a failure names the
// carrier rather than "the spy test broke".
type tokenSandbox struct{ token uintptr }

func (s *tokenSandbox) Prepare(_ context.Context, cmd *exec.Cmd, _ sandbox.CommandSpec) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Token = syscall.Token(s.token)
	return nil
}

func (s *tokenSandbox) Report() sandbox.CapabilityReport { return sandbox.CapabilityReport{} }
func (s *tokenSandbox) Close() error                     { return nil }

// TestPrepareCarriesTheSandboxTokenIntoTheSpec is the assertion the gap needed.
//
// Deleting the ProcessToken copy-back from childLaunchPosture.prepare leaves
// every other test in this package green — the four fields it does copy are
// unaffected — and ships unconfined children. This is the only thing that goes
// red.
func TestPrepareCarriesTheSandboxTokenIntoTheSpec(t *testing.T) {
	p := childLaunchPosture{Sandbox: &tokenSandbox{token: fakeTokenValue}}
	spec, err := p.prepare(context.Background(),
		LaunchSpec{Program: "cmd.exe", Args: []string{"/c", "rem"}}, sandbox.WorkspaceWrite)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if spec.ProcessToken != fakeTokenValue {
		t.Fatalf("the sandbox's token did not survive the copy-back: got %#x, want %#x.\n"+
			"Every child would run under this process's unrestricted token while the "+
			"capability report claimed os-isolated.", spec.ProcessToken, fakeTokenValue)
	}
}

// TestPrepareLeavesTheTokenZeroWithoutASandbox pins the other direction.
//
// A non-zero token where none was asked for would be handed to
// CreateProcessAsUser, which needs privileges yanshi does not hold — so the
// symptom of getting this wrong is not a weaker sandbox but every spawn
// failing.
func TestPrepareLeavesTheTokenZeroWithoutASandbox(t *testing.T) {
	p := childLaunchPosture{}
	spec, err := p.prepare(context.Background(), LaunchSpec{Program: "cmd.exe"}, sandbox.ReadOnly)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if spec.ProcessToken != 0 {
		t.Fatalf("a launch with no sandbox carried token %#x", spec.ProcessToken)
	}
}

// TestApplySandboxTokenMergesIntoExistingSysProcAttr keeps the token from
// discarding what the spawn path already configured.
//
// OSProcessFactory.Start calls setProcessGroup before applySandboxToken, and a
// caller may have set CreationFlags. A wholesale assignment would drop them and
// the symptom — a console window, a cancellation that no longer kills the tree
// — is one nobody traces back to a sandbox change.
func TestApplySandboxTokenMergesIntoExistingSysProcAttr(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "rem")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	applySandboxToken(cmd, fakeTokenValue)
	if cmd.SysProcAttr.CreationFlags != syscall.CREATE_NEW_PROCESS_GROUP {
		t.Error("applySandboxToken replaced SysProcAttr and lost the caller's CreationFlags")
	}
	if cmd.SysProcAttr.Token != syscall.Token(fakeTokenValue) {
		t.Errorf("the token was not applied: %#x", uintptr(cmd.SysProcAttr.Token))
	}

	// A zero token must not even allocate a SysProcAttr: OSProcessFactory calls
	// this unconditionally, and materialising an empty one on every unsandboxed
	// spawn would change the CreateProcess arguments for a case that asked for
	// nothing.
	plain := exec.Command("cmd.exe", "/c", "rem")
	applySandboxToken(plain, 0)
	if plain.SysProcAttr != nil {
		t.Error("a zero token allocated a SysProcAttr on an unsandboxed command")
	}

	// Round-trip through the reader the copy-back uses, so the two halves of
	// the carrier are pinned against each other rather than each against a
	// literal.
	if got := sandboxTokenFromCmd(cmd); got != fakeTokenValue {
		t.Errorf("sandboxTokenFromCmd read back %#x, want %#x", got, fakeTokenValue)
	}
	if got := sandboxTokenFromCmd(plain); got != 0 {
		t.Errorf("sandboxTokenFromCmd invented a token for a plain command: %#x", got)
	}
}
