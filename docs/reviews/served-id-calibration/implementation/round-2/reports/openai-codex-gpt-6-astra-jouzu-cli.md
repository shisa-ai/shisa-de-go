# Review Report

## Summary

The main round-1 correctness issues are addressed: unknown response IDs disable automatic calibration, known checkpoints require compatibility, conflicting response identities reject reads, and applied provenance survives serialization and multi-label aggregation.

Three issues remain in probe diagnostics and recorder loading.

Reviewed `/home/morpheus/data/dev/shisa/shisa-de-go`; its branch reference points to `a8b2b29ea6fad341bd3babe36bc2276da69519bc`. No files were modified or tests run.

## Findings

### MEDIUM: Health reports an incompatible explicit record as applicable
- Severity: MEDIUM
- Confidence: HIGH
- Location: `client.go:980–983`, `client.go:1435–1437`
- Problem: `Health` assigns the boolean returned by `servedRecord` to `probe_calibration_applicable`. For an explicit record, that boolean means the caller authorized application, not that the record covers the response checkpoint. For example, explicitly supplying DE-2’s record while the response names DE-2.1 produces `probe_calibration_applicable: true` alongside a warning that the supplied record does not cover that checkpoint. `doctor` consequently prints contradictory diagnostics.
- Recommendation: Compute probe applicability independently with `Calibration.Applicability`, without tokenizer fallback. Keep explicit application authorization separate. Add a Health-level regression test for an explicit DE-2 record against a DE-2.1 response; the current doctor test only renders a fabricated report.

### LOW: The calibration digest can describe different bytes from the loaded record
- Severity: LOW
- Confidence: HIGH
- Location: `scripts/live_parity.py:463–475`
- Problem: `load_record` reads the file into `raw` for hashing, then calls `load_calibration_file`, which opens the file again. A replacement between these reads makes `LoadedRecord.sha256` describe the first contents while `LoadedRecord.record` contains the second. This contradicts the stated guarantee that the digest identifies the bytes actually loaded. The later Go checks can detect many such changes, but do not make the recorder’s provenance correct.
- Recommendation: Parse the captured `raw` bytes through the Python client’s existing dictionary loader and hash that same buffer. Add a regression test proving that record construction does not reopen the source file. No Python client changes are needed.

### LOW: Some unusable records reach the network probe before failing
- Severity: LOW
- Confidence: HIGH
- Location: `scripts/live_parity.py:468–475`, `scripts/live_parity.py:685`
- Problem: Successful `load_calibration_file` construction does not establish that temperatures are usable. The Python loader copies temperature values without converting them. A record with otherwise valid provenance and `"temperatures": {"noul": null}` therefore passes `load_record`, spends the image-probe request, and then raises `TypeError` when model construction evaluates `Calibration.fitted`. It exits outside the documented `RecordError`/exit-5 path. The fake loader tests do not exercise this deferred validation.
- Recommendation: Validate every temperature table and value before the capability probe, converting unusable records into `RecordError`. Test null and nonnumeric temperature values and assert exit 5, no probe calls, and no output files.

## Things Checked

- Response model decoding for completions, thoughts, fallbacks, and image reads.
- Identity reconciliation across candidate blocks and thinking stages.
- Automatic selection, unknown IDs, neutral aliases, explicit overrides, and readout compatibility.
- Private lazy cache initialization and isolation from exported calibration maps.
- Answer serialization, uniform/mixed decision metadata, and expanded multi-label provenance.
- Historical replay exceptions and independent automatic-selection coverage.
- Shared-record live parity, digest/identifier checks, and retained numerical, request, and strategy assertions.
- Round-1 reports and disposition, README guidance, PORT documentation, and the verification record distinguishing failed attempts from successful runs.

## Residual Risks

- No shell tool was available, so the commit delta and working-tree cleanliness could not be independently verified. Review was against the inspected working tree.
- Reported build, test, race, and live results were not rerun.
- Response model IDs remain trusted server assertions, not authentication of deployed weights.
- Tokenizer provenance is inferred from the source and filesystem after execution, rather than captured directly from the loaded tokenizer; concurrent cache changes remain an attribution risk.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "Health conflates explicit application with checkpoint applicability",
      "location": "client.go:980-983",
      "recommendation": "Compute response checkpoint applicability independently of explicit application authorization and add a Health regression test."
    },
    {
      "severity": "LOW",
      "title": "Calibration digest and record are obtained from separate file reads",
      "location": "scripts/live_parity.py:463-475",
      "recommendation": "Parse and hash the same captured byte buffer instead of reopening the calibration file."
    },
    {
      "severity": "LOW",
      "title": "Invalid temperatures can spend a probe request before rejection",
      "location": "scripts/live_parity.py:468-475",
      "recommendation": "Validate all temperature values before probing and test exit 5 with no requests for malformed records."
    }
  ]
}
END_REVIEW_JSON
