# Review Report

## Summary

The main per-checkpoint selection works for ordinary DE-2 and DE-2.1 names, and the revised live check now sends a request and inspects answer metadata. Five issues remain: Unicode boundaries can produce matches Python rejects, missing record paths can still select bundled calibration, explicit overrides produce misleading warnings, one README section retains the incorrect alias advice, and the live check uses the wrong calibration key when a question thinks.

Reviewed the working tree in `/home/morpheus/data/dev/shisa/shisa-de-go`; its `main` ref points to `12faffa0d594ce0287990d971b1bf120650a3957`. No files were modified or tests run.

## Findings

### MEDIUM: ASCII folding does not preserve Python’s checkpoint boundaries
- Severity: MEDIUM
- Confidence: HIGH
- Location: `calibration.go:96–113`
- Problem: Folding only the slug’s ASCII characters does not make the matcher a conservative subset of Python’s rule. Non-ASCII characters adjacent to the slug can lowercase into ASCII letters. For example, `shisa-de-2.1K` ends with U+212A, the Kelvin sign. Go leaves that character unchanged and accepts its first byte as a boundary, returning DE-2.1. Python lowercases it to `shisa-de-2.1k`, whose trailing `k` prevents a match. `Kshisa-de-2.1` fails similarly at the leading boundary. Consequently, the Go client can automatically apply a checkpoint’s fit where Python would leave the model unrecognized. The current multibyte-boundary tests cover Japanese characters but not characters whose lowercase mapping changes boundary membership.
- Recommendation: Make boundary validation account for Python’s lowercase mapping, or conservatively reject adjacent characters whose mapping could continue the checkpoint name. Add leading and trailing U+212A regression cases, and correct the subset claim in `docs/PORT.md` and the implementation comments.

### MEDIUM: Missing extensionless record paths still resolve to bundled calibration
- Severity: MEDIUM
- Confidence: HIGH
- Location: `calibration.go:575–623`
- Problem: Path precedence only applies to `.json` names or paths that already exist as non-directories. A missing path such as `/fits/shisa-de-2.1/deployment` or `./fits/shisa-de-2.1/deployment` falls through to `ReleasedCheckpoint`, which finds the directory’s slug and returns the bundled DE-2.1 record. The API accepts arbitrary record-file paths, not only `.json` files, so deleting or misspelling an extensionless deployment record silently changes the calibration instead of producing an error. This is the same substitution the round-2 fix intended to prevent.
- Recommendation: Recognize explicit filesystem syntax before checkpoint matching, including absolute, `./`, `../`, and home-relative paths, regardless of existence or extension. Define the remaining checkpoint/file ambiguity explicitly rather than applying the permissive served-name matcher to every argument. Add missing extensionless-path tests alongside the existing `.json` cases.

### MEDIUM: Explicit calibration overrides make the new warnings report the wrong selection
- Severity: MEDIUM
- Confidence: HIGH
- Location: `client.go:711–723`
- Problem: The disagreement warning always says “the model id chose the record,” even when `config.Calibration` supplied the actual record. For example, `Model: "shisa-ai/shisa-de-2"`, `Tokenizer: "shisa-ai/shisa-de-2.1"`, and an explicit DE-2.1 record correctly apply the override, but the warning falsely reports selection by the DE-2 model id and tells the caller to pass the override already supplied.

  The reverse configuration also defeats the supplied-family warning: with model DE-2.1, tokenizer DE-2, and an explicit DE-2 family record, `Applicability` returns `checkpoint` because it matches either name. The `level == ApplicabilityFamily` guard therefore suppresses the warning about using the family record instead of DE-2.1’s own record. The remaining disagreement warning falsely implies that the model id selected DE-2.1’s fit, although DE-2’s fit is applied.
- Recommendation: Make disagreement diagnostics distinguish automatic selection from an explicit override and name the record actually held. Determine whether a supplied bundled family record differs from the model-selected record directly, rather than relying on applicability that can be satisfied by the tokenizer. Add tests crossing both disagreement directions with explicit DE-2 and DE-2.1 records.

