# Round 1 coverage

Reviewed revision: `12a1a29` (feat: render the DE-2 prompt and serialize it the
way Python does).

Two reviewers returned findings: `ds4` (DeepSeek-V4.1-Flash) and `glm` (glm-5.3).
Neither reported a HIGH or CRITICAL finding; the findings were two MEDIUM and
eleven LOW, all addressed in `a76057a`.

`astra` did not review. Its report holds only
`[Error: Our servers are currently overloaded. Please try again later.]`. The
harness counted it as an executed reviewer because the CLI exited 0 with a
report file, so `results.json` records `failedReviewerCount: 0` and
`hasExecutionErrors: false`; that count does not describe the coverage. Round 2
is therefore a re-run for `astra`'s coverage as well as a check on the fixes.
