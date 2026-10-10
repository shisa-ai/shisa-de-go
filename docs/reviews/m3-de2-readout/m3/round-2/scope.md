# Review Scope: m3-de2-readout

- Mode: implementation
- Stage: m3
- Target: M3 round 2: the fixes in 29049db, and the M3 range 853305b..HEAD

## Summary

Round 2 of the M3 review of the Go port of the Python `shisa-de` client. The Python repository at /home/morpheus/data/dev/shisa/shisa-de is the reference implementation and the spec; `docs/READOUT-DE2.md` is copied from it verbatim and is the contract. This port implements DE-2 only.

Round 1 (docs/reviews/m3-de2-readout/m3/round-1) found one HIGH, six MEDIUM and several LOW findings. All are addressed in commit `29049db` ("fix: close the M3 round-1 review findings"), which is the primary target of this round. The full M3 range is `853305b..HEAD`.

What changed in `29049db`:

1. **DE-2-only guard (was HIGH).** `NewReadout` now calls `RequireDE2(config.Model, config.Tokenizer, config.Family)` before anything is loaded or sent, refuses anything but DE-2 with `*UnsupportedFamilyError`, and stores the resolution; `Readout.ResolvedFamily()` reports the family and whether it was declared, named by the model id, named by the tokenizer source, or assumed. `ReadoutConfig` gained a `Family string` declaration field.
2. **`LocalFilesOnly` (was MEDIUM).** It was stored and never read. `tokenizer/hub.go` gained `ResolveOptions{LocalFilesOnly bool}`, `ResolveWith`, `LoadWith` and `ErrNotCached`; `Resolve` and `Load` are now the same functions with the policy off, so M1 call sites are unchanged. `readout.go` passes the flag through.
3. **Null `token_ids` element (was MEDIUM).** `completionChoice.TokenIDs` is now `*TokenIDList`; `TokenIDList.UnmarshalJSON` requires every element to be a JSON integer, so `[null, 101]` is an error instead of becoming token 0.
4. **Nil `prompt_logprobs` entry (was MEDIUM, a panic).** `letterLogprob` now returns a `ReadoutError` for a null entry at the answer boundary.
5. **`ReadoutHTTPError` taxonomy (was MEDIUM).** It now embeds `*ReadoutError` and has `Unwrap() error { return e.ReadoutError }`, matching Python's `ReadoutHTTPError(ReadoutError)`.
6. **Mid-read downgrade (was LOW).** `readCodes` now uses `block = slots` when the capability is off, matching Python's `slots[start:start+LIMIT] if self._candidate_ids else slots`, so a downgrade after the first block re-reads every code and the retry's top-k replaces earlier values. New replay scenario `codes-late-downgrade` (a server that honours the first candidate-id request and rejects the second, with top-k scores that differ from the candidate-id scores).
7. **`SetTokenizer` (was MEDIUM).** It now clears `slots` and `boundaryChecked`. A read in flight keeps the tokenizer it started with: `answerPrefixIDFor(tok)` and `checkCodeBoundaryFor(tok, ...)` take the tokenizer explicitly, `CodeSlots` resolves the whole list under one lock, and `Think` uses the tokenizer it loaded.
8. **Response cap (was LOW).** 8 MB -> 64 MB, with the rationale corrected (`prompt_logprobs: 0` returns one entry per prompt token, not one position of top-logprobs).
9. **`closeEnough` (was MEDIUM).** Now checks key presence and finiteness (NaN/Inf) and compares to a relative tolerance with an absolute floor.
10. **Gate coverage.** Each recorded exchange now carries `method` and `path`, compared by the replay server; `requireReadoutError` asserts the recorded Python error class as well as the message pattern; the generator raises if the Python client raises an error the scenario did not expect; a `codes-no-text` scenario covers a choice with no `text`. New unit tests: DE-1 refusals, the family provenance, the error taxonomy, malformed responses, the response cap, the bearer header and model listing, a tokenizer swap, local-only resolution, and eight concurrent readers.
11. **Docs.** `docs/PORT.md` gained the error-taxonomy paragraph, the zero-value conventions, the field-level decoding strictness, the downgrade and `LocalFilesOnly` notes, and corrected the "stricter about three things" list to four.

