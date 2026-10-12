# shisa-de-go — Agent Guide

Go client library for Shisa DE decision models. This file exists to prevent the
four mistakes that would make the library quietly wrong: changing a readout
without bumping its version, writing a number into a doc that nobody measured,
applying one checkpoint's calibration to another, and reading a checkpoint
through the other family's contract.

## Summary

- `shisade` sends typed questions to a served DE model and reads the answers
  back. It never loads weights. The only text it generates is DE-2's bounded
  thought, which is read past, not returned as an answer.
- **This port implements DE-2 only** (`de2-codebook-v1`). DE-1 is not read.
  A model that resolves to DE-1 is an error, never an assumption: reading a
  checkpoint through the wrong family's contract returns answers nobody has
  measured, and this client has no DE-1 answers to return. The guard is
  `RequireDE2`, called from `NewReadout` before anything is loaded or sent, so a
  caller holding a `Readout` directly cannot bypass it. `ResolvedFamily` reports
  which contract was chosen and whether it was declared, named by the model id,
  named by the tokenizer source, or assumed.
- `docs/READOUT-DE2.md` is the contract and is copied verbatim from the Python
  repository. The code implements that page; the page does not describe the
  code. If they disagree, the page is right and the code is a bug — unless the
  page itself was wrong, in which case both change together. `docs/READOUT.md`
  (DE-1) is copied too, because DE-2 reuses its scaffold, but nothing in it is
  implemented here.
- Source of truth for readout identities: `readout.go` (`ReadoutVersionFor`, one
  current version per family, and `calibrationCompatible`).
- A calibration record is applied on its own only to the checkpoint it was
  fitted on, under a readout its fit still describes. The DE-2 record holds one
  fit per read (`direct`, `repeat2`, `think`), each applied only to that read.
- Every answer records its provenance (`Family`, `ReadoutVersion`, `Policy`,
  `Strategy`, `Calibration`, `Temperature`, `Calibrated`, `Requests`) so a
  caller can tell which read produced it. `Answer` and `Decision` carry them;
  `Decision.Meta` carries the call-level block.
- The Python package is the reference implementation. Where this port and the
  Python client disagree on a wire shape, the Python client and the readout page
  win; report the divergence instead of quietly matching the Go code to itself.

## Project Overview

Three layers, one contract. This is the target layout: M1 and M2 landed the
codebook, the tokenizer, the renderer, family resolution, the question types and
the Python-JSON writer; M3 landed the DE-2 readout itself; M4 landed the policy,
the calibration records and the friendly layer; M5 landed the command line; M6
landed the live suite and its parity gate; M7 landed the image read. The Key
Files table below says what is in the tree.

- `readout.go` — the raw reads: render the repeated prompt, request every
  option's code by token id, softmax over the codes, optionally generate a
  bounded thought and read the codes again after it, and read one image question
  through `/v1/chat/completions`.
- `images.go` — the image reference: a local PNG/JPEG/WebP file, an HTTP(S) URL
  or a data URL becomes the `image_url` content part, and nothing else does.
- `endpoint.go` — the HTTP layer under those reads: the completion and chat
  request shapes, the response shapes, and the error conventions.
- `policy.go` — sequences the reads: twice, then think when unsure.
- `calibration.go` — temperature scaling, the confidence statistic, record
  selection, and `Calibration.Applicability`.
- `client.go` — the friendly layer. `Classify` for label sets, `Decide` for
  typed questions.
- `cmd/shisa-de/` — the command line, and the only place that prints. It is a
  second consumer of the library's Python-compatibility surface
  (`PythonJSONWith`, `PythonStr`, `PythonRepr`, `TrimPythonSpace`,
  `MaxLogprobs`), so a change to one of those has to keep both clients' output
  identical. Where it can, it uses the library's Python spelling rather than
  Go's: `PythonStr` for a float in a message, `TrimPythonSpace` for a strip,
  `pythonQuote` for a `!r`.

What must not change casually:

