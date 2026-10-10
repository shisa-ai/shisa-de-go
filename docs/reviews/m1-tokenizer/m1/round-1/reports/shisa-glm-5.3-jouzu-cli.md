# Review Report

## Summary

The M1 tokenizer is a faithful port for the pinned file. I verified the pinned `tokenizer.json` (snapshot `8f21734b27b3462471c4662d5638e52195b005a3`, present in the local HF cache) declares exactly what the Go pipeline assumes: `Replace(" " → "▁")` normalizer, `Split(" ", MergedWithPrevious, invert=false)` pre-tokenizer, BPE with `fuse_unk: true`, `byte_fallback: true`, `ignore_merges: false`, array-form merges, all 256 uppercase byte tokens, and no lowercase-hex-shaped vocab tokens. The merge heap's stale-candidate guard holds for every case I could construct, and the leftmost tie-break matches Hugging Face. The heap merge, added-token longest-match, byte fallback, and concurrency story are sound.

The findings are about the edges the parity gate cannot see: `fuse_unk` is applied to added-token ids (a reachable divergence for text like `"<unk><unk>"`), the normalizer/pre-tokenizer/decoder parameters are validated by type only and then hardcoded, the vector generator does not pin the snapshot it claims to pin, and `Lookup`'s any-snapshot fallback can silently defeat the pin. None of these affect the pinned tokenizer on the parity corpus, which is exactly why they are worth fixing now.

## Findings

### MEDIUM: fuse_unk is applied to added-token ids, diverging from Hugging Face on adjacent `<unk>` text

- Severity: MEDIUM
- Confidence: HIGH
- Location: tokenizer/tokenizer.go:331 (`encodeToIDs`), `fuseUnknowns`
- Problem: In Hugging Face, `fuse_unk` is a model-stage operation: it collapses runs of unk ids inside one pre-token's model output. It never touches added-token ids, which bypass the model entirely. The Go code runs `fuseUnknowns` over the whole output, including ids emitted by added-token matches. For the pinned tokenizer, model output can never contain unk (every character is either in the vocab or falls back to its bytes, and all 256 byte tokens exist), so the only way unk id 3 enters the output is the added token `<unk>`. `Encode("<unk><unk>")` therefore returns `[3]` here, while `AutoTokenizer.encode("<unk><unk>", add_special_tokens=False)` returns `[3, 3]`. User evidence text can contain that literal, so a rendered prompt's token count can disagree with the Python client's. The parity corpus includes `"<unk>"` only alone, so the gate passes while the implementation is wrong for this input class. A secondary form of the same bug: fusing across pre-token boundaries (unreachable for the pinned file, whose pre-tokenizer never splits, but reachable for other files this loader accepts).
- Recommendation: Fuse only within one `encodePart` result, and not across ids emitted by added-token matches. Add a parity vector for `"<unk><unk>"` (and `<unk>` adjacent to unknown-region text). Confirm against `transformers` before committing, since this finding rests on the model-stage placement of `fuse_unk`, not a measurement.

### MEDIUM: normalizer and pre-tokenizer are validated by type only, then hardcoded

- Severity: MEDIUM
- Confidence: HIGH
- Location: tokenizer/tokenizer.go:225-261 (`checkNormalizer`, `checkPreTokenizer`), 348 (`normalize`), 357 (`preTokenize`)
- Problem: `checkNormalizer` accepts any `Replace` with a nonempty literal pattern, and `checkPreTokenizer` accepts any literal `Split` with one of four behaviors — but neither compares the file's `pattern.String` against `" "`, the normalizer's `content` against `"▁"`, or checks `invert` (parsed at tokenizer.go:80, never read). `normalize` and `preTokenize` then unconditionally apply the pinned values. A tokenizer.json declaring `Replace("\t" → "")`, a different replacement content, or `invert: true` is read silently wrong rather than rejected, contradicting the package's stated contract ("rejected rather than approximated").
- Recommendation: In `checkNormalizer`, require `Pattern.String == " "` and `Content == "▁"`; in `checkPreTokenizer`, require `Pattern.String == " "` and `Invert == false`; otherwise return `ErrUnsupported`. Alternatively pass the parsed values through instead of hardcoding them.

