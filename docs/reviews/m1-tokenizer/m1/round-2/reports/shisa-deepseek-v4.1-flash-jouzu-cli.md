# Review Report

## Summary

The two round-1 HIGH/CRITICAL findings are genuinely fixed, and I could not find a way to break either fix.

`fuse_unk` now runs inside `encodePart`, on one pre-token's model output, and added tokens are appended by `encodeToIDs` before/after that call without ever entering it, so `Encode("<unk><unk>")` returns `[3, 3]`. I verified against the pinned `tokenizer.json` in the local hub cache (`snapshots/8f21734b...`) that the regenerated corpus now holds Hugging Face's own answers for exactly this: `"<unk><unk>"` → `[3, 3]`, `"<unk><unk><unk>"` → `[3, 3, 3]`, `"<unk><|turn><unk>"` → `[3, 105, 3]`. The pre-fix code would have failed the parity run on those vectors, so the gate does cover the fix.

`Lookup`'s any-snapshot glob is gone: a snapshot hash resolves only under `snapshots/<hash>`, anything else only through `refs/<revision>`, across all cache roots, and a miss returns false so `Resolve` downloads instead of substituting. I read the pinned file's own stage declarations and confirmed every new check accepts it (normalizer `Replace " "→"▁"`, pre-tokenizer `Split " " MergedWithPrevious invert:false`, decoder `Sequence[Replace("▁"→" "), ByteFallback, Fuse]`, `dropout:null`, array-form merges, 24 added tokens whose declared ids equal their vocab ids).

The remaining findings are one regression test that cannot fail, one doc claim that contradicts the round's own measurement, and four small hardening/test-integrity gaps. No CRITICAL or HIGH.

Note on method: no shell tool is configured, so this is a read-only inspection of the working tree; I could not confirm the tree is exactly commit `3beed13`, and I did not run the suite.

## Findings

### MEDIUM: `TestFuseUnkDoesNotTouchAddedTokens` cannot fail — its fixture never enables `fuse_unk`
- Severity: MEDIUM
- Confidence: HIGH
- Location: tokenizer/tokenizer_test.go:313 (fixture), tokenizer/tokenizer_test.go:254 (`okModel`), tokenizer/tokenizer.go:499-501 (`encodePart`)
- Problem: the test builds its model from `okModel`, which is `{"type":"BPE","unk_token":"<unk>",...}` with no `fuse_unk`, so `t.fuseUnk` is false and `fuseUnknowns` is never called. The assertion `Encode("<unk><unk>") == [0, 0]` therefore holds whatever the fusion placement is: it passes against the pre-fix code (which was gated on `fuse_unk`), and it would pass against a version that fused nothing at all. It is the stated regression for the round-1 HIGH finding and it guards nothing. This matters because when the 32 MB `tokenizer.json` is absent and `SHISA_DE_TOKENIZER_REQUIRE` is unset — the default on a developer machine and in the CI the repo's own AGENTS.md describes — the parity run skips, and this unit test is the only coverage of the placement. (Mitigation: when the file *is* present, the parity corpus does cover it, via the `[3, 3]` vector.)
- Recommendation: set `"fuse_unk":true` in the fixture's model (a one-word change to the test body's `okModel` copy) so the fusion path is live, and add the complementary case: a fixture whose model can produce a genuine unk (byte vocabulary incomplete, `unk_token` present, `fuse_unk:true`) encoding text with two adjacent unknown characters, asserted to fuse to one unk while an adjacent added `<unk>` stays separate.