- The system line, the user JSON keys (`evidence`, `criterion`, `options`), the
  repeated-turn separator, the thinking system line, and the DE-2 request bodies
  (`max_tokens: 1`, `temperature: 0`, `logprobs: 20`, `logprob_token_ids`,
  `return_tokens_as_token_ids`). These are the measured scaffold.
- The image read's shape: the `image_url` content part before the text part, one
  direct read with no policy, the letter limit (26), the absence of a letter
  fallback, and no calibration. `docs/READOUT-DE2.md` section 6 specifies it.
- The codebook and its order, the gate (0.7), the option cap (26), the budget
  (1,024), the candidate-id block size (128), and the rule that the client
  writes the closing `<channel|>` after a thought.
- Changing any of the above changes answers, so bump the readout version and
  update `docs/READOUT-DE2.md` in the same commit.
- The empty-description rule: a label with no description renders as the label.
  Blank descriptions give the model indistinguishable options.
- The boundary check and the prompt-token check. They are the only things
  standing between a tokenizer mismatch and silently reading the wrong
  distribution.

## Key Files

| File | Purpose |
| --- | --- |
| `docs/READOUT-DE2.md` | The DE-2 contract: codebook, repeated read, code request, thinking read, serving, measured costs. Copied verbatim; the spec |
| `docs/READOUT.md` | The DE-1 contract, copied for the shared scaffold. Not implemented here |
| `docs/PORT.md` | How the Go port maps onto the Python reference, and where it deliberately differs |
| `README.md` | The API surface users see first |
| `readout.go` | Rendering, code resolution, boundary checks, requests, thought generation, image reads, softmax, readout versions |
| `images.go` | `PrepareImage` and `ValidateImageURL`: a local file, an HTTP(S) URL or a data URL into the `image_url` a chat request carries |
| `endpoint.go` | The completion and chat request and response shapes, the JSON POST/GET helpers, and `ReadoutHTTPError` |
| `render.go` | The pinned Gemma chat-template renderer for the two DE-2 message shapes |
| `pyjson.go` | `json.dumps(value, ensure_ascii=False)` and `json.loads` equivalents, because the user turn is serialized into the prompt |
| `questions.go` | `Noul`, `Choice`, `Score` and their wire shapes |
| `family.go` | Resolves DE-1/DE-2 from a declaration, the model id or the tokenizer, and says which |
| `tokenizer/` | The pure-Go Hugging Face BPE tokenizer. No cgo. `LoadWith`/`ResolveWith` take a local-only policy |
| `data/codebook-de2.json` | The 256 DE-2 answer codes, in order, with the audit they came from |
| `data/calibration-de2.json` | The DE-2 record: temperatures per read with the fit's provenance, and one withheld earlier fit |
| `data/python-printable.json` | Python's `str.isprintable()` over every code point, as 736 ranges, with the interpreter's Unicode version. `client.go`'s `pythonQuote` reads it instead of Go's `strconv.IsPrint`, which is the same rule against a different Unicode version |
| `pythonprintable.go` | The embedded table, its validation, and the binary-search lookup |
| `policy.go` | The DE-2 policy: repeated read, gate, thought, read after it; `ReadOptions` and the settings that select a read |
| `calibration.go` | Temperature scaling, the confidence statistic, record selection, and `Calibration.Applicability` |
| `client.go` | `DecisionModel.Classify`, `DecisionModel.Decide`, `SystemOne`, `Health`, `Decision`, `Answer`, `HeadSpec` |
| `scripts/gen_images_parity.py` | Records the Python client's verdict for every image reference this port accepts, and the Go test replays it |
| `scripts/gen_client_parity.py` | Records live Python `DecisionModel` exchanges against the hosted endpoint and writes the fixture the parity test replays |
| `example_test.go` | The README's snippets as `Example*` functions `go test` runs, against a stub endpoint, so a signature or behaviour change breaks them |
| `readme_test.go` | The documentation rot check: every `shisade.X` in `README.md` and `docs/PORT.md` is an exported identifier, the Go blocks parse, and the README's sections are present |
| `live_test.go` | The live suite behind `SHISA_DE_LIVE=1`, including the M6 gate: the fixed question set in `testdata/live_questions.json` through both clients in one session |
| `scripts/live_parity.py` | Runs the fixed live question set through the Python client and records the answers the Go test compares against; invoked by the test, and by hand to inspect one |
| `scripts/test_live_parity.py` | Offline tests for the recorder's endpoint and model resolution, its tokenizer pin and its drift arithmetic |
| `scripts/gen_python_printable.py` | Generates `data/python-printable.json` and `testdata/python_repr.json` from the running interpreter |
| `scripts/gen_pyjson_dumps.py` | Generates `testdata/pyjson_dumps.json`: `json.dumps` over `indent` and `ensure_ascii`, which is what the command line prints |
| `questions_test.go` | The validation rules Python cannot reach: a repeated option key, and a repeated label in a multi-label head |
| `cmd/shisa-de/` | `shisa-de doctor`, `ask`, `explain`. `main.go` is the parser and the shared flags, `flags.go` the option shapes `flag` lacks, `state.go` the state and label helpers, and one file per command |

