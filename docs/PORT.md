# How this port maps onto the Python reference

`github.com/shisa-ai/shisa-de` is the reference implementation. `docs/READOUT-DE2.md`
and `docs/READOUT.md` are copied from it verbatim and remain the specification;
they are not edited here. This page records the mapping and every place the Go
client deliberately behaves differently.

## File mapping

| Python | Go |
| --- | --- |
| `shisa_de/readout.py` | `readout.go`, `render.go`, `endpoint.go` |
| `shisa_de/policy.py` | `policy.go` (planned) |
| `shisa_de/calibration.py` | `calibration.go` (planned) |
| `shisa_de/family.py` | `family.go` |
| `shisa_de/questions.py` | `questions.go` |
| `shisa_de/client.py` | `client.go` (planned) |
| `json.dumps` / `json.loads` | `pyjson.go` |
| `httpx` | `net/http` in `endpoint.go` |
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
question types and the Python-JSON writer; M3 landed the DE-2 readout and its
HTTP layer. Rows marked planned are M4 or later.

## Deliberate differences

**DE-2 only.** This port implements `de2-codebook-v1`. A model that resolves to
DE-1 is an error. The Python client reads DE-1 correctly, so this is a missing
feature rather than a contract change: it must fail loudly, never fall back to
DE-2 reads. The Python client makes that decision in `DecisionModel`; this port
makes it in `NewReadout` as well, because a `Readout` can be held directly and
it is the thing that spends requests. Resolution is `ResolveFamily`'s order --
a declaration, then a slug in the model id, then a slug in the tokenizer source,
otherwise assumed DE-2 -- and the source is kept on the readout so an assumption
is reported as one.

**No Jinja.** The Python client renders with
`tokenizer.apply_chat_template(...)`, which runs the checkpoint's own Jinja
template. This port renders the two message shapes the readout uses with a
pinned function. It is byte-identical for those shapes and for no others, which
is the whole of what the readout needs. The system line is a parameter, as it is
in the Python client (`Readout(system_prompt=...)`); the thinking render always
uses the thinking line whatever the parameter holds, because that line is part
of the read rather than a preference.

What guards the pin: `TestRenderParity` compares 35 rendered prompts byte for
byte against the Python client, and that is the check on the render itself. The
runtime checks catch a different failure -- a client/server tokenizer mismatch.
Every read checks that the prompt ends on `AnswerPrefix`, and the first read
checks the server's `usage.prompt_tokens` against this client's; both compare
two tokenizations of the same bytes the client sent, so neither can see a render
that disagrees with the checkpoint's Jinja template.

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

**Resolution has an explicit local-only policy.** `ResolveWith` and `LoadWith`
take a `ResolveOptions{LocalFilesOnly: true}`, which returns `ErrNotCached` for a
source that is neither a readable file nor already in the hub cache, instead of
fetching it. `Resolve` and `Load` are the same functions with that policy off,
so the M1 call sites are unchanged.

**`Decode` reproduces the pinned decoder, including its invalid-byte
handling.** A run of consecutive byte-fallback tokens decodes to the run's text
when the bytes are valid UTF-8, and otherwise to one replacement character per
byte. Hugging Face's `ByteFallback` does the same; a plain concatenation of the
bytes would not, and neither would `from_utf8_lossy`, which consumes maximal
invalid subsequences. `Encode` never produces such a run for the pinned
vocabulary, so this only matters for hand-built or model-generated id lists.

**Errors instead of exceptions.** `ReadoutError` and `QuestionError` replace the
Python exception classes; callers use `errors.As`. `ReadoutHTTPError` keeps its
`status` and `detail` fields and is a `ReadoutError` as well, which is what the
Python hierarchy already is (`ReadoutHTTPError(ReadoutError)`): a handler for a
failed read catches a rejected request without naming both types. One place the
Go version is stricter is a response whose `choices` is missing or empty: the
Python client indexes `data["choices"][0]` and raises `KeyError` or
`IndexError`, while this port returns a `ReadoutError` saying which read came
back without a choice.

**The readout is stricter about four things the Python client tolerates.** Each
is a case where the Python client picks something and this port refuses to:

