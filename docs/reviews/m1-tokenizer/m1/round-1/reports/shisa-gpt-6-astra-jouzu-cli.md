# Review Report

## Summary

The heap-based merge appears correct for the pinned tokenizer’s valid merge table: stale candidates are checked against both adjacency and symbol identity, and equal ranks select the leftmost occurrence.

However, the implementation has a pinned-tokenizer encoding regression: adjacent literal `<unk>` added tokens are collapsed into one token. It also returns invalid UTF-8 when decoding incomplete byte-fallback sequences, accepts tokenizer configurations it does not implement, and can resolve an explicitly requested revision to an unrelated cached snapshot.

The golden-vector generator does not actually pin its tokenizer revision. The existing corpus therefore provides useful coverage, but it does not establish the full fidelity claimed.

This was a read-only inspection. No files were modified and no tests or builds were run.

## Findings

### HIGH: Unknown-token fusion collapses explicit added tokens
- Severity: HIGH
- Confidence: HIGH
- Location: `tokenizer/tokenizer.go:330-331`
- Problem: `fuseUnknowns` runs over the entire final encoding, including tokens emitted by added-token matching. The pinned tokenizer declares `<unk>` as an added token with ID 3. Encoding `<unk><unk>` therefore appends `[3, 3]` and then collapses it to `[3]`. Hugging Face’s unknown fusion belongs to BPE symbolization within a pre-token; it does not combine independently extracted added tokens. This changes prompt token counts for the pinned tokenizer. The corpus includes `<unk>` alone but not repeated adjacent occurrences.
- Recommendation: Perform unknown fusion during symbolization, before merging, and only within the appropriate pre-token. Never apply it to the assembled output containing added tokens. Add regression vectors for `<unk><unk>` and adjacent explicit and generated unknowns.

### HIGH: Cache lookup silently substitutes a different revision
- Severity: HIGH
- Confidence: HIGH
- Location: `tokenizer/hub.go:177-180`
- Problem: When the requested snapshot or ref is unavailable, `Lookup` returns the first tokenizer from any snapshot. Consequently, `Load("owner/model@exact-commit")` can succeed with a different revision instead of fetching the requested one. An unrelated snapshot in the first cache root also takes precedence over an exact match in a later root. `FromFile` performs no identity check, and the returned `Tokenizer` exposes no revision provenance. The parity suite may detect differences in covered strings, but it does not establish snapshot identity. The planned boundary and token-count checks would not detect every same-length ID substitution.
- Recommendation: Resolve only the requested snapshot or ref. Search all applicable roots for that revision, then report a miss and fetch it. Remove the arbitrary-snapshot fallback and add cache-resolution tests involving multiple snapshots and roots.

### MEDIUM: Accepted pipeline configurations are silently replaced by the pinned pipeline
- Severity: MEDIUM
- Confidence: HIGH
- Location: `tokenizer/tokenizer.go:225-260`, `tokenizer/tokenizer.go:348-385`
- Problem: The parser accepts absent normalizers, arbitrary literal `Replace` patterns and contents, and several `Split` behaviors. It then discards those settings: normalization always replaces spaces with U+2581, and pre-tokenization always assumes a space delimiter with `MergedWithPrevious`. `Invert` is accepted but ignored. The decoder is not parsed at all, and BPE `dropout` is also ignored. For example, a valid tokenizer with no normalizer and a vocabulary containing a literal space is accepted, but its spaces are rewritten before lookup. A tokenizer with a null or different decoder is nevertheless decoded using the pinned decoder.
- Recommendation: Either implement and retain the accepted settings or reject every configuration outside the exact supported pipeline with `ErrUnsupported`. Validate the decoder sequence and reject nonzero dropout. Add small, self-contained tests for null stages, different replacement contents, split behaviors, inversion, and different decoders.

