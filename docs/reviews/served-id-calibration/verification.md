# Served-ID calibration verification

Date: 2026-10-21. Endpoint: `https://api.shisa.ai/openai` (production).
Response fingerprint: `vllm-0.30.0-tp2-c2e7e64c`.
Python reference revision: `77a0e83f872b4747082a4f9ad6dd493f06ba5ac2`.
Tokenizer source: `shisa-ai/shisa-de-2`, cached `refs/main` revision
`fd9f487763b4d5b7dc4c719f2704290330a65d62`.

## Mechanical checks

- `go build ./...`, `go vet ./...`, `go test ./... -count=1`: pass.
- `go test -race ./... -count=1`: pass for all three packages.
- `python3 -m unittest discover -s scripts -p 'test_*.py'`: 74 tests pass.
- CLI recording regenerated from the existing Python checkout; no Python client
  files were edited. The Go replay documents the Python-only disabled
  `strip_state_quotes` report field.

## Live checks

| Model requested | Check | Outcome |
| --- | --- | --- |
| `shisa-ai/shisa-de-2` | Automatic calibration | Pass: response names DE-2.1; repeat2 choice temperature 1.64 and DE-2.1 record |
| `shisa-ai/shisa-de-2` | Thinking read | Pass |
| `shisa-ai/shisa-de-2.1` | Automatic calibration | Pass: DE-2.1 record, repeat2 choice temperature 1.64 |
| `shisa-ai/shisa-de-2.1` | Thinking read | Pass |
| `shisa-ai/shisa-de-2` | Python/Go parity, explicit shared DE-2 record | All ten cases pass, including image, thinking and 256 options; Python self-delta 0; largest cross-client probability delta in this run 1.22e-15 |
| `shisa-ai/shisa-de-2.1` | Python/Go parity, explicit shared DE-2.1 record | All ten cases pass; Python self-delta 0; largest cross-client probability delta in this run 2.55e-15 |

Parity uses the unchanged `1e-9` tolerance. The shared-record alias run tests
arithmetic parity, not whether DE-2's fit is appropriate for DE-2.1 weights.
Automatic selection is checked separately.

An earlier alias parity attempt failed the repeatability prerequisite: Python's
own passes differed by 0.011. No tolerance was relaxed. A subsequent attempt
using a local snapshot directory failed because Go's tokenizer repository
resolver does not accept that form. The successful runs used the supported hub
source above. These failed attempts are not counted as passes.

Development endpoint: not tested; its URL has not been supplied.
