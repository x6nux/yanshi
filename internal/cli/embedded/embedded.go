// Package embedded carries assets that must travel inside the yanshi binary
// rather than being read off the working directory.
//
// It exists because of a defect found by actually running the command rather
// than by reading it: `yanshi init` resolved its template with
// os.ReadFile("config.example.yaml"), so it only worked when the operator
// happened to be standing in the yanshi source tree. Everywhere else — which
// is every real first-run — it failed with `no such file or directory`. A
// bootstrap command that requires the thing it bootstraps to already be
// present is not a bootstrap command.
//
// The file here is a COPY of the repository-root config.example.yaml, which
// stays where it is because docs, CI and the README all point people at it.
// Two copies of one file is a drift hazard, so it is not left to discipline:
// internal/archtest::TestEmbeddedExampleConfigMatchesRoot compares the bytes
// and fails naming both paths. //go:embed cannot reach outside its own package
// directory, so a copy plus a gate is the available shape.
package embedded

import _ "embed"

// ExampleConfig is the contents of config.example.yaml, compiled into the
// binary so `yanshi init` and `yanshi doctor -fix` can produce a config from
// any working directory.
//
// It is a string rather than []byte because every consumer treats it as text
// (env-reference scanning, template writing) and a string cannot be mutated by
// a caller — the embedded asset is process-wide shared state.
//
//go:embed config.example.yaml
var ExampleConfig string
