# Review Scope: m1-tokenizer

- Mode: implementation
- Stage: M1
- Target: Commit 3beed13 in /home/morpheus/data/dev/shisa/shisa-de-go: the tokenizer fixes, against the round-1 report at docs/reviews/m1-tokenizer/m1/round-1/

## Summary

Round 2 of the M1 tokenizer review for the Go port of `shisa-de`. Round 1 produced three reports (kept at docs/reviews/m1-tokenizer/m1/round-1/). This round reviews the fixes in commit 3beed13, which follows the reviewed commit 91e1ffe.

The reviewers agreed the heap merge, the stale-candidate guard and the leftmost tie-break are correct, and that the added-token longest match is right. Two HIGH/CRITICAL-class findings came out of round 1, plus a set of MEDIUM and LOW ones. All were addressed. What to verify this round:

1. `fuse_unk` placement (was: HIGH). It ran over the assembled output, so `Encode("<unk><unk>")` returned `[3]` where `transformers` returns `[3, 3]`. It now runs inside `encodePart`, per pre-token, so added tokens are never fused. Verified before the fix that Hugging Face applies it per pre-token (`Split "," Removed` with `fuse_unk` gives `[0, 0]` for two unk pre-tokens, not `[0]`). Five `<unk>` vectors were added to the corpus and the parity run covers them. Check the placement is right and that no other path can fuse across added tokens or pre-tokens.

2. `Lookup` revision substitution (was: HIGH). It fell back to `filepath.Glob(.../snapshots/*/tokenizer.json)` and returned `matches[0]`, so an explicit snapshot pin could be satisfied by a different revision. The glob fallback is removed; `Lookup` resolves a snapshot hash only under `snapshots/<hash>`, and anything else only through `refs/<revision>`. `LoadPinned` reports the path it used and the tests fingerprint the loaded tokenizer (`Len() == 262144`, `<channel|> == 101`). Check the fallback is really gone and that nothing else can return an unpinned revision.

3. Pipeline stages were validated by type and then hardcoded (was: MEDIUM). `checkNormalizer`, `checkPreTokenizer` and the new `checkDecoder` now compare the parsed values against the pinned ones (normalizer `Replace " " -> U+2581`, not inverted; pre-tokenizer `Split " "` `MergedWithPrevious`, not inverted; decoder `Sequence[Replace(U+2581 -> " "), ByteFallback, Fuse]`) and return `ErrUnsupported` otherwise. BPE `dropout` is rejected when non-zero. Check for a shape that still slips through, and check the rejections do not refuse the pinned file.