### MEDIUM: The local-server instructions still recommend the ineffective alias remedy
- Severity: MEDIUM
- Confidence: HIGH
- Location: `README.md:431–433`
- Problem: The “Use a local server” section still instructs callers to pass the alias as `Model` and the real checkpoint as `Tokenizer`, without limiting that advice to aliases that name no released checkpoint. This contradicts the corrected calibration section. A reader following this instruction for DE-2.1 served as `shisa-ai/shisa-de-2` still gets DE-2’s calibration; setting the tokenizer does not override that selection. This leaves the round-1 documentation failure present in another setup path.
- Recommendation: Qualify the tokenizer advice as applying only to aliases that name no released checkpoint. State here that an alias naming a different released checkpoint requires an explicit `Calibration` record, and link to the corrected example.

### LOW: The live calibration check uses a strategy name as a calibration-table key
- Severity: LOW
- Confidence: HIGH
- Location: `live_test.go:1342–1356`
- Problem: `TemperatureFor(answer.Type, answer.Strategy)` works for `direct` and `repeat2`, but a thinking answer has strategy `repeat2-think` while the calibration table is keyed by `think`. If the live question crosses the gate, the implementation correctly applies the `think` temperature, but this test expects `1.0`. The comparison with the other checkpoint’s record also reads the missing key and reports equal temperatures. The test therefore fails on a correctly calibrated thinking answer.
- Recommendation: Translate `repeat2-think` to `think`, or derive the final read’s stage as the client does. Cover the translation with a deterministic offline test so the check does not depend on whether the live spam question happens to think.

## Things Checked

- Per-checkpoint record selection, model-before-tokenizer precedence, family fallback, explicit overrides, and applicability normalization.
- DE-2.1’s embedded temperatures and provenance, including its use of DE-2’s tokenizer.
- Python’s reference checkpoint matcher, lowercase handling, boundary expression, and calibration resolution.
- Offline tests for released names, record provenance, file precedence, disagreement warnings, and supplied family records.
- The rewritten live calibration check and the client’s distinction between answer strategy and final calibration stage.
- Calibration and alias guidance in `README.md`, `AGENTS.md`, `docs/PORT.md`, and the command-line flag descriptions.
- The restored DE-2 readout page and its Python measurement attribution.

## Residual Risks

- No shell execution tool was available, so repository status and the full commit-range diff were not independently inspected. Review conclusions are based on the referenced working-tree files.
- The scope’s reported formatting, vet, test, and race-test results were not rerun.
- No live endpoint was contacted. Calibration fit quality and the identity of weights served behind an alias remain outside this static review.
- Byte-for-byte identity of the copied documentation and calibration artifacts was not independently verified.

BEGIN_REVIEW_JSON
{
  "findings": [
    {
      "severity": "MEDIUM",
      "title": "ASCII folding does not preserve Python's checkpoint boundaries",
      "location": "calibration.go:96-113",
      "recommendation": "Account for Unicode lowercase mappings at checkpoint boundaries and add leading and trailing U+212A regression cases."
    },
    {
      "severity": "MEDIUM",
      "title": "Missing extensionless record paths still resolve to bundled calibration",
      "location": "calibration.go:575-623",
      "recommendation": "Treat explicit filesystem paths as paths regardless of extension or existence, and test missing extensionless records."
    },
    {
      "severity": "MEDIUM",
      "title": "Explicit calibration overrides make selection warnings misleading",
      "location": "client.go:711-723",
      "recommendation": "Report the actual supplied or automatically selected record and detect family-record overrides independently of tokenizer-based applicability."
    },
    {
      "severity": "MEDIUM",
      "title": "Local-server documentation retains the ineffective alias remedy",
      "location": "README.md:431-433",
      "recommendation": "Limit tokenizer fallback advice to unrecognized aliases and require explicit Calibration when the alias names another released checkpoint."
    },
    {
      "severity": "LOW",
      "title": "Live calibration check uses the wrong key for thinking answers",
      "location": "live_test.go:1342-1356",
      "recommendation": "Map repeat2-think to the think calibration entry and cover the mapping with an offline test."
    }
  ]
}
END_REVIEW_JSON
