# shisa-de-go

A Go client for [Shisa DE-2](https://huggingface.co/shisa-ai/shisa-de-2), a
model for classification and typed decisions.

Give it text and a set of labels or questions. It returns answers,
probabilities, and request usage, not generated prose. The client loads only a
tokenizer; model weights stay on the server.

**Status: early port.** Landed so far: the pinned codebook, the pure-Go
tokenizer, the DE-2 prompt renderer, the question types, family resolution, the
Python-compatible JSON writer and reader, the DE-2 readout itself -- the code
read, the fallback for a server without `logprob_token_ids`, the bounded thought
and the read after it -- the layers above it: the DE-2 policy, the calibration
records, and `DecisionModel` with `Classify` and `Decide`; the command line,
`shisa-de doctor`, `ask` and `explain`; and the live suite behind
`SHISA_DE_LIVE=1`, including the gate that runs the same questions through this
client and the Python one in one session and compares the answers.

The contract this port implements is
[`docs/READOUT-DE2.md`](docs/READOUT-DE2.md), copied verbatim from the Python
repository, which remains the reference implementation. What the Go port maps
onto, and where it deliberately differs, is in [`docs/PORT.md`](docs/PORT.md).

DE-2 is read from your own OpenAI-compatible server. This port does not read
DE-1: a model that resolves to DE-1 is an error rather than an assumption,
because reading a checkpoint through the other family's contract returns answers
nobody has measured. That includes the default model id, which is the Python
client's DE-1 one, so `Model` is required.

```go
model, err := shisade.NewDecisionModel(shisade.DecisionModelConfig{
    BaseURL: "http://localhost:8000/v1",
    Model:   "shisa-ai/shisa-de-2",
})
if err != nil {
    log.Fatal(err)
}
defer model.Close()

decision, err := model.Classify(ctx,
    map[string]any{"text": "My invoice was charged twice."},
    map[string]shisade.HeadSpec{
        "route": {Options: []shisade.Option{
            {Key: "billing", Description: "Charges and invoices"},
            {Key: "shipping", Description: "Delivery and tracking"},
        }},
    }, shisade.ClassifyOptions{})
if err != nil {
    log.Fatal(err)
}
fmt.Println(decision.Values["route"]) // billing
```

Every answer records the read that produced it (`Strategy`, `Calibrated`,
`Temperature`, `Requests`), and every decision carries a provenance block, so a
caller can tell a calibrated answer from a raw one and a direct read from one
that followed a thought.

## Command line

```bash
go install ./cmd/shisa-de
export SHISA_DE_API_KEY=...

shisa-de doctor   --model shisa-ai/shisa-de-2                 # is this endpoint usable?
shisa-de ask      --model shisa-ai/shisa-de-2 --labels billing,shipping
shisa-de explain  --model shisa-ai/shisa-de-2 --labels billing,shipping
```

`doctor` checks the endpoint, the served model id, the answer boundary and the
calibration record, and exits non-zero when the report is not ok. `ask` is the
one-line classification. `explain` prints every step of one question: the prompt
the model receives, the answer slots, the top-k the codes were read from, the
distribution, and a `curl` line that reproduces the request without Python.

The command line reads the endpoint from `SHISA_DE_ENDPOINT` and the key from
`SHISA_DE_API_KEY`, then `SHISA_API_KEY`. Its `--model` defaults to
`shisa-ai/shisa-de-2` rather than the library's DE-1 default, because this port
has no DE-1 read and a command whose zero-config form can only fail is not a
command. `PORT.md` records what else differs from the Python command line.

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

The tokenizer is pure Go, so the build needs no cgo and no Python. The
tokenizer parity tests compare against golden vectors derived from
`transformers`; they skip when the vectors are absent.

Live tests spend real API requests and are opt in:

```bash
SHISA_DE_LIVE=1 SHISA_API_KEY=... go test . -run Live
```

`TestLiveParityAgainstPython` is the interesting one: it runs the fixed question
set in `testdata/live_questions.json` through this client and through the Python
one, against the same endpoint in the same session, and requires the chosen
values and the distributions to agree. The Python half is recorded by
`scripts/live_parity.py`, which needs a `shisa-de` checkout (`SHISA_DE_PYTHON_REPO`
points at one elsewhere); without it the parity test skips and the rest of the
live suite still runs. `SHISA_DE_MODEL`, `SHISA_DE_ENDPOINT` and
`SHISA_DE_TOKENIZER` point the whole suite at another served checkpoint.

The recorder's own logic has offline tests that need nothing but `python3`:

```bash
python3 -m unittest discover -s scripts -p 'test_*.py'
```

## License

Apache 2.0. See [LICENSE](LICENSE).
