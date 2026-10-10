# Review Report

## Summary

I reviewed the M3 round-2 target read-only: the fixes in `29049db` and the accumulated M3 range, against the Python reference at `/home/morpheus/data/dev/shisa/shisa-de` (`shisa_de/readout.py`, `family.py`) and the copied contract page `docs/READOUT-DE2.md`. Files inspected: `readout.go`, `endpoint.go`, `family.go`, `errors.go`, `codebook.go`, `render.go`, `tokenizer/hub.go`, `readout_test.go`, `render_test.go`, `tokenizer/tokenizer_test.go`, `scripts/gen_readout_parity.py`, `testdata/readout_parity.meta.json`, `docs/PORT.md`, `AGENTS.md`.

The round-1 findings are genuinely closed. The DE-2-only guard is in `NewReadout` and `NewReadout` is the only constructor in the tree; `LocalFilesOnly` returns `ErrNotCached` before `Fetch` on every path the readout uses; `TokenIDList` rejects every non-integer element shape; the null fallback entry returns an error instead of dereferencing; the error taxonomy now unwraps both ways without breaking the `logprob_token_ids` sniff; the mid-read downgrade matches Python's `block = slots` including `start` accounting and the request count, and the `codes-late-downgrade` scenario does distinguish the two behaviours; `closeEnough` is correct. The replay gate now checks method, path and the recorded Python error class, and the fixture's 21 scenarios / 61 exchanges / digest match the provenance and the scope.

Findings are one MEDIUM (the boundary-approval cache is keyed by code only, so a slot id the current tokenizer never produced can skip the check) and four LOW items: a false bullet in `PORT.md`'s "stricter" list plus an unclaimed difference, a nil-embed panic in `ReadoutHTTPError`, an untested `closeEnough`, and a zero-value `Readout` that panics.

## Findings

### MEDIUM: The boundary-approval cache is keyed by code only, so an id from another tokenizer skips the check
- Severity: MEDIUM
- Confidence: HIGH (mechanism); LOW (reachability of case ii)
- Location: `readout.go:557`, with `readout.go:330` and `readout.go:413-419`
- Problem: `boundaryChecked` records only that a code's boundary was approved, not which token id was approved. `checkCodeBoundaryFor` therefore does `if r.boundaryChecked[slot.Code] { continue }` for any slot carrying that code, whatever `slot.TokenID` holds. Two paths reach a read that tokenizes the prompt with one tokenizer, validates nothing, and sends an id the other tokenizer produced:
  - (i) a caller-supplied `Slot`. `Slot` is exported with exported fields, `ReadCodes`/`ReadCodesAfterThought` take `[]Slot`, and nothing documents that the ids must come from `CodeSlots`. After one read approves code `A`, a second read with `Slot{Code: "A", TokenID: 999}` passes the check and puts 999 in `logprob_token_ids`.
  - (ii) the interleaving round 1 reported survives the fix. `SetTokenizer(B)` clears the caches, but they are repopulated by any read that uses B-resolved slots: resolve `slotsA` under A, `SetTokenizer(B)`, let a read with B-resolved slots approve the same codes, then read with `slotsA`. The per-code check is skipped and A's ids go to the server against a B-tokenized prompt.
  In both cases the server scores a different token and the read returns a distribution nobody asked for — the mismatch `checkCodeBoundaryFor` and `SetTokenizer` both describe themselves as preventing ("keeping them would let a read tokenize the prompt with one and validate the codes against another").
- Recommendation: record the approved id, not just the fact of approval — `boundaryChecked map[string]int` with `if id, ok := r.boundaryChecked[slot.Code]; ok && id == slot.TokenID { continue }` — which keeps the per-code caching the check's doc relies on and stays valid across any `Tokenizer` implementation. Add a regression test that does not depend on concurrency: read once with `CodeSlots`, then read the same prompt with the same codes and one mutated `TokenID`, and require a `ReadoutError`.

