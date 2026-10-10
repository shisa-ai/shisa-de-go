# Round 2 coverage

Reviewed revision: `e0a4baf` (fix commit `a76057a` on top of `12a1a29`).

`ds4` and `glm` both reviewed and both cleared every round-1 fix. Findings: two
MEDIUM (a repeated key written by `PythonJSON`, and `json.RawMessage` bypassing
the UTF-8 check) and ten LOW. All are addressed in `044542e`.

`astra` failed again with the same overload error and produced no review, so its
coverage is still missing. Round 3 re-runs it.
