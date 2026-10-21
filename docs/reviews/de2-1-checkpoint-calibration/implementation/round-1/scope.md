# Review Scope: de2.1-checkpoint-calibration

- Mode: implementation
- Stage: implementation
- Target: commit ecc90ff (HEAD), against bb503d4/v0.1.0

## Summary

The Go port of the Shisa DE-2 client now selects its calibration record per released checkpoint instead of per family, because Shisa DE-2.1 shipped as new weights over the same de2-codebook-v1 contract with its own fitted temperatures. DE-2's temperatures over-correct DE-2.1 (measured: ECE 0.034/0.045 read once and 0.048/0.042 read twice, the read-twice noul worse than raw), so applying the wrong record is worse than leaving answers raw.

What changed in commit ecc90ff:
- New data/calibration-de2.1.json, copied byte for byte from the Python client (the reference implementation), which fitted it. Readout version unchanged: de2-codebook-v1 for both checkpoints.
- calibration.go: new CheckpointCalibrationFiles map, new exported ReleasedCheckpoint(name) implementing Python's rule (exact id, or a name containing a released checkpoint's slug as its own token, with ASCII boundaries so shisa-de-2.10/2.2/21 and training arms like de2-v4-lr5e5-e3-s7 do not match), new CalibrationForCheckpoint(family, model, tokenizer), CalibrationFor(family) kept for the uncovered case, Applicability now normalises served names through ReleasedCheckpoint, ResolveCalibration accepts a checkpoint id.
- client.go: one call site switched to CalibrationForCheckpoint.
- cmd/shisa-de: --calibration help and the empty-spec error now name the checkpoints.
- Tests: name-matching table mirroring Python's, per-checkpoint record selection, applicability normalisation, client-level selection for six served ids, a test that the two records temper differently, and a live test gated on SHISA_DE_LIVE=1 that the applied record belongs to the served checkpoint.
- Docs: README (per-checkpoint records, the temperature table, how the record is chosen, an explicit warning that an alias which outlives the weights it named cannot be detected), docs/READOUT-DE2.md (ported the DE-2.1 section from Python's page), docs/PORT.md (four recorded differences), AGENTS.md (source of truth for record coverage).

Known non-goals: this port still reads DE-2 only and has no DE-1 record, so CheckpointCalibrationFiles deliberately omits shisa-ai/shisa-de-1 even though Python's table lists it. There is no strip_state_quotes counterpart. No readout version bump, since the answer-producing contract is unchanged. No live run against a DE-2.1 endpoint: the deployment is still in progress.

Context: the deployment plan is that Shisa DE-2.1 will be served with shisa-ai/shisa-de-2 and shisa-ai/shisa-de-1 retained as aliases. Once that happens the id shisa-ai/shisa-de-2 will name DE-2.1's weights, and no client can detect it from a response, so both this port and the Python client will apply DE-2's fit to DE-2.1 unless the caller passes Tokenizer or Calibration. I want that judged: is documenting it plus the Tokenizer/Calibration escape hatch the right call, or does it need a stronger mechanism?

## Primary files / regions

- `calibration.go` — reason: The new name-matching, record selection and applicability normalisation live here
- `data/calibration-de2.1.json` — reason: New embedded record, copied from the Python client
- `client.go` — reason: Record selection call site
- `calibration_test.go` — reason: Name-matching table and record selection tests
- `client_test.go` — reason: Client-level selection and tempering tests
- `cmd/shisa-de/main.go` — reason: CLI checkpoint spelling
- `live_test.go` — reason: New live check, not yet run
- `README.md` — reason: User-facing calibration and alias documentation
- `docs/READOUT-DE2.md` — reason: Ported DE-2.1 section of the contract page
- `docs/PORT.md` — reason: Recorded differences from the Python client
- `AGENTS.md` — reason: Source of truth for record coverage

## Review questions

- Is ReleasedCheckpoint equivalent to the Python regex rule for every input, including boundary cases the test table covers and ones it does not (multi-byte runes, a slug appearing twice, very short names)?
- Does the record-selection order (model id first, then tokenizer) match Python's calibration_for, and is ignoring a tokenizer that names a different checkpoint the right call?
- Can any served name reach checkpoint level and get the wrong record applied, which is the failure this change exists to prevent?
- Is documenting the undetectable alias case (an id outliving the weights it named) with Tokenizer/Calibration as the escape hatch sufficient, or does it need a stronger mechanism such as an explicit checkpoint override or a warning when the id is a known alias?
- Are the docs true after this change, in particular README's claim that everything else in the Python API has a Go counterpart?
- Does the new live test assert something that can actually fail, and would it catch the alias case or silently pass on it?

## Validation already run

- gofmt -l . (clean)
- go vet ./... (clean)
- go test ./... -count=1 (all packages pass)
- go test -race ./... -count=1 (all packages pass)
- Live suite not run: needs a DE-2.1 endpoint