### LOW: `PORT.md`'s difference list is inaccurate for the `token_ids` bullet and omits Softmax's sum order
- Severity: LOW
- Confidence: HIGH
- Location: `docs/PORT.md:125-138` (bullet at `:134-138`), `docs/PORT.md:27-31`, `readout.go:93-110`
- Problem: The heading reads "**The readout is stricter about four things the Python client tolerates.** Each is a case where the Python client picks something and this port refuses to", but the `token_ids` bullet says "the Python client checks the element type and this port does too". Python's `think` raises for `[null]` and `[1.5]` exactly as this port does (`any(isinstance(t, bool) or not isinstance(t, int) for t in token_ids)`), so this is not something Python tolerates and not a place the port differs. The bullet is the record of the round-1 MEDIUM fix, and as written it invites the next reader to conclude the strictness is a port-only choice and relax it back toward a plain `[]int` — the original defect. Separately, `Softmax` sums in a sorted key order (shortest key, then lexicographic) where Python sums in the caller's dict order and, on CPython 3.12+, with compensated summation. That is a deliberate difference, documented in the function's own comment, in a public function, and the page that claims to record "every place the Go client deliberately behaves differently" does not mention it.
- Recommendation: reword the bullet as a Go decoding hazard that the port now matches Python on, or move it out of the "stricter" list and say plainly that both clients refuse these shapes; add one sentence to the differences list about the softmax sum order and the tolerance the replay uses for it.

### LOW: `ReadoutHTTPError` with a nil embedded `*ReadoutError` panics in `Error()` and satisfies `errors.As` with a nil target
- Severity: LOW
- Confidence: HIGH
- Location: `endpoint.go:22-30`
- Problem: `Unwrap` returns `e.ReadoutError` unconditionally, and the embedded field is exported on an exported type, so `&shisade.ReadoutHTTPError{Status: 503}` is constructible from outside the package (or from a future call site that forgets the embed). `Error()` is promoted from the embedded pointer and panics on a nil receiver. Worse, `errors.As(err, &readoutError)` returns true with a nil `*ReadoutError` (the returned interface holds a typed nil, so the walk continues and matches the type), so a caller that logs `readoutError.Message` after a successful `As` panics too. Every construction inside this package sets the embed (`endpoint.go:200` is the only one), so no library path panics today.
- Recommendation: `func (e *ReadoutHTTPError) Unwrap() error { if e.ReadoutError == nil { return nil }; return e.ReadoutError }`, and consider giving `ReadoutHTTPError` its own `Error()` that tolerates a nil embed, so a malformed value is a bad message rather than a panic.

### LOW: `closeEnough`'s new key-presence, finiteness and relative-tolerance branches have no direct test
- Severity: LOW
- Confidence: HIGH
- Location: `readout_test.go:499-520`, used at `readout_test.go:278`
- Problem: The round-1 fix rewrote the helper and is correct, but only the replay path calls it, and every recorded probability agrees to roughly 1e-16 — far inside the tolerance. A mutation that made `closeEnough` return true unconditionally, or that dropped the NaN/Inf checks or the key-presence check, would not fail any test in the tree. Round-1 (astra) asked for direct tests of exactly these cases; the helper was changed instead of covered.
- Recommendation: add a small table test: equal maps pass; a missing key, an extra key, a NaN on either side, an infinite value, and a difference past the tolerance each fail; a difference inside the relative tolerance (with a value near the 1e-9 floor) passes.

### LOW: A zero-value `Readout` is constructible and panics; the guard documentation overstates that a directly held `Readout` cannot bypass the guard
- Severity: LOW
- Confidence: HIGH
- Location: `readout.go:312-345`, `readout.go:881-890`, `AGENTS.md:17-21`, `docs/PORT.md:29-37`
- Problem: `Readout` has only unexported fields, so `shisade.Readout{}` compiles from another package exactly as `http.Server{}` does. Nothing on that value is validated: `SetTokenizer` succeeds, `CodeSlots` resolves slots, and the first read panics in `doJSON` — `client.Do` on a nil `*http.Client` — instead of returning a `ReadoutError`. It cannot produce an answer, so the DE-2 guard is not in fact bypassable; but "a caller holding a `Readout` directly cannot bypass it" is true only of a readout built by `NewReadout`, and the panic is a nil-dereference in a library that returns errors everywhere else.
- Recommendation: either return a `ReadoutError` when `r.client` is nil, or state in `NewReadout`/`PORT.md` that `NewReadout` is the only supported constructor and that the zero value is not usable.

## Things Checked

