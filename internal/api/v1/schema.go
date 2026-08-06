package v1

import (
	sdkschema "github.com/x6nux/yanshi/sdk/schema"
)

// SchemaBytes returns the v1 Agent API JSON Schema — the same document the
// TypeScript and Python clients validate against, byte for byte, because both
// read the one file sdk/schema embeds.
//
// It used to return a Go map literal maintained here by hand, with 3 $defs and
// its own $id, while the SDK's document had 21 and a different identity. The
// literal's own comment promised a "Task 9" that would expand it; that never
// happened, so the only schema the product serves described a contract nobody
// enforced. Serving the SDK's document removes the synchronisation step rather
// than automating it.
//
// The bytes are a fresh copy on every call so a caller cannot mutate the
// embedded document and silently change what HTTP and JSON-RPC report.
func SchemaBytes() []byte { return sdkschema.V1() }
