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
  measured, and this client has no DE-1 answers to return.
- `docs/READOUT-DE2.md` is the contract and is copied verbatim from the Python
  repository. The code implements that page; the page does not describe the
  code. If they disagree, the page is right and the code is a bug — unless the
  page itself was wrong, in which case both change together. `docs/READOUT.md`
  (DE-1) is copied too, because DE-2 reuses its scaffold, but nothing in it is
  implemented here.
- Source of truth for readout identities: `readout.go` (`ReadoutVersions`, one
  current version per family, and `calibrationCompatible`).
- A calibration record is applied on its own only to the checkpoint it was
  fitted on, under a readout its fit still describes. The DE-2 record holds one
  fit per read (`direct`, `repeat2`, `think`), each applied only to that read.
- Every answer records its provenance (`Family`, `ReadoutVersion`, `Policy`,
  `Strategy`, `Calibration`, `Temperature`, `Calibrated`, `Requests`) so a
  caller can tell which read produced it.
- The Python package is the reference implementation. Where this port and the
  Python client disagree on a wire shape, the Python client and the readout page
  win; report the divergence instead of quietly matching the Go code to itself.

## Project Overview

Two layers, one contract:

- `readout.go` — the raw reads: render the repeated prompt, request every
  option's code by token id, softmax over the codes, optionally generate a
  bounded thought and read the codes again after it.
- `policy.go` — sequences the reads: twice, then think when unsure.
- `client.go` — the friendly layer. `Classify` for label sets, `Decide` for
  typed questions.

What must not change casually:

- The system line, the user JSON keys (`evidence`, `criterion`, `options`), the
  repeated-turn separator, the thinking system line, and the DE-2 request bodies
  (`max_tokens: 1`, `temperature: 0`, `logprobs: 20`, `logprob_token_ids`,
  `return_tokens_as_token_ids`). These are the measured scaffold.
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
| `readout.go` | Rendering, code resolution, boundary checks, requests, thought generation, softmax, readout versions |
| `policy.go` | The DE-2 policy: repeated read, gate, thought, read after it; `ReadOptions` and the per-model and per-call settings that select a read |
| `client.go` | `DecisionModel.Classify`, `DecisionModel.Decide`, `Health`, `Decision`, `Answer` |
| `questions.go` | `Noul`, `Choice`, `Score` and their wire shapes |
| `calibration.go` | Temperature scaling, the confidence statistic, record selection, and `Calibration.Applicability` |
| `family.go` | Resolves DE-1/DE-2 from a declaration, the model id or the tokenizer, and says which |
| `tokenizer/` | The pure-Go Hugging Face BPE tokenizer. No cgo |
| `data/codebook-de2.json` | The 256 DE-2 answer codes, in order, with the audit they came from |
| `data/calibration-de2.json` | The DE-2 record: temperatures per read with the fit's provenance, and one withheld earlier fit |
| `cmd/shisa-de/` | `shisa-de doctor`, `ask`, `explain` |

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
  lazily, once, behind a `sync.Once`; nothing downloads at package init.
- Go style: `gofmt`, exported identifiers documented, errors wrapped with `%w`
  and tested with `errors.Is` / `errors.As`. No cgo. No `init()` that does work.
- Prefer returning `error` over panicking. Panics are reserved for programming
  errors in caller-supplied arguments that the type system cannot express.

### Before Claiming Done

- `go build ./...`, `go vet ./...`, and `go test ./...` pass.
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
| Tokenizer parity | `go test ./tokenizer/ -run Parity` (needs `SHISA_DE_TOKENIZER_JSON`) |
| Live suite, hosted DE-2 | `SHISA_DE_LIVE=1 go test ./... -run Live` |
| Endpoint check | `shisa-de doctor`, and `shisa-de doctor --probe` to spend two requests on the reads |
| Readout walkthrough | `shisa-de explain --labels spam,ham` |

The live suite spends real requests and needs `SHISA_API_KEY`. It is skipped
unless `SHISA_DE_LIVE=1`.

The tokenizer parity suite compares this package's ids against
`tokenizers`-derived golden vectors checked into `testdata/`. Regenerate them
with the Python repository's `scripts/` when the pinned snapshot changes; a
parity test that has no vectors is a skip, not a pass.

## Evidence Rules

- Measured numbers carry a date and the serving fingerprint. The fingerprint in
  `docs/READOUT-DE2.md` (`vllm-0.30.0-709530de`) is the local DE-2 server the
  page's numbers came from. The hosted endpoint's fingerprint on 2026-10-10 was
  `vllm-0.30.0-tp2-ca69195e`. Logprobs move between serving shapes; a number
  without its fingerprint is not reproducible.
- The research repository's numbers are its own, not this repo's. Cite them with
  their source and say they were not reproduced here.
- The model card's published numbers are the card's, not this repo's. When a
  card value and a local measurement disagree, report both and say which is
  which.
- Do not present a temperature-scaled probability as a raw one, or the reverse.
  `Calibrated` is on every answer for that reason.
- This port has produced no measurements of its own yet. Say so rather than
  restating the Python repository's numbers as if this code had been measured.

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
- Readout versions live in one place, `readout.go`: one per family in
  `ReadoutVersions`. Do not define a copy elsewhere.
- The Python repository is the reference. Do not change it from here, and do not
  copy a fix into this port without checking whether it belongs upstream too.

## Blockers

- If the served checkpoint's tokenizer is unavailable, the readout cannot run
  and the boundary check cannot pass. Stop and report; do not guess token ids.
- If a live result contradicts `docs/READOUT-DE2.md`, treat the page as the spec
  and the result as a finding. Report the discrepancy with the fingerprint
  rather than editing the page to match one run.
