# Review Scope: de2.1-checkpoint-calibration

- Mode: implementation
- Stage: implementation
- Target: commits ecc90ff..12faffa (HEAD), against bb503d4/v0.1.0

## Summary

Round 2 of the review of per-checkpoint calibration selection in the Go port of the Shisa DE-2 client.

Round 1 found one HIGH (reported by all three reviewers) and eight MEDIUM/LOW. Commit 33357ae fixes them:

HIGH: the README documented `Tokenizer` as the remedy for an alias whose id is itself a released checkpoint, and that does nothing -- `CalibrationForCheckpoint` returns at the model id, `Applicability` then reports `checkpoint`, and DE-2's fit is applied to DE-2.1's answers with no warning. I did not adopt the reviewers' preferred fix of preferring the tokenizer, because DE-2.1 is served with DE-2's tokenizer (the record's own `fitted_on` says so), so preferring the tokenizer would apply DE-2's fit to DE-2.1 in the reverse configuration, and refusing the disagreement would leave that correct configuration raw. Instead the docs now describe the code (tokenizer is a fallback for an id that names no released checkpoint; the alias case needs `Calibration`), and the detectable part is reported: a warning when the model id and tokenizer name different released checkpoints, and another when a supplied record is the family's while the served checkpoint has its own record. Tests pin both directions.

MEDIUM: `ResolveCalibration` matched the permissive served-name rule before loading a file, so `/fits/shisa-de-2.1/deployment.json` returned the bundled record without opening the file. Now a `.json` suffix or an existing regular file is loaded as a path first, and a misspelt `.json` path is an error. Tested with a real file under a checkpoint-named directory and a missing one.

MEDIUM/LOW: the live check never made a request (the client is lazy), derived its expectation only from the model id, and compared the record's temperature table with itself. It now reads through the endpoint, derives the expectation the way the client does (model id, then tokenizer), asserts the answer's own `Temperature` and `Calibrated`, requires a non-empty per-read table, and fails if the other checkpoint's record shares the temperature it is checking.

LOW: `ReleasedCheckpoint` folded case with `strings.ToLower`, which maps U+0130 to `i` where Python gives `i` plus U+0307, so `SH\u0130SA-DE-2.1` named DE-2.1 here and nothing there. It now folds ASCII only, which recognises a subset of Python's names and never one Python does not; cases added for U+0130, U+017F and multi-byte boundaries. Match order moved from a sort over a map to an ordered slice, because Python's stable sort keeps table order among equal-length slugs. `Applicability` now normalises the record's own model too, so a record fitted on a suffixed repo name reaches `checkpoint` level for it. Provenance assertions now run over every shipped record, not just DE-2's.

LOW: `docs/READOUT-DE2.md` had Go spellings (`Answer.Strategy`, `Calibration: &record`, Go record paths) in a page claimed byte-identical to the Python reference. The page is now restored to byte-identical, and PORT.md records the Go adaptations plus a note that the page's measurements were collected with the Python client and that this port embeds the records without reproducing any fit.

Known non-goals: no DE-1 record, no `strip_state_quotes`, no readout version bump, and no live run against a DE-2.1 endpoint (the deployment is still in progress). The alias case remains undetectable from a response by construction; the port reports disagreements it can see and documents the one it cannot.

## Primary files / regions

- `calibration.go` — reason: ReleasedCheckpoint (ASCII fold, ordered slice), Applicability normalisation, ResolveCalibration path handling
- `client.go` — reason: The two new warnings: disagreeing names, and a supplied family record
- `calibration_test.go` — reason: Case-fold table, provenance over every record, applicability normalisation, path resolution
- `client_test.go` — reason: The disagreement and supplied-family warning tests
- `live_test.go` — reason: Rewritten live check
- `README.md` — reason: The corrected alias, tokenizer, fine-tune and family-spelling documentation
- `docs/PORT.md` — reason: Recorded adaptations and the measurement attribution
- `AGENTS.md` — reason: Corrected alias rule
- `cmd/shisa-de/main.go` — reason: Flag usage now says what a family spelling means

## Review questions

- Does the README now describe what the code does for every configuration it mentions, in particular the alias case, the tokenizer fallback, the fine-tune case and the family spelling?
- Is the conflict warning correct in both directions, and does it fire on any configuration that is actually correct without saying something false?
- Can ResolveCalibration still resolve a spec to the wrong record: a checkpoint id that looks like a path, a relative file that does not exist, a family name that collides with a checkpoint?
- Is the live test now able to fail for the reason it claims, and can it still pass while the wrong record is applied?
- Does the ASCII-only fold keep ReleasedCheckpoint a subset of Python's rule for every input, or does it invent a match Python would not make?
- Did restoring the readout page to byte-identical with Python leave any Go-specific statement stranded, and does PORT.md now record every adaptation?

## Validation already run

- gofmt -l . (clean)
- go vet ./... (clean)
- go test ./... -count=1 (all packages pass)
- go test -race ./... -count=1 (all packages pass)
- docs/READOUT-DE2.md diffed against the Python reference (identical)
- Live suite not run: needs a DE-2.1 endpoint