### MEDIUM: `docs/PORT.md` puts a duplicate vocab id on the wrong side of the rejection contract
- Severity: MEDIUM
- Confidence: HIGH
- Location: docs/PORT.md:54-59
- Problem: the new paragraph says "The same applies to a malformed file Hugging Face also refuses: a negative or duplicate vocab id, an added token whose declared id disagrees with the vocab, or a merge whose operands or concatenation are missing." The round's own measurement record says otherwise for two of the three: "HF accepts a duplicate id, a sparse id, no normalizer, a wrong Split behavior and a Strip decoder", and the scope's item 4 says the duplicate id is rejected precisely *because* Hugging Face accepts it and Go's map made the winner iteration-dependent. The added-token id mismatch is likewise presented as something Hugging Face refuses, with no measurement behind it (the record only establishes that this port rejects it). A reader auditing the port's fidelity from this page gets the opposite of the measured behaviour for the duplicate id, in the one paragraph that explains why each rejection exists — and this repository treats such claims as load-bearing ("any number added to a doc was measured").
- Recommendation: split the sentence into the shapes Hugging Face also refuses (negative id, merge with a missing operand or concatenation) and the shapes this port refuses deliberately where Hugging Face reads the file (duplicate id, added token whose declared id disagrees with the vocab, plus the pipeline-shape rejections already listed above it), citing the measurement for each.

### LOW: `Lookup` joins the unvalidated content of a `refs` file into a path
- Severity: LOW
- Confidence: HIGH
- Location: tokenizer/hub.go:288-291
- Problem: the refs branch reads `refs/<revision>` (correctly validated) and then joins its *content* straight into a path: `filepath.Join(base, "snapshots", strings.TrimSpace(string(target)), "tokenizer.json")`. `revision` is checked by `checkSource`, but the hash read from the file is not, so a refs file containing `../../../..` resolves outside the cache root and `exists`/`FromFile` will load whatever `tokenizer.json` is found there. It is only reachable by something that can already write inside the Hugging Face cache (so it is hardening, not a hole in the trust boundary), but it is the one path join in the new hub code that is not validated, and the round-2 fix was specifically about that class.
- Recommendation: require the refs content to match the shape of a revision (`isSnapshot` after trimming, i.e. 7-64 lowercase hex characters) before joining; return false otherwise so the caller downloads the requested revision instead.

### LOW: The provenance file's `sha256` and `decode_cases` are never checked, so the decode corpus can shrink silently
- Severity: LOW
- Confidence: HIGH
- Location: tokenizer/tokenizer_test.go:97-118 (`TestParityVectors` meta checks), tokenizer/tokenizer_test.go:182-206 (`TestDecode`), scripts/gen_tokenizer_parity.py:243-257 (meta)
- Problem: `tokenizer_parity.meta.json` records `sha256` and `decode_cases`, and neither is read: `parityMeta` (tokenizer_test.go:74-81) has no `Sha256` field and no `DecodeCases` assertion, and `TestDecode` only fails on an empty fixture. So the round-2 mechanism ("a corpus that silently shrank is a failure rather than a smaller green run") covers the strings fixture only. Deleting 79 of the 80 decode vectors — including every byte-fallback case, which is the only constraint on the new invalid-byte rule and which `Encode` cannot reproduce — leaves the suite green. The AGENTS.md claim "the test fails if the fixture disagrees with the provenance" is therefore only half true, and the sha256 gives a false impression of a content commitment.
- Recommendation: add `DecodeCases int \`json:"decode_cases"\`` to `parityMeta` and assert `len(cases) == meta.DecodeCases` in `TestDecode`; also hash the fixture bytes in the test and compare against `meta.sha256` (and consider a digest for the decode fixture), or drop the field from the meta so it does not look like a check that exists.

### LOW: The pinned-tokenizer fingerprint is duplicated and weaker outside the tokenizer package
- Severity: LOW
- Confidence: HIGH
- Location: tokenizer/tokenizer_test.go:57-68 (`checkPinned`), tokenizer/tokenizer_test.go:403-411 (`loadPinnedBench`), render_test.go:292-296 (`pinnedTokenizer`)
- Problem: the round-1 consolidation removed the three copies of the *resolution* order, but the identity assertion is still written per caller and has drifted: `checkPinned` checks `Len() == 262144` and `<channel|> == 101`; `pinnedTokenizer` checks only the vocab size, so the parent package's `TestRenderTokenCounts` would accept a same-size tokenizer (a different snapshot of the same family is exactly the case the id check exists for); `loadPinnedBench` checks nothing at all and skips unconditionally without honouring `RequireEnv`. `LoadPinned` knows the repo and revision it was asked for, so this is the one place the check can live without duplication.
- Recommendation: move the fingerprint into `LoadPinned` (or export `VerifyPinned(*Tokenizer) error` with the pinned constants) and call it from all three sites; have `loadPinnedBench` honour `RequireEnv` for consistency with `loadPinned`.

