# Review Report

## Summary

The round-1 findings are addressed and the fixes are correct where I could verify them by inspection. `fuse_unk` now runs inside `encodePart`, so it is confined to one pre-token's model output and cannot touch added-token ids or cross pre-token or piece boundaries; the regression test would fail against the pre-fix code and the corpus carries the adjacent-`<unk>` vectors (I confirmed `"<unk><unk>"` is present in the fixture). The `Lookup` glob fallback is gone everywhere (grep confirms no `filepath.Glob` outside the round-1 reports), the snapshot path resolves only `snapshots/<hash>`, and anything else goes through `refs/<revision>`. The normalizer, pre-tokenizer and decoder are now compared against the pinned values, not just parsed by type. The 80 decode vectors pin the measured invalid-byte rule — including the two cases that distinguish it from both plain concatenation and `from_utf8_lossy` (`E1 80 C2` → 3 replacement characters, and `C2 A9 80 C2 A9` → 5, which confirms the run is evaluated all-or-nothing) — and the implementation matches them. `Fetch` validates before rename, caps the body, re-fetches an unparseable cache entry exactly once per call with no loop, and `checkSource` closes the traversal and request-shaping holes I could construct.

What remains is a set of LOW findings: one doc claim in `docs/PORT.md` that this round's own measurement contradicts, the decode-fixture count that the meta file records but no test enforces, the meta's sha256 that nothing verifies, an incomplete consolidation of the pinned-tokenizer fingerprint (the render tests and the benchmark are weaker than the tokenizer tests), one validation gap in `checkDecoder` that is inconsistent with its sibling checks, an unverified `Decode` edge for out-of-vocab ids inside a byte run, and file tables in `PORT.md`/`AGENTS.md` that name files that do not exist yet.

This was a read-only review without a shell: I verified the working tree, not commit 3beed13 directly, and I did not run the test suite; the scope's validation record is the evidence for the passing runs.

## Findings

### LOW: PORT.md claims Hugging Face refuses duplicate vocab ids; this round's measurement says it accepts them
- Severity: LOW
- Confidence: HIGH
- Location: docs/PORT.md, "The tokenizer pipeline is pinned, not general" paragraph
- Problem: The sentence "The same applies to a malformed file Hugging Face also refuses: a negative or duplicate vocab id, an added token whose declared id disagrees with the vocab, or a merge whose operands or concatenation is missing" bundles the duplicate-id rejection into the class of files Hugging Face also refuses. The round-2 scope and its validation record state the opposite for duplicates: "Hugging Face accepts them but Go's map made the winner depend on iteration order", and the recorded check ran "HF accepts a duplicate id". So rejecting duplicates is a deliberate difference of this port (like the accepted-but-refused normalizer/decoder shapes), not a shared refusal. The same sentence also asserts Hugging Face refuses an added token whose declared id disagrees with the vocab, which neither round measured. The doc is the authoritative record of the rejection contract, and this sentence misstates it.
- Recommendation: Move the duplicate-id rejection (and the added-token id mismatch, unless it is measured) into the "Hugging Face accepts, this port refuses" list, keeping "negative id" and "broken merge" as the shapes Hugging Face also refuses. If the added-token mismatch claim is kept, measure it against `tokenizers` first.

### LOW: the decode-vector count is recorded in the meta file but no test enforces it
- Severity: LOW
- Confidence: HIGH
- Location: tokenizer/tokenizer_test.go:131 (`parityMeta.DecodeCases`), TestDecode (around line 149)
- Problem: The provenance mechanism built for the encode corpus — assert the fixture's counts against `testdata/tokenizer_parity.meta.json` so a shrunken corpus fails rather than producing a smaller green run — was not applied to the decode corpus. `parityMeta.DecodeCases` is parsed and never read; `TestDecode` only checks `len(cases) != 0`. The decode fixture can silently shrink from 80 cases to 1 and nothing notices, which is the exact failure mode the meta file was introduced to catch. `AGENTS.md`'s claim that "the test fails if the fixture disagrees with the provenance" is therefore only true for the encode fixture.
- Recommendation: In `TestDecode`, read the meta file (or pass it in) and assert `len(cases) == meta.DecodeCases`, and assert the decode fixture's model/revision while at it.

