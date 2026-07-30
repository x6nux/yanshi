//go:build race

package http

// raceDetectorEnabled is true when the binary is built with -race. The WS
// permission tests' orchestrator + ADK tool-execution path runs noticeably
// slower under the race detector (instrumentation on every memory access),
// so readFrame scales its deadline up when this is true to avoid spurious
// i/o-timeout failures that have nothing to do with the code under test.
const raceDetectorEnabled = true
