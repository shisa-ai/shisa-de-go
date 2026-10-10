# Review Scope: m1-tokenizer

- Mode: implementation
- Stage: M1
- Target: Commit 91e1ffe in /home/morpheus/data/dev/shisa/shisa-de-go: the pure-Go Hugging Face BPE tokenizer

## Summary

M1 of a Go port of the Python `shisa-de` client. The Python client calls `transformers.AutoTokenizer` to render prompts and resolve token ids; this port implements the tokenizer itself in pure Go, with no cgo, so the library needs no Python runtime.

What was built (all new files, commit 91e1ffe):
- `tokenizer/tokenizer.go` — parse `tokenizer.json`; added-token extraction over the raw text; `Replace(" " -> U+2581)` normalizer; `Split(" ")` pre-tokenizer; per-character symbolization with byte fallback; `Encode`, `Decode`, `TokenID`, `Len`. Unsupported file shapes (non-BPE model, regex normalizer/pre-tokenizer patterns, continuing_subword_prefix, end_of_word_suffix, added tokens with lstrip/rstrip/single_word/normalized, an added token absent from the vocab) are rejected with `ErrUnsupported` rather than approximated.
- `tokenizer/bpe.go` — the merge, using a binary heap keyed by `(rank, left index)` over a doubly linked list, with stale-candidate detection.
- `tokenizer/hub.go` — `Load`/`Resolve`/`Fetch`/`Lookup`: a source is a local path or a Hugging Face repo id; the hub cache is searched before downloading; downloads are written to a temp file and renamed.
- `tokenizer/tokenizer_test.go` — parity, codebook-boundary, decode, an anti-quadratic timing guard, and unsupported-file rejection.
- `scripts/gen_tokenizer_parity.py` — generates the vectors by running `transformers` for real.
- `testdata/tokenizer_parity.json` — 1,051 strings and 48,296 ids.

The claim to check: the ids are byte-identical to Hugging Face's for the pinned tokenizer (`shisa-ai/shisa-de-2`, snapshot 8f21734b27b3462471c4662d5638e52195b005a3, vocab 262144, 514906 merges, byte_fallback, fuse_unk, ignore_merges false, 24 added tokens).

Two facts established by measurement and worth challenging: the pinned tokenizer's `Split(" ")` pre-tokenizer is a no-op in the real pipeline because the normalizer replaces every space with U+2581 first, so a whole prompt becomes ONE pre-token and the merge runs over tens of thousands of symbols; and one-at-a-time lowest-rank merging with a leftmost tie-break matches Hugging Face (verified against a Python reference implementation before the Go was written).

## Primary files / regions

- `tokenizer/tokenizer.go` — lines/regions: new, 1-420 — reason: The pipeline: parsing, added-token matching, normalization, pre-tokenization, symbolization, the public Encode/Decode
- `tokenizer/bpe.go` — lines/regions: new, 1-110 — reason: The heap merge, where a stale candidate could merge the wrong pair
- `tokenizer/hub.go` — lines/regions: new, 1-210 — reason: Download, cache lookup, and the hub-cache fallbacks
- `tokenizer/tokenizer_test.go` — lines/regions: new, 1-270 — reason: The parity gate and what it does when the tokenizer file is absent
- `scripts/gen_tokenizer_parity.py` — lines/regions: new, 1-210 — reason: How the golden vectors were produced; whether the corpus is shaped to the implementation
- `docs/PORT.md` — lines/regions: verified-parity section — reason: Claims about parity and speed that this review should treat as unproven

## Review questions

