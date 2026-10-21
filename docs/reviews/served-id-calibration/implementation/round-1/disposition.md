# Served-ID calibration: round 1 disposition

Candidate: `de33223`. Reviewers: astra, ds4, glm. All three executed on their
configured primary models. The reports contain four HIGH findings, including a
duplicate live-gate finding; the fixes cover every reported severity.

| Finding | Disposition and coverage |
| --- | --- |
| Unknown response checkpoint keeps configured fit | Disable automatic calibration; regression tests assert raw probability, empty applied record and diagnostic. |
| Neutral alias cannot activate its held family record | Resolve the response checkpoint even when it equals the held record's model; test DE-2 and DE-2.1, including a suffixed ID. |
| Replacement fit bypasses compatibility | Require `Calibration.Applicability` at checkpoint level, without tokenizer fallback; test an incompatible readout. |
| Blocks, fallbacks and thought ignore conflicting identities | Reconcile every contributing response, reject conflicts and propagate sole fallback/thought identity; test consistent, missing and conflicting cases. |
| Applied provenance lost from metadata, serialization or multi-label output | Extend non-echoed answers and decision metadata with applied/configured records and per-head identity; retain expanded heads through aggregation. Mixed records have no single applied record. |
| Explicit record disagreement is silent | Preserve the override and report a per-answer diagnostic without mutating shared client warnings. |
| Health/doctor only describe configuration | Probe reports the differing response model, selected fit and applicability; add JSON and text output coverage. |
| Per-answer embedded JSON parsing | Private lazy per-client record cache; concurrent access and isolation from exported record mutation are tested. |
| Live calibration/thinking tests use configured fit | Derive expected identity, fit and stage independently; verify applied record as well as temperature. |
| Live Python comparison uses different fits | Supply one explicit file to both clients, check bytes/digest/ID, retain all numeric, request and strategy checks. Automatic selection remains a separate test. No Python repository edits. |
| Image response identity lacks coverage | Stub image test checks identity propagation, serialization and uncalibrated output. |
| Contradictory alias docs and unsupported weight proof | Describe server assertions, missing/echoed IDs and explicit overrides; correct historical results and distinguish rounded-value calculations from observed output. |
| Historical replay's two non-echoed cases | Preserve legacy field comparisons, assert the new provenance, and explicitly request raw output for the historical uncovered-checkpoint recording. Automatic activation is independently covered. |

The required CLI recording regeneration picked up Python's disabled
`strip_state_quotes` health field. That feature remains out of scope. The replay
requires its recorded value to be false and excludes only that field from the
legacy JSON comparison; the fixture itself remains unmodified generated output.

Validation and live outcomes are recorded with the fix commit and the next
review scope. A failed live repeatability check is not a parity pass.