### MEDIUM: the decoder and post-processor are not validated; Decode is hardcoded to the pinned decoder

- Severity: MEDIUM
- Confidence: HIGH
- Location: tokenizer/tokenizer.go:67 (`PostProcess` parsed, never checked), 289 (`Decode`)
- Problem: `tokenizerFile` has no `decoder` field at all, so any decoder is loaded and decoded with the pinned one (Replace ▁→space, byte-token conversion, concatenation). A file with a `Strip` (leading-space removal, as Llama carries), a different `Replace`, or no decoder is silently decoded wrong. The post-processor is captured as `json.RawMessage` and never examined; for the pinned file it is a no-op `TemplateProcessing` with empty `special_tokens` (verified in the pinned file), but a file whose post-processor adds special tokens is silently read as if it had none. `Encode` documents that it does not apply post-processing, which is the right call for the readout — but the package's reject-don't-approximate contract should still reject shapes it silently reinterprets.
- Recommendation: Parse and validate the `decoder` field against the pinned sequence (Replace ▁→space, ByteFallback, Fuse), and reject anything else with `ErrUnsupported`; reject post-processors other than the no-op template shape.

### MEDIUM: the parity generator does not pin the snapshot it claims to pin

- Severity: MEDIUM
- Confidence: HIGH
- Location: scripts/gen_tokenizer_parity.py:162
- Problem: The docstring says "The tokenizer is the pinned one: snapshot 8f21734b27b3462471c4662d5638e52195b005a3", but `AutoTokenizer.from_pretrained(MODEL)` passes no `revision`, so it resolves `main`. Today the local `refs/main` still points at the pinned snapshot (verified), so the current vectors are consistent — but the next time the hub's `main` moves, regeneration produces vectors from a different tokenizer than the one the Go tests load (pinned first). The result is either spurious parity failures or, combined with the Lookup fallback below, verification of the wrong snapshot.
- Recommendation: Read the snapshot from `data/codebook-de2.json` (it is recorded there) and pass `revision=snapshot` to `from_pretrained`; fail loudly if `main` and the pinned snapshot disagree.

### MEDIUM: Lookup's any-snapshot fallback can return a different revision than requested, and the caller cannot tell