4. Malformed files (was: MEDIUM). A merge whose operands or concatenation are missing was silently dropped; Hugging Face refuses it (`Token \`ab\` out of vocabulary`). It is now an error. Negative ids are rejected (Hugging Face rejects them as u32), duplicate ids are rejected (Hugging Face accepts them but Go's map made the winner depend on iteration order), and an added token whose declared id disagrees with the vocab is rejected. The reverse vocabulary changed from a slice sized by the largest id to a map, so a sparse id like 2000000000 no longer allocates. Check the validation is complete and that `Decode` still skips unknown ids.

5. `Decode` byte-fallback runs (was: MEDIUM). It concatenated the bytes. Measured against `transformers` 5.19.0 / `tokenizers` 0.23.3: a run of consecutive byte tokens decodes to the run's text when the bytes are valid UTF-8, and otherwise to one U+FFFD per byte. That is neither a plain concatenation nor `from_utf8_lossy`, which consumes maximal invalid subsequences (`E1 80 C2` gives 3 replacement characters here and 2 under `from_utf8_lossy`). 80 decode vectors record it. Check the implementation matches on runs split by ordinary tokens, and check the `utf8.Valid` all-or-nothing rule is what the measurements show.

6. Partial byte fallback (was: MEDIUM). It emitted the byte tokens that existed and unk for the rest. Measured: Hugging Face falls back for the whole character or not at all (a character `é` = `C3 A9` with only `<0xC3>` present encodes to `[unk]`, not `[<0xC3>, unk]`). `byteFallbackIDs` now requires every byte. Check the rule.

7. `Fetch` hardening (was: MEDIUM). It installed any body with status < 400, treated any non-empty cache file as good, had no size cap, and joined unvalidated repo/revision strings into paths. Now: the body is parsed with `FromFile` before `os.Rename`, capped at 256 MiB, an unparseable cache entry is treated as absent and re-fetched, `checkSource` rejects `..`/leading `/`/`\`/`?`/`#`/NUL components, URL path segments are escaped, and `cacheDir` fails rather than falling back to `/tmp`. Check for a remaining hole and check the cache-hit re-fetch cannot loop.

8. Corpus provenance (was: MEDIUM/LOW). `scripts/gen_tokenizer_parity.py` called `from_pretrained(MODEL)` with no revision, read three corpus inputs from a hardcoded machine path behind `if path.exists()`, and wrote no provenance. It now pins `REVISION`, fails loudly on a missing corpus input (`SHISA_DE_PY_REPO` overrides the checkout), and writes `testdata/tokenizer_parity.meta.json` with model, revision, `transformers`/`tokenizers` versions, string count, id count and a sha256. `TestParityVectors` asserts the counts against the meta file. Check the test would actually catch a shrunken corpus and that the meta cannot silently disagree with the fixture.

9. CI visibility (was: LOW). A missing tokenizer.json skipped. `SHISA_DE_TOKENIZER_REQUIRE=1` now makes it a failure. Verified: with `HOME=/tmp/nohome` the default run skips and `REQUIRE=1` fails; a bogus `SHISA_DE_TOKENIZER_JSON` fails loudly instead of falling through.

10. Provenance comments (was: LOW). Two test files said the pin came from `data/codebook-de2.json`, which actually records `google/gemma-4-26B-A4B-it@4d7ae498...` for the codebook's `token_ids`. The comments now state each pin's real source. `loadPinned`/`pinnedTokenizer`/`loadPinnedBench` were three copies that had drifted; they now call one exported `LoadPinned`.

## Primary files / regions

- `tokenizer/tokenizer.go` — lines/regions: struct/decoder/model specs, FromBytes validation, checkNormalizer/checkPreTokenizer/checkDecoder, Decode, byteToken, encodeToIDs, encodePart, byteFallbackIDs, newAddedVocab — reason: The fuse_unk placement, the strict pipeline checks, id and merge validation, and the decoder
- `tokenizer/hub.go` — lines/regions: LoadPinned, ErrNoPinnedTokenizer, env constants, Fetch, checkSource, escapePath, cacheDir, Lookup — reason: The revision pin, the download validation and the path checks
- `tokenizer/tokenizer_test.go` — lines/regions: loadPinned/checkPinned, parityMeta, TestParityVectors, TestDecode, the rejection table, TestFromBytesAcceptsALargeVocabID, TestFuseUnkDoesNotTouchAddedTokens, TestByteFallbackIsAllOrNothing, loadPinnedBench — reason: The gate, the fingerprint and the new unit tests
- `render_test.go` — lines/regions: pinnedTokenizer — reason: The shared helper and the fingerprint
- `scripts/gen_tokenizer_parity.py` — lines/regions: whole file — reason: The pinned revision, the required inputs and the provenance file
- `testdata/tokenizer_decode_parity.json, testdata/tokenizer_parity.meta.json, testdata/tokenizer_parity.json` — lines/regions: new: decode vectors and provenance — reason: The regenerated fixtures
- `docs/PORT.md` — lines/regions: deliberate differences, verified parity — reason: Claims about strictness and the decoder that this review should check

## Review questions

- Is the fuse_unk placement now exactly right? Trace Encode for text that mixes added tokens, byte-fallback output and model output. Can unk still be fused across an added-token boundary, across a pre-token boundary, or across the boundary between two encodePiece calls? Is there any remaining input where this package and transformers disagree on fuse_unk?
- Is `Lookup` now incapable of returning a revision other than the one asked for? Check both the snapshot-hash path and the refs path, the multi-root loop, and `LoadPinned`'s candidate order. Can a stale `testdata/tokenizer.json` or a `SHISA_DE_TOKENIZER_JSON` pointing at another revision still be used as the pinned tokenizer, and would the fingerprint catch it?
- Is the pipeline validation complete? Name a tokenizer.json that Hugging Face reads and this package still reads wrong rather than rejecting, or one that this package now rejects but Hugging Face reads and that the pinned checkpoint could plausibly be. In particular, is rejecting an absent normalizer or decoder too strict, and is `post_processor` still unvalidated in a way that matters given `Encode` never applies it?
- Is the malformed-file validation right? Check that rejecting a merge with missing operands or a concatenation matches Hugging Face, that rejecting duplicate and negative ids is a defensible difference, and that the map-based reverse lookup has no remaining way to be nondeterministic or to panic. Does anything still accept an id it should not?
- Does `Decode` match `tokenizers` 0.23.3 for every byte-fallback sequence? The implementation buffers consecutive byte tokens and, at the end of the run, emits the run's text if `utf8.Valid` and one U+FFFD per byte otherwise. Check it against runs split by an ordinary token, a run that ends at the end of the slice, an empty run, a `<0xXX>`-shaped token that is not a byte token (lowercase hex is accepted by Hugging Face's parser), and ids that are absent from the vocab. Is `byteToken`'s six-character test exactly Hugging Face's?
- Does `byteFallbackIDs` match Hugging Face for a character whose byte tokens are partially present, and does it interact correctly with `ignore_merges` and with `fuse_unk`?
- Is `Fetch` safe now? Check the order of parse-validate and rename, the size cap, the concurrent-fetch case, and whether treating an unparseable cache entry as absent can loop or re-download forever. Check `checkSource` and `escapePath` for a repository or revision that still escapes the cache directory or changes the request, including a revision with slashes and a repo with a `:` or a Windows drive letter.
- Does `TestParityVectors` actually constrain the corpus? It now asserts the string and id counts against `testdata/tokenizer_parity.meta.json` and the model and revision. Can a regenerated fixture with fewer or different vectors still pass? Can the meta file be edited to match a shrunken fixture? Is the sha256 in the meta used for anything?
- Do the new unit tests test the implementation rather than restate it? In particular, do `TestFuseUnkDoesNotTouchAddedTokens` and `TestByteFallbackIsAllOrNothing` use fixtures that would fail against the pre-fix code, and does the rejection table's `valid` reference case actually load?
- Is the `LoadPinned` consolidation correct? Check the candidate order, the hard failure on an explicitly set but unreadable `SHISA_DE_TOKENIZER_JSON`, the relative testdata paths under both package directories, and whether the fingerprint is applied everywhere the pinned tokenizer is loaded.
- Are the new claims in docs/PORT.md and AGENTS.md accurate? Check the stated counts (1,056 strings, 48,312 ids, 80 decode cases), the claim that Hugging Face accepts and this port rejects the listed shapes, and the description of `Decode`'s invalid-byte rule.