- The `prompt_logprobs` fallback reads the first entry of the last prompt
  position, whatever the map holds. `prompt_logprobs: 0` asks for the prompt's
  own token and nothing else, so a response with more than one entry there is a
  server answering a different question; this port errors rather than scoring a
  token nobody asked about. A null entry at that position is an error too, where
  the Python client reads `None` through and reports the code as unreturned.
- A `token_ids` element that is not a JSON integer is an error, including
  `null`. Go's decoder turns a null element into token 0 and leaves it, so a
  plain `[]int` would continue a thought with a token the server never
  produced; the Python client checks the element type and this port does too.
- `CodeRead.Sampled` is a `string`, not `str | None`. A response that omits
  `choices[0].text` gives `""` here and `None` there. Nothing reads the value as
  anything but provenance, and Go's zero value carries the same meaning.
- One response body is capped at 64 MB before it is read. The Python client
  reads whatever arrives. The cap has to clear the largest honest response,
  which is the `prompt_logprobs: 0` fallback's one entry per prompt token rather
  than one position of top-logprobs, so it is sized for a prompt far longer than
  any DE-2 prompt is.

**Go's zero values cannot express two of the Python constructor arguments.**
`ReadoutConfig.SystemPrompt` of `""` renders `DirectSystem`, so Python's explicit
empty system line cannot be asked for; and `ReadoutConfig.MaxLogprobs` of 0 sends
`logprobs: 20`, where Python's `max_logprobs=0` sends `logprobs: 0` and makes
every code fall back. Both mappings are documented on the fields; neither is
reachable from a normal caller.

**Field-level decoding is typed where Python coerces.** `usage.prompt_tokens`,
`rank`, `choices[0].text` and a model entry's `id` are decoded into Go types, so
a string where a number belongs, or a number where a string belongs, fails the
response rather than being coerced. The Python client's `int(... or 0)`,
`entry.get("rank")` and `entry.get("id")` accept the coercion and yield `None`
for a missing id where this port yields `""`. No OpenAI-compatible server sends
any of these shapes.

**A mid-read capability downgrade re-reads every code, as Python does.** When a
server rejects `logprob_token_ids` after an earlier block was accepted, both
clients drop the field and ask again for the whole option list in one request,
so the retry's top-k replaces the codes the earlier blocks already read. The
replay fixture's `codes-late-downgrade` scenario pins this with a server whose
top-k scores differ from its candidate-id scores.

**`LocalFilesOnly` is threaded into tokenizer resolution.** The Python client
passes `local_files_only=True` to `from_pretrained`; this port passes the same
flag to `tokenizer.LoadWith`, which returns `tokenizer.ErrNotCached` instead of
fetching when the source is neither a readable file nor already in the hub
cache.

The Python client also accepts any JSON value from the endpoint and rejects a
non-object after parsing; this port decodes straight into the object type, so a
JSON array fails with the same "did not return a JSON object" error.

**Lazy loading.** The Python client loads the tokenizer inside
`Readout.ensure_tokenizer`. The Go client does the same, holding the tokenizer
and the resolved slots behind one mutex, so concurrent first calls serialize on
one load and a failed load leaves nothing behind to be retried.

**Concurrency.** `ThreadPoolExecutor` becomes goroutines with a bounded worker
pool.

## Verified parity

Every check recorded here is offline: it was produced on 2026-10-10 by running
the Python client and `transformers` over the pinned snapshot
(`shisa-ai/shisa-de-2@8f21734b27b3462471c4662d5638e52195b005a3`) and checking the
results in as fixtures. No requests were made to a served model, so nothing below
carries a serving fingerprint and nothing below is a measurement of a served
checkpoint. The Python client is `shisa-de` `0.4.0`.

This port has produced no measurements of its own against a served model.

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
- **Render.** All 35 fixture renders are byte-identical to what
  `Readout._payload` and `Readout.render` produced for the same case: both
  template shapes (the repeated/direct read and the thinking read), a
  caller-supplied system line for each shape, the empty-description rule,
  non-string descriptions, 17 evidence shapes, widths 2/26/27/77/256, and long
  evidence. Each fixture prompt also re-tokenizes to the same ids the Python
  client produced. The readout page's 238 and 137 token counts are pinned; the
  fixture's third pin (216, the Japanese choice) is a regression check and is
  not a published value.
  `testdata/render_parity.meta.json` records the model, the revision, the
  library versions, the counts and a sha256 of the fixture, and the test fails
  if the fixture disagrees with it. Regenerate with
  `scripts/gen_render_parity.py`, which pins the revision it passes to
  `Readout` as `tokenizer_revision`.