- Is the merge in bpe.go correct? Construct a case where a stale heap candidate could merge a pair that is no longer adjacent or whose symbols have changed, and say whether the (removed, next[l]==r, symbols[l]==a, symbols[r]==b) guard catches it. Is the leftmost tie-break on equal ranks right, or can equal ranks produce a different result from Hugging Face?
- Which tokenizer.json shapes would this package read SILENTLY WRONG rather than reject? The rejection list is explicit; find what it misses. Consider: a post_processor that adds special tokens, a decoder this package does not implement, added tokens with lstrip/rstrip/single_word/normalized set, `ignore_merges` true, `fuse_unk` false, a merge whose concatenation is not in the vocab (it is dropped), a vocab id above len(byID), duplicate ids in the vocab, and a `Split` behavior other than the four listed.
- Is the byte-fallback symbolization faithful? It looks up each Unicode scalar in the vocab and, when absent, emits `<0xXX>` for the character's UTF-8 bytes. Check what Hugging Face does for a character absent from the vocab, for a character whose bytes are partly present as byte tokens, for invalid UTF-8 in a Go string (the loop uses DecodeRuneInString, which yields RuneError with size 1), and for a lone surrogate encoding.
- Does the parity corpus actually constrain the implementation, or is it shaped to what the implementation already does? The generator runs transformers for real, but the corpus was chosen by the same person who wrote the implementation. Name a class of input that is missing, and say whether a wrong implementation would pass.
- Is the test's behaviour when tokenizer.json is absent acceptable? It SKIPs. Is a skip distinguishable from a pass in a way that matters for CI, and is the resolution order (SHISA_DE_TOKENIZER_JSON, tokenizer/testdata/tokenizer.json, HF hub cache by pinned snapshot, by ref, then any snapshot) safe?
- Is `Lookup`'s fallback to 'any snapshot that has a tokenizer.json' a correctness risk? It can return a tokenizer from a different revision than the pinned one, and the caller cannot tell. Does anything downstream catch that?
- Is `Fetch` safe against a partial or hostile download, and against a cache directory that another process is writing? Consider redirects to a different host, an HTML error page returned with status 200, a concurrent Fetch from two processes, and a revision string containing `..` or an absolute path.
- Does `Decode` match the pinned decoder? It replaces U+2581 with a space, converts a 6-character `<0xXX>` token to its byte, and concatenates. Check it against a byte-fallback sequence that spans several tokens, against a token whose text literally is `<0x41>`, and against the `Fuse` step it does not implement separately.
- Is the anti-quadratic timing test meaningful? It asserts under 5 seconds for an 8580-id prompt, and the measured time is 17.7ms. Would it catch a regression to the naive algorithm, and does it depend on the widest fixture surviving?
- Is anything in this package unsafe for concurrent use after loading? The doc comment claims it is. Check for mutation of shared state in Encode/Decode, including the `ids[:0]` reuse in fuseUnknowns and the shared slices returned by Codebook/CodesFor in the parent package.

## Known non-goals

- Byte offsets, truncation, padding, post-processing, and training are deliberately not implemented.
- No cgo and no dependency on Hugging Face's Rust library; a pure-Go implementation was chosen over a 49 MB per-platform static library.
- No cross-call cache of encoded words, unlike Hugging Face's per-word cache. The measured 8580-id prompt is 17.7ms, against 13ms for the Rust implementation.
- The parent package's Codebook/CodesFor are M0 and are not in scope here.
- DE-1 is not implemented anywhere in this port; a DE-1 model is refused rather than read.
- The 32 MB tokenizer.json is deliberately not checked in, so the parity suite skips without it.
- This review is about the Go implementation's fidelity to Hugging Face, not about whether the pinned snapshot is the right one to pin.

## Validation already run

- gofmt -l . (clean)
- go build ./... (clean)
- go vet ./... (clean)
- go test ./... (pass)
- go test ./tokenizer/ -run TestParityVectors -v (pass, 1051 vectors)
- go test ./tokenizer/ -run TestWidePromptIsNotQuadratic -v (8580 ids in 17.7ms)
- go test ./tokenizer/ -bench BenchmarkEncodeWidePrompt -benchtime 3x (19.1ms/op, 1.45 MB/s)
- HOME=/tmp/no-such-home go test ./tokenizer/ -run TestParityVectors -v (SKIP, as designed)
- daulet/tokenizers (cgo to HF's Rust) on the same machine: 13ms for the same 8580-id prompt, exact match on 5 hand-checked prompts
- sugarme/tokenizer v0.3.0: wrong on added tokens inside a full prompt (250 ids where Python gives 238) and 14-35s on the 8580-id prompt