- Severity: MEDIUM
- Confidence: HIGH
- Location: tokenizer/hub.go:177-181; tokenizer/tokenizer_test.go:30-34
- Problem: `Lookup(repo, revision)` falls back to "any snapshot that has a tokenizer.json" (`matches[0]`, alphabetical glob order) whenever the named revision's file is missing. This fires even when the caller explicitly asked for the pinned snapshot: `loadPinned` calls `Lookup(pinnedRepo, pinnedSnapshot)` first, and that call can hand back a different snapshot's tokenizer, which the test then treats as the pinned one. Nothing downstream catches it — there is no fingerprint assertion on the loaded tokenizer (no vocab-length or code-id check before `TestParityVectors` runs; `TestParityCoversEveryCodebookCode` checks code ids but only as a separate test, and only if the vectors test didn't already fail confusingly).
- Recommendation: Drop the glob fallback, or restrict it to the case where no revision was requested and report which snapshot was found. Independently, assert a cheap fingerprint of the loaded tokenizer (e.g. `Len() == 262144` plus `<channel|> == 101`) at the top of `loadPinned`.

### LOW: Fetch installs unvalidated response bodies and a poisoned cache entry persists

- Severity: LOW
- Confidence: MEDIUM
- Location: tokenizer/hub.go:75-77, 96-122
- Problem: `Fetch` only checks the status code. A misbehaving proxy or CDN returning an HTML error page with status 200 is written to the cache and renamed into place; every later call sees `dest` with `Size > 0` and returns it without re-fetching, so the failure persists until someone deletes the file by hand, with an error message that names the parse failure rather than the bad download. There is also no size cap on `io.Copy`. Concurrent fetches from two processes are safe (distinct temp files, atomic rename), and Go's client strips the `Authorization` header on cross-host redirects.
- Recommendation: Validate the download before renaming — at minimum `FromBytes` on the written temp file, or a Content-Type/parse check — and delete the temp file on validation failure. Consider a `MaxBytesReader`-style cap.

### LOW: repo and revision are joined into filesystem paths without rejecting traversal

- Severity: LOW
- Confidence: HIGH
- Location: tokenizer/hub.go:75 (`Fetch`), 172 (`Lookup`)
- Problem: `filepath.Join(cacheDir(), filepath.FromSlash(repo), revision, "tokenizer.json")` and the `refs/<revision>` read accept `..` and separator-containing components, so a source like `"../x@../../y"` writes or reads outside the cache directory. The source string is user configuration, not remote input, so this is hardening rather than an exploitable hole, but the rejection is one line. The hub URL side is harmless (fixed host; HF normalizes dot segments).
- Recommendation: Reject repo or revision containing `..`, path separators, or drive letters with `ErrUnsupported`-style errors before joining.

### LOW: Decode has no golden vectors, and its invalid-UTF-8 handling diverges from Hugging Face

- Severity: LOW
- Confidence: MEDIUM
- Location: tokenizer/tokenizer.go:289-312; tokenizer/tokenizer_test.go:138-147
- Problem: The parity vectors record ids only; HF's own `decode` output is never compared, so `TestDecode`'s round-trip on 8 strings is the only constraint on `Decode`. Two divergence classes exist relative to HF's `ByteFallback` decoder: (1) Go's `string([]byte)` conversion emits U+FFFD per invalid byte, while Rust's `from_utf8_lossy` consumes maximal invalid subsequences, so some byte-token sequences that do not form valid UTF-8 (e.g. `E1 80 C2`) produce a different number of replacement characters; (2) the byte-token check accepts lowercase hex via `ParseUint`, while HF's regex is uppercase-only — I grepped the pinned vocab and found no lowercase-hex-shaped tokens, and byte tokens only encode real UTF-8 bytes, so both classes are unreachable for ids this tokenizer produces; they matter only when decoding arbitrary model-emitted id sequences. Byte sequences spanning several `<0xXX>` tokens decode correctly (verified by reasoning against the Sequence[Replace, ByteFallback, Fuse] decoder in the pinned file; there is no Strip).
- Recommendation: Add decode golden strings to the generator (`tokenizer.decode` on interesting id sequences, including a multi-token byte-fallback sequence and a leading-space case) so `Decode` is parity-checked like `Encode`.

### LOW: vocab id sanity is not validated; duplicate ids make Decode nondeterministic

- Severity: LOW
- Confidence: MEDIUM
- Location: tokenizer/tokenizer.go:144-170
- Problem: The loader accepts duplicate vocab ids (two tokens, one id): `byID[id] = token` keeps whichever token map iteration visits last, so `Decode` becomes nondeterministic, and a merge whose produced id collides with a part's id could defeat the `symbols[l] != item.a` staleness check in `bpe.go` (the only theoretical hole I found in the guard). A merge whose parts or concatenation are missing from the vocab is silently dropped; HF's behavior for such files is unverified here. Negative and gapped ids are accepted.
- Recommendation: On load, reject duplicate ids and ids outside `[0, len(vocab))`, or at minimum document the assumption that ids are unique. Keep dropping unproducible merges, but note it in `FromBytes` so the divergence from HF is a decision, not an accident.

### LOW: the parity suite skips invisibly in CI

- Severity: LOW
- Confidence: HIGH
- Location: tokenizer/tokenizer_test.go:52-54
- Problem: When no tokenizer.json is found, the parity tests `t.Skipf` and `go test ./...` exits 0. A skip is distinguishable from a pass only by someone reading the output; nothing gates it. This is a deliberate tradeoff (the file is 32 MB and not checked in), but CI has no way to say "this run must include parity".
- Recommendation: Add an opt-in strict mode (e.g. `SHISA_DE_REQUIRE_PARITY=1` turns the skip into a failure, mirroring the Python repo's `SHISA_DE_LIVE` pattern) and use it in CI.

## Things Checked

- **bpe.go merge correctness.** Constructed case: symbols `[A, B, C]`, ranks `(B,C)=0 < (A,B)=1`. Popping `(B,C)` first sets `symbols[1] = id(BC)` and `removed[2]`; the stale `(A,B)` candidate is later rejected by `symbols[r] != item.b`. Case where the left partner's symbol changes: a merge at `l` sets `symbols[l]` to the merged id; candidates with `left == l` are rejected by `symbols[l] != item.a`, candidates with `right == l` by `symbols[r] != item.b`. Removal cases are caught by `removed[]` or `next[l] != r`. The only hole I found is the duplicate-vocab-id collision (above). Tie-break: ranks are unique per pair (they are indices into the merges list), so equal ranks occur only for the same pair at multiple positions, where leftmost-first matches HF's `(rank, position)` heap ordering and the Python reference's first-minimum scan.
- **Pinned file vs. hardcoded pipeline.** Read the pinned `tokenizer.json` from the local HF cache: normalizer, pre-tokenizer, post-processor, decoder, `fuse_unk`, `byte_fallback`, `ignore_merges`, array-form merges, all 256 byte tokens present, no lowercase-hex tokens, 24 added tokens all with `lstrip/rstrip/single_word/normalized` false. `ignore_merges: true` is accepted and handled as whole-word-vocab-check-then-merge, which matches observed HF behavior (Llama 3 carries `ignore_merges: true` and still subword-merges unseen words) — not flagged.
- **`preTokenize` is dead code for the pinned file** (the normalizer removes every space first), and its `MergedWithPrevious` implementation is correct by inspection for the leading-delimiter case.
- **Byte fallback.** Per-character vocab lookup then per-byte fallback with unk for missing byte tokens matches HF's structure. Invalid UTF-8 in a Go string yields `RuneError` with size 1, so a raw byte 0xFF encodes as `<0xFF>` where a lossy-converting pipeline would encode U+FFFD — unreachable for this client, whose text arrives as valid UTF-8 from JSON. Lone-surrogate input takes the same byte-fallback path; also unreachable via JSON.
- **Corpus shape.** The corpus is broad (all added tokens and prefix pairs, all 256 codes with and without `<channel|>`, scaffold fragments, JSON payloads, scripts, control characters, 400 seeded random strings, 60 KB of real repo text, an 8,580-id wide prompt). Two gaps: no decode vectors at all, and no adjacent-`<unk>` pair — the second is what lets finding 1 pass the gate.
- **Resolution order and Lookup** (see findings); `refs/main` currently equals the pinned snapshot locally.
- **Fetch safety:** redirect handling (Go strips `Authorization` cross-host), concurrent fetch (temp + rename is safe), status handling, and the traversal and poisoning gaps above.
- **Anti-quadratic test:** the 5-second bound against a measured 17.7 ms has ~280x headroom; a naive repeated-scan on 8,580 symbols would take far longer (sugarme took 14-35 s with a different slow algorithm), so the guard would fire. The `< 8000 ids` fixture check keeps it meaningful if the widest vector is lost.
- **Concurrency:** `Encode`/`Decode` allocate per call; `vocab`, `merges`, `byID`, `byteID/byteOK`, and `addedVocab` are read-only after load; `fuseUnknowns`'s `ids[:0]` reuse is on a local slice. The parent package's `Codebook`/`CodesFor` return shared slices but both are documented "must not be modified". The doc comment's claim holds.
- **docs/PORT.md verified-parity section:** the listed numbers match the validation record; "byte-identical to `transformers`' ids on every one" is supported by the vectors test only to the extent the corpus constrains (see the two gaps), and the speed claims carry their date and machine.

## Residual Risks

- Finding 1 rests on Hugging Face applying `fuse_unk` at the model stage rather than over the final encoding. I could not execute `transformers` here to measure `encode("<unk><unk>")`; verify against a real `AutoTokenizer` before fixing.
- HF's exact behavior for malformed tokenizer.json (duplicate ids, merges with missing parts, unknown characters with no unk and no byte fallback) was reviewed from the package's perspective only; the divergences flagged are about what this loader silently accepts, not about HF's own handling.
- `Decode`'s U+FFFD-count divergence on invalid byte sequences was reasoned from Go's conversion rules and Rust's `from_utf8_lossy`, not measured.
- The M0 codebook's `token_ids` cross-check partially mitigates snapshot drift, but nothing asserts the loaded tokenizer's identity before the vectors run.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "fuse_unk is applied to added-token ids; Encode(\"<unk><unk>\") diverges from Hugging Face",
      "location": "tokenizer/tokenizer.go:331",
      "recommendation": "Fuse unk only within one encodePart's model output, not across added-token ids or pre-token boundaries; add a parity vector for adjacent <unk> and verify against transformers."
    },
    {
      "severity": "MEDIUM",
      "title": "Normalizer and pre-tokenizer validated by type only; pattern, content, and invert are hardcoded to the pinned values",
      "location": "tokenizer/tokenizer.go:225-261",
      "recommendation": "Require the pinned pattern ' ', normalizer content '▁', and invert=false, rejecting other values with ErrUnsupported."
    },
    {
      "severity": "MEDIUM",
      "title": "Decoder and post_processor are never validated; Decode is hardcoded to the pinned decoder",
      "location": "tokenizer/tokenizer.go:289",
      "recommendation": "Parse and validate the decoder field against the pinned Sequence[Replace, ByteFallback, Fuse] and reject other post_processor shapes."
    },
    {
      "severity": "MEDIUM",
      "title": "Parity vector generator does not pin the tokenizer snapshot its docstring claims",
      "location": "scripts/gen_tokenizer_parity.py:162",
      "recommendation": "Pass revision=<pinned snapshot from data/codebook-de2.json> to AutoTokenizer.from_pretrained."
    },
    {
      "severity": "MEDIUM",
      "title": "Lookup's any-snapshot fallback can return an unpinned revision without the caller knowing",
      "location": "tokenizer/hub.go:177-181",
      "recommendation": "Drop or restrict the glob fallback, and fingerprint the loaded tokenizer (vocab length, <channel|> id) before running parity vectors."
    },
    {
      "severity": "LOW",
      "title": "Fetch installs unvalidated response bodies; a bad 200 poisons the cache permanently",
      "location": "tokenizer/hub.go:96-122",
      "recommendation": "Validate the download parses as a tokenizer (FromBytes) before renaming, and cap the download size."
    },
    {
      "severity": "LOW",
      "title": "repo and revision joined into filesystem paths without rejecting '..' or separators",
      "location": "tokenizer/hub.go:75",
      "recommendation": "Reject traversal-containing repo/revision strings before any path join."
    },
    {
      "severity": "LOW",
      "title": "Decode has no golden vectors; invalid-UTF-8 U+FFFD counts and lowercase-hex handling can diverge from HF",
      "location": "tokenizer/tokenizer_test.go:138-147",
      "recommendation": "Record tokenizer.decode outputs in the parity generator and replay them, including a multi-token byte-fallback sequence."
    },
    {
      "severity": "LOW",
      "title": "Duplicate or out-of-range vocab ids are accepted, making byID nondeterministic",
      "location": "tokenizer/tokenizer.go:144-170",
      "recommendation": "Reject duplicate vocab ids at load time."
    },
    {
      "severity": "LOW",
      "title": "Parity suite skips invisibly in CI when tokenizer.json is absent",
      "location": "tokenizer/tokenizer_test.go:52-54",
      "recommendation": "Add an opt-in strict mode (SHISA_DE_REQUIRE_PARITY=1) that turns the skip into a failure for CI."
    }
  ]
}
END_REVIEW_JSON