### MEDIUM: Decode returns raw invalid bytes instead of ByteFallback replacement text
- Severity: MEDIUM
- Confidence: HIGH
- Location: `tokenizer/tokenizer.go:289-305`
- Problem: `Decode` appends byte-token values directly and returns `string(b)` without validating UTF-8. In the pinned vocabulary, ID 366 is `<0x80>`; decoding `[366]` therefore returns an invalid one-byte Go string. Hugging Face’s `ByteFallback` decoder emits replacement characters for invalid or incomplete byte-token runs. This matters for arbitrary generated IDs, including a generation cut off during a multibyte character. Existing decode tests only round-trip complete encoded strings and cannot expose it.
- Recommendation: Buffer consecutive byte-fallback tokens and reproduce Hugging Face’s valid/invalid UTF-8 handling when the run ends. Add direct decode vectors for isolated continuation bytes, truncated sequences, invalid sequences, valid multibyte sequences, and byte runs separated by ordinary tokens.

### MEDIUM: Partial byte fallback produces a different symbol sequence
- Severity: MEDIUM
- Confidence: HIGH
- Location: `tokenizer/tokenizer.go:404-412`
- Problem: When a character is absent from the vocabulary, the implementation handles its bytes independently. Available byte tokens are emitted, while missing bytes become unknown tokens or disappear. Hugging Face attempts fallback for the complete character; if any required byte token is missing, it falls back to ordinary unknown-character handling. For example, an absent `é` with `<0xC3>` present but `<0xA9>` missing produces a partial byte token followed by an unknown here, rather than one unknown. The parser accepts such vocabularies without warning. The pinned tokenizer’s complete byte vocabulary masks this defect.
- Recommendation: Collect the complete character’s byte IDs before emitting any. If one is absent, take the character-level unknown path. Alternatively, reject incomplete byte-fallback vocabularies if supporting them is unnecessary. Cover both `fuse_unk` settings in fixture-independent tests.

### MEDIUM: Unvalidated vocabulary IDs can panic or corrupt token identity
- Severity: MEDIUM
- Confidence: HIGH
- Location: `tokenizer/tokenizer.go:159-174`, `tokenizer/tokenizer.go:451-465`
- Problem: The reverse vocabulary is allocated from unchecked `int32` IDs. An ID of `2147483647` makes `maxID+1` overflow and causes a negative-length allocation panic; other large sparse IDs can force excessive allocation. Negative IDs are accepted and may be emitted by `Encode`. Duplicate IDs overwrite reverse entries in nondeterministic Go map iteration order. Added-token content is checked for membership, but its declared ID is not checked against the vocabulary ID, so `Encode`, `TokenID`, and `Decode` can disagree about the same token.
- Recommendation: Validate nonnegative, unique IDs and consistent added-token mappings before allocation. Use a bounded or sparse reverse representation, or explicitly require the supported dense vocabulary shape. Return errors rather than panicking or allocating according to an unchecked maximum. Add malformed-ID tests.

### MEDIUM: Invalid merge tables are accepted after silently deleting rules
- Severity: MEDIUM
- Confidence: HIGH
- Location: `tokenizer/tokenizer.go:177-194`
- Problem: A merge with a missing operand or missing concatenated vocabulary entry is silently discarded. Hugging Face rejects an inconsistent BPE merge table rather than loading a different one. Here a damaged or unsupported tokenizer can load successfully and produce plausible but different IDs, contrary to the package’s rejection contract.
- Recommendation: Reject inconsistent merge entries with an error identifying the merge rank and missing token. Add tests for missing left operands, right operands, and concatenated outputs.

### MEDIUM: Repository and revision strings can escape the cache directory
- Severity: MEDIUM
- Confidence: HIGH
- Location: `tokenizer/hub.go:75-83`, `tokenizer/hub.go:171-172`
- Problem: Public `Fetch` and `Lookup` arguments are used as filesystem path components without validation. A revision such as `../../../outside` can escape the intended repository/cache subtree. `Fetch` checks for and returns an existing nonempty `tokenizer.json` at that escaped path before making a request, and otherwise creates directories there before the HTTP request succeeds. Its final rename uses the same unchecked destination. Repository and revision values are also interpolated into the URL without component-aware escaping.
- Recommendation: Validate repository identifiers and revisions, reject traversal components, and verify filesystem containment before accessing paths. Preserve legitimate slash-containing branch names without allowing `..` traversal, and construct the request URL with appropriate escaping. Add read/write containment tests.

