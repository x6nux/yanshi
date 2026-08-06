// Package schema embeds the versioned JSON Schema documents that define the
// Agent API wire contract.
//
// The JSON files are the single source of truth. They live here, next to the
// TypeScript and Python clients, because those load them from disk at test
// time (Ajv registers v1 and compiles v1.1 against it); Go cannot embed across
// a directory boundary, so the alternative was a second copy under
// internal/api/ kept in step by hand.
//
// That "kept in step by hand" is exactly what went wrong before: three
// documents each called themselves the v1 schema — a Go literal with 3 $defs
// serving GET /api/v1/schema/agent-v1.json, this file with 21, and the v1.1
// overlay — and the endpoint the product actually serves served the poorest of
// the three, under a DIFFERENT $id than the one every SDK client validated
// against. A client that fetched the schema to learn the contract got a
// document that did not describe the contract its own SDK enforced.
//
// This package exists so there is one file and no synchronisation step.
// internal/api/v1.SchemaBytes returns these bytes verbatim.
package schema

import (
	_ "embed"
)

//go:embed v1/agent-api.schema.json
var v1Schema []byte

//go:embed v1.1/agent-api.schema.json
var v11Schema []byte

// V1 returns the v1 Agent API JSON Schema.
//
// The slice is a copy: the embedded bytes back the only schema endpoint the
// product serves, and a caller that mutated them in place would change what
// every subsequent client is told the contract is.
func V1() []byte { return append([]byte(nil), v1Schema...) }

// V11 returns the v1.1 overlay schema, an allOf/$ref layer over V1.
//
// It is not served by the v1 endpoint — it exists so clients have an
// executable sample of what a future minor version looks like, and so the
// version-negotiation tests have a real second document to negotiate over.
func V11() []byte { return append([]byte(nil), v11Schema...) }