### LOW: the meta file's sha256 is written but never verified
- Severity: LOW
- Confidence: HIGH
- Location: scripts/gen_tokenizer_parity.py:241; tokenizer/tokenizer_test.go (TestParityVectors)
- Problem: The generator records `sha256` of the parity fixture body, and `TestParityVectors` reads the meta file, but nothing in the repository computes or compares the hash. The scope asks whether the meta can be edited to match a shrunken fixture: yes — the string and id counts bind fixture to meta, but a deliberate or accidental coordinated edit of both files passes. The sha256 would close that (it binds the fixture content to one recorded digest) but it is currently dead provenance.
- Recommendation: Have `TestParityVectors` hash `testdata/tokenizer_parity.json` and compare against `meta.sha256`, or drop the field if it is meant only for out-of-band use — say so in the generator docstring either way.

### LOW: the pinned-tokenizer fingerprint is not applied at every load, and the render helper is not the consolidated one
- Severity: LOW
- Confidence: HIGH
- Location: render_test.go:283-298 (`pinnedTokenizer`), tokenizer/tokenizer_test.go:403-412 (`loadPinnedBench`), docs/PORT.md "Tokenizer identity"
- Problem: `checkPinned` (vocab size 262,144 plus `<channel|>` == 101) is applied only by the tokenizer package's `loadPinned`. `render_test.go`'s `pinnedTokenizer` checks the vocab size only, `loadPinnedBench` checks neither, and PORT.md claims "the test checks the loaded tokenizer's vocab size (262,144) and `<channel|>` id (101)", which overstates the coverage. A stale `testdata/tokenizer.json` or a `SHISA_DE_TOKENIZER_JSON` pointing at another revision of the same model with an equal vocab size would pass the render token-count test silently. The round-1 consolidation goal — one place where the identity assertion lives — is only half met: the candidate order and the hard failure on an unusable `SHISA_DE_TOKENIZER_JSON` were consolidated into `LoadPinned`, the fingerprint was not. The render helper also does not honor `SHISA_DE_TOKENIZER_DOWNLOAD`, so under that flag the tokenizer tests download and run while the render token-count test silently skips.
- Recommendation: Export the fingerprint check (for example `CheckPinned(t, tokenizer)` or a fingerprint argument to `LoadPinned`) and call it from `pinnedTokenizer` and `loadPinnedBench`; route the render helper's skip/download/require logic through the same path as `loadPinned`.

### LOW: checkDecoder does not reject a Regex pattern on the Replace stage, unlike its sibling checks
- Severity: LOW
- Confidence: MEDIUM
- Location: tokenizer/tokenizer.go, `checkDecoder` (the `replace.Type != "Replace" || replace.Pattern.String != wordBoundary` comparison)
- Problem: `checkNormalizer` and `checkPreTokenizer` explicitly reject `spec.Pattern.Regex != ""`; `checkDecoder` compares only `replace.Pattern.String` and `replace.Content` and never looks at `replace.Pattern.Regex`. A decoder declaring `{"type":"Replace","pattern":{"String":"▁","Regex":"▁+"},"content":" "}` passes validation and is decoded with the hardcoded literal replacement. Whether Hugging Face would use the String or the Regex for a doubly-declared pattern is unverified, so this is a shape that may be read silently wrong rather than rejected — the exact failure class the strict checks exist to prevent, and the pinned checkpoint is not plausibly this shape, so the practical risk is small.
- Recommendation: Add `replace.Pattern.Regex != ""` to the rejection in `checkDecoder`, mirroring the normalizer and pre-tokenizer checks.

