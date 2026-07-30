//go:build windows

package tea

import (
	"testing"

	"github.com/erikgeiser/coninput"
)

// TestKeyType_ReturnDistinguishesCtrl verifies the fork's keyType() distinguishes
// Ctrl+Enter from Enter on Windows: VK_RETURN with a Ctrl modifier bit set
// returns KeyCtrlEnter; without it returns KeyEnter. Upstream bubbletea v1.3.10
// has no Ctrl field on Key and collapses both presses to KeyEnter.
func TestKeyType_ReturnDistinguishesCtrl(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state coninput.ControlKeyState
		want  KeyType
	}{
		{"plain enter", 0, KeyEnter},
		{"left ctrl", coninput.LEFT_CTRL_PRESSED, KeyCtrlEnter},
		{"right ctrl", coninput.RIGHT_CTRL_PRESSED, KeyCtrlEnter},
		{"both ctrl", coninput.LEFT_CTRL_PRESSED | coninput.RIGHT_CTRL_PRESSED, KeyCtrlEnter},
		{"shift only", coninput.SHIFT_PRESSED, KeyEnter},
		{"shift+ctrl", coninput.SHIFT_PRESSED | coninput.LEFT_CTRL_PRESSED, KeyCtrlEnter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := coninput.KeyEventRecord{
				KeyDown:         true,
				VirtualKeyCode:  coninput.VK_RETURN,
				ControlKeyState: tc.state,
			}
			if got := keyType(e); got != tc.want {
				t.Fatalf("keyType(VK_RETURN, state=%#x) = %v, want %v", uint32(tc.state), got, tc.want)
			}
		})
	}
}
