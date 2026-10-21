# Changelog

## v0.2.0 — 2026-10-21

DE-2.1 calibration support and response-based checkpoint selection. The
`de2-codebook-v1` readout contract is unchanged.

### Changes

- Bundle separate calibration records for DE-2 and DE-2.1, selected by released
  checkpoint names, including recognized suffixed names.
- Use a different server-reported checkpoint ID to select a compatible fit.
  Unknown response checkpoints stay raw. Missing IDs and echoes of the declared
  or actual request ID preserve configuration.
- Reject conflicting checkpoint identities across candidate blocks, fallback
  requests and thinking stages.
- Preserve explicitly supplied calibration records and report disagreements.
- Preserve applied-record provenance in serialized answers, decision metadata
  and multi-label results. Doctor probes report differing response identities
  and calibration applicability separately from explicit authorization.
- Cache response-selected records privately per client and report load failures.
- Compare Python and Go using the same explicit calibration file in the live
  parity gate; test automatic selection separately. Parse and hash the same
  record bytes, and reject invalid temperatures before making requests.

### Verification

Build, vet, the complete Go offline suite, the race detector and all 82 recorder
tests passed. Production checks passed for the canonical DE-2.1 ID and the DE-2
alias, including automatic calibration and all ten shared-record parity cases.
The final served-ID review round had no HIGH/CRITICAL findings; remaining
findings were fixed with regression coverage. The Python client was not modified.

Detailed measurements, serving fingerprint, reviewer fallback and preliminary
failed attempts are recorded in
[the verification log](docs/reviews/served-id-calibration/verification.md) and
[round-2 disposition](docs/reviews/served-id-calibration/implementation/round-2/disposition.md).
The development endpoint was not tested because its URL was not supplied.

### Commit and tag record

- `a8b2b29`: calibration safety, response identity and provenance fixes.
- `06a78e6`: remaining review findings and regression coverage.
- The annotated local `v0.2.0` tag points to `06a78e6`. This changelog entry was
  added afterward; the tag was not moved. The release commits and tag have not
  been pushed.