### MEDIUM: Invalid successful responses permanently poison the download cache
- Severity: MEDIUM
- Confidence: HIGH
- Location: `tokenizer/hub.go:76-77`, `tokenizer/hub.go:97-119`
- Problem: Any response below HTTP 400 is copied and installed without checking that it contains a supported tokenizer. A nonempty HTML error page returned with HTTP 200 becomes the cached tokenizer. `Load` subsequently reports a parsing error, but later attempts reuse the same nonempty file indefinitely rather than downloading again. The response body also has no size limit, so the timeout does not prevent substantial disk consumption.
- Recommendation: Require an appropriate successful status, bound the download size, and validate the temporary file before publishing it. Define recovery for invalid existing cache entries. Test HTTP 200 HTML, malformed or truncated JSON, oversized responses, and concurrent downloads.

### MEDIUM: The parity generator uses the moving default revision
- Severity: MEDIUM
- Confidence: HIGH
- Location: `scripts/gen_tokenizer_parity.py:162`
- Problem: Although the script documents snapshot `8f21734b27b3462471c4662d5638e52195b005a3`, it calls `AutoTokenizer.from_pretrained(MODEL)` without `revision`. Regenerating the vectors after the repository’s default branch changes can silently replace the oracle with another tokenizer. The output contains no revision or tokenizer digest with which to detect that change.
- Recommendation: Pass the explicit snapshot to `from_pretrained` and record the resolved tokenizer identity alongside the vectors. Have the parity test verify that identity rather than relying on a filename or cache lookup.

## Things Checked

- **Pinned pipeline:** Inspected the cached tokenizer’s configuration. Its normalizer replaces every ASCII space with U+2581, so its subsequent literal-space splitter is indeed a no-op on normalized ordinary pieces. Added tokens still partition a full prompt into separate pieces.
- **Stale heap candidates:** For symbols `a, b, c`, merging `(a, b)` first invalidates a queued `(b, c)` through `removed[b]`. Merging `(b, c)` first invalidates `(a, b)` through the changed `symbols[b]`. The adjacency check additionally rejects candidates whose original neighbors no longer adjoin. The linked-list updates and surviving-symbol output order appear consistent.
- **Equal-rank ordering:** A valid merge table assigns ranks by entry position. Equal-rank candidates are occurrences of the same merge rule. For overlapping occurrences such as `a a a`, the lower left index selects the leftmost pair. Stable slice indices preserve that ordering after merges.
- **Model switches:** `ignore_merges` has a whole-pre-token vocabulary shortcut and otherwise proceeds through BPE. With `fuse_unk=false`, the final fusion pass is disabled. The location and scope of fusion when enabled are incorrect as reported above.
- **Added-token flags:** `normalized`, `lstrip`, `rstrip`, and `single_word` are rejected. Longest matching at a position is implemented. An added token absent from the vocabulary is rejected, although that path returns a generic error rather than the advertised `ErrUnsupported`.
- **Post-processing:** Ignoring special-token insertion is consistent with the explicitly documented `Encode(... without adding special tokens)` surface and the oracle’s `add_special_tokens=False`. The package-level promise to reject unsupported pipeline requirements is broader than its actual validation.
- **Decode ordering:** Complete valid byte-token sequences concatenate correctly. A token whose vocabulary text is `<0x41>` should decode as `A` under the pinned `ByteFallback` decoder; that interpretation is not itself a defect. Concatenation implements the final `Fuse` operation for valid text. Invalid byte runs are the missing behavior.
- **Concurrency after loading:** Maps and added-token groups are read-only during encoding and decoding. Merge buffers and output slices are per-call. `ids[:0]` in `fuseUnknowns` reuses only a call-local slice; its defect is semantic, not a shared-state race. Parent-package `Codebook`/`CodesFor` behavior was excluded as requested.
- **Download concurrency and redirects:** Unique temporary files followed by rename prevent readers from observing an in-progress write. Concurrent successful fetches can replace each other’s complete files. Go’s default HTTP redirect policy normally strips authorization on unrelated-host redirects; no unconditional cross-host bearer-token forwarding was identified.
- **Corpus coverage:** The generator obtains expected IDs from Transformers rather than this implementation. However, it omits adjacent repeated `<unk>` tokens, malformed/incomplete decode sequences, and alternate accepted tokenizer configurations. Implementations wrong in these ways can pass the existing corpus.
- **Performance guard:** The test requires the widest fixture to retain at least 8,000 expected IDs. Its five-second threshold is a coarse regression guard, not proof of nonquadratic complexity; a faster quadratic implementation could still pass.
- **Missing tokenizer behavior:** Skips are deliberate and visible with verbose or JSON test output, but ordinary `go test` can report package success when all substantive tokenizer tests skip. No mandatory parity CI gate was present in the inspected tree.