## Workflow Expectations

### Before Starting

- Read `docs/READOUT-DE2.md` for anything that touches rendering, codes,
  requests, policy, or scoring.
- `git status -sb`; the repo is small and commits are frequent.
- Check whether the endpoint you are testing against is the hosted one or a
  local vLLM, and record which one produced any number you keep.
- `go env GOCACHE GOTMPDIR TMPDIR` before building. Use the inherited values;
  see the global guide.

### During Work

- One logical unit per commit. A readout change and its doc update are one unit.
- New behavior gets a test against the stub tokenizer and a stub server, and a
  live test only when the behavior depends on the served model.
- Keep the package importable without network access. The tokenizer is loaded
  lazily, on the first read, and nothing downloads at package init.
- Go style: `gofmt`, exported identifiers documented, errors wrapped with `%w`
  and tested with `errors.Is` / `errors.As`. No cgo. No `init()` that does work.
- Prefer returning `error` over panicking. Panics are reserved for programming
  errors in caller-supplied arguments that the type system cannot express.

### Before Claiming Done

- `go build ./...`, `go vet ./...`, and `go test ./...` pass. The last one
  includes the `Example*` functions and the documentation check, so a change to
  an exported name, a struct field, or an answer's behaviour fails the suite
  until `README.md` and `example_test.go` are updated in the same commit. That
  is deliberate: it is the only thing keeping the README true.
- A readout version identifies the answer-producing contract, not the client
  build or the contents of `readout.go`. Bump it only when rendering, requests,
  code handling, policy, or scoring semantics change, and update
  `docs/READOUT-DE2.md` in the same commit. Do not bump it for
  implementation-only fixes such as locks, lazy initialization, refactoring, or
  error handling that leave the contract unchanged. Track those with the package
  version. Editing `readout.go` alone is never a reason to bump a readout
  version.
- When you bump a version, decide its `calibrationCompatible` entry in the same
  commit. List the predecessors only if the direct text read is answer-identical
  to theirs; otherwise leave them out, which stops every record fitted against
  them from being applied until it is re-fitted. Never edit a calibration
  record's `readout_version` to make it match: it records what the fit was made
  against.
- Any number added to a doc was measured, and the doc says when and against
  which serving fingerprint.

## Verification

