# Review Scope: m3-de2-readout

- Mode: implementation
- Stage: m3
- Target: M3 round 3: the fixes in 6b35ee2, and the M3 range 853305b..HEAD

## Summary

Round 3 (the configured maximum) of the M3 review of the Go port of the Python `shisa-de` client. The Python repository at /home/morpheus/data/dev/shisa/shisa-de is the reference implementation and the spec; `docs/READOUT-DE2.md` is copied from it verbatim and is the contract. This port implements DE-2 only.

Round 1 (docs/reviews/m3-de2-readout/m3/round-1) found one HIGH, six MEDIUM and several LOW; all were addressed in `29049db`. Round 2 (round-2) found one HIGH, one MEDIUM and several LOW; all were addressed in `6b35ee2`, which is the primary target of this round. The full M3 range is `853305b..HEAD`.

What changed in `6b35ee2`:

1. **`CheckCodeBoundary` self-deadlock (was HIGH, a regression from round 1).** The exported wrapper took `r.mu` and then called `checkCodeBoundaryFor`, which took `r.mu` again, so every call hung while holding the lock and wedged every other read. The wrapper now uses `tokenizerSnapshot()` (which locks and releases) and then calls the helper, exactly as `readCodes` does. `TestCheckCodeBoundaryIsCallable` calls it, and the reintroduced deadlock makes that test time out.
2. **The approval cache keyed by code alone (was MEDIUM).** `boundaryChecked` is now `map[string]int` (code -> the approved token id) and the check skips a code only when the cached id equals `slot.TokenID`. `TestCheckCodeBoundaryRefusesAForeignTokenID` pins a caller-built `Slot{Code: approvedCode, TokenID: 999}`.
3. **In-flight read poisoning the cache (was MEDIUM, found independently by glm).** `Readout` gained a `tokenizerGeneration` counter, incremented by `SetTokenizer`. `checkCodeBoundaryFor` takes the generation the read captured and only reads or writes the approval cache while `r.tokenizerGeneration` still matches; a read that lost the race re-checks without caching. `TestSetTokenizerForgetsBoundaryApprovals` pins the approval half of the invalidation.
4. **`ReadoutHTTPError` (was LOW).** `Error()` and `Unwrap()` are spelled out: a nil embed reports a status-only message instead of panicking, and `Unwrap` returns an untyped nil so `errors.As` cannot hand back a nil `*ReadoutError`. `TestReadoutHTTPErrorWithoutAnEmbed` pins both.
5. **Zero-value `Readout` (was LOW).** `httpClient()` returns a `ReadoutError` naming `NewReadout` instead of letting `net/http` panic on a nil client; the type documents `NewReadout` as the only constructor. `TestZeroReadoutReportsItself` pins it.
6. **Gate coverage (was LOW).** Each recorded exchange now carries the two headers the client sets (`Content-Type`, `Authorization` when configured) and the replay server compares them; the fixture's `think_system` is asserted against `ThinkSystem`; `TestReadoutResponseCap` asserts the cap exactly (64 MB) instead of a 16 MB lower bound; `closeEnough` has a 14-case table test; `Readout.Slot`, `Readout.Render`, `Readout.AnswerPrefixID` and `Readout.CheckCodeBoundary` have direct tests.
7. **Docs (was LOW).** The `token_ids` bullet moved out of the "stricter" list, because both clients refuse the same shapes; the `Softmax` sum order was added to the deliberate-differences list; the field-level decoding paragraph now names the `top_logprobs` and `prompt_logprobs` map values.

Validation already run: `gofmt -l .` clean, `go build ./...`, `go vet ./...`, `go test ./...` and `go test -race ./...` all pass. The deadlock test was checked against the reintroduced deadlock, and the header comparison against a mutated `Content-Type`.

## Primary files / regions

- `readout.go` — reason: the deadlock fix, the approval cache and tokenizer generation, httpClient, the exported entry points
- `endpoint.go` — reason: ReadoutHTTPError's Error and Unwrap, TokenIDList, the response cap
- `readout_test.go` — reason: the new direct tests and the changed replay server
- `scripts/gen_readout_parity.py` — reason: the header recording and the late-downgrade scenario
- `docs/PORT.md` — reason: the claimed differences must be true and complete

## Review questions

- Round 2 found that the exported `CheckCodeBoundary` self-deadlocked because a wrapper and its helper both took the mutex. Sweep every exported method on `Readout` for that class of bug: any locking wrapper calling a locking helper, any helper called both with and without the lock held, and any exported method with no test. `TestCheckCodeBoundaryIsCallable` now covers one; is the class closed?
- The boundary-approval cache is now `map[string]int` (code -> approved token id) and is only trusted and written while the readout still holds the tokenizer generation the read captured. Trace `SetTokenizer` interleavings against `CodeSlots`, `ReadCodes`, `ReadCodesAfterThought`, `Think` and `CheckCodeBoundary`: can any read still send a token id the current tokenizer never produced, skip a check it should run, or leave an approval a later read trusts?
- `ReadoutHTTPError` now has an explicit `Error()` and a nil-safe `Unwrap()`. Does `errors.As` behave correctly for every construction a caller can build outside the package (`ReadoutHTTPError{}`, `&ReadoutHTTPError{Status: 503}`, one with the embed set), and does the `logprob_token_ids` sniff in `readCodes` still work? Does a zero-value `Readout` now report every unusable entry point rather than panicking?
- Round 1 and round 2 fixes both changed behaviour that the replay gate covers. Does the gate still compare everything it claims to (method, path, headers, bodies, every result and thought field, error class, request counts in both directions), and is any comparison still unable to fail? Is the `codes-late-downgrade` scenario still discriminating?
- docs/PORT.md claims a specific list of deliberate differences and of places this port is stricter. Round 2 found two inaccurate claims; are the remaining claims true and complete, including the `Softmax` sum-order paragraph and the field-level decoding paragraph?
- Is anything in `readout.go` still missing a test that would catch a regression, in the way `CheckCodeBoundary` was? Name the exported behaviour with the weakest coverage.

## Known non-goals

- DE-1 is not implemented and must not be: a model that resolves to DE-1 is an error.
- `policy.go`, `calibration.go`, `client.go`, `cmd/shisa-de/` and the live test suite do not exist yet (M4/M5/M6).
- No live requests were made and no number here is a measurement against a served model.
- `testdata/readout_parity.json` is a generated fixture; review the generator and the test, not the fixture bytes.
- The M1/M2 packages (`pyjson.go`, `questions.go`, `family.go`, `codebook.go`) are out of scope; `tokenizer/` is in scope only for `hub.go`'s local-only resolution policy.

## Validation already run

- gofmt -l . (clean)
- go build ./... (clean)
- go vet ./... (clean)
- go test ./... -count=1 (shisade 2.5s, shisade/tokenizer 7.9s, both ok)
- go test -race ./... -count=1 (shisade 14.5s, shisade/tokenizer 43.6s, both ok)
- python3 scripts/gen_readout_parity.py (regenerates 21 scenarios, 61 exchanges)
