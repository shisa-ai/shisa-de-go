# Review Scope: m2-render-family-softmax

- Mode: implementation
- Stage: M2
- Target: Commit 12a1a29 in /home/morpheus/data/dev/shisa/shisa-de-go: the DE-2 renderer, Python-compatible JSON, questions, family resolution and softmax

## Summary

M2 of a Go port of the Python `shisa-de` client, commit 12a1a29. M1 (the pure-Go tokenizer) was reviewed separately and is settled; do not re-review `tokenizer/` except where M2 uses it.

M2 is the half of the readout that turns a question into the exact prompt bytes the Python client sends, and the identity machinery around it. It was written before M1's review and has not been reviewed.

What was built:

- `render.go` — the pinned Gemma-4 chat-template renderer. The Python client renders with `tokenizer.apply_chat_template(...)`, which runs the checkpoint's own Jinja template; this port renders the two message shapes the readout uses instead of pulling in a Jinja engine. `Render(state, question, RenderOptions{Repeat, Thinking, OptionLimit})` builds the payload, serializes the user turn with the Python-compatible writer, optionally repeats it with `InputRepeat` between copies, and writes the system/user/model turns. `Payload` returns the JSON object plus the options in presentation order.
- `pyjson.go` — `json.dumps(value, ensure_ascii=False)` and `json.loads` equivalents. This exists because the user turn is the serialized payload, so a byte of difference is a different prompt and a different answer. `encoding/json` writes `{"a":1}` where Python writes `{"a": 1}`, escapes `<`, `>` and `&` as `\u003c`/`\u003e`/`\u0026` where Python leaves them, and formats floats over a different range. `PythonJSON` writes; `DecodeJSON` parses like `json.loads`, keeping integers and floats apart and keeping object key order, because Go maps have none and the key order reaches the prompt.
- `questions.go` — `Noul`, `Choice`, `Score`, `Option`, the `Question` interface, `Validate`, and `RenderOption`, which shows a label with no description as its key. Option order is the contract: option i is shown code i.
- `family.go` — `Family` (de1/de2), `NormalizeFamily`, `slugFamily`/`hasFamilySlug`, `ModelFamily`, `FamilyIsExplicit`, `ResolveFamily` (declared, then model id, then tokenizer, otherwise `DefaultFamily` with `SourceAssumed`), and `RequireDE2`, which refuses a DE-1 checkpoint with `UnsupportedFamilyError` rather than reading it as DE-2.
- `readout.go` — the measured scaffold strings (`DirectSystem`, `ThinkSystem`, `InputRepeat`, `AnswerPrefix`), `ReadoutVersionFor`, `CalibrationCompatibleWith`, and `Softmax`.
- `scripts/gen_render_parity.py` and `testdata/render_parity.json` — 33 recorded renders from the Python client, each with the prompt bytes and its token ids. The gate is that every fixture's prompt is byte-identical and its token count matches.

The two facts worth challenging: that the pinned renderer is byte-identical to `apply_chat_template` for the two shapes the readout uses (and only those), and that `PythonJSON` matches `json.dumps(value, ensure_ascii=False)` on every value the client can be handed.

## Primary files / regions

- `render.go` — lines/regions: new, 1-119 — reason: The pinned template renderer and Payload
- `pyjson.go` — lines/regions: new, 1-353 — reason: The json.dumps/json.loads equivalents the prompt bytes depend on
- `questions.go` — lines/regions: new, 1-147 — reason: Noul/Choice/Score, Validate and the empty-description rule
- `family.go` — lines/regions: new, 1-179 — reason: Family resolution and the DE-2-only refusal
- `readout.go` — lines/regions: new, 1-104 — reason: The scaffold constants, readout versions, calibration compatibility and Softmax
- `scripts/gen_render_parity.py` — lines/regions: new, 1-181 — reason: How the 33 golden prompts were produced; whether the corpus is shaped to the implementation
- `render_test.go` — lines/regions: new, 1-270 — reason: The gate and what it does when the tokenizer file is absent

## Review questions

