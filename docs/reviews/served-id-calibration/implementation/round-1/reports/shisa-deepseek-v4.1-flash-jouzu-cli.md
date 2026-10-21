# Review Report

## Summary

The change is small, coherent, and does what the scope says: `model` is read off both response shapes, `CodeRead.ServedModel` carries it, and `servedRecord` moves the applied record onto the checkpoint the server named without mutating the client. The staged and image paths both use it, `Answer.Calibration` / `Answer.ServedModel` make the result visible per answer, and `Decision.Meta` is left alone so the Python parity fixtures still replay.

Four things do not hold up. The live suite was not re-run and now contradicts the change on the deployed endpoint: three live tests derive their expectation from the configured record, and one of them is described in `docs/PORT.md` as passing with the served record applied. The served record bypasses `Calibration.Applicability`, so it can apply a fit whose readout version this client no longer reproduces — the exact invariant AGENTS.md states. `Decision.Meta["calibration"]` now names a record that did not temper the answers. And the `served == m.Calibration.Model` short-circuit makes the new rule do nothing in the one non-echoing configuration the offline fixtures actually contain.

I could not run `gofmt`, `go vet`, `go test` or `git diff`: no shell tool is available in this session, so the scope's validation results are taken as reported and the parent commit's blobs were not inspected. Everything below is from the working tree at `de33223` (`.git/refs/heads/main`).

## Findings

