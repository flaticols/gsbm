# fix failing TestForgetPresence_RemovesEntry

## Overview

Investigate and fix the failing test reported in [issue #12](https://github.com/flaticols/gsbm/issues/12):

```
--- FAIL: TestForgetPresence_RemovesEntry (0.00s)
    presence_track_test.go:215: setup: tag 1 should be present
FAIL    go.flaticols.dev/gsbm/storage/gsbm    4.465s
```

The failure happens in setup, before the test's actual assertion: a tag the test just marked with `MarkPresent` is not visible to `IsPresent`. Either the runtime contract for `MarkPresent`/`IsPresent` regressed (production bug) or the test mis-set the expectation (test bug). Root-cause first, then fix the side that's actually wrong.

## Context

- File: `storage/gsbm/presence_track_test.go:215` (line referenced in failure).
- Implementation: `storage/gsbm/presence_track.go` — package-level `sync.Map`-backed sidecar keyed by receiver pointer (`unsafe.Pointer`). API: `MarkPresent(receiver any, tag uint32)`, `ClearPresence(receiver any)`, `IsPresent(receiver any, tag uint32) bool`, `ForgetPresence(receiver any)`.
- Caller invariant: `ForgetPresence` evicts the entry from the sidecar; after eviction `IsPresent(v, tag)` returns false for every tag.
- The test name suggests it does `MarkPresent(v, 1) → assert IsPresent → ForgetPresence(v) → assert !IsPresent`. The failure is on the **first** assertion, so the bug is upstream of `ForgetPresence`.
- Tracked plan-side rule: do NOT fix the test to make it green; the failing assertion is the canary.

## Development Approach

- **Testing approach**: TDD — the failing test already exists; reproduce locally first, then narrow the cause before changing anything.
- One small commit per investigation step is fine; keep diffs reviewable.
- Update this plan when scope changes during implementation.

### CRITICAL: do not commit the plan file to the branch

This plan lives at `docs/plans/20260511-issue-12-fix-forget-presence-test.md` as a local scratchpad. `docs/` contains only `spec.md` going forward. Stage files individually (`git add <path>`), never `git add -A`. If a plan accidentally lands in a commit, `git rm --cached docs/plans/**` and amend before pushing.

### CRITICAL: do not fix code or tests to make tests green

When a test fails, investigate the root cause. Production wrong → fix production; test wrong → cite the spec / runtime contract, then fix the test. Forbidden: loosening assertions, swapping `errors.Is` for `err != nil`, swallowing errors, `t.Skip`, regenerating goldens to dodge hand-written tests, editing sentinels to make a stuck test pass. If you can't determine the root cause within reasonable effort, surface a `⚠️` blocker.

## Testing Strategy

- Reproduce the failure locally before touching code (`go test ./storage/gsbm/ -run TestForgetPresence_RemovesEntry -count=1 -v`).
- Add diagnostic helpers (temporary `t.Logf` of the sidecar key, the bitmap, the `tag>>6` slot) if needed; remove before committing.
- Once root cause is identified, write a smaller minimal test that reproduces the same failure shape — this becomes a regression guard alongside the existing test.
- All `presence_track_test.go` tests must pass; if other tests in the package start failing under the fix, those are related symptoms and must be investigated, not silenced.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document blockers with ⚠️ prefix

## Technical Details

Likely failure modes to rule out in order of suspicion:

1. **Receiver-identity drift**: `MarkPresent` and `IsPresent` key the sidecar by `unsafe.Pointer` derived from `reflect.ValueOf(receiver).Pointer()` or similar. If the test passes a value (not a pointer), or boxes the same address differently in the two calls, the lookup misses. Check at `presence_track.go` how the key is derived.
2. **Slot computation off-by-one**: tag bit position is `(tag - 1) / 64` for slot, `(tag - 1) % 64` for bit, with tag 1 landing at slot 0 bit 0. If the code uses `tag / 64` and `tag % 64` instead, tag 1 lands at slot 0 bit 1, but tag 0 isn't valid so this only bites at boundaries.
3. **`MaxTrackedTag` cap mis-application**: if the cap is enforced as `tag > MaxTrackedTag` returning early without marking, tag 1 should always be inside; if the cap is mis-spelled as `tag >= 0` it always returns early.
4. **Pre-existing sidecar leak from other tests**: if a prior test in the package leaks a sidecar entry keyed at the same address (pooled receiver address reuse), `MarkPresent` may see a stale mask that was supposed to be cleared. Look at test-order dependencies.

For each candidate, the test in question pins the contract — the production code is the side that has to match unless the test's setup is logically inconsistent (e.g., calling `MarkPresent(v, 0)` and expecting tag 0 to be present, which would violate the tag-0-reserved rule).

## Implementation Steps

### Task 1: Reproduce and isolate

- [ ] run `go test ./storage/gsbm/ -run TestForgetPresence_RemovesEntry -count=1 -v` locally; confirm the failure shape matches the issue
- [ ] read `storage/gsbm/presence_track_test.go:215` and the surrounding test fully; record the exact setup (receiver type, address, tag value, prior `Clear/Forget` calls) in this Task's notes
- [ ] read `storage/gsbm/presence_track.go` `MarkPresent`/`IsPresent`/`ForgetPresence` implementations; trace what a `MarkPresent(testReceiver, 1)` followed by `IsPresent(testReceiver, 1)` should do byte-for-byte
- [ ] add temporary debug logging in the test (under a local `t.Logf` block, NOT a `t.Helper`-flagged production helper) to print the sidecar key, the mask, the slot, and the bit at the failure point
- [ ] run the failing test once more with debug output; commit the debug output verbatim into this Task's notes
- [ ] do NOT add tests yet — investigation in progress
- [ ] do NOT run the project test suite yet; the failure is the focus

### Task 2: Identify root cause

- [ ] from Task 1 debug output, classify the failure into one of: receiver-key mismatch / slot or bit miscomputation / cap mis-application / cross-test sidecar leak / something else
- [ ] write a one-paragraph root-cause summary in this Task's notes naming the offending file:line and the exact misbehavior (e.g., "MarkPresent stores at slot 0 bit 1; IsPresent reads slot 0 bit 0")
- [ ] before fixing, confirm the test's assertion is correct against the documented `presence_track.go` contract — if the test is wrong (e.g., expects behavior the contract forbids), the fix is on the test side with a cited rationale; if the production code is wrong, the fix is on the production side and the test stays
- [ ] write tests covering the minimal reproduction: a new test that does exactly `MarkPresent(v, 1)` then `IsPresent(v, 1)` and asserts true; this test must FAIL right now and PASS after the fix in Task 3
- [ ] run project tests - the new minimal-repro test failing is expected; everything else must pass before next task

### Task 3: Apply the fix and remove debug

- [ ] apply the root-cause fix in `storage/gsbm/presence_track.go` (or `presence_track_test.go` if the test's expectation was wrong); cite the specific line and the contract reference in the commit message
- [ ] remove the temporary debug logging added in Task 1
- [ ] re-run `go test ./storage/gsbm/ -run TestForgetPresence_RemovesEntry -count=1 -v`; must pass
- [ ] re-run the minimal-repro test added in Task 2; must pass
- [ ] write a regression-guard table-driven test if the root cause was a boundary issue (tags near 1, 64, 128, `MaxTrackedTag` — bit-position boundaries)
- [ ] run project tests - full suite must pass before next task

### Task 4: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented: failing test passes; root cause documented in commit; regression test added
- [ ] run full project test suite: `go test ./... -count=1`
- [ ] run `go vet ./...` and `go build ./...`; clean
- [ ] confirm no other test in `storage/gsbm/` regressed under the fix
- [ ] close out: comment on issue #12 with a pointer to the merge commit

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- If the root cause was a cross-test sidecar leak, consider adding a `t.Cleanup` helper that calls `ForgetPresence` on every receiver mentioned in test setup. Track as a follow-up if it touches more files than this plan should.
- If the bug was in the production bit-position math, an audit of the codegen's emitted `MarkPresent` calls is worth scheduling — the codegen produces those calls at codegen time and they pin the same arithmetic.
