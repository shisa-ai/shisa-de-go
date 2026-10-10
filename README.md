# shisa-de-go

A Go client for [Shisa DE-2](https://huggingface.co/shisa-ai/shisa-de-2), a
model for classification and typed decisions.

Give it text and a set of labels or questions. It returns answers,
probabilities, and request usage, not generated prose. The client loads only a
tokenizer; model weights stay on the server.

**Status: early port.** Landed so far: the pinned codebook, the pure-Go
tokenizer, the DE-2 prompt renderer, the question types, family resolution, the
Python-compatible JSON writer and reader, and the DE-2 readout itself -- the
code read, the fallback for a server without `logprob_token_ids`, the bounded
thought and the read after it. The policy, calibration, client and CLI are still
to come.

The contract this port implements is
[`docs/READOUT-DE2.md`](docs/READOUT-DE2.md), copied verbatim from the Python
repository, which remains the reference implementation. What the Go port maps
onto, and where it deliberately differs, is in [`docs/PORT.md`](docs/PORT.md).

DE-2 is read from your own OpenAI-compatible server. This port does not read
DE-1: a model that resolves to DE-1 is an error rather than an assumption,
because reading a checkpoint through the other family's contract returns answers
nobody has measured.

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

The tokenizer is pure Go, so the build needs no cgo and no Python. The
tokenizer parity tests compare against golden vectors derived from
`transformers`; they skip when the vectors are absent.

Live tests that spend real API requests are planned (M6):

```bash
SHISA_DE_LIVE=1 SHISA_API_KEY=... go test ./... -run Live
```

There is no such test in the tree yet, and `-run Live` currently matches
nothing.

## License

Apache 2.0. See [LICENSE](LICENSE).
