# How this port maps onto the Python reference

`github.com/shisa-ai/shisa-de` is the reference implementation. `docs/READOUT-DE2.md`
and `docs/READOUT.md` are copied from it verbatim and remain the specification;
they are not edited here. This page records the mapping and every place the Go
client deliberately behaves differently.

## File mapping

| Python | Go |
| --- | --- |
| `shisa_de/readout.py` | `readout.go`, `render.go` |
| `shisa_de/policy.py` | `policy.go` (planned) |
| `shisa_de/calibration.py` | `calibration.go` (planned) |
| `shisa_de/family.py` | `family.go` |
| `shisa_de/questions.py` | `questions.go` |
| `shisa_de/client.py` | `client.go` (planned) |
| `json.dumps` / `json.loads` | `pyjson.go` |
| `shisa_de/overflow.py` | not ported; DE-1 only |
| `shisa_de/images.py` | not ported yet |
| `shisa_de/data/*.json` | `data/*.json`, embedded with `go:embed` |
| `shisa_de/cli.py` | `cmd/shisa-de/` (planned) |
| `tests/test_offline.py` | `*_test.go` with a stub tokenizer and `httptest` |
| `tests/test_live.py` | live tests behind `SHISA_DE_LIVE=1` |

The tokenizer has no Python counterpart as a module: the Python client calls
`transformers.AutoTokenizer`. This port implements the tokenizer in
`tokenizer/` so the library needs neither cgo nor a Python runtime.

The file mapping is the target layout, not a description of the tree: M1 landed
the codebook and the tokenizer; M2 landed the renderer, family resolution, the
question types and the Python-JSON writer. Rows marked planned are M3 or later.

## Deliberate differences

**DE-2 only.** This port implements `de2-codebook-v1`. A model that resolves to
DE-1 is an error. The Python client reads DE-1 correctly, so this is a missing
feature rather than a contract change: it must fail loudly, never fall back to
DE-2 reads.

**No Jinja.** The Python client renders with
`tokenizer.apply_chat_template(...)`, which runs the checkpoint's own Jinja
template. This port renders the two message shapes the readout uses with a
pinned function. It is byte-identical for those shapes and for no others, which
is the whole of what the readout needs. The `usage.prompt_tokens` check and the
code-boundary check are what catch a divergence.

**Python's JSON, not Go's.** The user turn is
`json.dumps(payload, ensure_ascii=False)`, and its bytes are the prompt, so
`pyjson.go` reimplements that writer rather than using `encoding/json`. Go's
encoder writes `{"a":1}` where Python writes `{"a": 1}`, escapes `<`, `>` and
`&`, and formats floats over a different range; each difference would move the
prompt away from the one the calibration was fitted on. `DecodeJSON` keeps
Python's int-or-float distinction, which an `any` from `json.Unmarshal` loses,
and keeps an object's key order in `Object` because the order reaches the prompt;
a Go map is accepted and has its keys sorted. `PythonJSON` is deliberately
stricter than Python in two places, both of which have no Python counterpart: a
string that is not valid UTF-8 is an error rather than U+FFFD, and a
`json.Number` must be a JSON number literal. `DecodeJSON` rejects the bare
`NaN`/`Infinity` literals that `json.loads` accepts by default, while
`PythonJSON` still writes them for a NaN or infinite float.