## Residual Risks

- No runtime reproductions, parity reruns, network requests, race tests, or benchmarks were performed. Findings are based on source inspection and concrete execution-path analysis.
- The inspected working tree is at `12a1a29647b4a997981d7490d2da5b497d796a16`, after target commit `91e1ffe`. Without a configured shell tool, I could not compare the target commit against the working tree or establish whether uncommitted changes exist.
- Invalid UTF-8 input has no documented policy. `Encode` preserves individual invalid bytes through fallback rather than operating on Unicode scalars. UTF-8 encodings of lone surrogates take the same path. This should be explicitly rejected or given a documented compatibility policy; Python strings and Rust tokenizer inputs do not provide an equivalent arbitrary-byte input contract.
- Published parity and speed results were not independently reproduced. The inspected cached tokenizer supports the stated pipeline configuration, but the generator’s missing revision prevents its source alone from establishing which snapshot produced the committed vectors.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "HIGH",
      "title": "Unknown-token fusion collapses explicit added tokens",
      "location": "tokenizer/tokenizer.go:330-331",
      "recommendation": "Fuse unknown characters during pre-token symbolization, not across final output, and add repeated <unk> parity cases."
    },
    {
      "severity": "HIGH",
      "title": "Cache lookup silently substitutes a different revision",
      "location": "tokenizer/hub.go:177-180",
      "recommendation": "Remove arbitrary-snapshot fallback and resolve only the requested snapshot or ref across cache roots."
    },
    {
      "severity": "MEDIUM",
      "title": "Accepted pipeline configurations are silently replaced by the pinned pipeline",
      "location": "tokenizer/tokenizer.go:225-260",
      "recommendation": "Implement accepted settings or strictly reject unsupported normalizers, split settings, decoders, and dropout."
    },
    {
      "severity": "MEDIUM",
      "title": "Decode returns raw invalid bytes instead of ByteFallback replacement text",
      "location": "tokenizer/tokenizer.go:289-305",
      "recommendation": "Implement UTF-8 validation and replacement handling for consecutive byte-token runs, with direct decode parity tests."
    },
    {
      "severity": "MEDIUM",
      "title": "Partial byte fallback produces a different symbol sequence",
      "location": "tokenizer/tokenizer.go:404-412",
      "recommendation": "Require all byte tokens for a character before emitting fallback bytes, otherwise use character-level unknown handling."
    },
    {
      "severity": "MEDIUM",
      "title": "Unvalidated vocabulary IDs can panic or corrupt token identity",
      "location": "tokenizer/tokenizer.go:159-174",
      "recommendation": "Validate ID ranges, uniqueness, and added-token mappings before bounded reverse-vocabulary allocation."
    },
    {
      "severity": "MEDIUM",
      "title": "Invalid merge tables are accepted after silently deleting rules",
      "location": "tokenizer/tokenizer.go:177-194",
      "recommendation": "Reject merge entries with missing operands or concatenated vocabulary tokens."
    },
    {
      "severity": "MEDIUM",
      "title": "Repository and revision strings can escape the cache directory",
      "location": "tokenizer/hub.go:75-83",
      "recommendation": "Validate identifiers, enforce filesystem containment, and escape URL components appropriately."
    },
    {
      "severity": "MEDIUM",
      "title": "Invalid successful responses permanently poison the download cache",
      "location": "tokenizer/hub.go:97-119",
      "recommendation": "Bound and validate downloads before atomic publication and provide recovery for invalid cache entries."
    },
    {
      "severity": "MEDIUM",
      "title": "The parity generator uses the moving default revision",
      "location": "scripts/gen_tokenizer_parity.py:162",
      "recommendation": "Pass the pinned snapshot explicitly and record and verify tokenizer identity with the vectors."
    }
  ]
}
END_REVIEW_JSON
