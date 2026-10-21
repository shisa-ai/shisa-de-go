# Review Scope: served-id-calibration

- Mode: implementation
- Stage: implementation
- Target: commit de33223 (HEAD), against 954adba

## Summary

The Go port of the Shisa DE-2 client selects its calibration record per released checkpoint. A model id can be an alias: the hosted endpoint answers shisa-ai/shisa-de-2 with DE-2.1's weights, and until now the client applied DE-2's temperatures to them, reporting 0.914502148 where DE-2.1's own fit gives 0.961568819 on the same raw read.

This commit uses the one signal a response carries. A gateway holding an alias reports the canonical id it served: measured 2026-10-21 against https://api.shisa.ai/openai, fingerprint vllm-0.30.0-tp2-c2e7e64c, a request naming shisa-ai/shisa-de-2 returns "model": "shisa-ai/shisa-de-2.1" on both /v1/chat/completions and /v1/completions.

What changed in de33223:
- endpoint.go: chatResponse and completionResponse gain Model, the id the server says it served.
- readout.go: CodeRead gains ServedModel, set from the first response in a read that names one (readCodes, via a noteServed closure over the block loop) and directly in ReadImage.
- client.go: new servedRecord(served, applied) decides the record per answer. It returns the configured record when the served id is empty, equals the configured model, equals the configured record's model, or when the caller supplied a Calibration; otherwise, when ReleasedCheckpoint(served) names a checkpoint with its own fitted record, it returns that record and reports it applied. It never mutates DecisionModel.Calibration, so a client shared across goroutines does not change underneath callers. Both answer paths use it (image and staged), and the staged path recomputes `covered` from the chosen record's reads.
- client.go: Answer gains Calibration (the record that tempered it, "" when raw) and ServedModel. answerFromRead sets both.
- Decision.Meta is deliberately unchanged: the parity fixtures are generated from Python, which has no served-id logic, and adding keys broke TestClientParity for two fixtures whose response model differs from the configured id. The served id is reported on the answer instead.
- Tests: three new tests (served id moves the record; a supplied record is not overruled; an echoed id changes nothing), plus README, AGENTS.md and docs/PORT.md corrections.

Known non-goals: no DE-1 record; no readout version bump, since the answer-producing contract is unchanged; no change to the Python client, which does not do this and can therefore differ on an aliased endpoint; the parity fixtures cannot cover the aliased case because they are generated from Python.

## Primary files / regions

- `client.go` — reason: servedRecord, the two call sites, Answer fields, answerFromRead
- `readout.go` — reason: CodeRead.ServedModel and the noteServed closure in the block loop
- `endpoint.go` — reason: Model on both response shapes
- `client_test.go` — reason: the three new tests
- `README.md` — reason: the alias paragraph now describes detection instead of impossibility
- `AGENTS.md` — reason: the alias rule in the source-of-truth list
- `docs/PORT.md` — reason: the served-id difference from the Python client and the live-check paragraph

## Review questions

- Can any response produce a record that was not fitted on the weights it is applied to, or apply a record whose readout version does not describe the read?
- Is the noteServed closure correct when a read spans several blocks and the fallback path runs, and can a later block's id be silently ignored when it should not be?
- Is refusing to overrule a supplied Calibration right, or should a disagreement be reported somehow? Today it is silent.
- Does leaving Decision.Meta unchanged hide a real change from callers, given that Meta['calibration'] can now name a record that did not temper the answers?
- Is the served-id rule safe against a server that reports a model id the client did not ask for and that is not what it served, such as a proxy rewriting ids?
- Are the three tests enough to pin the behaviour, and does each fail if servedRecord is reverted?
- Do the README, AGENTS.md and PORT.md claims match what the code does in every case, including the echoing server and the supplied record?

## Validation already run

- gofmt -l . (clean)
- go vet ./... (clean)
- go test ./... -count=1 (all packages pass)
- go test -race ./... -count=1 (all packages pass)
- live, 2026-10-21, https://api.shisa.ai/openai, fingerprint vllm-0.30.0-tp2-c2e7e64c: shisa-ai/shisa-de-2 reports served=shisa-ai/shisa-de-2.1, temp 1.98, prob 0.961568819, matching shisa-ai/shisa-de-2.1