**No cgo.** The tokenizer is pure Go. `sugarme/tokenizer` was evaluated and
rejected: it does not match added tokens inside a rendered prompt, and it took
14 to 35 seconds on an 8,580-token prompt. `daulet/tokenizers` (cgo to
Hugging Face's Rust implementation) is exact and fast but needs a 49 MB
platform-specific static library.

**The tokenizer pipeline is pinned, not general.** The package implements the
pinned checkpoint's normalizer, pre-tokenizer and decoder and checks the file
for exactly those, rejecting anything else with `ErrUnsupported`. Hugging Face
accepts a file with no normalizer, a different `Split` behavior, a `Strip`
decoder or BPE dropout and reads it with that pipeline; this port refuses
rather than reading it as if it were the pinned one. It also refuses some files
Hugging Face reads for reasons of its own: a duplicate vocab id (Go's reverse
lookup would otherwise depend on map iteration order) and an added token whose
declared id disagrees with the vocab. Finally, it refuses what Hugging Face
refuses too: a negative vocab id, a merge whose operands or concatenation are
missing, and a file with neither a resolvable `unk_token` nor a complete byte
vocabulary, where an unknown character would silently produce no token.
`Lookup` also resolves only the revision asked for, never another snapshot that
happens to be cached.

**`Decode` reproduces the pinned decoder, including its invalid-byte
handling.** A run of consecutive byte-fallback tokens decodes to the run's text
when the bytes are valid UTF-8, and otherwise to one replacement character per
byte. Hugging Face's `ByteFallback` does the same; a plain concatenation of the
bytes would not, and neither would `from_utf8_lossy`, which consumes maximal
invalid subsequences. `Encode` never produces such a run for the pinned
vocabulary, so this only matters for hand-built or model-generated id lists.

**Errors instead of exceptions.** `ReadoutError` and `QuestionError` replace the
Python exception classes; callers use `errors.As`. `ReadoutHTTPError` keeps its
`status` and `detail` fields.

**Lazy loading.** The Python client loads the tokenizer inside
`Readout.ensure_tokenizer`. The Go client does the same behind a `sync.Once`, so
nothing downloads at package init.

**Concurrency.** `ThreadPoolExecutor` becomes goroutines with a bounded worker
pool.

## Verified parity

Checked on 2026-10-10 against `shisa-ai/shisa-de-2` on the hosted endpoint and
the Python client at `shisa-de` `0.4.0`:

- **Tokenizer.** 1,056 strings, 48,392 token ids: this package's ids equal
  `transformers`' ids on every one. The corpus covers all 24 added tokens and
  their prefix pairs, adjacent explicit `<unk>` tokens (the case that catches
  `fuse_unk` applied to added tokens), all 256 codes, the rendered scaffold,
  JSON payloads, Japanese, emoji, combining marks, control characters, 60 KB of
  real text from both repositories, and 400 random strings. A second fixture
  replays 83 `tokenizer.decode` outputs, including byte-fallback runs that
  `Encode` cannot produce and a run split by an id outside the vocabulary.
  `testdata/tokenizer_parity.meta.json` records the model, the revision, the
  library versions, the counts and a sha256 of each fixture; the test fails if
  either fixture disagrees with it. Regenerate with
  `scripts/gen_tokenizer_parity.py`, which pins the revision it passes to
  `from_pretrained`.
- **Render.** All 33 fixture renders are byte-identical to what
  `Readout._payload` and `Readout.render` produced for the same case: both
  template shapes (the repeated/direct read and the thinking read), the
  empty-description rule, non-string descriptions, 17 evidence shapes, widths
  2/26/27/77/256, and long evidence. Each fixture prompt also re-tokenizes to
  the same ids the Python client produced. The readout page's 238 and 137 token
  counts are pinned; the fixture's third pin (216, the Japanese choice) is a
  regression check and is not a published value.
  `testdata/render_parity.meta.json` records the model, the revision, the
  library versions, the counts and a sha256 of the fixture, and the test fails
  if the fixture disagrees with it. Regenerate with
  `scripts/gen_render_parity.py`, which pins the revision it passes to
  `Readout` as `tokenizer_revision`.
- **Python JSON.** 37 fixed points, produced by Python's own `json.dumps`: the
  33 payloads above replayed through `DecodeJSON` then `PythonJSON`, plus
  scalar, float, string and nested values. The fixtures constrain the encoder
  rather than being shaped by it. Duplicate object keys, `-0`, out-of-range
  exponents and Python's `str.strip()` whitespace set have their own unit tests;
  each was checked against CPython's `json` module directly.
- **Tokenizer identity.** The test checks the loaded tokenizer's vocab size
  (262,144) and `<channel|>` id (101) before running the vectors, so a fallback
  to a different revision fails loudly. Set `SHISA_DE_TOKENIZER_REQUIRE=1` to
  make a missing tokenizer.json a failure instead of a skip.
- All 256 code ids in `data/codebook-de2.json` match
  `AutoTokenizer.from_pretrained("shisa-ai/shisa-de-2")`, and `<channel|>` is
  token 101.
- `shisa-ai/shisa-de-1` and `shisa-ai/shisa-de-2` share that tokenizer
  (vocab 262144), so code ids are not family-specific.
- **Speed.** The 256-option repeated prompt (27,810 bytes, 8,580 ids) encodes
  in 18 ms on an Intel Xeon E5-2690 v4, against 13 ms for `daulet/tokenizers`
  (Hugging Face's Rust implementation through cgo) on the same machine. The
  tokenizer normalizes a whole prompt into a single pre-token, so the merge
  runs over tens of thousands of symbols in one call; that is why the merge is
  heap-based rather than a repeated scan.

Numbers measured with the Python client are the Python client's. This port has
produced no measurements of its own against a served model.