### LOW: Decode does not split a byte-fallback run at an out-of-vocab id, and the rule is untested
- Severity: LOW
- Confidence: LOW
- Location: tokenizer/tokenizer.go, `Decode` (the `!ok` continue path)
- Problem: An id absent from the vocabulary is skipped without flushing the pending byte run, so `[<0xE6>, unknownID, <0x97>]` decodes as one run of two bytes here. Whether Hugging Face's decoder splits the run at an unknown id (or refuses the sequence entirely) is unverified, and the 80 decode vectors do not include an out-of-vocab id. The doc comment states "Ids outside the vocabulary are skipped" but not what that means for run boundaries. This is reachable only from hand-built or model-generated id lists, never from this client's own reads.
- Recommendation: Try to record a decode vector with an out-of-vocab id inside a byte run against `tokenizers`; if it refuses, document that Decode's skip-without-flush is this port's own rule for an input Hugging Face rejects.

### LOW: PORT.md and AGENTS.md list files that do not exist in the tree
- Severity: LOW
- Confidence: HIGH
- Location: docs/PORT.md (file-mapping table), AGENTS.md (Key Files table)
- Problem: Both tables name `client.go`, `policy.go`, `calibration.go`, and `cmd/shisa-de/`, and the mapping row for `tests/test_offline.py` promises `httptest` stubs; none of these exist in the working tree (M1 is the tokenizer stage, so this is presumably the target layout). An agent or contributor following either guide will look for files that are not there, and the guides read as descriptions of the present repo.
- Recommendation: Either mark those entries as planned (M2), or trim the tables to what exists and extend them when the files land.

## Things Checked