### LOW: Unknown characters are silently dropped when neither a complete byte vocabulary nor a resolved unk token exists
- Severity: LOW
- Confidence: MEDIUM
- Location: tokenizer/tokenizer.go:483-492
- Problem: `encodePart` appends nothing for a character that is not in the vocab when `byteFallback` is false and `hasUnk` is false, and also when `byteFallback` is true but one of the character's byte tokens is missing and `hasUnk` is false. Such a file loads successfully (nothing rejects it: `hasUnk` is only set when `unk_token` is present *and* in the vocab, and `byteFallback` is accepted either way), so `Encode` returns fewer ids than the text implies with no error — the "rejected rather than approximated" contract failing quietly. The pinned file cannot reach it (all 256 byte tokens are present, ids 238-493, and `<unk>` is id 3), which is why it has no test.
- Recommendation: reject at load time a file where a character could produce nothing, i.e. require `byte_fallback` with a complete byte vocabulary or a resolvable `unk_token`; alternatively return an error from `encodePart` for that case. A one-line check in `FromBytes` is enough for the pinned contract.

### LOW: The rejection table's "empty vocab" case never reaches the empty-vocab check
- Severity: LOW
- Confidence: HIGH
- Location: tokenizer/tokenizer_test.go:263, tokenizer/tokenizer.go:161-163
- Problem: the case body is `{"model": {"type": "BPE", "vocab": {}}}` with no normalizer, so `checkNormalizer` fires first (`Type == ""`) and the test passes on an `ErrUnsupported` that has nothing to do with an empty vocab. The check at tokenizer.go:161-163 is therefore untested, and the case name misleads a reader into thinking it is covered. The other bodies that omit stages are fine because the stage they are named for is checked before the missing one, except "regex normalizer"/"unknown normalizer", which also stop at the normalizer — correctly, since that is what they test.
- Recommendation: build the "empty vocab" body from the pinned `okNormalizer`, `okPreTokenizer` and `okDecoder` with `"model":{"type":"BPE","vocab":{}}`, so the case exercises the check it names.

## Things Checked

