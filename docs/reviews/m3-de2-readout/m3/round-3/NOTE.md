# Round 3 coverage and disposition

Reviewers: `astra`, `ds4`, `glm`. Two produced usable reports.

`astra` failed for the third time in a row, in the same way as M2 round 3 and
both earlier M3 rounds: its report file holds only `[Error: Our servers are
currently overloaded. Please try again later.]` and the CLI exited 0, so the
harness recorded the round as having no execution errors and three successful
executions. Across M2 round 3 and all three M3 rounds, `astra` has produced
no review content at all. `glm` and `ds4` ran on their primary candidates.

Both reports found only LOW items, and no HIGH or CRITICAL. Round 3 is the
configured maximum, so no further round was run.

The two reports converged on the same two findings:

- The generation guard in `checkCodeBoundaryFor` -- the whole of the round-2
  in-flight fix -- had no test. `ds4` checked the mutations (dropping
  `current`, or making it unconditionally true) and both left the suite
  green, including `-race`, because the race detector cannot see a logical
  cache-poisoning that involves no data race.
- The `prompt_logprobs` null sentence in `docs/PORT.md` had both clients
  wrong. The case the port rejects is a null map *value*, which crashes the
  Python client with an `AttributeError` rather than being read through; a
  null last *position* is the same "returned nothing" error on both sides and
  so is not a difference.

`ds4` found that the gate's header comparison was dead for `Authorization`:
the generator built every readout with an empty key, so no recorded exchange
carried the header the paragraph named.

`glm` found that `docs/PORT.md` overstated the `Softmax` match for a
codebook-ordered prefix (the order matches, the arithmetic still differs by
~1e-16 on CPython 3.12+), that the difference list was missing three entries
the round-2 work introduced or exposed (the (code, token id)-keyed approval
cache, the `ListModels` error taxonomy, and `SetTokenizer`), that `idPrompt`
aliased the caller's slice where Python copies it, and that `Readout.Render`
reinterprets an empty `SystemPrompt` differently from the package function.

All are addressed in `ed1cdef`.