- **DE-2 guard.** `grep` for `&Readout{` finds exactly one construction site (`readout.go:380`), so `NewReadout` is the only way to a configured readout. `RequireDE2(config.Model, config.Tokenizer, config.Family)` runs after the URL/model checks and before anything is loaded or sent, and `ResolveFamily`'s order (declared, model id, tokenizer, assumed) matches `resolve_family` in `family.py`, including `NormalizeFamily`'s `[^a-z0-9]` stripping. `TestNewReadoutRefusesDE1` covers a model id, a bare `de-1`, a tokenizer source, a declaration, and a declaration overriding a DE-2 model id; `TestNewReadoutRecordsTheFamily` covers all four sources. An unknown declaration (`Family: "de3"`) returns a plain error rather than `*UnsupportedFamilyError` — still refused, just not that type. `ResolvedFamily` cannot report a family other than DE-2, because a non-DE-2 resolution returns an error instead of a readout.
- **`TokenIDList`.** `UnmarshalJSON` decodes into `[]json.RawMessage` and requires each element to unmarshal into a non-nil `*int`, so `null`, `1.5`, `true`, `"5"`, `{}` and nested arrays are all errors, matching Python's type test. `token_ids: null` never reaches the method: the field is a `*TokenIDList` and Go's decoder nils a pointer for the `null` literal, which the "null token_ids field" test pins. A response whose `token_ids` is not an array fails the whole decode, so it surfaces as the generic "did not return a JSON object" error rather than Python's "carries no token_ids" wording — same class, different message, only for shapes no server sends.
- **Nil fallback entry.** `lastPromptLogprobEntry` returns nil for an empty `prompt_logprobs`, a null element, or an empty map, and `letterLogprob` turns that into the "fallback returned nothing" error the caller already handled; a nil map *value* (`{"65": null}`) is caught by the `entry == nil` check with the "null entry" message. No nil dereference remains on that path, and the multi-entry refusal is unchanged.
- **`LocalFilesOnly`.** `ResolveWith` checks the file, `checkSource`, and the hub cache, then returns `ErrNotCached` before `Fetch`; `Load`/`Resolve` are the same functions with the policy off, so M1 call sites are untouched. `readout.go:426` is the only caller of `LoadWith`, and it passes `r.localFilesOnly`. `TestReadoutLocalFilesOnly` and `TestLocalFilesOnlyNeverFetches` both assert `errors.Is(err, ErrNotCached)`, and the latter also pins that a readable file resolves either way and that the pinned repo either resolves or reports `ErrNotCached`.
- **Error taxonomy.** `errors.As` finds `*ReadoutHTTPError` and then the embedded `*ReadoutError` through `Unwrap`; the `logprob_token_ids` sniff in `readCodes` still uses `errors.As(err, &httpError)` with `Status`/`Detail` intact. `requireReadoutError` asserts the negative for the `ReadoutError` class, so the new embedding cannot let an HTTP error pass as a plain one in the `error-fallback-empty`/`error-prompt-tokens`/`error-boundary` scenarios.
- **Mid-read downgrade.** I traced the 30-code immediate rejection, the two-step `codes-topk-rejected` scenario and the 129-code late downgrade against `read_codes`: with the capability off the block is the whole slot list, `start += len(block)` ends the loop, the rejected request is not counted in `Requests` or `PromptTokens`, `Sampled` is only taken at `start == 0`, and the retry's top-k overwrites every earlier code — all as in Python. `codes-late-downgrade` (129 codes, first block honoured, second rejected, `top_k=256`, top-k scores deliberately different from the candidate-id scores) records three exchanges, and the old `slots[start:]` behaviour would leave codes 0-127 with the candidate-id scores, so the fixture's `logprobs` comparison fails on it. The request bodies match either way, so the result comparison is what distinguishes them.
- **`SetTokenizer` and concurrency.** `CodeSlots` resolves the whole list under one lock; `answerPrefixIDFor` and `checkCodeBoundaryFor` take the tokenizer explicitly; `Think` uses the tokenizer it loaded and the prefix id resolved from it; `checkPromptTokens` uses only immutable fields. No `*Locked` helper re-locks, so there is no deadlock, and `TestReadoutConcurrentReads` plus `-race` cover the shared state. The one gap is the approval cache (finding 1); `TestSetTokenizerDiscardsDerivedState` pins only the slot half of the invalidation, not the approval half.
- **Replay gate.** The generator records `method` and `path` per exchange and the replay server compares both; the fixture's error class is asserted (with the negative check above); `error_record` makes the generator fail when Python raises something the scenario did not expect; `codes-no-text` pins the `Sampled` string-vs-`None` difference; every `readResult` and `thoughtResult` field is compared, the read-after-thought ids are rebuilt by the test and compared, and both request-count directions are checked. The stub tokenizer is rebuilt from the fixture's word list with the same longest-match-first rule as the generator's regex. `readout_parity.meta.json` records 21 scenarios, 61 exchanges and digest `05248905c289…`, matching the scope; I recomputed the exchange total from the generator (including the failed 400 in `codes-topk-rejected` and the fallback counts) and got 61, which corroborates the fixture.
- **`closeEnough`.** Length equality plus per-key presence, NaN/Inf rejection on both sides, and `|got-want| > tol * max(|want|, 1e-9)` is a sound relative test with an absolute floor; the comment now matches the implementation.
- **`PORT.md` claims spot-checked.** The downgrade paragraph, the `LocalFilesOnly` paragraph, the zero-value conventions, the field-level strictness, the "no `choices`" paragraph, the mid-read paragraph and the fixture inventory are all true of the code. The `token_ids` bullet is the exception (finding 2). Readout identities and `calibrationCompatible` still match `READOUT_VERSION`/`DE2_READOUT_VERSION`/`CALIBRATION_COMPATIBLE`.