## Known non-goals

- Truncation, padding, offsets, post-processing and training are still not implemented and are not expected here.
- The pinned checkpoint is still the only pipeline this package supports; generalizing to other normalizers, pre-tokenizers or decoders is out of scope, which is why the strict rejections were chosen over implementing them.
- No cgo and no dependency on Hugging Face's Rust library.
- The post-processor is parsed but never applied, matching `add_special_tokens=False`; whether that deserves a validation error rather than a documented decision is in scope, but implementing template processing is not.
- DE-1 is not implemented anywhere in this port.
- The M0 codebook and the M2 renderer, family resolution and softmax are separate review topics.
- This review is about fidelity to Hugging Face and the rejection contract, not about whether the pinned snapshot is the right one to pin.

## Validation already run

- gofmt -l . (clean), go build ./... , go vet ./... (clean)
- go test ./... (pass)
- go test ./tokenizer/ -v (parity: 1056 strings, 48312 ids; decode: 80 cases)
- SHISA_DE_TOKENIZER_REQUIRE=1 go test ./... (pass, does not skip)
- HOME=/tmp/nohome go test ./tokenizer/ -run TestParityVectors (SKIP); with SHISA_DE_TOKENIZER_REQUIRE=1 (FAIL, as designed)
- SHISA_DE_TOKENIZER_JSON=/nonexistent.json go test ./tokenizer/ -run TestParityVectors (FAIL, as designed)
- transformers 5.19.0 / tokenizers 0.23.3 against shisa-ai/shisa-de-2@8f21734b27b3462471c4662d5638e52195b005a3: encode('<unk><unk>') = [3, 3]; decode byte runs as described; partial byte fallback = [unk]; HF rejects a merge with a missing operand and a negative id; HF accepts a duplicate id, a sparse id, no normalizer, a wrong Split behavior and a Strip decoder