- **fuse_unk placement (round-1 HIGH).** Traced `Encode` end to end: `encodeToIDs` appends added-token ids directly and calls `encodePiece` per inter-added-token run; `encodePiece` pre-tokenizes and calls `encodePart` per pre-token; `fuseUnknowns` runs inside `encodePart`, on that pre-token's merged symbols only. Unk can no longer be fused across an added-token boundary, a pre-token boundary, or an `encodePiece` boundary, which matches Hugging Face's model-stage, per-word fuse (the round-1 measurement: two unk pre-tokens give `[0, 0]`, not `[0]`). Byte-fallback ids are never the unk id, so `fuse_unk` leaves them alone, as it should. `TestFuseUnkDoesNotTouchAddedTokens` asserts `[0, 0]` where the pre-fix code produced `[0]`, so it fails against the old behavior. The corpus's `"<unk><unk>"` vector is present in the fixture (grep-confirmed); the other four adjacent-`<unk>` vectors are in the generator source and the scope's validation run.
- **Lookup revision pin (round-1 HIGH).** No glob fallback remains anywhere (grep). The snapshot path resolves exactly `snapshots/<revision>/tokenizer.json` and never falls through; the branch path reads `refs/<revision>` and resolves the hash it contains. `checkSource` runs before any join in `Fetch`, `Resolve`, and `Lookup`. `LoadPinned` fails hard on an explicitly set but unusable `SHISA_DE_TOKENIZER_JSON`, tries the testdata candidates, then the hub cache, and reports the path it used. A stale `testdata/tokenizer.json` can still be picked (it precedes the cache lookup), but `checkPinned` catches a wrong vocab size or `<channel|>` id in the tokenizer tests — the render and bench gaps are the finding above.
- **Pipeline validation.** `checkNormalizer` requires `Replace` with literal `" "` and content `▁`, no Regex, not inverted; `checkPreTokenizer` requires `Split` on `" "` with `MergedWithPrevious`, not inverted; `checkDecoder` requires `Sequence[Replace(▁→" "), ByteFallback, Fuse]` in that order; BPE `dropout` is rejected when non-zero; `continuing_subword_prefix`/`end_of_word_suffix` are rejected. The rejection table's valid reference case loads (I traced each of its stages against the checks). Absent normalizer/decoder are rejected although Hugging Face reads them, which PORT.md documents as deliberate; the non-goal list makes that the right call. The post-processor remains unvalidated but is also never applied, which is faithful to `add_special_tokens=False` for every post-processor shape, so the missing rejection changes nothing for `Encode`; the decision is documented in the package doc.
- **Malformed-file validation.** Negative ids, duplicate ids, merges with a missing operand or concatenation, and added tokens whose declared id disagrees with the vocab are all rejected with `*ErrUnsupported` (the rejection table covers everything but a missing right operand, which is symmetric with the covered left-operand case). The reverse vocabulary is a map, so a sparse id like 2000000000 neither allocates nor makes `Decode` nondeterministic, and `TestFromBytesAcceptsALargeVocabID` pins that. `Decode` skips out-of-vocab ids (the run-boundary question is the finding above).
- **Decode against the 80 vectors.** Verified the fixture's oracle outputs by inspection against the implementation: `[E1 80 C2]` → 3 replacement characters (rules out `from_utf8_lossy`'s 2), `[C2 A9 80 C2 A9]` → 5 replacement characters (confirms a run is all-or-nothing: the embedded `80` invalidates the whole run, including the otherwise-valid `©` pairs), `A` + truncated run → `A` + 2 replacement characters and the reverse (runs split by an ordinary token flush correctly), the surrogate `ED A0 80` → 3, the truncated emoji → 3, the complete emoji and two- and three-byte characters → text, and a run ending at the slice end flushes (final `flush()`). `byteToken`'s six-character shape (`<0x` + two hex digits + `>`, both cases) matches Hugging Face's byte-token pattern; lowercase hex is unreachable for the pinned vocab, which has only uppercase byte tokens. The empty run and empty input are no-ops.
- **byteFallbackIDs.** Requires every byte of the character or returns false, and the caller then emits a single unk — `TestByteFallbackIsAllOrNothing` asserts `[1, 2]` for the complete case and `[0]` for the partial case, where the pre-fix code emitted `[1, 0]`, so it fails against the old behavior. With `ignore_merges` the whole pre-token is looked up before any character split; with `fuse_unk` the byte ids are not the unk id and are never fused. Both interactions match the Hugging Face structure the round-1 review verified.
- **Fetch hardening.** `checkSource` rejects empty, `.`, `..`, and empty components, leading `/`, and any `\`, `?`, `#`, or NUL, for both repo and revision, before any path join or URL build; a revision with slashes (branch names) is allowed and each segment is escaped by `escapePath`. A repo containing `:` or a bare drive letter cannot escape: components are joined under the cache directory, a drive-relative component is not absolute, and a leading separator or backslash is rejected. The body is written to a temp file through a `LimitReader` of cap+1, the cap is checked, the temp file is parsed with `FromFile` before the rename, and the deferred remove cleans up on every failure path. An unparseable cache entry is re-fetched at most once per call and the validated install then parses on every later call, so there is no loop. Concurrent fetches use distinct temp files and an atomic rename. `cacheDir` returns an error rather than falling back to a predictable `/tmp` path.
- **Corpus provenance.** The generator pins `REVISION` and passes it to `from_pretrained`, fails loudly on a missing corpus input (`read_required`), and writes model, revision, library versions, counts, and a sha256 to the meta file. `TestParityVectors` asserts the model and revision and the string and id counts against the meta, so a regenerated fixture from a different snapshot or a silently shrunken encode corpus fails. The decode-count and sha256 gaps are the findings above. The byte-token id base (238) and the codebook id in the decode cases are pinned-vocab constants; the revision pin makes them safe.
- **CI visibility.** `SHISA_DE_TOKENIZER_REQUIRE=1` turns `loadPinned`'s skip into a failure in both the tokenizer package and `render_test.go`; the download fallback under `DownloadEnv` is fingerprint-checked via `checkPinned`. The benchmark skips without the require gate, which is defensible for a benchmark.
- **Provenance comments.** Both test files now state that the codebook's `token_ids` were audited on `google/gemma-4-26B-A4B-it@4d7ae498...`, which matches `data/codebook-de2.json`'s recorded `tokenizer`/`tokenizer_snapshot`, and separate that from the parity vectors' `shisa-ai/shisa-de-2@8f21734b...` pin. Accurate.
- **PORT.md counts.** 1,056 strings, 48,312 ids, and 80 decode cases match the meta file, and I recounted the generator's decode cases to exactly 80 (64 codebook cases, 14 byte-run cases, 2 ordinary-token-split cases). The decode invalid-byte description matches the fixture evidence. The duplicate-id sentence does not (finding above).
- **LoadPinned paths.** The three relative candidates resolve correctly from both package directories (`tokenizer/` and the repo root); `testdata/tokenizer.json` is gitignored, so on a clean machine only the hub-cache path or the env var can supply the file.