- Is the renderer byte-identical to `apply_chat_template` for the shapes the readout uses? Walk the template constants and the assembled string for: the direct read (Repeat 1), the repeated read (Repeat 2), the thinking read (Thinking true, Repeat 1 and 2), and Repeat values above 2. Check the `\n` placement around each turn, the position of `<|think|>\n`, the absence of the answer prefix in the thinking shape, and whether `Repeat <= 1` and a negative Repeat behave as documented. Is there a shape the readout needs that this renderer cannot produce, or one it produces that the Python client would render differently?
- Is `PythonJSON` faithful to `json.dumps(value, ensure_ascii=False)` for every value the client can be handed? The user state comes from a caller and reaches this function, so check: the `, ` and `: ` separators, escaping of `"`, `\\`, control characters, `\b`/`\f`/`\n`/`\r`/`\t`, non-ASCII (which must NOT be escaped), `<`/`>`/`&`, lone surrogates and invalid UTF-8 in a Go string, `NaN`/`+Inf`/`-Inf`, negative zero, the float repr boundaries Python switches between `repr` and exponent form (`1e16`, `1e15`, `1e-4`, `1e-5`), integers above 2^53, `int64`/`uint64`, `nil`, booleans, empty containers, deeply nested values, and a key that is not a valid identifier. Name a value where this differs from Python and say whether the parity fixtures would catch it.
- Does `DecodeJSON` reproduce `json.loads`? Check duplicate object keys, numbers that are integers written with an exponent or a decimal point, very large integers, unicode escapes including surrogate pairs, leading/trailing whitespace, and whether the returned `Object` preserves key order. Is `Object` a slice of pairs for a reason that survives review, and can a caller accidentally mutate shared state through it?
- Is the option-to-code assignment right end to end? `Payload` calls `question.Options()` then `CodesFor(len(options))` and writes `letter` = code i for option i. Check that `Options()` order is stable and presentation order for `Noul`, `Choice` and `Score`, that the empty-description rule (`RenderOption`) matches what the Python client sends, and that `Validate` bounds are the contract's (the DE-2 option cap of 26 letters or the 256-code limit) rather than the renderer's default.
- Is `Softmax` correct and safe? It takes logprobs keyed by code and normalizes over the keys given. Check the numerical handling (the max subtraction, underflow to zero, a total of zero, `-Inf` for a code the server did not return), whether `map` iteration order can change the result, what happens with one key, and whether returning an error for empty input is the right contract. Should it reject keys that are not codes, or is that the caller's job?
- Is the family resolution right? Check `hasFamilySlug` against ids that should and should not name a family: `shisa-ai/shisa-de-1`, `de1`, `de-1`, `DE_2`, `de1-cont-lr1e5`, `de-13`, `de1x`, `my-de_1-finetune`, `de2-vs-de1-ablation`, `de-2`, a tokenizer path containing `de-1`, and an id naming both. Is the declared-then-model-then-tokenizer order the documented one, is `SourceAssumed` always reported as an assumption, and does `RequireDE2` refuse rather than fall back?
- Is `NormalizeFamily` right? It lowercases and keeps only `[a-z0-9]`, so `DE-2` and `de_2` both become `de2`. What does it do with `de--2`, `de 2`, `2`, an empty string, and a string that normalizes to a family by accident?
- Are the scaffold constants exact? Compare `DirectSystem`, `ThinkSystem`, `InputRepeat` and `AnswerPrefix` against docs/READOUT-DE2.md and the Python client's `shisa_de/readout.py`. A single character here changes every answer, so check for a stray space, newline or trailing period, and check that the thinking system line is the direct line with only the intended sentence replaced.
- Does the parity corpus actually constrain the renderer? The generator records prompt bytes and token ids from the Python client. Name a renderer mistake it would not catch, and say whether the token-count check adds anything beyond the byte comparison. Does the test fail or skip when the 32 MB tokenizer.json is absent, and is that the right behaviour for the byte half of the gate?
- Is `readout.go`'s identity machinery right? Check `ReadoutVersionFor`, `CalibrationCompatibleWith` (including the direction of the argument order and whether an unknown current version can wrongly match), and the `calibrationCompatible` table. `DE2ReadoutVersion` maps to an empty list: is that the intended statement, and does the DE-1 entry belong in a port that does not implement DE-1?
- Are the new claims in docs/PORT.md and AGENTS.md about M2 accurate? Check the file mapping, the 'No Jinja' and 'Python's JSON, not Go's' paragraphs, and whether anything in the render or JSON layer is described as verified when it is only asserted.

## Known non-goals

- The tokenizer is M1 and is settled; it is in scope only where M2 calls it (the render parity token counts).
- DE-1 is not implemented in this port. `Render`'s defaults are described as the DE-1 scaffold, and `readout.go` carries DE-1 constants, but no DE-1 read exists and one is not expected here.
- No HTTP, no policy sequencing, no calibration fitting and no CLI: those are M3 and later.
- `encoding/json` compatibility is explicitly not the goal; matching Python's `json` module is.
- The renderer is deliberately pinned to the Gemma-4 template `shisa-ai/shisa-de-2` carries, not a general Jinja engine.
- Images and the image readout are not ported yet.

## Validation already run

- gofmt -l . (clean), go build ./..., go vet ./... (clean)
- go test ./... (pass)
- go test -run TestRender -v: 33 render fixtures, prompt bytes equal the Python client's and token counts match
- go test ./tokenizer/ (parity against transformers, 1,056 strings / 48,392 ids, 83 decode cases)
