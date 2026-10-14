# shisa-de-go

A Go client for [Shisa DE-2](https://huggingface.co/shisa-ai/shisa-de-2), a model
for classification and typed decisions.

Give it text and a set of labels or questions. It returns answers, probabilities,
and request usage, not generated prose. The client loads only a tokenizer; the
model weights stay on the server.

```go
import shisade "github.com/shisa-ai/shisa-de-go"
```

The contract this port implements is
[`docs/READOUT-DE2.md`](docs/READOUT-DE2.md), copied verbatim from the Python
repository, which remains the reference implementation. What the Go port maps
onto, and where it deliberately differs, is in [`docs/PORT.md`](docs/PORT.md).

**This port reads DE-2, text and images.** It does not read **DE-1**, and it has
no text-choice **overflow** path, because DE-1's wide-choice handling is the only
thing that needs one. DE-1 is retired as a model, so nothing is served through
that contract any more: the hosted name is an alias for DE-2. Both absences are
explained under [What this port does not have](#what-this-port-does-not-have).

## Install

```bash
go get github.com/shisa-ai/shisa-de-go
```

Requires Go 1.25 or newer. There is no cgo and no Python: the tokenizer is pure
Go, so a build needs nothing but the toolchain.

## Quick start

DE-2 is served from your own OpenAI-compatible server; the
[DE-2 readout](docs/READOUT-DE2.md#8-serving) has the `vllm serve` command. Point
the client at it and classify text against a set of labels:

```go
model, err := shisade.NewDecisionModel(shisade.DecisionModelConfig{
	BaseURL: "http://localhost:8000/v1",
	Model:   "shisa-ai/shisa-de-2",
})
if err != nil {
	return err
}
defer model.Close()

decision, err := model.Classify(ctx,
	map[string]any{"text": "WINNER! Claim your free prize now!"},
	map[string]shisade.HeadSpec{"intent": {Options: []shisade.Option{
		{Key: "spam", Description: "Unsolicited bulk or scam message"},
		{Key: "ham", Description: "Ordinary message"},
	}}}, shisade.ClassifyOptions{})
if err != nil {
	return err
}
fmt.Println(decision.Values["intent"]) // spam
```

Nothing is fetched at construction: the tokenizer is downloaded from Hugging
Face on the first call, not by `NewDecisionModel`, so building a client is free
and a client you never use costs nothing.

Check your setup from the command line:

```bash
shisa-de doctor --model shisa-ai/shisa-de-2
```

## Classify with labels

Each named label set is a separate question. Labels can be plain strings or a
mapping from labels to descriptions:

```go
decision, err := model.Classify(ctx,
	map[string]any{
		"subject": "Charged twice",
		"body":    "Please refund the duplicate charge.",
	},
	map[string]shisade.HeadSpec{"intent": {Options: []shisade.Option{
		{Key: "refund_request", Description: "The customer wants money returned"},
		{Key: "order_status", Description: "The customer wants a delivery update"},
	}}}, shisade.ClassifyOptions{IncludeProbabilities: true})
if err != nil {
	return err
}

// The value is the answer's full view, and the answer itself is on Answers.
view := decision.Values["intent"].(shisade.Object)
label, _ := view.Get("label")
fmt.Println(label)
fmt.Println(decision.Answers["intent"].Probabilities)
fmt.Println(decision.Usage["requests"])
```

By default each name maps straight to its label. `IncludeConfidence` returns the
label with a confidence, and `IncludeProbabilities` adds the full probability
map; both change `Values[head]` from the bare label to an `Object`, whose keys
are in the order the Python client writes them: the label, then the confidence,
then the distribution. A Go map would sort them, and the distribution's own keys
would sort too, where the Python client uses your option order. `Object` is the
ordered map `PythonJSONWith` renders; `Object.Get` reads one key.

A `HeadSpec` has four shapes:

| Shape | Behavior |
| --- | --- |
| `Options: []Option{{Key: "a"}, {Key: "b"}}` | Choose one option; a nil `Description` shows the key |
| `Options: []Option{{Key: "a", Description: "..."}}` | Choose one option using its description |
| `Options: ..., Instructions: "Which applies?"` | Supply your own question |
| `Options: ..., MultiLabel: true, Threshold: &t` | Ask yes/no for each option; return the keys above the threshold |
| `Levels: []any{"low", "medium", "high"}` | Choose a level on an ordered scale |

`Threshold` nil means 0.5. A multi-label value is a `[]string` of the keys above
the cutoff. A `Levels` head returns the level name, and the weighted position is
on `decision.Answers[head].Score`.

## Ask typed questions

`Decide` supports yes/no probabilities (`Noul`), named choices (`Choice`), and
ordered scores (`Score`):

```go
decision, err := model.Decide(ctx,
	"Reply with your full card number and CVV to claim your prize.",
	map[string]shisade.Question{
		"is_spam": shisade.NewNoul("Is this message spam?"),
		"asks_for": shisade.NewChoice("What does the sender want?", []shisade.Option{
			{Key: "card_details", Description: "Payment card details"},
			{Key: "callback", Description: "A support callback"},
			{Key: "nothing", Description: "Nothing; it is routine"},
		}),
		"risk": shisade.NewScore("How risky is acting on this message?",
			[]any{"Safe", "Suspicious", "Dangerous"}),
	}, shisade.CallOptions{})
if err != nil {
	return err
}

fmt.Println(decision.Values["asks_for"])                     // card_details
fmt.Println(decision.Answers["risk"].Level)                  // Dangerous
fmt.Println(decision.Answers["asks_for"].Probabilities)
fmt.Println(decision.Usage["requests"])
```

- `Noul` returns the probability of yes.
- `Choice` returns the selected option key.
- `Score` returns the probability-weighted, zero-based position in the scale. The
  most likely level is on `decision.Answers[name].Level`.

Questions run concurrently, bounded by `DecisionModelConfig.MaxWorkers` (8 by
default). The order the questions are asked in is by head name and not by map
iteration, so a recorded exchange replays. `decision.Usage` holds the request,
token, thought-token and logical-read counts, and `decision.Answers[name]` holds
each answer's probabilities, confidence and cost.

Option keys within a head must be distinct: a repeated key would read two codes
into one option and the distribution would not sum to one, so it is refused
before anything is sent. The same goes for a repeated label in a multi-label
head.

## Classify images

Pass a local image path, an HTTP(S) image URL, or a base64 image data URL as
`Image`:

```go
decision, err := model.Classify(ctx, map[string]any{},
	map[string]shisade.HeadSpec{"animal": {Options: []shisade.Option{
		{Key: "cat"}, {Key: "dog"}, {Key: "bird"},
	}}},
	shisade.ClassifyOptions{
		IncludeProbabilities: true,
		CallOptions:          shisade.CallOptions{Image: "photo.jpg"},
	})
if err != nil {
	return err
}

view := decision.Values["animal"].(shisade.Object)
label, _ := view.Get("label")
fmt.Println(label)
fmt.Println(decision.Answers["animal"].Probabilities)
```

```bash
shisa-de ask --image photo.jpg --labels cat,dog,bird --prompt 'What animal is shown?'
```

`Decide(..., CallOptions{Image: "photo.jpg"})` asks typed questions about an
image too. Local files are uploaded; URLs are fetched by the server. Supported
formats are PNG, JPEG, and WebP.

`PrepareImage` is what turns a local path into the data URL a request carries,
and `ValidateImageURL` checks one that is already a URL. Both are what `Classify`
and `Decide` call, and both return an `*ImageError` carrying the Python client's
message when a reference is rejected, so a caller who would rather fail before
spending a request can call them directly.

If your server's chat template has been fixed so that an image request renders
the system turn the way a text request does, `ImageReadOptions{RequireStringSystem:
true}` on a direct `Readout.ReadImage` call makes a differing render an error
instead of a recorded verdict. `DecisionModel` leaves it off, because a stock
vLLM renders the extra space on every image request and rejects images outright
when told to pass strings.

Image questions take at most 26 options and return uncalibrated probabilities.
That is the default rather than a rule: `Calibrated` is off for an image because
the shipped record was fitted on the text path, and a record with an `image` fit
is the only thing that would temper one. If the server does not return a logprob
for every option, the client returns an error rather than guessing. On your own
server, raise `--max-logprobs` and set
`DecisionModelConfig{ImageTopLogprobs: ...}` to match for label sets above 20.

An image question is not read the way a text question is. An image cannot travel
through the text readout at all, because the client renders that prompt itself
with the checkpoint's chat template and only the server can expand an image
placeholder into the image tokens the checkpoint expects. So it posts to
`/v1/chat/completions`, lets the server render the prompt, and takes one direct
read: no repeat, no thinking step, at most 26 options, no fallback for a code
outside the returned top-k, and no calibration. The readout version is still
`de2-codebook-v1`.

vLLM hands the chat template every message of an image request as content parts,
and the template then writes a space after a system message that arrives that way,
so the image prompt carries one token the text prompt does not. DE-2 was trained
on the string rendering, without images. This port records the difference and
answers anyway, which is what the Python client does: the verdict is
`CodeRead.SystemRender` on the read, and
`Decision.Raw[head]["system_render"]` on the answer. `doctor --probe` reports it
as `chat_system_render`.
[`docs/READOUT-DE2.md`](docs/READOUT-DE2.md#6-images) section 6 has the
measurement.

## Large label sets

DE-2 reads up to 256 options in one question and returns an ordinary probability
distribution over them. There is no chunking and no finalist round, so a wide
choice needs nothing special:

```go
options := make([]shisade.Option, 0, 200)
for index := 0; index < 200; index++ {
	options = append(options, shisade.Option{
		Key:         fmt.Sprintf("queue_%d", index),
		Description: fmt.Sprintf("Support queue %d", index),
	})
}
decision, err := model.Decide(ctx, "Route this support request",
	map[string]shisade.Question{"route": shisade.NewChoice("Which queue applies?", options)},
	shisade.CallOptions{})
if err != nil {
	return err
}

answer := decision.Answers["route"]
fmt.Println(answer.Choice, answer.Confidence, answer.Requests)
```

Up to 128 options is one request and more than that is two, because the candidate
block is written in two parts. The distribution covers all 200 options, so
`Confidence` is meaningful and the answer is calibrated like any other.

Two limits are worth knowing:

- **256 options** is the cap. `CodesFor` returns an error above it, and a
  question with more is refused before anything is sent.
- **Questions above 26 options never think.** That is the adopted policy: the
  gate only applies where the model has been measured to reason usefully.

## Probabilities and calibration

Every answer carries its probabilities and records whether they were calibrated,
in `answer.Calibrated` and `answer.Temperature`.

| Call | Probabilities returned |
| --- | --- |
| Images | Raw by default: `Calibrated` is off with an image, and no shipped fit covers that read |
| DE-2 text | Calibrated by default, with a separate fit for each read; pass `Calibrated` to get raw |
| A checkpoint the shipped record does not cover | Raw, with a warning in `DecisionModel.Warnings` |

Each bundled calibration is applied only to the checkpoint it was fitted on,
`shisa-ai/shisa-de-2`. DE-2 has three reads and each has its own fit: a question
read once, read twice, and read after a thinking step are tempered differently,
and `answer.Temperature` says which was used.

Calibration depends on the model, the way it is read, and how it is served.
Refit before using these probabilities to drive a threshold on your own
deployment. The [DE-2 readout](docs/READOUT-DE2.md#7-calibration) has the
details, including the caveat that probabilities read after a thinking step are
the least reliable.

To use a calibration you fitted yourself, or to vouch for the bundled one on
another checkpoint, pass it in:

```go
record, err := shisade.LoadCalibrationFile("my.json")
if err != nil {
	return err
}
model, err := shisade.NewDecisionModel(shisade.DecisionModelConfig{
	BaseURL:     "http://localhost:8000/v1",
	Model:       "my-de2-finetune",
	Family:      "de2",
	Calibration: &record,
})
```

A record passed in is your assertion and is applied as given. `Health` still
reports how far it matches the served model, in `calibration_level` and
`calibration_match`.

### When the probability is the result

Pass `Probability: true` when you need the distribution itself, such as a
forecast, rather than the winning label:

```go
off := false
decision, err := model.Decide(ctx,
	map[string]any{"forecast": "Rain is expected tomorrow afternoon."},
	map[string]shisade.Question{"rain": shisade.NewNoul("Will it rain tomorrow?")},
	shisade.CallOptions{Probability: true, Calibrated: &off})
if err != nil {
	return err
}
pYes := decision.Values["rain"]
distribution := decision.Answers["rain"].Probabilities
```

The flag guarantees that every answer is one distribution over all of its
options, taken from a single logical read: on DE-2 it skips the thinking step, so
a choice up to 256 options is accepted and answered in one pass. It does not
change prompts, answer shapes, or calibration. It is separate from
`IncludeProbabilities`, which only changes what `Classify` returns.

`Calibrated` is a `*bool` so that unset and false are different: nil means "on",
which is the default for text.

## Use a local server

Start an OpenAI-compatible server with the model loaded, then point the client at
it. `FromEndpoint` is the shorthand:

```go
empty := ""
model, err := shisade.FromEndpoint("http://127.0.0.1:8000/v1", "shisa-ai/shisa-de-2",
	shisade.DecisionModelConfig{APIKey: &empty})
if err != nil {
	return err
}
defer model.Close()

decision, err := model.Classify(ctx,
	"I was charged twice. Please refund the duplicate payment.",
	map[string]shisade.HeadSpec{"intent": {Options: []shisade.Option{
		{Key: "refund_request"}, {Key: "order_status"}, {Key: "cancel_order"},
	}}}, shisade.ClassifyOptions{})
```

```bash
export SHISA_DE_ENDPOINT=http://127.0.0.1:8000/v1
shisa-de doctor --model shisa-ai/shisa-de-2
shisa-de ask --model shisa-ai/shisa-de-2 --labels refund_request,order_status
```

- A server root URL and a URL ending in `/v1` are both accepted; the `/v1` is
  stripped.
- `APIKey` is a `*string`, so `&empty` disables authentication rather than
  falling back to the environment. A nil `APIKey` reads `SHISA_DE_API_KEY`, then
  `SHISA_API_KEY`, and sends that to a local endpoint too.
- If the server uses an alias for the checkpoint, pass the alias as `Model` and
  the real checkpoint as `Tokenizer`. The tokenizer name also tells the client
  which family the alias is, so a DE-2 alias keeps the DE-2 calibration.
- The server needs `/v1/completions` with `logprobs` and `logprob_token_ids`, and
  `doctor` needs `/v1/models`. vLLM supports both; `docs/READOUT-DE2.md` has the
  exact request shapes. An image question also needs `/v1/chat/completions` with
  vision enabled, and a logprob limit at least as high as the option count:
  raise vLLM's `--max-logprobs` and match it with `ImageTopLogprobs`. Leave
  `--chat-template-content-format` at its default; setting it to `string` makes
  vLLM 0.30.0 reject image requests.

For a checkpoint your server names differently, say the family out loud and name
a tokenizer, because the family decides how the model is asked and the tokenizer
decides which codes are read:

```go
model, err := shisade.FromPretrained("my-de2-finetune", shisade.DecisionModelConfig{
	BaseURL:   "http://127.0.0.1:8000/v1",
	Family:    "de2",
	Tokenizer: "google/gemma-4-26B-A4B-it",
})
if err != nil {
	return err
}
fmt.Println(model.Family, model.FamilySource, model.FamilyExplicit)
```

**Say which family the model is.** DE-1 and DE-2 are asked differently, so a
checkpoint read as the wrong family returns answers nobody has measured. The
client recognizes `de-1` or `de-2` in the model id or the tokenizer name. Anything
else is treated as DE-2 with a warning in `DecisionModel.Warnings`, so pass
`Family: "de2"` when the id does not say. A checkpoint that resolves to DE-1 is
an error, not a fallback, because this port cannot read it.

### Configuration

| Setting | Field | Resolution order |
| --- | --- | --- |
| Endpoint | `BaseURL` | the field, then `SHISA_DE_ENDPOINT`, then `https://api.shisa.ai/openai` |
| API key | `APIKey` | the field (an empty string disables auth), then `SHISA_DE_API_KEY`, then `SHISA_API_KEY` |
| Model | `Model` | the field, otherwise `shisa-ai/shisa-de-2` |
| Tokenizer | `Tokenizer` | the field, otherwise the model id |
| Family | `Family` | the field, then `de-1` or `de-2` in the model id, then in the tokenizer name, otherwise DE-2 with a warning |
| Read | `Overrides` | the family's default read |

The zero-config model id is `shisa-ai/shisa-de-2`, so `NewDecisionModel` with an
empty config reads the hosted DE-2 checkpoint. It used to be the Python client's
DE-1 one, which this port cannot read; DE-1 is retired, the hosted name is an
alias for DE-2, and the two clients' defaults are the same checkpoint again.

## Read settings

DE-2 has one default way of reading a question and two ways to trade quality for
latency. Set them on the model to change its default, or on a call to change that
call:

| Setting | What it does | Default |
| --- | --- | --- |
| `Policy` | A whole read: `"repeat-think"`, `"repeat"`, or `"direct"` | `"repeat-think"` |
| `Reads` | `"single"` or `"double"`: how often the question is shown in the prompt | `"double"` |
| `Reasoning` | Think when the answer is unsure, then answer again | `true` |
| `ReasoningProb` | Think when the top probability is below this | `0.7` |
| `ReasoningLen` | The most tokens a thinking step may run to | `1024` |

```go
// Model default: never think.
gate := 0.9
budget := 256
model, err := shisade.NewDecisionModel(shisade.DecisionModelConfig{
	BaseURL: "http://localhost:8000/v1",
	Model:   "shisa-ai/shisa-de-2",
	Overrides: shisade.OptionOverrides{
		ReasoningProb: &gate,
		ThinkBudget:   &budget,
	},
})
if err != nil {
	return err
}

direct := "direct"
decision, err := model.Decide(ctx, state, questions,
	shisade.CallOptions{Overrides: shisade.OptionOverrides{Policy: &direct}})
if err != nil {
	return err
}
fmt.Println(model.Policy(), model.Reasoning(), model.ThinkGate(), model.ThinkBudget())
```

A call inherits whatever it does not set. `decision.Meta["policy"]` records the
policy the call ran under, and each answer's `Strategy` says which read actually
produced it.

**Fastest or best.** The three DE-2 reads are also named, as `Policy`:

| Goal | `Policy` | Strategy | HTTP requests per question |
| --- | --- | --- | --- |
| Best quality (default) | `"repeat-think"` | `repeat2` or `repeat2-think` | 1, or 3 when it thinks |
| Balanced | `"repeat"` | `repeat2` | 1 |
| Lowest latency | `"direct"` | `direct` | 1, with about half the prompt tokens |

A repeated read shows the question to the model **twice inside one prompt**, so
`repeat2` costs one HTTP request and not two. Only a thinking answer costs more,
and that is three: the read, the thought, and the read after it.

Things to know:

- `Reasoning: true` always shows the question twice first, and `Reads: "single"`
  never thinks. Asking for both at once is an error.
- Questions above 26 options never think, and neither does a call made with
  `Probability: true`.
- An unknown policy, or a setting that disagrees with the policy it was combined
  with, is refused by name before anything is sent.
- `ReasoningProb`/`ThinkGate` and `ReasoningLen`/`ThinkBudget` are the same two
  settings under two names.

## Command line

| Command | What it does |
| --- | --- |
| `shisa-de doctor` | Checks the endpoint, the model id, the tokenizer, the answer boundary, and whether the calibration belongs to the served model. Exits non-zero on a problem |
| `shisa-de doctor --probe` | Also spends one question on the read, and reports how the server renders the image path |
| `shisa-de ask` | Classifies `--state` (text or JSON) against `--labels`, optionally with `--image` |
| `shisa-de explain` | Prints the prompt, the answer slots, the one request with the answer codes by token id, the distribution, and a `curl` line that reproduces the request without Python |

```bash
go install ./cmd/shisa-de
export SHISA_DE_API_KEY=...

shisa-de doctor   --model shisa-ai/shisa-de-2
shisa-de ask      --model shisa-ai/shisa-de-2 --labels billing,shipping
shisa-de ask      --model shisa-ai/shisa-de-2 --image photo.jpg --labels cat,dog,bird
shisa-de explain  --model shisa-ai/shisa-de-2 --labels billing,shipping
```

All of them accept `--base-url`, `--model`, `--tokenizer`, `--family`,
`--calibration`, `--policy` and `--timeout`. The read settings are `--reads`,
`--reasoning` / `--no-reasoning`, `--reasoning-prob`, `--reasoning-len`, and
`--compound` / `--no-compound`, which this port refuses when it is turned on:
DE-2 reads up to 256 options in one prompt, so there is nothing to compound.
`ask` also takes `--image` and `--image-top-logprobs`, and nothing else does,
because an image read is the only read that uses them. The endpoint comes
from `SHISA_DE_ENDPOINT` and the key from `SHISA_DE_API_KEY`, then
`SHISA_API_KEY`; the command line's `--model` defaults to the library's
`DefaultModel`, which is `shisa-ai/shisa-de-2` and is the only checkpoint this
port reads. It used to have a default of its own, because the library's was the
DE-1 one and a command whose zero-config form can only fail is not a command;
DE-1 is retired and the two defaults are the same value now.

`PORT.md` records what else differs from the Python command line.

## Performance

Measured on an 8-core Intel Xeon E5-2690 v4 against the hosted
`shisa-ai/shisa-de-2` (`vllm-0.30.0-tp2-ca69195e`), 2026-10-10, both clients
answering the same question:

| | Go | Python |
| --- | ---: | ---: |
| Client construction | 0.6 ms | 91 ms |
| Cold process to first answer | 2,220 ms | 5,041 ms |
| Warm decision, median | 86.9 ms | 87.8 ms |
| Warm decision, minimum | 85.2 ms | 85.2 ms |

**Per-call latency is the same.** The server round trip is about 85 ms and the
client's own work is a few milliseconds on both sides, so it does not surface.
The minimums agree to the tenth of a millisecond. If you make decisions in a
long-lived process, there is no performance reason to prefer either client.

**Startup is where they differ.** The Go client constructs in under a
millisecond because it defers the tokenizer to the first call, which then costs
about 2.2 seconds; the Python client's first call costs about 5.0 seconds,
including a 1.15-second `import transformers`. So Go is roughly 2.3 times faster
from a cold process to a first answer, and about 2.8 seconds faster for a CLI
invoked once per question.

The Go 2.2 seconds is mostly two things: about 0.85 s parsing the 32 MB
`tokenizer.json` with `encoding/json`, and about 1.0 s building 514,906 merge
ranks and 262,144 vocabulary entries.

**Tokenization is not faster than `transformers`.** For the same 8,879-byte
prompt, this package encodes in 4.67 ms against 5.08 ms for `transformers`' Rust
tokenizer and 3.44 ms for its pure-Python one. The port is in the same band, not
ahead of it; [`docs/PORT.md`](docs/PORT.md#performance) also records the
comparison against `daulet/tokenizers` that the tokenizer was written against.

## What this port does not have

The Python client reads two families. This port reads one of them, and the two
things it is missing are both consequences of that:

**DE-1.** The Python client reads DE-1 and DE-2. This port implements
`de2-codebook-v1` only, and a checkpoint that resolves to DE-1 is an error rather
than an assumption, because reading a checkpoint through the other family's
contract returns answers nobody has measured. DE-1 is retired as a model: the
hosted `shisa-ai/shisa-de-1` is still listed and still answers, but it is an alias
for `shisa-ai/shisa-de-2`, which is this port's default. A DE-1 checkpoint of your
own needs the [Python client](https://github.com/shisa-ai/shisa-de).

**Wide-choice overflow.** DE-1 caps a text choice at 26 options and the Python
client splits anything larger into chunks with a finalist round. DE-2 reads up to
256 options natively, so nothing here needs that, and `Overflow` is refused by
name rather than accepted and ignored.

**DE-1's single fixed read.** DE-1 has one way to be read. DE-2's policy
machinery is the only read selection this port has, and `Policy: "direct"` is
what a single DE-2 read is called here.

Everything else in the Python client's API has a Go counterpart, and
[`docs/PORT.md`](docs/PORT.md) maps them file by file.

## How it works

The client renders each question into a prompt, asks the server for one token,
and reads the probabilities of the answer codes at that position. Nothing is
generated except DE-2's optional thinking step. The exact prompts, requests and
arithmetic are specified so that another client can be written from them:

- [The DE-2 readout](docs/READOUT-DE2.md)

The Go port's own choices, and the places it deliberately differs from the
reference, are in [`docs/PORT.md`](docs/PORT.md).

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

[`.github/workflows/tests.yml`](.github/workflows/tests.yml) runs `gofmt -l .`,
`go vet ./...`, the suite with and without `-race`, the recorder's Python tests
and a build of the command. It sets `SHISA_DE_TOKENIZER_DOWNLOAD=1` and
`SHISA_DE_TOKENIZER_REQUIRE=1`, so the parity suite downloads the pinned
tokenizer and fails rather than skipping, and caches it between runs. It never
sets `SHISA_DE_LIVE`, so no CI run needs an API key.

The tokenizer parity tests compare against golden vectors derived from
`transformers`; they skip when the vectors are absent, and
`SHISA_DE_TOKENIZER_REQUIRE=1` turns that skip into a failure.

The examples in `example_test.go` are the snippets in this file in a form
`go test` runs, so a signature change breaks them rather than leaving this page
wrong. `readme_test.go` checks that every `shisade.X` this page names is an
exported identifier, which catches a rename that the examples do not cover.

Live tests spend real API requests and are opt in:

```bash
SHISA_DE_LIVE=1 SHISA_API_KEY=... go test . -run Live
```

`TestLiveParityAgainstPython` is the interesting one: it runs the fixed question
set in `testdata/live_questions.json` through this client and through the Python
one, against the same endpoint in the same session, and requires the chosen
values and the distributions to agree. The set ends with an image question, so
the comparison covers the one read that uses `/v1/chat/completions` and a
different request shape; the recorder measures whether the endpoint will serve an
image at all and skips that one case with the server's own refusal when it will
not, while any other probe failure stops the recording rather than being
recorded as a deployment fact. `TestLiveImageReadUsesTheImage` is the companion
that the parity comparison cannot be: it asks a red and a blue image the same
question and requires the answer to follow, so a server that accepted the image
and ignored it fails rather than agreeing with itself. The Python half is
recorded by `scripts/live_parity.py`, which needs a `shisa-de` checkout
(`SHISA_DE_PYTHON_REPO` points at one elsewhere); without it the parity test
skips and the rest of the live suite still runs. `SHISA_DE_MODEL`,
`SHISA_DE_ENDPOINT` and `SHISA_DE_TOKENIZER` point the whole suite at another
served checkpoint.

The recorder's own logic has offline tests that need nothing but `python3`:

```bash
python3 -m unittest discover -s scripts -p 'test_*.py'
```

## License

Apache 2.0. See [LICENSE](LICENSE). See the
[model card](https://huggingface.co/shisa-ai/shisa-de-2) for the model's license
and usage requirements.