- **`fuse_unk` placement, traced end to end.** `Encode` → `encodeToIDs` (added-token longest match appends `tok.ID` directly; the surrounding runs go to `encodePiece`) → `encodePiece` (normalize, then `preTokenize`) → `encodePart`, and `fuseUnknowns` is called only at tokenizer.go:499-501, on that part's post-merge symbols. Nothing else calls it (grep: one call site). So unk cannot be fused across an added-token boundary, across a pre-token boundary, or across two `encodePiece` calls. Verified in the fixture (by probing literal substrings, since the file is one 496 KB line): `"<unk><unk>"` → `[3, 3]`, `"<unk><unk><unk>"` → `[3, 3, 3]`, `"<unk><|turn><unk>"` → `[3, 105, 3]`. With `byte_fallback:true` and all 256 byte tokens present, the pinned model can never emit unk, so the only unk source is the added token — which is exactly the class that used to be fused.
- **`Lookup` revision substitution.** The glob fallback is absent from the file. `isSnapshot("8f21734b...")` is true, so the pin resolves only under `snapshots/<hash>/tokenizer.json`; the refs branch is only taken for a non-hash revision and uses `refs/<revision>` → the hash it contains. Roots are tried in order (`HF_HUB_CACHE`, `HF_HOME/hub`, `~/.cache/huggingface/hub`) and a miss in one root falls through to the next, with no cross-root substitution. `Resolve` checks the source shape, then `Lookup`, then `Fetch`; `Load` on an explicit *file path* bypasses the pin by design. A stale `testdata/tokenizer.json` or a `SHISA_DE_TOKENIZER_JSON` naming another revision still wins over the hub cache — that is the documented order and the reason `checkPinned` exists — and the fingerprint catches a different vocab size or `<channel|>` id; a different revision of the same family would have to be caught by the id comparison in `TestParityVectors` (which it would be, loudly). `isSnapshot` also classifies an all-hex branch name of 7-64 characters as a hash, so such a branch is never resolved through `refs/` and is re-downloaded instead of reused; the result is correct, only less cache reuse.
- **Pipeline validation against the real pinned file.** Read from `~/.cache/huggingface/hub/models--shisa-ai--shisa-de-2/snapshots/8f21734b.../tokenizer.json`: `normalizer` `Replace {"String":" "}` → `"▁"` (no `invert`); `pre_tokenizer` `Split {"String":" "}` `MergedWithPrevious` `invert:false`; `post_processor` `TemplateProcessing`, `single:[Sequence A]`, `special_tokens: {}`; `decoder` `Sequence[Replace({"String":"▁"} → " "), ByteFallback, Fuse]`; `model` `BPE`, `dropout:null`, `unk_token:"<unk>"`, `fuse_unk:true`, `byte_fallback:true`, `ignore_merges:false`, `continuing_subword_prefix:null`, `end_of_word_suffix:null`, 24 added tokens (ids 0,1,2,3,4,46,47,48,49,50,51,52,98,100,101,105,106,255999,256000,258880-258884) each matching the vocab, merges as two-element arrays starting at line 262437, `<0x41>` = 303 so byte tokens occupy 238-493. Every new check accepts this file and rejects the shapes in the rejection table, so the strictness does not refuse the pin.
- **Rejection table.** Walked all 19 cases: each errors, and each error is `*ErrUnsupported` (the parse-error paths for a non-`int32` id or a malformed pattern are the only ones that would return a plain error, and no case exercises them). The `valid` reference body (`okNormalizer` + `okPreTokenizer` + `okDecoder` + `okModel`) does load: `merges:[["a","b"]]` with `ab` in the vocab satisfies the new merge check. The "empty vocab" case is the one exception noted above.
- **Malformed-file validation.** Negative ids and duplicate ids are both rejected while filling `byID`, and the duplicate check makes the outcome deterministic despite the map (`return` on the second occurrence, so no iteration-order dependence). `byID` is now a map, so `TestFromBytesAcceptsALargeVocabID` (id 2000000000) loads and `Decode([2000000000])` returns `"z"` — no `maxID+1` allocation, no panic, and an id that cannot fit `int32` is a JSON parse error rather than a silent skip. `Decode` still skips ids outside the map. Merges are validated for both operands and the concatenation before insertion, so nothing is silently dropped; `parseMerge` handles both the array and the `"a b"` string form. `newAddedVocab` rejects an empty content, a token absent from the vocab, an id that disagrees with the vocab, and the `normalized`/`lstrip`/`rstrip`/`single_word` flags.
- **`Decode` against the pinned decoder.** Verified all 80 fixture cases by hand against the implementation: the 64 single/double code ids concatenate (`[236776,8686]` → `"AAA"`); every byte-run case matches the "valid UTF-8 → run text, otherwise one U+FFFD per byte" rule, including `E1 80 C2` → 3 replacement characters (not `from_utf8_lossy`'s 2), `C2 A9 FF` → 3, `ED A0 80` → 3, `F0 9F 99 82` → `"🙂"`, `E6 97 A5` → `"日"`; runs split by an ordinary token (`[236776,468,389]` → `"A\uFFFD\uFFFD"`, `[468,389,236776]` → `"\uFFFD\uFFFDA"`) and runs ending at the end of the slice both flush correctly; an empty run returns the empty string. The fixture's ids are consistent with the generator's `238 + value` mapping and with `<0x41>` = 303, which is good evidence the file is real `tokenizer.decode` output rather than hand-written. Lowercase hex and non-2-digit `<0x..>` forms are accepted differently by `byteToken` (len==6 + `ParseUint`) than a permissive HF parser might be, but the pinned vocab contains only the 256 two-digit uppercase byte tokens (grepped: no `<0x[0-9A-F]>` or `<0x[0-9A-F]{3,}>` token exists), so this is unreachable for the pin.
- **`Fetch`.** The body is copied through `io.LimitReader(..., maxTokenizerBytes+1)` with a `> maxTokenizerBytes` check, then `FromFile(temp.Name())` runs before `os.Rename`, so a 200-with-HTML or a truncated body is never installed and the temp file is removed by the deferred `os.Remove`. The cache-hit path re-parses and treats an unparseable entry as absent; the re-fetch cannot loop, because a second bad body makes `Fetch` return an error rather than retry. Status >= 400 is reported with 400 bytes of detail; a 3xx is followed by the client and `Authorization` is only attached when a token exists. `checkSource` rejects a leading `/`, any `\`, `?`, `#` or NUL, and every empty/`.`/`..` component (so `a/b/`, `a/../b`, `/abs` and `..` all fail), and `escapePath` escapes per segment with `url.PathEscape`; `cacheDir` returns an error instead of falling back to `/tmp`. A repo containing `:` or a Windows drive letter does not escape the cache (`filepath.Join` keeps it as a literal component), it only produces a nonsense URL.
- **Concurrency.** `Tokenizer` is still read-only after `FromBytes`; `fuseUnknowns`' in-place `ids[:0]` writes only into `encodePart`'s own slice, and `merge` returns a fresh slice. No new shared state was introduced by the round-2 changes.
- **Provenance and corpus.** `gen_tokenizer_parity.py` pins `MODEL`/`REVISION` and passes `revision=REVISION` to `from_pretrained`, reads every corpus input through `read_required` (failing with the `SHISA_DE_PY_REPO` hint), and writes the meta with model, revision, library versions, string count, id count, decode-case count and a sha256 of the strings fixture. `TestParityVectors` asserts model, revision, string count and total id count. The scope's counts are consistent: 1,051 → 1,056 strings matches the five added `<unk>` vectors exactly, and the +16 id delta against the +15 those five explain is accounted for by the corpus also containing 2,000-character slices of `AGENTS.md`, which this commit edited.
- **Docs.** `docs/PORT.md`'s counts (1,056 / 48,312 / 80 decode cases) match the meta and the fixture; the `Decode` description matches the measurements; the `Lookup` claim matches the code; the "pipeline is pinned, not general" claim matches the code except for the duplicate-id sentence above. `AGENTS.md`'s tokenizer rows match the code and the environment variables (`SHISA_DE_TOKENIZER_JSON`, `SHISA_DE_TOKENIZER_REQUIRE`, `SHISA_DE_TOKENIZER_CACHE`, `HF_HUB_CACHE`, `HF_TOKEN`, `HUGGING_FACE_HUB_TOKEN`, `SHISA_DE_TOKENIZER_DOWNLOAD`).
- **`post_processor`.** Still parsed and never inspected. That is not a fidelity gap for `Encode`: the pinned template has an empty `special_tokens` map, and Hugging Face only applies the post-processor when special tokens are requested, which this package never does (the corpus's `<pad>` → `[0]` and single-token `"A"` vectors would fail otherwise). Offsets and type ids, the other things a post-processor can change, are not part of this package's output.
- **Not done:** I ran no commands (no shell tool), did not recount the 1,056 vectors or 48,312 ids (the fixture is one line larger than the read limit), did not exercise `Fetch` against the network, and did not execute `transformers` to re-measure any Hugging Face behaviour.

## Residual Risks

- **The tree under review is unverified.** With no shell tool I read the working tree; it contains every round-2 change described in the scope, but I cannot prove it equals commit `3beed13` or that no uncommitted edits exist.
- **Hugging Face internals I could not read on this machine.** Whether the `ByteFallback` decoder accepts lowercase hex and non-2-digit byte tokens (Go requires exactly six characters; unreachable for the pinned vocab, checked by grep), whether Hugging Face errors or falls back to unk when a character's byte tokens are incomplete (the scope records `[unk]`, which is what this port does when a unk exists), and whether the BPE merge table is keyed by symbol strings. If it is keyed by strings, a *synthetic* file containing a merge whose operand is the unk token would be read differently here (this port keys merges by id, so an unknown-character symbol could merge), but the pinned file has no such merge — `<unk>` occurs exactly three times in it: as an added token, as `unk_token`, and as a vocab key.
- **The decode fixture is the oracle and the thing under review.** I verified the implementation matches all 80 recorded cases by hand and that the ids are self-consistent with the generator, but I did not reproduce the generation, so I cannot independently confirm the recorded texts are what `tokenizers` 0.23.3 returns.
- **The provenance chain has one unfalsifiable link.** The meta's revision and the code's pin constants agree, and the fixture's ids are checked against the implementation, but a fixture regenerated from another tokenizer with the meta's `revision` field hand-edited would pass; the unused sha256 does not close this.
- **The corpus couples to this repository's own docs.** `LOCAL_FILES` includes `README.md`, `AGENTS.md` and `docs/READOUT-DE2.md`, so any documentation edit changes recorded ids and makes the fixture's provenance stale until someone regenerates it with network access — and nothing (including the unchecked sha256) flags that the fixture no longer matches the tree that produced it.
- **Strictness rejects semantically equivalent shapes.** A checkpoint that spells the same pipeline as `Sequence[Replace(" "→"▁")]`, adds a no-op decoder stage, or carries a normalizer of a different JSON type would be rejected even though Hugging Face reads it identically. That is the documented trade-off and it is safe (the pin is the only supported pipeline), but a future re-pin to a differently-spelled but equivalent file would need a code change rather than a config change.
- **M2 has not run yet.** The tokenizer is used only by tests at this commit, so the prompt-token and code-boundary checks that are supposed to catch a tokenizer mismatch have not been exercised against a served model in this port.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "fuse_unk regression test's fixture never enables fuse_unk, so it cannot fail",
      "location": "tokenizer/tokenizer_test.go:313",
      "recommendation": "Set fuse_unk:true in the fixture's model (and add a case where a genuine unknown character fuses while an adjacent added <unk> does not), so the test exercises the placement it claims to guard."
    },
    {
      "severity": "MEDIUM",
      "title": "PORT.md says Hugging Face refuses a duplicate vocab id, contradicting the measured record",
      "location": "docs/PORT.md:55",
      "recommendation": "Split the sentence: negative ids and broken merges are refused by Hugging Face too; the duplicate id and the added-token id mismatch are shapes Hugging Face reads and this port rejects deliberately."
    },
    {
      "severity": "LOW",
      "title": "Lookup joins the unvalidated content of a refs file into a path",
      "location": "tokenizer/hub.go:289",
      "recommendation": "Require the refs content to look like a revision (7-64 lowercase hex characters) before joining, returning false otherwise so the requested revision is downloaded."
    },
    {
      "severity": "LOW",
      "title": "meta.json sha256 and decode_cases are never checked, so the decode corpus can shrink silently",
      "location": "tokenizer/tokenizer_test.go:182",
      "recommendation": "Assert len(cases) == meta.decode_cases in TestDecode and verify the fixture's sha256 against the meta, or drop the unused fields."
    },
    {
      "severity": "LOW",
      "title": "Pinned-tokenizer fingerprint duplicated and weaker outside the tokenizer package",
      "location": "render_test.go:292",
      "recommendation": "Move the fingerprint into LoadPinned or an exported VerifyPinned and use it from checkPinned, pinnedTokenizer and loadPinnedBench."
    },
    {
      "severity": "LOW",
      "title": "Unknown characters are silently dropped when neither byte tokens nor a resolvable unk exist",
      "location": "tokenizer/tokenizer.go:483",
      "recommendation": "Reject at load a file that could produce no token for an unknown character (require byte_fallback with a complete byte vocabulary or a resolvable unk_token)."
    },
    {
      "severity": "LOW",
      "title": "Rejection table's empty-vocab case never reaches the empty-vocab check",
      "location": "tokenizer/tokenizer_test.go:263",
      "recommendation": "Build the empty-vocab body from the pinned normalizer, pre-tokenizer and decoder so it exercises the check it names."
    }
  ]
}
END_REVIEW_JSON
