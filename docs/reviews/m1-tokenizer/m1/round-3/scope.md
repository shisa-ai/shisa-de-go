# Review Scope: m1-tokenizer

- Mode: implementation
- Stage: M1
- Target: Commits 3beed13 and 3ff59e3 in /home/morpheus/data/dev/shisa/shisa-de-go: the tokenizer and its fixes, against round 1 (docs/reviews/m1-tokenizer/m1/round-1/) and the ds4/glm round-2 reports

## Summary

Round 3 of the M1 tokenizer review for the Go port of `shisa-de`, run because the astra reviewer's round-2 report was a 71-byte server-overload error (`[Error: Our servers are currently overloaded...]`), so astra never reviewed the round-2 state. ds4 and glm did review it in round 2 and reported zero HIGH/CRITICAL findings.

This round covers commits 3beed13 (round-1 fixes) and 3ff59e3 (round-2 fixes). The round-2 reports from ds4 and glm are at docs/reviews/m1-tokenizer/m1/round-2/reports/, and their findings are all addressed in 3ff59e3:

- `TestFuseUnkDoesNotTouchAddedTokens` built its model from `okModel`, which did not set `fuse_unk`, so `fuseUnknowns` was never called and the test passed against the pre-fix code. The fixture now sets `fuse_unk` and the test covers a fused model unk next to an unfused added token.
- The parity meta's `sha256` and `decode_cases` were never read. `TestParityVectors` now verifies the strings fixture's digest; `TestDecode` verifies the decode fixture's digest and count; the generator writes `decode_sha256` too.
- The pinned fingerprint was duplicated and weaker outside the tokenizer package (the render helper checked only vocab size, the benchmark checked nothing). It is now `tokenizer.VerifyPinned`, with `PinnedRepo`/`PinnedSnapshot`/`PinnedVocabSize`/`PinnedCloseID` in hub.go, called from `checkPinned`, `pinnedTokenizer` and `loadPinnedBench`. The render helper also honours `SHISA_DE_TOKENIZER_DOWNLOAD`.
- `checkDecoder` did not reject a `Regex` declared alongside the literal pattern; it does now.
- A file with neither a resolvable `unk_token` nor a complete byte vocabulary would silently drop an unknown character; `FromBytes` rejects it.
- The rejection table's "empty vocab" case omitted the normalizer and so never reached the empty-vocab check; it now supplies the pinned pipeline, and two cases cover the new unk/byte-fallback rule.
- `Lookup` joined a hub-cache refs file's content straight into a path; it must now look like a snapshot hash.
- Measured: Hugging Face skips an id outside the vocabulary without ending a byte-fallback run (`[<0xC2>, unknown, <0xA9>]` decodes to one character), which `Decode` already did; three decode vectors record it.
- `docs/PORT.md` said Hugging Face also refuses a duplicate vocab id (it accepts one; the port refuses it to keep the reverse lookup deterministic). The paragraph now separates shared refusals from deliberate ones, and both it and AGENTS.md mark the not-yet-ported files as planned.

The two round-1 HIGH findings, both verified fixed in round 2, were: `fuse_unk` ran over the assembled output and collapsed explicit added `<unk>` tokens (`Encode("<unk><unk>")` returned `[3]` where `transformers` returns `[3, 3]`), and `Lookup` fell back to any cached snapshot so an explicit pin could be satisfied by a different revision.

## Primary files / regions

- `tokenizer/tokenizer.go` — lines/regions: FromBytes validation, checkNormalizer/checkPreTokenizer/checkDecoder, Decode, encodeToIDs, encodePart, byteFallbackIDs, newAddedVocab — reason: The fuse_unk placement, the strict pipeline checks, id/merge/unk validation and the decoder
- `tokenizer/hub.go` — lines/regions: LoadPinned, VerifyPinned, Pinned* constants, Fetch, checkSource, escapePath, cacheDir, Lookup — reason: The revision pin, the download validation, the path checks and the shared fingerprint
- `tokenizer/tokenizer_test.go` — lines/regions: loadPinned/checkPinned/loadParityMeta, TestParityVectors, TestDecode, the rejection table, TestFuseUnkDoesNotTouchAddedTokens, TestByteFallbackIsAllOrNothing, loadPinnedBench — reason: The gate, the digests, the fingerprint and the new unit tests
- `render_test.go` — lines/regions: pinnedTokenizer — reason: The shared helper, fingerprint and download flag
- `scripts/gen_tokenizer_parity.py` — lines/regions: whole file — reason: The pinned revision, required inputs, OOV decode cases and the provenance digests
- `testdata/tokenizer_parity.json, testdata/tokenizer_decode_parity.json, testdata/tokenizer_parity.meta.json` — lines/regions: regenerated — reason: The fixtures and their provenance
- `docs/PORT.md` — lines/regions: deliberate differences, verified parity, file mapping — reason: Claims about strictness, the decoder, the counts and the target layout
- `AGENTS.md` — lines/regions: Key Files, verification — reason: Planned-file marking and the parity commands

