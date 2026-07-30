// sdk/ts/src/generated.ts
//
// Re-export the canonical D1-generated types so SDK modules all reach through a
// single named boundary. D1 owns `sdk/ts/v1.ts` via `go run ./cmd/api-schema`;
// hand edits to that file are wiped on the next D1 generate. D2 SDK modules
// MUST import from "./generated.js" (or the package root) — never from
// "../v1.js" directly — so a future canonical schema swap changes one path.
export * from "../v1.js";