## Residual Risks

- No commands were run: no shell tool is available to me, so `gofmt`, `go build`, `go vet`, `go test` and `go test -race` results are the scope's claims, not reproduced here, and no commit ancestry or working-tree status was verified.
- `testdata/readout_parity.json` is a 507 KB single-line file that the read tool cannot page; I reviewed the generator, the test and the provenance, and corroborated the scenario/exchange counts arithmetically rather than reading the recorded bodies.
- No live request was made, so nothing here is a measurement against a served model.
- The softmax sum order means a probability within ~1e-16 of the 0.7 gate could fall on the other side from Python's. There is no gate code in this port yet, and no fixture sits near the threshold, so the risk is unmeasured rather than absent.
- The exported readout surface is exercised mostly indirectly: `Readout.Render`, `Readout.Slot`, `Readout.AnswerPrefixID` and `Readout.CheckCodeBoundary` have no direct test, and `Readout.Render` (the method the M4 client layer will call) has none at all.
- The replay server still does not compare request headers, so `Content-Type: application/json` — which every request must carry — is unverified; the bearer token is covered by `TestReadoutSendsTheBearerToken`.
- `readout_parity.meta.json`'s `python_repo` is asserted only for non-emptiness, so a fixture generated from a different Python revision still passes the provenance test as long as it is self-consistent, and the recorded `think_system` is never read by any test.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "Boundary-approval cache keyed by code only, so a foreign token id skips the check",
      "location": "readout.go:557 (with readout.go:330, readout.go:413-419)",
      "recommendation": "Record the approved token id (map[string]int) and skip only when it equals slot.TokenID; add a non-concurrent regression test that a mutated TokenID is refused after the code was approved."
    },
    {
      "severity": "LOW",
      "title": "PORT.md's token_ids bullet is not a difference, and Softmax's sum order is an unclaimed one",
      "location": "docs/PORT.md:125-138, docs/PORT.md:27-31, readout.go:93-110",
      "recommendation": "Reword the token_ids bullet as a Go decoding hazard both clients refuse, and add the softmax sum-order difference to the deliberate-differences list."
    },
    {
      "severity": "LOW",
      "title": "ReadoutHTTPError with a nil embedded *ReadoutError panics and satisfies errors.As with a nil target",
      "location": "endpoint.go:22-30",
      "recommendation": "Return nil from Unwrap when the embedded error is nil, and give ReadoutHTTPError an Error() that tolerates a nil embed."
    },
    {
      "severity": "LOW",
      "title": "closeEnough's new key-presence, finiteness and tolerance branches are untested",
      "location": "readout_test.go:499-520",
      "recommendation": "Add a table test covering equal maps, missing/extra keys, NaN, Inf and an out-of-tolerance difference."
    },
    {
      "severity": "LOW",
      "title": "A zero-value Readout is constructible and panics; the guard docs overstate unbypassability",
      "location": "readout.go:312-345, AGENTS.md:17-21, docs/PORT.md:29-37",
      "recommendation": "Return a ReadoutError for a nil client, or document NewReadout as the only supported constructor."
    }
  ]
}
END_REVIEW_JSON
