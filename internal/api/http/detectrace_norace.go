//go:build !race

package http

// raceDetectorEnabled is false for normal (non -race) builds. See
// detectrace_race.go for the rationale.
const raceDetectorEnabled = false
