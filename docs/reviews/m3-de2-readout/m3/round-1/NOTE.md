# Round 1 coverage and disposition

Reviewers: `astra`, `ds4`, `glm`. Two produced reports; `glm` did not.

`glm` timed out twice (exit 143) on both its primary and its fallback
candidate, and both report files hold only the timeout record, so `glm`
produced no review. The harness counted the round as having an execution
error. The failure is in the reviewer, not in the harness: both candidates
are `glm-5.3` through the same provider, and the same reviewer produced a
usable report for M2 rounds 1 and 2.

The substantive coverage is `astra` and `ds4`:

- `astra` found one HIGH (nothing enforced the DE-2-only rule on `Readout`
  itself), five MEDIUM (the unused `LocalFilesOnly`, a null `token_ids`
  element becoming token 0, a nil `prompt_logprobs` entry panicking, a
  tokenizer swap leaving stale ids and approvals, and a `closeEnough` that
  accepted NaN), and three LOW.
- `ds4` found one MEDIUM (`ReadoutHTTPError` was not a `ReadoutError`, so
  the Go taxonomy did not match the Python class hierarchy the port claims
  to replace) and seven LOW.

Both independently found the mid-read `logprob_token_ids` downgrade
divergence and the unverified request path. Every finding is addressed in
`29049db`; nothing was rejected. Two claims in the report that were
checked rather than assumed: the response cap was raised from 8 MB to
64 MB after `ds4` noted the `prompt_logprobs: 0` fallback returns one entry
per prompt token rather than one position of top-logprobs, and the Softmax
comment no longer claims rounding cannot move the gate.