- **Python JSON.** 39 fixed points, produced by Python's own `json.dumps`: the
  35 payloads above replayed through `DecodeJSON` then `PythonJSON`, plus
  scalar, float, string and nested values. The fixtures constrain the encoder
  rather than being shaped by it. Duplicate object keys, `-0`, out-of-range
  exponents, invalid UTF-8 and Python's `str.strip()` whitespace set have their
  own unit tests; each was checked against CPython's `json` module directly.
- **Readout.** 21 scenarios, 61 recorded exchanges, replayed request for request
  and field for field. Each exchange records the method and the path as well as
  the bodies, so the request line is checked too. The scenarios cover: widths 2,
  26, 27, 128, 129 and 256 (the last two crossing the 128-candidate block
  boundary), a server that honours `logprob_token_ids`, one that ignores it, one
  that rejects it with a 400 and one that accepts the first block and rejects the
  second (so the downgrade happens mid-read and the retry's top-k replaces the
  codes already read), a server that keys the logprobs by decoded text rather
  than `token_id:<id>`, a server that sends no `choices[0].text`, the debug read,
  the thinking read closed by the model and cut by the budget, the read after a
  thought, the fallback after a thought (where the code is appended as a token id
  rather than as text), and four failures: a server whose `usage.prompt_tokens`
  disagrees with the client's tokenization, a fallback that returns nothing, a
  thinking response with no `token_ids`, and a prompt that does not end on the
  terminator (which sends no request at all). Every request body the Go client
  sends is compared to the Python client's after JSON decoding, and every result
  -- logprobs, probabilities, request and prompt-token counts, missing codes,
  ranks, sampled text, debug top-logprobs, thought ids and closure -- is compared
  to the Python client's. The stub tokenizer is rebuilt from the fixture's
  recorded word list, so a divergence in the two implementations surfaces as a
  wrong token count rather than passing. `testdata/readout_parity.meta.json`
  records the Python revision, the readout version, the limits, the counts and a
  sha256 of the fixture; the test fails if the fixture disagrees with it.
- **Readout behaviour outside the replay.** The response shapes the fixture does
  not carry have their own tests: a completion with no choices, a server that
  reports no `usage`, a null or fractional `token_ids` element, a null
  `prompt_logprobs` entry (which used to panic), a body past the response cap,
  the DE-1 refusals for a model id, a tokenizer source and a declaration, the
  `ReadoutHTTPError`-is-a-`ReadoutError` taxonomy, the bearer header and the
  model listing, a tokenizer swap discarding the ids resolved from the old one,
  and eight concurrent readers. `go test -race` is what decides the concurrency
  claim.
- **Tokenizer identity.** The test checks the loaded tokenizer's vocab size
  (262,144) and `<channel|>` id (101) before running the vectors, so a fallback
  to a different revision fails loudly. Set `SHISA_DE_TOKENIZER_REQUIRE=1` to
  make a missing tokenizer.json a failure instead of a skip.
- **Codebook.** The 256 codes and their order are the research repository's
  single-token answer-boundary audit
  (`evals/reports/de2-token-code-audit.json`), whose `token_ids` were resolved on
  `google/gemma-4-26B-A4B-it@4d7ae4984b7db7de8f8457170b3f1a419ee76d52`.
  `data/codebook-de2.json` records that provenance. The code is the contract and
  the ids are not read at runtime; the client resolves each code's id from the
  served tokenizer. What ties the two together is the tokenizer identity check
  above: `shisa-ai/shisa-de-1` and `shisa-ai/shisa-de-2` share the Gemma
  tokenizer (vocab 262,144), so a code's id is not family-specific.
- **Speed.** The 256-option repeated prompt (27,810 bytes, 8,580 ids) encodes
  in 18 ms on an Intel Xeon E5-2690 v4, against 13 ms for `daulet/tokenizers`
  (Hugging Face's Rust implementation through cgo) on the same machine. The
  tokenizer normalizes a whole prompt into a single pre-token, so the merge
  runs over tens of thousands of symbols in one call; that is why the merge is
  heap-based rather than a repeated scan.
