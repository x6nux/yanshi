// sdk/ts/src/validators.ts
//
// Pattern helpers that the transport layer uses to fail closed on malformed
// v1 envelopes (version / item id / item sequence). D1's `sdk/ts/v1.ts` ships
// only data types; these helpers are the SDK-owned complement so the
// generated-types regeneration cycle does not need to add a runtime.
//
// The version pattern accepts "v1" today and any future "v1.<minor>" minor
// for additive forward compatibility. Major bumps ("v2") fail closed at the
// transport layer.

// The version pattern accepts "v1" today and any future "v1.<minor>" minor
// for additive forward compatibility. Other majors ("v2") and malformed
// values ("v1.", "garbage") fail closed at the transport layer. The pattern
// is anchored on the literal "v1" prefix on purpose — it is not a generic
// semver matcher.
export const VERSION_PATTERN = /^v1(?:\.[0-9]+)?$/;
export const ITEM_ID_PATTERN = /^item-[1-9][0-9]*$/;

export function isValidVersion(version: string | undefined): boolean {
  return typeof version === "string" && VERSION_PATTERN.test(version);
}

export function isValidItemId(id: string | undefined): boolean {
  return typeof id === "string" && ITEM_ID_PATTERN.test(id);
}

// isValidEventId is the legacy name kept for SDK symmetry with the Python port
// and for tests that imported it from earlier D2 drafts. Today D1 identifies
// items by `sequence` (a monotonic int) and uses `id` strings shaped like
// "item-<n>"; both checks ultimately guard the same boundary.
export const isValidEventId = isValidItemId;
