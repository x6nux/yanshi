---
name: dev-quick-fix
description: Use when making a single-file, obvious change such as a typo, a config tweak, a rename, or a small bug fix with no design needed
---

# Quick Fix (T0)

Use when the change is single-file and obvious.

## Workflow
1. Locate: call `fs_search` with a regexp for the relevant symbol/text.
2. Confirm: call `fs_read` on the exact file and line.
3. Edit: call `fs_edit` with a unique `old_string`.
4. Verify: call `shell_run` with the project's test/build command (one command per call — metacharacters like && or | are rejected).
5. Commit if tests pass.

## Escalate
If the fix touches more than one file, needs a new test, or the scope is unclear, stop and re-tier to T1 (`dev-standard-feature`).