| Scope | Commands |
| --- | --- |
| Build and vet | `go build ./...` and `go vet ./...` |
| Offline suite | `go test ./...` |
| Documented examples | `go test . -run Example` (runs the `Example*` functions in `example_test.go`, whose `// Output:` blocks are the README's snippets) |
| Documentation rot | `go test . -run TestReadme` (every `shisade.X` the README and `PORT.md` name has to be an exported identifier; the blocks also have to parse) |
| Tokenizer parity | `go test ./tokenizer/` (reads the pinned tokenizer from the hub cache or `SHISA_DE_TOKENIZER_JSON`; `SHISA_DE_TOKENIZER_REQUIRE=1` fails instead of skipping) |
| Readout replay | `go test . -run TestReadout` (replays recorded Python request/response pairs, text and image, through an `httptest` stub; offline) |
| Image reference replay | `go test . -run TestImagesParity` (replays recorded Python `prepare_image`/`validate_image_url` verdicts and error text; offline) |
| Client replay | `go test . -run TestClientParity` (replays recorded live Python `DecisionModel` exchanges through an `httptest` stub; offline) |
| Race detector | `go test -race ./...` (the concurrency claim for `Readout` rests on this) |
| Live suite, hosted DE-2 | `SHISA_DE_LIVE=1 go test . -run Live` (spends real requests; the parity gate also runs the Python client, so it needs the `shisa-de` checkout, `SHISA_DE_PYTHON_REPO` to point elsewhere) |
| Recorder's own logic | `python3 -m unittest discover -s scripts -p 'test_*.py'` (offline; the endpoint and model resolution, the tokenizer pin and the drift arithmetic, with no network and no `shisa_de` checkout) |
| Endpoint check | `go run ./cmd/shisa-de doctor --model shisa-ai/shisa-de-2`, and `--probe` to spend one question on the read |
| Readout walkthrough | `go run ./cmd/shisa-de explain --model shisa-ai/shisa-de-2 --labels billing,shipping` |

The live suite spends real requests and needs `SHISA_API_KEY`. It is skipped
unless `SHISA_DE_LIVE=1`. `SHISA_DE_MODEL`, `SHISA_DE_ENDPOINT` and
`SHISA_DE_TOKENIZER` point it at another served checkpoint, which is the same
environment contract the Python suite uses.

The served checkpoint is deterministic within a session and not across sessions:
three back-to-back reads of one prompt have come back bit-identical, and the same
prompt a day later has not. So `TestLiveParityAgainstPython` records the Python
client's answers in the same run rather than reading a checked-in file, and the
recorder runs the set twice to measure the server's own repeatability. When those
two passes disagree, the test reports the server rather than the port. Compare
recorded exchanges offline with `TestClientParity` when the port is what is in
question. Both halves resolve `SHISA_DE_ENDPOINT` and `SHISA_DE_MODEL`, and the
gate fails rather than logs when they disagree: two clients against two servers
is the one comparison the design exists to prevent.

`doctor` and `explain` also spend real requests. They read the endpoint from
`SHISA_DE_ENDPOINT` and the key from `SHISA_DE_API_KEY`, then `SHISA_API_KEY`.
The command line's `--model` defaults to `shisa-ai/shisa-de-2`, not the
library's DE-1 `DefaultModel`, because this port has no DE-1 read.

The tokenizer parity suite compares this package's ids against
`transformers`-derived golden vectors checked into `testdata/`, and its decode
outputs against a second fixture. Regenerate both with
`scripts/gen_tokenizer_parity.py`, which pins the revision and writes
`testdata/tokenizer_parity.meta.json`; the test fails if the fixture disagrees
with the provenance. A parity test that has no vectors is a skip, not a pass,
and `SHISA_DE_TOKENIZER_REQUIRE=1` turns that skip into a failure.

The render and readout suites compare against fixtures produced by running the
Python client. `scripts/gen_render_parity.py` writes the rendered prompts, their
token ids and the `json.dumps` fixed points;
`scripts/gen_readout_parity.py` writes every request body and response body the
Python readout sent and received, in order, plus what it made of them. Each
writes a `.meta.json` with the provenance and a digest of the fixture, and each
test fails when the fixture disagrees with it. Both need the Python repository
next to this one; neither needs network access.

`scripts/gen_client_parity.py` is the one generator that spends real requests: it
drives the Python `DecisionModel` against the hosted endpoint through a
forwarding transport and records each exchange. The parity test replays those
recorded responses, so the test itself is offline. Regenerating the fixture needs
a key in `SHISA_DE_API_KEY`, `SHISA_API_KEY` or a bare `API_KEY` (in that order)
and the Python repository. It reads the tokenizer revision from the hub cache and
pins it, so a later run against a different snapshot fails rather than silently
replacing the measurements. The fixture's `.meta.json` records the endpoint, the
served `system_fingerprint`, the date, the tokenizer revision and the readout
version, and the test fails when the fixture disagrees with it.

## Evidence Rules

- Measured numbers carry a date and the serving fingerprint, which is the
  server's `system_fingerprint` and not an endpoint label. The fingerprint in
  `docs/READOUT-DE2.md` (`vllm-0.30.0-709530de`) is the local DE-2 server the
  page's numbers came from. The client parity fixture's responses carried
  `vllm-0.30.0-tp2-ca69195e` on 2026-10-10. Logprobs move between serving shapes;
  a number without its fingerprint is not reproducible.
- The research repository's numbers are its own, not this repo's. Cite them with
  their source and say they were not reproduced here.
- The model card's published numbers are the card's, not this repo's. When a
  card value and a local measurement disagree, report both and say which is
  which.
- Do not present a temperature-scaled probability as a raw one, or the reverse.
  `Calibrated` is on every answer for that reason.
- The hosted checkpoint is deterministic within a session and not across
  sessions. Three back-to-back reads of one prompt came back bit-identical, and
  the recorder's own two passes over the whole live set differ by 0; the same
  prompt a day later answered with the same chosen option and probabilities that
  differed by up to 4e-3. Never compare a live run against an earlier recording
  and call the difference a defect. `TestLiveParityAgainstPython` records both
  clients in one session and measures the server's own repeatability so it can
  say which one moved.
- The port's own live measurements are the M6 gate: the fixed question set
  through both clients in one session, agreeing to 1e-19 up to a few multiples
  of 1e-16, against `vllm-0.30.0-tp2-ca69195e` on 2026-10-10. The worst key moves
  between runs, so quote the range rather than one run's maximum. Anything else
  measured here is the Python client's result replayed, and should be said that
  way rather than restated as if this code had been measured against a server.
- The Python suite's `test_de2_thinks_when_the_repeated_read_is_unsure` expects
  `calibrated is False`, which is stale against the hosted `shisa-ai/shisa-de-2`
  -- the checkpoint the shipped record was fitted on, where the Python client
  returns `True` at 6.8 today -- and correct against any other served checkpoint,
  where the record does not apply. `docs/PORT.md` records both sides. The Go test
  derives the expectation from the model's own applicability rather than writing
  the temperature down; do not replace it with a literal.

## Git Discipline

Conventional commit prefixes, imperative mood, subject at 72 characters or
fewer, body bullets for the change and the validation.

- Commit immediately when a logical unit is complete and verified.
- Never `git add .`, `git add -A`, or `git commit -a`. Stage exact paths.
- Leave unrelated work alone. Do not `git restore`, `git checkout --`,
  `git reset --hard`, or `git clean -fd` unless asked.
- A local commit does not authorize a push, tag, or release. Ask first.
- No AI attribution or co-author footers.

## Coordination Hygiene

- `docs/READOUT-DE2.md` and `README.md` are the high-conflict files; two agents
  editing them at once will conflict. Coordinate before editing either.
- `docs/PORT.md` carries one paragraph per deliberate difference. A difference
  that is not in it is an accident, not a decision.
- Readout versions live in one place, `readout.go`: one per family in
  `ReadoutVersionFor`. Do not define a copy elsewhere.
- The Python repository is the reference. Do not change it from here, and do not
  copy a fix into this port without checking whether it belongs upstream too.

## Blockers

- If the served checkpoint's tokenizer is unavailable, the readout cannot run
  and the boundary check cannot pass. Stop and report; do not guess token ids.
- If a live result contradicts `docs/READOUT-DE2.md`, treat the page as the spec
  and the result as a finding. Report the discrepancy with the fingerprint
  rather than editing the page to match one run.