### HIGH: The live suite contradicts the new record selection, and PORT.md says it passes
- Severity: HIGH
- Confidence: HIGH
- Location: live_test.go:1355, live_test.go:1019-1030, live_test.go:644-653, docs/PORT.md:1033-1036
- Problem: `TestLiveCalibrationMatchesTheServedCheckpoint` computes `want := model.Calibration.TemperatureFor(...)` — the *configured* record — and compares it with `answer.Temperature`. Against the hosted endpoint the commit measured (`SHISA_DE_MODEL=shisa-ai/shisa-de-2` served by DE-2.1) the configured record is DE-2's and the answer now carries DE-2.1's, so the test fails on its own comparison (e.g. 1.98 against 2.69 for `direct` noul). `TestLiveDE2ThinksWhenTheRepeatedReadIsUnsure` does the same at live_test.go:1019-1022 (`6.8` against `5.23` for `think`). `TestLiveParityAgainstPython` compares `Calibrated`, `Temperature` exactly and every probability within `liveTolerance` (1e-9) against the Python client, which has no served-id logic and therefore applies DE-2's fit to DE-2.1's weights: the measured pair in the scope differs by 4.7e-2, so the gate fails. `docs/PORT.md:1033-1036` states the opposite — "the client now moves that choice onto the id the server reported, so `SHISA_DE_MODEL=shisa-ai/shisa-de-2` served by DE-2.1 passes with DE-2.1's record applied" — and the test's own doc comment at live_test.go:1302-1309 still says the alias "cannot be seen through". The commit's validation line covers `go test ./...`, which skips the live suite, so none of this was exercised.
- Recommendation: derive the live expectations from the record the answer reports (load the record for `answer.ServedModel`/`answer.Calibration`, or assert `answer.Calibration` equals the served checkpoint's record and take `want` from that), rewrite the stale doc comment, and make the parity gate say what it can compare: either record both clients with the same record supplied (`--calibration`) or report a calibration difference as the known divergence instead of a parity failure. Correct the `PORT.md` paragraph and add the live run to the commit's validation list once it passes.

### MEDIUM: The served record is applied without an applicability check, so a fit against a different readout version can be applied
- Severity: MEDIUM
- Confidence: HIGH
- Location: client.go:1414-1427
- Problem: `servedRecord` checks the family (inside `CalibrationForCheckpoint`), `record.Model == checkpoint` and `record.Fitted()`, then returns `(record, true)`. It never consults `CalibrationCompatibleWith(record.ReadoutVersion, m.ReadoutVersion)`, which is the check `Applicability` performs and which AGENTS.md states as the rule: "A calibration record is applied on its own only to the checkpoint it was fitted on, under a readout its fit still describes." Today both shipped records are `de2-codebook-v1` and so is this port, so nothing triggers it. The next readout bump does: `m.CalibrationApplied` becomes false and `Health` warns "answers are raw probabilities", and then the aliased path applies the shipped record anyway because the served id differs from both `m.Model` and `m.Calibration.Model`.
- Recommendation: return `(record, true)` only when `CalibrationCompatibleWith(record.ReadoutVersion, m.ReadoutVersion)` holds (and keep the family check); otherwise return the configured record with the configured `applied`, so the mismatch warning stays true. A test with a doctored served record at an older readout version would pin it.

### MEDIUM: `Decision.Meta["calibration"]` can name a record that did not temper the answers
- Severity: MEDIUM
- Confidence: HIGH
- Location: client.go:1344 (and the `Meta` doc at client.go:236-241)
- Problem: `Meta["calibration"]` is always `m.Calibration.ID()`, while the answers may have been tempered by the served checkpoint's record; `calibration_by_head` reports only the boolean `answer.Calibrated`. So on an aliased endpoint a caller reading `decision.Meta` — documented as "the provenance: model, family, readout version, policy and calibration" — attributes the probabilities to DE-2's fit when DE-2.1's produced them. The repo's evidence rule is explicit about not presenting a number under the wrong provenance. `Answer.Calibration` is correct, so the information exists; the decision-level block contradicts it.
- Recommendation: derive the block from what the answers carry. When every answer reports the same non-empty `Calibration`, put that id in `Meta["calibration"]` (and its model in a new key only if a name that cannot collide with Python's can be found); when they differ, keep the configured id and set `calibrated` to nil as the code already does for a mixed call. The parity fixtures are unaffected because `servedRecord` never moves off the configured record in any recorded case — worth confirming by running `TestClientParity` after the change, and worth stating in `PORT.md`.

### MEDIUM: The `served == m.Calibration.Model` short-circuit makes the rule a no-op when the held record is the family default
- Severity: MEDIUM
- Confidence: MEDIUM
- Location: client.go:1415-1417
- Problem: The family record's `model` is `shisa-ai/shisa-de-2` (`data/calibration-de2.json`). A caller configured with an id that names no released checkpoint — `de2-v4-lr5e5-e3-s7`, the training-arm shape the README describes — holds that record with `CalibrationApplied == false`, and a server that reports `"model": "shisa-ai/shisa-de-2"` (the hosted endpoint does this for any id it is asked for; the scope measured it for `shisa-ai/shisa-de-1`) hits the `served == m.Calibration.Model` branch and returns `(configured, false)`. The answer stays raw even though the server named a released checkpoint and the client holds exactly that checkpoint's fit, which is the case the new rule exists for. The offline fixture `decide-…` with `declared_model: de2-candidate-lr5e5` is the same shape and passes only because of this branch.
- Recommendation: in that branch, when `applied` is false and `ReleasedCheckpoint(served)` is the held record's checkpoint (and the record is fitted and readout-compatible), return `(m.Calibration, true)`. Note the consequence: that fixture's Python answer is raw, so the case would need either a recorded exception or an expectation the replay can express. If the current behaviour is deliberate, say so in `PORT.md` and `README.md`, which currently read as "the record for the id the server named is applied".

### LOW: A supplied record's disagreement with the served id is recorded but never reported
- Severity: LOW
- Confidence: HIGH
- Location: client.go:1415 (`m.calibrationSupplied`)
- Problem: `servedRecord` returns the supplied record as given, which is right, but nothing says that the response named a different checkpoint. `Answer.ServedModel` and `Answer.Calibration` do carry both facts, so a caller can compare them, but the client that already warns at construction for a family record on a checkpoint with its own record says nothing here.
- Recommendation: at minimum document on `Answer.ServedModel` that it is not checked against `Answer.Calibration` for a supplied record, and in `README.md`'s paragraph about passing `Calibration`. A warning cannot be appended after construction, and a `Meta` key would break parity, so the answer fields are the right place.

### LOW: `noteServed` takes the first id and ignores later blocks, the fallback and the thought request
- Severity: LOW
- Confidence: HIGH
- Location: readout.go:765-771, readout.go:816
- Problem: The comment asserts "Every request in the read names the same model, so the first response that reports an id settles it", which is an assumption rather than a check. A read that spans two blocks and gets two different ids silently uses the first; a server that names a model only in the `prompt_logprobs` fallback response leaves `ServedModel` empty, because `letterLogprob` never calls `noteServed`. Mixing logprobs from two checkpoints into one softmax is not a read this client can interpret, and it now has the evidence to say so.
- Recommendation: call `noteServed` in `letterLogprob` too, and either record a differing id (a second field, or refuse the read) or state in the comment that the first id is trusted and a mixed read is undetectable. A test with two blocks carrying different ids would pin whichever choice is made.

### LOW: The served record is re-parsed from the embedded JSON on every answer
- Severity: LOW
- Confidence: HIGH
- Location: client.go:1422
- Problem: `CalibrationForCheckpoint` → `LoadCalibration` reads and unmarshals the record (including `test_ece`) per answer, so a multi-head `Decide` against an aliased endpoint parses the same record once per head, and every call re-parses.
- Recommendation: resolve the served record once (at construction for the configured id, and lazily per checkpoint behind a small map or `sync.Once`) and hand `servedRecord` the already-parsed value.

### LOW: `doctor` and `Health` never show the id that decided the record
- Severity: LOW
- Confidence: HIGH
- Location: client.go:795-806 (`Health`), cmd/shisa-de/doctor.go:84-100, README.md:333-336
- Problem: `Health` reports the configured record, its level and `calibration_applied`, and nothing about `ServedModel`. On an aliased endpoint `doctor` therefore prints "calibration shisa-ai/shisa-de-2|… / applied yes" while the answers are tempered by DE-2.1's fit, and `README.md`'s "`shisa-de doctor` prints the record it chose and the checkpoint it was fitted on … so the answer is visible before it is trusted" is no longer sufficient.
- Recommendation: report the served id from the read probe (the answer already carries it) under a new report key, and say in the README that the calibration line is the configuration while `Answer.Calibration` is what tempered a given answer.

### LOW: PORT.md's claim about what the parity fixtures cover is wrong
- Severity: LOW
- Confidence: HIGH
- Location: docs/PORT.md:416-419
- Problem: "the parity fixtures are generated from Python and therefore cover only the echoing case" is not what the fixture holds: `decide-aliased-checkpoint` and the `de2-candidate-lr5e5` case both carry a response `model` that differs from the configured `DecisionModel.Model`. They are the reason a `Meta` key broke `TestClientParity`, and they are also the cases the short-circuit above keeps passing. A reader cannot tell from this sentence that the non-echoing shape is present but inert.
- Recommendation: describe what the fixtures actually contain — the response id differs from the configured id in two cases, and the client leaves them on the configured record because the held record's model equals the served id — and say which of those is intended.

## Things Checked

- HEAD is `de33223df779249827c54dcf7cfde9d904349caa` per `.git/refs/heads/main`, matching the scope's target; the commit message is `.git/COMMIT_EDITMSG`.
- `endpoint.go:85-88` and `endpoint.go:118-121` add `Model` to `completionResponse` and `chatResponse` as a typed `string`, consistent with the port's field-level decoding policy; a null or absent `model` leaves `""`, which every consumer treats as "reported nothing".
- `readout.go:179-184` documents `CodeRead.ServedModel`; `readout.go:765-771` defines `noteServed` and `readout.go:816` calls it after every successful block response and before `firstChoice`, so a rejected request (including the `logprob_token_ids` 400/422 downgrade) never contributes an id, and the downgrade retry cannot double-set one.
- `readout.go:1167-1178` sets `ServedModel: response.Model` directly in `ReadImage`; the image path makes one request, so there is no block question there.
- `client.go:1259-1261` (image) and `client.go:1276-1281` (staged) both call `servedRecord`; the staged path uses `read.Read()` — the final component — which is the read the answer's distribution comes from, so the `think` fit is selected from the read it describes. `covered` is now computed from the chosen record; today it is equivalent to the previous expression because the chosen record is either the configured one (a supplied record short-circuits) or a shipped record, and both shipped records carry `Reads`.
- `servedRecord` reads only fields and returns a value copy, so a shared `DecisionModel` does not change under concurrent callers; `TestAServedIdMovesTheRecordOntoTheWeightsItNames` asserts `model.Calibration.Model` is unchanged afterwards.
- `answerFromRead` (client.go:1435-1470) sets `Calibration` only when a temperature other than 1.0 was applied, so an image read or a read the chosen record does not cover reports `""` rather than naming a record that did nothing.
- The three new tests: only `TestAServedIdMovesTheRecordOntoTheWeightsItNames` fails if `servedRecord` is reverted to `return m.Calibration, applied`; the supplied-record and echoed-id tests pass under that reversion because they pin constraints rather than the feature. None covers the image path, the multi-block/fallback path, the family-record short-circuit, or a served id naming a checkpoint with no fitted record.
- The parity replay (`client_parity_test.go`) configures `DecisionModel.Model` from `declared_model` while the readout posts `fixture.Model`, and every recorded response names `shisa-ai/shisa-de-2`; that equals the configured record's model in every case, so `servedRecord` returns the configured record and `Meta` is unchanged. `Answer.ServedModel` is not in `ToDict`/`ToWire`, so the answer comparison is unaffected.
- `Answer.Calibration` and `Answer.ServedModel` are exported fields with doc comments; `README.md:326-331`, `AGENTS.md:43-49` and `docs/PORT.md:405-427` describe the mechanism.
- Read `calibration.go` in full: `CalibrationForCheckpoint` checks the record's family but not the readout version; `Applicability` is the only caller of `CalibrationCompatibleWith`; the family record's `model` is `shisa-ai/shisa-de-2`, which is what the short-circuit compares against.
- Read `policy.go` in full: `PolicyRead.Read()` is the last component, `Requests`/`PromptTokens`/`LogicalReads` are unchanged, and the gate still reads raw probabilities.

## Residual Risks

- The live suite was not run here, and by inspection it cannot pass against the endpoint the commit measured. Whether it failed before the commit in some other configuration is not something I could check without the parent commit's blobs.
- The whole feature rests on the response `model` being the truth about the weights. A proxy that rewrites ids — including one that rewrites them to a string containing a released checkpoint's name, which `ReleasedCheckpoint` matches as a token — now changes which temperatures are applied. Nothing in the client can distinguish that from a gateway holding a real alias, and `docs/PORT.md` does not say so.
- `Answer.ServedModel` is per answer, not per decision, so a call whose heads were answered by different checkpoints looks uniform in `Decision.Meta`. That is a property of the parity constraint, not of this commit alone.
- The live parity gate's meaning is now conditional on both clients holding the same record. Once the gate is adjusted, the next regeneration of `testdata/client_parity.json` against the deployed endpoint will record Python's raw-on-alias answers, and the Go replay will diverge unless the fixture is recorded with an explicit `Calibration` or the aliased case is excluded deliberately.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "HIGH",
      "title": "Live suite contradicts the new record selection; PORT.md says it passes",
      "location": "live_test.go:1355, live_test.go:1019-1030, live_test.go:644-653, docs/PORT.md:1033-1036",
      "recommendation": "Derive the live expectations from the record the answer reports, make the parity gate handle the clients' differing records explicitly, fix the stale doc comment and the PORT.md claim, and re-run SHISA_DE_LIVE=1 before claiming the live reads validated this."
    },
    {
      "severity": "MEDIUM",
      "title": "Served record applied without a readout-version compatibility check",
      "location": "client.go:1414-1427",
      "recommendation": "Require CalibrationCompatibleWith(record.ReadoutVersion, m.ReadoutVersion) before returning (record, true), so a fit against a superseded readout stays held rather than applied."
    },
    {
      "severity": "MEDIUM",
      "title": "Decision.Meta calibration can name a record that did not temper the answers",
      "location": "client.go:1344",
      "recommendation": "Derive Meta[\"calibration\"] from the record the answers report when they agree, keep the configured id and a nil calibrated flag when they do not, and confirm TestClientParity still replays."
    },
    {
      "severity": "MEDIUM",
      "title": "served == m.Calibration.Model short-circuit makes the rule a no-op for a held family record",
      "location": "client.go:1415-1417",
      "recommendation": "Apply the held record when the served id names its checkpoint and it is fitted and compatible, or document that an uncovered configured id stays raw even when the server names a covered checkpoint."
    },
    {
      "severity": "LOW",
      "title": "Supplied record's disagreement with the served id is never reported",
      "location": "client.go:1415",
      "recommendation": "Document on Answer.ServedModel and in the README that a supplied record is not compared with the served id, since the answer fields are the only place the disagreement can appear."
    },
    {
      "severity": "LOW",
      "title": "noteServed takes the first id and ignores later blocks, the fallback and the thought request",
      "location": "readout.go:765-771, readout.go:816",
      "recommendation": "Record the fallback response's id as well, and either report a differing id across blocks or state that the first id is trusted and a mixed read cannot be detected."
    },
    {
      "severity": "LOW",
      "title": "Served record is re-parsed from embedded JSON on every answer",
      "location": "client.go:1422",
      "recommendation": "Resolve and cache the record per checkpoint once instead of calling CalibrationForCheckpoint per head."
    },
    {
      "severity": "LOW",
      "title": "doctor and Health never show the id that decided the record",
      "location": "client.go:795-806, cmd/shisa-de/doctor.go:84-100, README.md:333-336",
      "recommendation": "Report the served id from the probe and say in the README that the calibration line is the configuration while Answer.Calibration is what tempered an answer."
    },
    {
      "severity": "LOW",
      "title": "PORT.md misstates which parity fixtures exist",
      "location": "docs/PORT.md:416-419",
      "recommendation": "Describe the two recorded cases whose response model differs from the configured id and say why the client leaves them on the configured record."
    }
  ]
}
END_REVIEW_JSON