## Review questions

- Is the fuse_unk placement now right, and does the regression test actually fail against the pre-fix code? The fixture sets fuse_unk with no byte fallback and an added <unk>, and asserts Encode("xy") == [0], Encode("<unk><unk>") == [0, 0] and Encode("x<unk>") == [0, 0]. Trace Encode and say whether any path can still fuse across an added-token, pre-token or encodePiece boundary, and whether the parity corpus's <unk> vectors cover the same ground when the 32 MB file is present.
- Is the tokenizer still faithful to Hugging Face on the pinned checkpoint, and is every rejection now either measured or documented as deliberate? Check the list in docs/PORT.md against what the code does and against the measurements in the validation record. Name any shape that is still read silently wrong, and any shape this port now refuses that the pinned checkpoint or a plausible re-pin would need.
- Does Decode match tokenizers 0.23.3 for every byte-fallback sequence, including a run split by an id outside the vocabulary, a run ending at the end of the slice, and an empty run? The rule is: buffer consecutive byte tokens; at the end of the run emit the run's text if utf8.Valid, otherwise one U+FFFD per byte; an id absent from the vocab is skipped without ending the run. Is the last part right, and is the negative-id case (Hugging Face raises, this port skips) documented?
- Does the provenance mechanism actually bind the fixtures? TestParityVectors now checks the strings fixture's sha256, string count and id count against testdata/tokenizer_parity.meta.json, and TestDecode checks the decode fixture's sha256 and case count. Can a fixture and its meta still be edited together to pass while the recorded oracle is wrong? Is anything else in the meta unverified?
- Is the pinned-tokenizer identity check now in one place and applied everywhere? tokenizer.VerifyPinned checks the vocab size (262,144) and <channel|> (101); checkPinned, pinnedTokenizer and loadPinnedBench call it. Is there a remaining load path that skips it, and is the check strong enough to catch a different revision of the same model?
- Is the new FromBytes rule (reject a file with neither a resolvable unk_token nor a complete byte vocabulary) correct and not too strict? Does it refuse anything the pinned checkpoint or a plausible re-pin needs, and does it actually close the silent-drop path in encodePart?
- Is Lookup now incapable of returning a revision other than the one asked for? Check the snapshot path, the refs path (whose content must now look like a snapshot hash), the multi-root loop and LoadPinned's candidate order. Can a stale testdata/tokenizer.json or a SHISA_DE_TOKENIZER_JSON naming another revision still be used, and would VerifyPinned catch it?
- Do the new and changed tests test the implementation rather than restate it? Check the fuse_unk cases, the two new unk/byte-fallback rejection cases, the corrected empty-vocab case, and TestFromBytesAcceptsALargeVocabID. Does any of them pass against the pre-fix code?
- Are the claims in docs/PORT.md and AGENTS.md accurate now? Check the counts (1,056 strings, 48,392 ids, 83 decode cases), the split between shapes Hugging Face refuses and shapes this port refuses deliberately, the planned-file marking, and the description of Decode's invalid-byte rule.

## Known non-goals

- Truncation, padding, offsets, post-processing and training are not implemented and are not expected here.
- Only the pinned pipeline is supported; generalizing to other normalizers, pre-tokenizers or decoders is out of scope, which is why strict rejection was chosen.
- No cgo and no dependency on Hugging Face's Rust library.
- The post-processor is parsed but never applied, matching add_special_tokens=False.
- DE-1 is not implemented anywhere in this port.
- The M0 codebook and the M2 renderer, family resolution and softmax are separate review topics.
- The corpus includes slices of this repository's own README.md, AGENTS.md and docs/READOUT-DE2.md, so regenerating it after a doc edit changes recorded ids; that coupling is known and not part of this round.

## Validation already run

- gofmt -l . (clean), go build ./..., go vet ./... (clean)
- go test ./... (pass); go test -count=1 ./... (pass)
- SHISA_DE_TOKENIZER_REQUIRE=1 go test -count=1 ./... (pass, does not skip)
- SHISA_DE_TOKENIZER_JSON=/nope.json go test -count=1 ./... (FAIL, as designed)
- parity: 1,056 strings, 48,392 ids, sha256 bc45a8cf...; decode: 83 cases, sha256 afbb0509...
- transformers 5.19.0 / tokenizers 0.23.3 against shisa-ai/shisa-de-2@8f21734b...: encode('<unk><unk>') = [3, 3]; decode([<0xC2>, 999999999, <0xA9>]) = '©' (the run is not split by an out-of-vocab id); decode byte runs are all-or-nothing with one U+FFFD per byte when invalid; a character with a missing byte token becomes unk; HF accepts a duplicate vocab id but rejects a negative id and a merge with a missing operand