Validation already run: `gofmt -l .` clean, `go build ./...`, `go vet ./...`, `go test ./...` and `go test -race ./...` all pass. Each new gate was checked against a deliberate mutation of the behaviour it covers (the request path, the block selection, the rank recording, the softmax, the text-keyed lookup).

## Primary files / regions

- `readout.go` — reason: the DE-2 guard, the downgrade fix, the nil fallback entry, the tokenizer-generation fixes, SetTokenizer
- `endpoint.go` — reason: TokenIDList, the ReadoutHTTPError taxonomy, the response cap, the doJSONLimit seam
- `tokenizer/hub.go` — reason: ResolveOptions, ResolveWith, LoadWith, ErrNotCached
- `readout_test.go` — reason: the new unit tests and the changed replay server
- `scripts/gen_readout_parity.py` — reason: the late-downgrade scenario, the method/path recording, the expected-error check
- `docs/PORT.md` — reason: the claimed differences must be true and complete

## Review questions

- Round 1 found the DE-2-only guard missing from `NewReadout`. It now calls `RequireDE2` before anything is loaded or sent, and `ResolvedFamily` reports the source. Is the guard now complete and unbypassable through any exported path, and is the resolution order right? Check `TestNewReadoutRefusesDE1` for gaps.
- The null-token-id, nil-fallback-entry and `LocalFilesOnly` fixes: does each actually close the hole it names, without introducing a new one? In particular, is `TokenIDList` strict about every non-integer shape Go's decoder would otherwise coerce, and does the local-only policy ever reach `Fetch`?
- `ReadoutHTTPError` now unwraps to a `*ReadoutError`. Does that make `errors.As` work for both types in every direction a caller would use, without breaking the `logprob_token_ids` sniff in `readCodes`? Is there any error path where the embedded `*ReadoutError` is nil and `Error()` would panic?
- The mid-read downgrade now matches the Python client (`block = slots` when the capability is off). Is the match exact, including `start` accounting and the request count, and does the `codes-late-downgrade` scenario actually distinguish the two behaviours?
- `SetTokenizer` now discards the slot and boundary caches and a read keeps the tokenizer it started with. Trace a concurrent `SetTokenizer` against `CodeSlots`, `ReadCodes` and `Think`: can any read still tokenize with one tokenizer and validate against another, or mix slots from two?
- The replay gate gained method/path checks, error-class checks and a generator that refuses an unexpected error. Is there still a comparison that cannot fail, a fixture field that is decoded but never checked, or a scenario that would pass with the readout broken? Is the `closeEnough` rewrite (relative tolerance with an absolute floor, key presence, finiteness) correct?
- docs/PORT.md now claims a specific list of deliberate differences and of places this port is stricter. Is any claim in it false, or is any actual difference still unclaimed?

## Known non-goals

- DE-1 is not implemented and must not be: a model that resolves to DE-1 is an error.
- `policy.go`, `calibration.go`, `client.go`, `cmd/shisa-de/` and the live test suite do not exist yet (M4/M5/M6).
- No live requests were made and no number here is a measurement against a served model.
- `testdata/readout_parity.json` is a generated fixture; review the generator and the test, not the fixture bytes.
- The M1/M2 packages (`tokenizer/`, `pyjson.go`, `questions.go`, `family.go`, `codebook.go`) are out of scope except `tokenizer/hub.go`, which gained the local-only resolution policy in this round's fix commit.

## Validation already run

- gofmt -l . (clean)
- go build ./... (clean)
- go vet ./... (clean)
- go test ./... -count=1 (shisade 2.3s, shisade/tokenizer 7.7s, both ok)
- go test -race ./... -count=1 (shisade 14.1s, shisade/tokenizer 44.8s, both ok)
- python3 scripts/gen_readout_parity.py (regenerates 21 scenarios, 61 exchanges, digest 05248905c289)
