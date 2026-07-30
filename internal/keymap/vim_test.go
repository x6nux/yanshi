package keymap

import (
	"testing"
)

// TestVim_TriStateDistinguishesUnsetAndFalse (structural fix #9): *bool so
// we can tell "user did not configure" (nil) from "user explicitly disabled"
// (false). This affects whether the TUI falls back to prefs.json.
func TestVim_TriStateDistinguishesUnsetAndFalse(t *testing.T) {
	var vim *bool
	mode := effectiveVimMode(vim, true /*prefsDefault*/)
	if mode != true {
		t.Fatal("nil Vim + prefsDefault=true must yield true")
	}
	f := false
	mode = effectiveVimMode(&f, true)
	if mode != false {
		t.Fatal("Vim=false must override prefsDefault=true")
	}
	tt := true
	mode = effectiveVimMode(&tt, false)
	if mode != true {
		t.Fatal("Vim=true must override prefsDefault=false")
	}
}

func TestVim_ModalResultSeparatesActionFromConsumption(t *testing.T) {
	v := NewVimMachine()

	// Insert-mode printable input is not consumed by Vim; textarea receives it.
	got := v.HandleKey("j", ActionNone)
	if got.Action != ActionNone || got.Consumed {
		t.Fatalf("insert j = %#v, want literal passthrough", got)
	}

	// Escape and i/a/o are transitions: no semantic action, but the original
	// key is consumed so it is never inserted into the textarea.
	got = v.HandleKey("esc", ActionNone)
	if v.Mode() != VimModeNormal || got.Action != ActionNone || !got.Consumed {
		t.Fatalf("escape = %#v mode=%v", got, v.Mode())
	}
	for _, key := range []string{"i", "a", "o"} {
		v.SetMode(VimModeNormal)
		got = v.HandleKey(key, ActionNone)
		if v.Mode() != VimModeInsert || got.Action != ActionNone || !got.Consumed {
			t.Fatalf("%s transition = %#v mode=%v", key, got, v.Mode())
		}
	}

	v.SetMode(VimModeNormal)
	if got = v.HandleKey("j", ActionNone); got.Action != ActionScrollDown || !got.Consumed {
		t.Fatalf("normal j = %#v", got)
	}
	if got = v.HandleKey("k", ActionNone); got.Action != ActionScrollUp || !got.Consumed {
		t.Fatalf("normal k = %#v", got)
	}
}

func TestVim_VisualModeTransitionsOnly(t *testing.T) {
	v := NewVimMachine()
	v.SetMode(VimModeNormal)
	got := v.HandleKey("v", ActionNone)
	if v.Mode() != VimModeVisual || !got.Consumed {
		t.Fatalf("v = %#v mode=%v", got, v.Mode())
	}
	// D3 does not implement text-selection extension. j/k retain viewport
	// navigation, and unknown visual keys are consumed rather than typed.
	if got = v.HandleKey("j", ActionNone); got.Action != ActionScrollDown || !got.Consumed {
		t.Fatalf("visual j = %#v", got)
	}
	if got = v.HandleKey("x", ActionNone); got.Action != ActionNone || !got.Consumed {
		t.Fatalf("visual x = %#v", got)
	}
}
