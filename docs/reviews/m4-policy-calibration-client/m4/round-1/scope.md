# Review Scope: m4-policy-calibration-client

- Mode: implementation
- Stage: m4
- Target: M4 implementation (policy.go, calibration.go, client.go, tests, parity fixture)
- Source plan: docs/PORT.md

## Summary

M4 of the shisa-de Python-to-Go port: the DE-2 policy, the calibration records, and the friendly layer (DecisionModel, Classify, Decide, Health). New files policy.go, calibration.go, client.go and their tests, plus scripts/gen_client_parity.py and the live-recorded testdata/client_parity.json fixture. Two small changes to already-reviewed files: a nil-receiver guard in Readout.httpClient and a longer UnsupportedFamilyError message.

The contract is docs/READOUT-DE2.md (copied verbatim from the Python reference at /home/morpheus/data/dev/shisa/shisa-de, which is the specification). docs/PORT.md records every deliberate difference; a difference not listed there is a bug.

What to check hardest:
1. Readout policy semantics against shisa_de/policy.py: the gate (0.7), the read after a thought, the option cap (26), the direct/repeat/repeat-think policies, which read each answer is read under (direct/repeat2/think), and the fallback after a thought. Does the Go code apply the right temperature to the right read?
2. Calibration against shisa_de/calibration.py: the per-read fit selection, the applicability levels, CalibrationForRead, the confidence statistic, and TemperDistribution/TemperBinary arithmetic.
3. client.go against shisa_de/client.py: the answer fields, the Decision views (Values/Answers/Raw/Usage/Meta), the TypeSafe wire shape, the meta and usage key sets, the classify head shorthands and the default questions, the multi-label threshold and its three views, and whether Decide returns the value where Classify returns the label.
4. Any place the Go code silently differs from the Python client on a wire shape or an answer value.

Known non-goals, do not report:
- DE-1 is deliberately not implemented; a DE-1 model is an error. DE-1 code paths in the Python client are out of scope.
- No image path: no shisa_de/images.py port, no chat_system_render probe, input_type is always "text".
- No CLI (cmd/shisa-de) yet; that is M5.
- No live test suite yet; that is M6.
- The Python client's ThreadPoolExecutor becomes goroutines with a sorted work order; that is deliberate.
- ResolveOptions takes an OptionOverrides struct with pointers where Python uses keyword defaults; that is deliberate.

## Primary files / regions

- `policy.go` — reason: the DE-2 read sequencing, the gate, the option cap and ReadOptions resolution
- `calibration.go` — reason: temperature scaling, the confidence statistic and record selection
- `client.go` — reason: the friendly layer: DecisionModel, Classify, Decide, Health, Answer, Decision
- `calibration_test.go` — reason: calibration coverage
- `policy_test.go` — reason: policy coverage
- `client_test.go` — reason: friendly-layer coverage
- `client_parity_test.go` — reason: the replay of the recorded Python exchanges
- `scripts/gen_client_parity.py` — reason: the fixture generator; check it records what the test claims
- `family.go` — reason: the UnsupportedFamilyError message changed
- `readout.go` — reason: the nil-receiver guard in httpClient

## Review questions

- Does the Go policy apply each calibration temperature to the read the fit describes, and does an answer read after a thought stay raw when the record only fits the direct read?
- Does any answer value, probability, level, legend or meta/usage key differ from what the Python client returns for the same recorded exchange?
- Does ReadOptions resolution match shisa_de/policy.py for every family and every layer of overrides, including the aliased names?
- Does the multi-label path produce the same Values shape and the same Answers/Raw split as the Python client?
- Is the client parity test actually able to fail, or can a wrong answer pass because the fixture is compared too loosely?

## Validation already run

- gofmt -l . (clean)
- go vet ./... (clean)
- go test ./... -count=1 (passes)
- go test -race ./... -count=1 (passes)
- go test . -count=1 -run TestClientParity (23 cases, 32 recorded exchanges)

## Diff summary / excerpts

New: policy.go (~516 lines), calibration.go (~457), client.go (~1225), policy_test.go (~649), calibration_test.go (~434), client_test.go (~1043), client_parity_test.go (~759), scripts/gen_client_parity.py, testdata/client_parity.json (23 cases / 32 exchanges), testdata/client_parity.meta.json.
Modified: readout.go (httpClient nil-receiver guard), family.go (UnsupportedFamilyError message names the fix).
Docs: docs/PORT.md (one paragraph per deliberate difference; verified-parity entries for policy, calibration, the friendly layer and the client replay), AGENTS.md, README.md.

The client parity fixture was recorded live against the hosted DE-2 endpoint (shisa-hosted-2026-10-10, 2026-10-10) by driving the Python DecisionModel through a forwarding transport; the Go test replays the recorded responses offline and fails on any request body the Python client never sent.