## Residual Risks

- I reviewed the working tree without a shell: I could not run `git show 3beed13`, `go test`, or `go vet`, and I did not recount the 1,056-vector and 48,312-id totals myself. The scope's validation record is the evidence those runs pass; the count assertions in `TestParityVectors` are what keep them true.
- The two rounds disagree on whether Hugging Face's byte-token pattern accepts lowercase hex (round 1: uppercase-only; this round's scope: lowercase accepted). No decode vector pins either behavior and the pinned vocab has no lowercase-shaped byte tokens, so the divergence class is hand-built vocabularies only — but the claim in the `byteToken` comment is unverified in this repo.
- The decode oracle is `transformers`' `decode` defaults (`skip_special_tokens=False`, no cleanup). If the Python client ever decodes with cleanup or special-token skipping enabled, this `Decode` has no equivalent path; that is mostly an M2 concern.
- `Lookup` joins the contents of a hub-cache `refs/<revision>` file into a path without re-validating it; the file is written by the local hub tooling, so this is a local trust boundary, and an attacker who can write there can plant a tokenizer.json anyway. `isSnapshot` treats any 7-to-64-character lowercase-hex revision as a hash, so a branch with that shape is looked up under `snapshots/` and missed rather than misread — `Fetch` then handles it, so the failure mode is a download, never a wrong file.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "LOW",
      "title": "PORT.md claims Hugging Face refuses duplicate vocab ids; the round-2 measurement says it accepts them",
      "location": "docs/PORT.md:deliberate-differences, pinned-pipeline paragraph",
      "recommendation": "Move the duplicate-id rejection (and the unmeasured added-token id-mismatch claim) into the accepted-but-refused list; keep negative id and broken merge as the shapes Hugging Face also refuses."
    },
    {
      "severity": "LOW",
      "title": "The decode-vector count in the meta file is parsed but never enforced",
      "location": "tokenizer/tokenizer_test.go:131",
      "recommendation": "Assert len(cases) == meta.DecodeCases in TestDecode so a shrunken decode corpus fails."
    },
    {
      "severity": "LOW",
      "title": "The meta file's sha256 is written but nothing verifies it",
      "location": "scripts/gen_tokenizer_parity.py:241",
      "recommendation": "Hash the fixture in TestParityVectors and compare against meta.sha256, or document the field as out-of-band provenance only."
    },
    {
      "severity": "LOW",
      "title": "The pinned-tokenizer fingerprint is not applied at every load; render and bench helpers are weaker than loadPinned",
      "location": "render_test.go:283",
      "recommendation": "Export the fingerprint check and call it from pinnedTokenizer and loadPinnedBench; route the render helper through the same require/download logic."
    },
    {
      "severity": "LOW",
      "title": "checkDecoder does not reject a Regex pattern on the Replace stage, unlike the normalizer and pre-tokenizer checks",
      "location": "tokenizer/tokenizer.go:checkDecoder",
      "recommendation": "Add replace.Pattern.Regex != \"\" to the rejection so a doubly-declared pattern cannot slip through."
    },
    {
      "severity": "LOW",
      "title": "Decode does not split a byte-fallback run at an out-of-vocab id; the rule is untested and unverified against Hugging Face",
      "location": "tokenizer/tokenizer.go:Decode",
      "recommendation": "Record a decode vector with an out-of-vocab id inside a byte run, or document the skip-without-flush rule as this port's own for inputs Hugging Face refuses."
    },
    {
      "severity": "LOW",
      "title": "PORT.md and AGENTS.md list client.go, policy.go, calibration.go and cmd/shisa-de, which do not exist yet",
      "location": "docs/PORT.md:file-mapping, AGENTS.md:key-files",
      "recommendation": "Mark the planned files as M2 or trim the tables to what exists."
    }
  ]
}
END_REVIEW_JSON
