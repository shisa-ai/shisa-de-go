# Review Report

## Summary

Read-only review of the Go port at `/home/morpheus/data/dev/shisa/shisa-de-go` against the Python reference at `/home/morpheus/data/dev/shisa/shisa-de` and the copied contract `docs/READOUT-DE2.md`. I reviewed the round-3 target (`6b35ee2`'s regions) and the accumulated M3 surface: `readout.go`, `endpoint.go`, `readout_test.go`, `scripts/gen_readout_parity.py`, `tokenizer/hub.go` (local-only policy), `docs/PORT.md`, and the two fixtures' provenance.

The round-2 findings are closed, and I could not break the fixes:

- The `CheckCodeBoundary` self-deadlock is gone. I swept every `r.mu` acquisition in `readout.go` (nine locking functions) and every caller: no locking function calls another locking function, `answerPrefixIDFor`/`slotLocked`/`ensureTokenizerLocked` never lock, and every exported `Readout` method now has a test. The class is closed.
- The approval cache is now `map[string]int` and is trusted and written only while `r.tokenizerGeneration` still equals the generation the read captured. Since a generation match implies the same tokenizer object, a hit means the same `(tokenizer, code, token id)` triple was checked before, and a read that lost a `SetTokenizer` race re-checks without caching. I traced the interleavings and could not construct a read that skips a check it should run, sends an id the current tokenizer never produced, or leaves a later read a stale approval.
- `ReadoutHTTPError`'s explicit `Error()`/`Unwrap()` behave correctly for `&ReadoutHTTPError{Status: 503}` (status message, `Unwrap() == nil`, `errors.As` does not find a nil `*ReadoutError`) and for the in-package construction with the embed set; the `logprob_token_ids` sniff still finds the concrete type because `doJSONLimit` returns it unwrapped. A zero-value `Readout` returns an error from every read entry point without panicking (`ResolveWith("")` fails before any fetch).
- The gate's new coverage is real: I re-derived the exchange total from the generator (61) and it matches the fixture and the provenance, including that `error-fallback-empty` records only two exchanges because the read aborts on the first empty fallback.

Findings are three LOW items: the round-2 generation guard has no test, `PORT.md`'s null-entry sentence misdescribes both clients, and the gate's header comparison is narrower than the claim it is given.

## Findings

### LOW: The tokenizer-generation guard — this round's primary MEDIUM fix — has no regression test
- Severity: LOW
- Confidence: HIGH
- Location: `readout.go:573-608` (the guard at `:577`, `:589`, `:601`), with `readout_test.go:1041`
- Problem: `checkCodeBoundaryFor` decides whether to trust and write `boundaryChecked` from `current := r.tokenizerGeneration == generation`. That guard is the whole of the in-flight fix, and nothing in the tree fails if it is deleted. `TestSetTokenizerForgetsBoundaryApprovals` passes either way, because it exercises the half that existed before this round: `SetTokenizer` clears the cache, so the re-check happens with an empty map regardless of `current`. `TestSetTokenizerDiscardsDerivedState` covers only the slot cache, `TestCheckCodeBoundaryRefusesAForeignTokenID` only the id half of the key, and the replay never swaps a tokenizer. I checked the mutations: dropping `current` entirely, or making it unconditionally true, leaves `go test ./...` green. This is exactly the shape of the round-2 escape (a correct-looking helper with no caller test), and the round-3 validation note records a mutation check for the deadlock test and the header comparison but none for this guard.
- Recommendation: add a package-internal test that calls the helper with a stale generation and asserts nothing was cached — e.g. capture `tok, gen, _ := readout.tokenizerSnapshot()`, call `readout.SetTokenizer(other)`, call `readout.checkCodeBoundaryFor(tok, gen, []int{prefixID}, slots)`, and require both that it does not skip the per-code check and that `readout.boundaryChecked` is still empty afterwards; then a second call under the new tokenizer must still re-check. That pins the guard without depending on a timing-sensitive interleaving.

### LOW: `PORT.md`'s null-entry sentence is wrong about both clients
- Severity: LOW
- Confidence: HIGH
- Location: `docs/PORT.md:132-133` (inside the "stricter about three things" bullet at `:127-133`)
- Problem: the sentence reads "A null entry at that position is an error too, where the Python client reads `None` through and reports the code as unreturned." Neither reading holds:
  - The case this port actually rejects with the "null entry" message is a null map *value* at the last position (`{"65": null}` → a nil `*promptLogprobEntry`, `readout.go:829-834`). Python's `_letter_logprob` takes `entry = next(iter(entries.values()))` → `None` → `entry.get("logprob")` → `AttributeError: 'NoneType' object has no attribute 'get'`. Python does not read it through and does not report the code as unreturned; it raises an exception that is not a `ReadoutError` at all. The port is stricter here, but for a different reason than the sentence gives.
  - A null *position* (`prompt_logprobs: [null, null]`) is not a difference: Go's `lastPromptLogprobEntry` returns nil, `len(entries) == 0` returns `nil` logprob, and the caller raises "code %q was not returned for its token id and the prompt_logprobs fallback returned nothing for it" — the same `ReadoutError`, with the same wording, that Python raises after `if not entries: return None, None, prompt_tokens`.
  This is the same class of defect round 2 found twice in this file: the list is the record of deliberate divergences, so an inaccurate description of the reference invites a later reader to treat the port's behaviour as arbitrary and relax it.
- Recommendation: replace the sentence with the two precise cases: a null *value* in the last position's map is a `ReadoutError` here where the Python client raises `AttributeError`; a null last position is the "returned nothing" error on both sides, so it is not a difference.

### LOW: The gate's header comparison covers `Content-Type` only; the `Authorization` half of the claim is unreachable
- Severity: LOW
- Confidence: HIGH
- Location: `readout_test.go:392-400`, `scripts/gen_readout_parity.py:182` and `:191`, `docs/PORT.md:284-287`
- Problem: `PORT.md` states that every recorded exchange carries the two headers the client sets and that "the replay server compares all of them, so a readout that ... dropped a header fails the gate". The comparison loops over `exchange.Headers`, so it only checks names the fixture records, and the generator's `Recorder` builds its `Readout` with `api_key=""` (`gen_readout_parity.py:182`), so every recorded exchange holds exactly `{"content-type": "application/json"}`. I confirmed this against the fixture: the `"headers": {"content-type": "application/json"}` key is present on every exchange and the string `authorization` appears nowhere in it. The `Authorization` branch of the loop is therefore dead with the current generator, and the replay test could not exercise it even if it were recorded, because it builds its `Readout` with no `APIKey`. The same paragraph also sits between two bullets of the "Verified parity" list, which makes it read as part of the preceding `Readout` bullet. (The header itself is covered elsewhere by `TestReadoutSendsTheBearerToken`, so this is a claim-versus-coverage gap, not an untested behaviour.)
- Recommendation: either narrow the sentence to what the gate can fail on (method, path and `Content-Type`, with the bearer header covered by its own test and no API key configured in the fixture), or give the generator a keyed scenario — a per-scenario `api_key` recorded in the exchange and a matching `ReadoutConfig.APIKey` in `TestReadoutReplay` — so the `Authorization` comparison is live. Also move the paragraph inside the `Readout` bullet it belongs to.

## Things Checked

- **Exported-method lock sweep.** `readout.go` acquires `r.mu` in nine places: `SetTokenizer`, `ensureTokenizer`, `tokenizerSnapshot`, `Slot`, `CodeSlots`, `AnswerPrefixID`, `checkCodeBoundaryFor`, `usesCandidateIDs`, `disableCandidateIDs`. `ensureTokenizer`/`tokenizerSnapshot` call only `ensureTokenizerLocked`; `Slot`/`CodeSlots` only `slotLocked`; `AnswerPrefixID` calls `ensureTokenizerLocked` and `answerPrefixIDFor`. `CheckCodeBoundary` and `readCodes` call `tokenizerSnapshot()` (lock/release) and then `checkCodeBoundaryFor` (lock). `Think`, `ListModels` and `Render` take no lock. No locking wrapper calls a locking helper; no helper is entered with the lock held and re-locks. All eleven exported `Readout` methods have a direct or replay test.
- **Approval-cache soundness.** The per-code check is a pure function of `(tok, code, tokenID)` plus the `prefixID` derived from `tok`, so a `code → tokenID` hit under an unchanged generation cannot differ from a fresh check. Generation only increments in `SetTokenizer`, which also replaces the tokenizer and clears the map, so a generation match implies the same tokenizer object; `ensureTokenizerLocked` assigns a loaded tokenizer without bumping the generation, which is safe because it only fills a nil field and no read can have captured a tokenizer before it existed. The map field is re-read under the lock inside the loop, so a concurrent `SetTokenizer` cannot swap it mid-loop.
- **Interleavings.** Read captures A at g, `SetTokenizer(B)` → the read's `current` is false, it checks every code against A and caches nothing; a read under B caches B-approvals at g+1 only. A caller holding slots resolved from A and reading after `SetTokenizer(B)` gets the "boundary moves" error rather than sending A's ids. `CodeSlots` resolves the whole list under one lock, so a returned list is never mixed across tokenizers.
- **`errors.As` matrix.** `*ReadoutHTTPError` with the embed set: found as itself and as `*ReadoutError` through the explicit `Unwrap`. `&ReadoutHTTPError{Status: 503}`: `Error()` names the status, `Unwrap()` returns an untyped nil, `errors.As(err, &readoutError)` is false. A `ReadoutHTTPError` value does not implement `error` (pointer receiver), so it cannot be passed as one. `doJSONLimit` returns the concrete `*ReadoutHTTPError` unwrapped, so the `readCodes` sniff's `errors.As` + `Status`/`Detail` still work.
- **Zero value.** `ResolvedFamily` and `Render` do not error (they need no tokenizer or client), but every read entry point does: `Slot`/`CodeSlots`/`AnswerPrefixID`/`CheckCodeBoundary`/`ReadCodes`/`ReadCodesAfterThought`/`Think` fail in `ResolveWith("")` before any network use, and `ListModels` in `httpClient()`. No panic path.
- **Replay gate inventory.** Method and path are compared (always recorded); bodies after JSON decode; every `readResult` field (logprobs, probabilities via `closeEnough`, requests, prompt tokens, missing codes, ranks, sampled, debug prompt, debug top-logprobs) and every `thoughtResult` field; the after-thought id list is rebuilt by the test and compared; the recorded Python error class is asserted with the negative check that a plain `ReadoutError` is not secretly an HTTP error; both request-count directions are checked (`s.at >= len(s.exchanges)` in the handler, `replay.at != len(step.Exchanges)` per step); provenance checks readout version, `think_system`, limits, scenario and exchange counts and the sha256. I re-derived the 61 exchanges from the generator (including that `error-fallback-empty` aborts after its first fallback, and that `codes-late-downgrade` needs no fallback because `top_k=256`) and it matches the fixture and the provenance.
- **`codes-late-downgrade`.** Request 1 (128 ids) is honoured, request 2 (1 id) is rejected, request 3 re-reads the whole 129-slot list with `top_k=256` and scores from `topk_score`, which differs from the candidate-id scores. A client that re-read only `slots[start:]` would keep the first block's candidate-id logprobs and fail the result comparison; the request bodies are identical either way, so the logprobs comparison is what discriminates, and it does.
- **`PORT.md` claims spot-checked against the code and the fixtures.** The softmax sum-order paragraph is accurate: the codebook is `A`–`Z` followed by 230 pairs in lexical order (`data/codebook-de2.json`), so sorting by shortest-key-then-lexicographic equals codebook order for the prefixes `CodeSlots` produces; the 1e-12 relative tolerance is what the replay uses. The field-level decoding paragraph now names the `top_logprobs`/`prompt_logprobs` map values and its Python comparisons (`int(... or 0)`, `entry.get("rank")`, `entry.get("id")`, the maps failing later in `math.exp`) hold. The `token_ids` hazard has moved out of the "stricter" list and is described as both clients refusing the same shapes. The zero-value conventions (`SystemPrompt`/`MaxLogprobs`), the `choices`-missing strictness, the downgrade paragraph, the `LocalFilesOnly` paragraph, the non-object-JSON paragraph and the fixture counts (35 renders, 39 serialization fixed points, 1,056 strings / 48,392 ids / 83 decode cases, Python `shisa-de` 0.4.0) are all true.
- **`tokenizer/hub.go`.** `ResolveWith` returns `ErrNotCached` before `Fetch` whenever `LocalFilesOnly` is set, after the readable-file and hub-cache lookups; `Lookup` resolves only the requested revision and never substitutes another snapshot. The readout is the only `LoadWith` caller and passes the flag through.
- **Completeness note.** The only port-only exported surface I found that `PORT.md` does not mention is `Readout.SetTokenizer` (a test hook with no Python counterpart); unlike the null-entry sentence, nothing in it misdescribes the reference, so I did not raise it as a finding.

## Residual Risks

- No shell tool is available to me, so `gofmt`, `go build`, `go vet`, `go test` and `go test -race` results are the scope's claims, not reproduced here, and no commit ancestry, diff or working-tree status was verified. Every finding is against the inspected working-tree files.
- `testdata/readout_parity.json` is a 510 KB single line the read tool cannot page, so I reviewed the generator, the meta and the test and re-derived the exchange count arithmetically rather than reading the recorded bodies. The recorded bodies themselves are unverified here.
- No live request was made, so nothing here is a measurement against a served model.
- `readout_parity.meta.json`'s `python_repo` is asserted only for non-emptiness, so a fixture generated from a different Python revision still passes the provenance test as long as it is internally consistent.
- The `Softmax` sum order leaves a ~1e-16 relative difference from CPython; no gate code exists in this port yet and no fixture sits near 0.7, so the risk is unmeasured rather than absent.
- If the generation guard is left untested, the in-flight approval-cache poisoning can return without any test failing; the guard is the only thing preventing it.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "LOW",
      "title": "The tokenizer-generation guard has no regression test",
      "location": "readout.go:573-608 (guard at :577, :589, :601), readout_test.go:1041",
      "recommendation": "Add a package-internal test that calls checkCodeBoundaryFor with a stale generation after SetTokenizer and asserts the per-code check still runs and boundaryChecked stays empty."
    },
    {
      "severity": "LOW",
      "title": "PORT.md's null-entry sentence misdescribes both clients",
      "location": "docs/PORT.md:132-133",
      "recommendation": "Say that a null map value at the last prompt position is a ReadoutError here where Python raises AttributeError, and that a null last position is the same 'returned nothing' error on both sides."
    },
    {
      "severity": "LOW",
      "title": "The replay gate compares only Content-Type; the Authorization claim is unreachable",
      "location": "readout_test.go:392-400, scripts/gen_readout_parity.py:182, docs/PORT.md:284-287",
      "recommendation": "Narrow the claim to the headers the fixture records (Content-Type, with the bearer header covered by its own test), or add a keyed scenario with a matching APIKey in the replay test; also move the paragraph inside the Readout bullet."
    }
  ]
}
END_REVIEW_JSON
