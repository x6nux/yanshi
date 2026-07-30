// sdk/ts/src/extensions.ts
//
// SDK-owned wire extensions that D1 does not yet emit. These are provisional
// (D2-only) and live in the SDK package so they round-trip cleanly through
// the JSON serializer without depending on D1 regenerating v1.ts. When D1
// adds them canonically, delete this file and import from generated.js.
//
//   - ContextItem (selection/openFile) : IDE -> server; carries the user's
//     current selection and bounded open files. D1 ignores unknown fields
//     today, so the wire stays safe.
//   - FileChange                       : server -> IDE; carries the diff
//     payload. D1 does not yet emit fileChange items; the IDE diff renderer
//     degrades gracefully when the field is absent.

export interface Range {
  start: number;
  end: number;
}

export interface SelectionContext {
  kind: "selection";
  path: string;
  content: string;
  range: Range;
  truncated?: boolean;
}

export interface OpenFileContext {
  kind: "openFile";
  path: string;
  content: string;
  truncated?: boolean;
}

export type ContextItem = SelectionContext | OpenFileContext;

export interface FileChange {
  path: string;
  before?: string;
  after?: string;
  unifiedDiff?: string;
}
