# How this port maps onto the Python reference

`github.com/shisa-ai/shisa-de` is the reference implementation. `docs/READOUT-DE2.md`
and `docs/READOUT.md` are copied from it verbatim and remain the specification;
they are not edited here. This page records the mapping and every place the Go
client deliberately behaves differently.

## File mapping

| Python | Go |
| --- | --- |
| `shisa_de/readout.py` | `readout.go`, `render.go` |
| `shisa_de/policy.py` | `policy.go` |
| `shisa_de/calibration.py` | `calibration.go` |
| `shisa_de/family.py` | `family.go` |
| `shisa_de/questions.py` | `questions.go` |
| `shisa_de/client.py` | `client.go` |
| `json.dumps` / `json.loads` | `pyjson.go` |
| `shisa_de/overflow.py` | not ported; DE-1 only |
| `shisa_de/images.py` | not ported yet |
| `shisa_de/data/*.json` | `data/*.json`, embedded with `go:embed` |
| `shisa_de/cli.py` | `cmd/shisa-de/` |
| `tests/test_offline.py` | `*_test.go` with a stub tokenizer and `httptest` |
| `tests/test_live.py` | live tests behind `SHISA_DE_LIVE=1` |

The tokenizer has no Python counterpart as a module: the Python client calls
`transformers.AutoTokenizer`. This port implements the tokenizer in
`tokenizer/` so the library needs neither cgo nor a Python runtime.

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

**No cgo.** The tokenizer is pure Go. `sugarme/tokenizer` was evaluated and
rejected: it does not match added tokens inside a rendered prompt, and it took
14 to 35 seconds on an 8,580-token prompt. `daulet/tokenizers` (cgo to
Hugging Face's Rust implementation) is exact and fast but needs a 49 MB
platform-specific static library.

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

- **Tokenizer.** 1,051 strings, 48,296 token ids: this package's ids equal
  `transformers`' ids on every one. The corpus covers all 24 added tokens and
  their prefix pairs, all 256 codes, the rendered scaffold, JSON payloads,
  Japanese, emoji, combining marks, control characters, 60 KB of real text from
  both repositories, and 400 random strings. Regenerate with
  `scripts/gen_tokenizer_parity.py`.
- The rendered DE-2 repeated prompt for the section 2 spam question is
  byte-identical to the Python render and to the page, 238 tokens.
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
