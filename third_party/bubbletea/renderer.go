package tea

// renderer is the interface for Bubble Tea renderers.
type renderer interface {
	// Start the renderer.
	start()

	// Stop the renderer, but render the final frame in the buffer, if any.
	stop()

	// Stop the renderer without doing any final rendering.
	kill()

	// Write a frame to the renderer. The renderer can write this data to
	// output at its discretion.
	write(string)

	// Request a full re-render. Note that this will not trigger a render
	// immediately. Rather, this method causes the next render to be a full
	// repaint. Because of this, it's safe to call this method multiple times
	// in succession.
	repaint()

	// Clears the terminal.
	clearScreen()

	// Whether or not the alternate screen buffer is enabled.
	altScreen() bool
	// Enable the alternate screen buffer.
	enterAltScreen()
	// Disable the alternate screen buffer.
	exitAltScreen()

	// Show the cursor.
	showCursor()
	// Hide the cursor.
	hideCursor()

	// enableMouseCellMotion enables mouse click, release, wheel and motion
	// events if a mouse button is pressed (i.e., drag events).
	enableMouseCellMotion()

	// disableMouseCellMotion disables Mouse Cell Motion tracking.
	disableMouseCellMotion()

	// enableMouseAllMotion enables mouse click, release, wheel and motion
	// events, regardless of whether a mouse button is pressed. Many modern
	// terminals support this, but not all.
	enableMouseAllMotion()

	// disableMouseAllMotion disables All Motion mouse tracking.
	disableMouseAllMotion()

	// enableMouseSGRMode enables mouse extended mode (SGR).
	enableMouseSGRMode()

	// disableMouseSGRMode disables mouse extended mode (SGR).
	disableMouseSGRMode()

	// enableBracketedPaste enables bracketed paste, where characters
	// inside the input are not interpreted when pasted as a whole.
	enableBracketedPaste()

	// disableBracketedPaste disables bracketed paste.
	disableBracketedPaste()

	// bracketedPasteActive reports whether bracketed paste mode is
	// currently enabled.
	bracketedPasteActive() bool

	// setWindowTitle sets the terminal window title.
	setWindowTitle(string)

	// pushWindowTitle saves the terminal's current window title onto its
	// title stack (XTWINOPS "CSI 22;2t"), so a later popWindowTitle call can
	// restore it. Used by Program.SetWindowTitle (screen.go) so a caller who
	// sets a dynamic title does not have to know or remember what the
	// terminal's title was before the program started — see
	// Program.titlePushed's doc comment (tea.go) for why this only fires
	// once per process lifetime.
	pushWindowTitle()

	// popWindowTitle restores the window title most recently saved by
	// pushWindowTitle (XTWINOPS "CSI 23;2t"). Called from shutdown() — the
	// single choke point reached by every exit path (normal quit, Kill, and
	// panic recovery) — so W-E-05's "restore the original title on exit"
	// requirement holds regardless of how the program stopped.
	popWindowTitle()

	// reportFocus returns whether reporting focus events is enabled.
	reportFocus() bool

	// enableReportFocus reports focus events to the program.
	enableReportFocus()

	// disableReportFocus stops reporting focus events to the program.
	disableReportFocus()

	// resetLinesRendered ensures exec output remains on screen on exit
	resetLinesRendered()
}

// repaintMsg forces a full repaint.
type repaintMsg struct{}
