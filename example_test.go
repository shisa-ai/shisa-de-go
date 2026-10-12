package shisade

// The examples in this file are the Go README's snippets in a form `go test`
// runs, so a signature or behaviour change breaks them instead of leaving the
// README wrong.
//
// They read from a stub endpoint rather than a real one: an example has to pass
// on a machine with no server and no network, and its output has to be the same
// every time for `// Output:` to mean anything. The API calls and the field
// names are exactly what a caller writes against a served DE-2; only the
// transport differs.
//
// One thing to know while reading them: DE-2's default read shows the question
// to the model **twice inside one prompt**, so a `repeat2` answer costs one
// HTTP request, not two. Only a thinking answer costs more, and that is three.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
)

// -- the stub the examples read from ------------------------------------------

// exampleModel is a DecisionModel over a stub endpoint that answers with the
// given recorded bodies, in the order the questions are asked.
//
// It panics rather than reporting an error, because an example has no
// `*testing.T` to report to and a broken example must fail loudly.
func exampleModel(bodies ...string) *DecisionModel {
	// The whole codebook, not just the codes the bodies use: Health's boundary
	// check verifies that every one of the 256 codes is a single token, and it
	// fails the report if the tokenizer does not know them.
	codebook, err := Codebook()
	if err != nil {
		panic(err)
	}
	codes, err := CodesFor(len(codebook))
	if err != nil {
		panic(err)
	}
	words := map[string]int{AnswerPrefix: 101}
	for index, code := range codes {
		words[code] = 1000 + index
	}
	tokenizer := newFixtureTokenizer(words)

	at := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"shisa-ai/shisa-de-2"}]}`)
			return
		}
		if at >= len(bodies) {
			panic(fmt.Sprintf("the example made request %d, but %d responses were recorded",
				at+1, len(bodies)))
		}
		// The client checks the prompt-token count the server reports against its
		// own tokenization, so the stub has to count the same way.
		var sent struct {
			Prompt any `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			panic(err)
		}
		tokens := 0
		switch prompt := sent.Prompt.(type) {
		case string:
			tokens = len(tokenizer.Encode(prompt))
		case []any:
			tokens = len(prompt)
		}
		body := bodies[at]
		at++
		fmt.Fprint(w, strings.ReplaceAll(body, promptTokensPlaceholder, fmt.Sprint(tokens)))
	}))

	readout, err := NewReadout(ReadoutConfig{
		BaseURL: server.URL, Model: "shisa-ai/shisa-de-2", HTTPClient: server.Client(),
	})
	if err != nil {
		panic(err)
	}
	readout.SetTokenizer(tokenizer)
	model, err := NewDecisionModel(DecisionModelConfig{
		BaseURL:   server.URL,
		Model:     "shisa-ai/shisa-de-2",
		Tokenizer: "shisa-ai/shisa-de-2",
		Readout:   readout,
		// One worker, so the recorded bodies line up with the head order rather
		// than with whichever head happened to be scheduled first.
		MaxWorkers: 1,
	})
	if err != nil {
		panic(err)
	}
	return model
}

// exampleRead is a code read where the option at `winner` is the clear peak and
// every other option sits ten logits below it. One call answers one request.
func exampleRead(count, winner int) string {
	codes, err := CodesFor(count)
	if err != nil {
		panic(err)
	}
	weights := make(map[string]float64, count)
	for _, code := range codes {
		weights[code] = -10
	}
	weights[codes[winner]] = 0
	return codeReadBody(weights)
}

// exampleKeys are a distribution's option keys, sorted, so an example can print
// the shape of an answer without printing floats.
func exampleKeys(probabilities map[string]float64) []string {
	keys := make([]string, 0, len(probabilities))
	for key := range probabilities {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// -- quick start --------------------------------------------------------------

// The shortest useful call: classify text against a set of labels.
//
// A real client names the server and the checkpoint, as ExampleNewDecisionModel
// shows. The examples here read from a stub instead, so they pass with no server
// running and print the same output every time.
func Example() {
	model := exampleModel(exampleRead(2, 0))
	defer model.Close()

	decision, err := model.Classify(context.Background(),
		map[string]any{"text": "WINNER! Claim your free prize now!"},
		map[string]HeadSpec{"intent": {Options: []Option{
			{Key: "spam", Description: "Unsolicited bulk or scam message"},
			{Key: "ham", Description: "Ordinary message"},
		}}}, ClassifyOptions{})
	if err != nil {
		panic(err)
	}
	fmt.Println(decision.Values["intent"])
	// Output: spam
}

// A client names the endpoint and the checkpoint. Nothing is loaded or fetched
// here: the tokenizer is read on the first call, not at construction, and the
// weights never leave the server.
func ExampleNewDecisionModel() {
	model, err := NewDecisionModel(DecisionModelConfig{
		BaseURL: "http://localhost:8000/v1",
		Model:   "shisa-ai/shisa-de-2",
	})
	if err != nil {
		panic(err)
	}
	defer model.Close()

	fmt.Println(model.BaseURL)
	fmt.Println(model.Model)
	fmt.Println(model.Family)
	fmt.Println(model.FamilySource)
	fmt.Println(model.ReadoutVersion)
	// Output:
	// http://localhost:8000
	// shisa-ai/shisa-de-2
	// de2
	// model id
	// de2-codebook-v1
}

// -- classify images ----------------------------------------------------------

// An image question takes the same arguments as a text one, with the image on
// CallOptions. It is one direct read, so its probabilities are uncalibrated.
func ExampleDecisionModel_Classify_image() {
	model := exampleImageModel(3, 0)
	defer model.Close()

	decision, err := model.Classify(context.Background(), map[string]any{},
		map[string]HeadSpec{"animal": {Options: []Option{
			{Key: "cat"}, {Key: "dog"}, {Key: "bird"},
		}}},
		ClassifyOptions{
			IncludeProbabilities: true,
			CallOptions: CallOptions{
				Image: "data:image/png;base64,iVBORw0KGgo=",
			},
		})
	if err != nil {
		panic(err)
	}

	view := decision.Values["animal"].(Object)
	label, _ := view.Get("label")
	fmt.Println(label)
	fmt.Println(decision.Answers["animal"].Calibrated)
	fmt.Println(exampleKeys(decision.Answers["animal"].Probabilities))
	// Output:
	// cat
	// false
	// [bird cat dog]
}

// -- classify with labels -----------------------------------------------------

// Labels can carry descriptions, and an answer can carry its distribution.
func ExampleDecisionModel_Classify() {
	model := exampleModel(exampleRead(2, 0))
	defer model.Close()

	decision, err := model.Classify(context.Background(),
		map[string]any{
			"subject": "Charged twice",
			"body":    "Please refund the duplicate charge.",
		},
		map[string]HeadSpec{"intent": {Options: []Option{
			{Key: "refund_request", Description: "The customer wants money returned"},
			{Key: "order_status", Description: "The customer wants a delivery update"},
		}}}, ClassifyOptions{IncludeProbabilities: true})
	if err != nil {
		panic(err)
	}

	// With IncludeProbabilities the value is the answer's full view, in the
	// order the Python client writes it: the label, the confidence, then the
	// distribution. `Object` is what keeps that order; a Go map would sort it.
	view := decision.Values["intent"].(Object)
	label, _ := view.Get("label")
	fmt.Println(label)
	fmt.Println(exampleKeys(decision.Answers["intent"].Probabilities))
	// Output:
	// refund_request
	// [order_status refund_request]
}

// exampleImageModel is exampleModel's chat counterpart.
//
// An image question cannot be rendered by this client: only the server can
// expand an image placeholder into the image tokens the checkpoint expects, so
// the question is sent as messages and the answer is read from a chat response.
// That makes the stub different in two ways. It answers on the chat endpoint,
// and it has to return `prompt_logprobs`, because the read confirms the answer
// boundary from the prompt the server says it built.
//
// The stub rebuilds the served prompt from the messages it was sent, using the
// same template fragments this package renders with, so the system-render
// verdict is `string` rather than a difference the example would then have to
// explain.
func exampleImageModel(count, winner int) *DecisionModel {
	codebook, err := Codebook()
	if err != nil {
		panic(err)
	}
	codes, err := CodesFor(len(codebook))
	if err != nil {
		panic(err)
	}
	words := map[string]int{AnswerPrefix: 101}
	for index, code := range codes {
		words[code] = 1000 + index
	}
	tokenizer := newFixtureTokenizer(words)

	// The option letters, not the codebook: an image question is a chat request
	// with at most 26 options, so it keeps the DE-1 scaffold's letters.
	letters, err := CodesFor(count)
	if err != nil {
		panic(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"shisa-ai/shisa-de-2"}]}`)
			return
		}
		var sent struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			panic(err)
		}
		if len(sent.Messages) != 2 {
			panic(fmt.Sprintf("the example sent %d messages, want 2", len(sent.Messages)))
		}
		// The system turn is a string and the user turn is content parts: the
		// image, then the question as text.
		system, _ := sent.Messages[0].Content.(string)
		parts, ok := sent.Messages[1].Content.([]any)
		if !ok {
			panic("the user turn is not content parts")
		}
		user := ""
		for _, part := range parts {
			if fields, ok := part.(map[string]any); ok && fields["type"] == "text" {
				user, _ = fields["text"].(string)
			}
		}
		if user == "" {
			panic("the user turn carries no text part")
		}
		served := templateBOS + templateTurnOpen + "system" + templateRoleEnd +
			system + templateTurnClose + templateRoleEnd +
			templateTurnOpen + "user" + templateRoleEnd + user +
			templateTurnClose + templateRoleEnd + templateTurnOpen + "model" + templateRoleEnd +
			templateThought + AnswerPrefix
		ids := tokenizer.Encode(served)
		// The first prompt position carries no logprob, which is what
		// `prompt_logprobs: 0` returns, and the last one is the answer boundary
		// the read checks.
		promptLogprobs := make([]string, 0, len(ids))
		promptLogprobs = append(promptLogprobs, "null")
		for _, id := range ids[1:] {
			decoded, err := json.Marshal(tokenizer.Decode([]int{id}))
			if err != nil {
				panic(err)
			}
			promptLogprobs = append(promptLogprobs, fmt.Sprintf(
				`{"%d":{"logprob":-0.01,"rank":1,"decoded_token":%s}}`, id, decoded))
		}
		top := make([]string, 0, len(letters))
		for index, letter := range letters {
			logprob := -10.0
			if index == winner {
				logprob = 0
			}
			top = append(top, fmt.Sprintf(`{"token":%q,"logprob":%v}`, letter, logprob))
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},`+
			`"logprobs":{"content":[{"token":%q,"logprob":%v,"top_logprobs":[%s]}]}}],`+
			`"prompt_logprobs":[%s],"usage":{"prompt_tokens":%d,"completion_tokens":1}}`,
			letters[winner], letters[winner], 0, strings.Join(top, ","),
			strings.Join(promptLogprobs, ","), len(ids))
	}))

	readout, err := NewReadout(ReadoutConfig{
		BaseURL: server.URL, Model: "shisa-ai/shisa-de-2", HTTPClient: server.Client(),
	})
	if err != nil {
		panic(err)
	}
	readout.SetTokenizer(tokenizer)
	model, err := NewDecisionModel(DecisionModelConfig{
		BaseURL:   server.URL,
		Model:     "shisa-ai/shisa-de-2",
		Tokenizer: "shisa-ai/shisa-de-2",
		Readout:   readout,
	})
	if err != nil {
		panic(err)
	}
	return model
}

// A label set can be an ordered scale instead. The value is the
// probability-weighted position, and the level it sits on is on the answer.
func ExampleDecisionModel_Classify_score() {
	model := exampleModel(exampleRead(3, 2))
	defer model.Close()

	decision, err := model.Classify(context.Background(),
		map[string]any{"text": "The delivery is four days late."},
		map[string]HeadSpec{"urgency": {Levels: []any{"low", "medium", "high"}}},
		ClassifyOptions{})
	if err != nil {
		panic(err)
	}
	fmt.Println(decision.Values["urgency"])
	fmt.Printf("%.3f\n", *decision.Answers["urgency"].Score)
	// Output:
	// high
	// 1.985
}

// MultiLabel asks one yes/no question per label and returns the labels above the
// threshold, which is how a head where several labels can apply at once is read.
func ExampleDecisionModel_Classify_multiLabel() {
	model := exampleModel(exampleRead(2, 0), exampleRead(2, 1))
	defer model.Close()

	decision, err := model.Classify(context.Background(),
		map[string]any{"text": "The invoice is wrong and the site is down."},
		map[string]HeadSpec{"topic": {
			Options:    []Option{{Key: "billing"}, {Key: "outage"}},
			MultiLabel: true,
			// Nil Threshold means 0.5.
			Instructions: "Does this message concern {label}?",
		}}, ClassifyOptions{})
	if err != nil {
		panic(err)
	}
	fmt.Println(decision.Values["topic"])
	// Output: [billing]
}

// -- ask typed questions ------------------------------------------------------

// Decide takes Noul (yes/no), Choice (one of several) and Score (an ordered
// scale) in one call. The heads run concurrently, so the order the questions are
// asked in is by name and not by map iteration.
func ExampleDecisionModel_Decide() {
	// The heads are asked in name order: asks_for, is_spam, risk.
	model := exampleModel(
		exampleRead(3, 0), // asks_for: three options, the first wins
		exampleRead(2, 0), // is_spam: a noul over two codes
		exampleRead(3, 2)) // risk: three levels, the last wins
	defer model.Close()

	decision, err := model.Decide(context.Background(),
		"Reply with your full card number and CVV to claim your prize.",
		map[string]Question{
			"is_spam": NewNoul("Is this message spam?"),
			"asks_for": NewChoice("What does the sender want?", []Option{
				{Key: "card_details", Description: "Payment card details"},
				{Key: "callback", Description: "A support callback"},
				{Key: "nothing", Description: "Nothing; it is routine"},
			}),
			"risk": NewScore("How risky is acting on this message?",
				[]any{"Safe", "Suspicious", "Dangerous"}),
		}, CallOptions{})
	if err != nil {
		panic(err)
	}

	// A noul's value is the probability of yes, a choice's is the option key,
	// and a score's is the weighted position with its level on the answer.
	fmt.Printf("is_spam   %.3f\n", decision.Values["is_spam"])
	fmt.Println("asks_for ", decision.Values["asks_for"])
	fmt.Printf("risk      %.3f %s\n", decision.Values["risk"],
		*decision.Answers["risk"].Level)
	fmt.Println("requests ", decision.Usage["requests"])
	// Output:
	// is_spam   0.974
	// asks_for  card_details
	// risk      1.985 Dangerous
	// requests  3
}

// -- probabilities and calibration -------------------------------------------

// Every answer says whether its probabilities were calibrated, which read
// produced them, and what it cost. A DE-2 answer read twice is tempered by the
// repeated read's own fit; one that thought first is tempered by the thinking
// read's.
func ExampleDecisionModel_Decide_calibration() {
	model := exampleModel(exampleRead(2, 0))
	defer model.Close()

	decision, err := model.Decide(context.Background(), map[string]any{"text": "hi"},
		map[string]Question{"intent": NewChoice("Which applies?", []Option{
			{Key: "billing"}, {Key: "shipping"}})}, CallOptions{})
	if err != nil {
		panic(err)
	}
	answer := decision.Answers["intent"]
	fmt.Println("strategy  ", answer.Strategy)
	fmt.Println("calibrated", answer.Calibrated)
	fmt.Println("temperature", answer.Temperature)
	// A repeated read is one request: the question is shown twice in one prompt.
	fmt.Println("requests  ", answer.Requests)
	// Output:
	// strategy   repeat2
	// calibrated true
	// temperature 1.89
	// requests   1
}

// Pass Calibrated to get the raw distribution instead, which is what a
// threshold you fitted yourself should be driven by.
func ExampleDecisionModel_Decide_uncalibrated() {
	model := exampleModel(exampleRead(2, 0))
	defer model.Close()

	off := false
	decision, err := model.Decide(context.Background(), map[string]any{"text": "hi"},
		map[string]Question{"intent": NewChoice("Which applies?", []Option{
			{Key: "billing"}, {Key: "shipping"}})}, CallOptions{Calibrated: &off})
	if err != nil {
		panic(err)
	}
	fmt.Println(decision.Answers["intent"].Calibrated)
	fmt.Println(decision.Answers["intent"].Temperature)
	// Output:
	// false
	// 1
}

// When the distribution itself is the answer, Probability asks for one logical
// read per question: it skips DE-2's thinking step, so every answer is a single
// pass over all of its options.
func ExampleDecisionModel_Decide_probability() {
	model := exampleModel(exampleRead(2, 0))
	defer model.Close()

	off := false
	decision, err := model.Decide(context.Background(),
		map[string]any{"forecast": "Rain is expected tomorrow afternoon."},
		map[string]Question{"rain": NewNoul("Will it rain tomorrow?")},
		CallOptions{Probability: true, Calibrated: &off})
	if err != nil {
		panic(err)
	}
	answer := decision.Answers["rain"]
	fmt.Println("strategy", answer.Strategy)
	fmt.Println("keys    ", exampleKeys(answer.Probabilities))
	fmt.Printf("p(yes)   %.6f\n", answer.Probabilities["yes"])
	// Output:
	// strategy repeat2
	// keys     [no yes]
	// p(yes)   0.999955
}

// -- large label sets ---------------------------------------------------------

// DE-2 reads up to 256 options in one question and returns an ordinary
// distribution over them: no chunking and no finalist round. Up to 128 options
// is one request and more than that is two, because the candidate block is
// written in two parts. Option keys have to be distinct.
func ExampleDecisionModel_Decide_largeLabelSet() {
	options := make([]Option, 0, 200)
	for index := 0; index < 200; index++ {
		options = append(options, Option{
			Key:         fmt.Sprintf("queue_%d", index),
			Description: fmt.Sprintf("Support queue %d", index),
		})
	}
	model := exampleModel(exampleRead(200, 77), exampleRead(200, 77))
	defer model.Close()

	decision, err := model.Decide(context.Background(), "Route this support request",
		map[string]Question{"route": NewChoice("Which queue applies?", options)},
		CallOptions{})
	if err != nil {
		panic(err)
	}
	answer := decision.Answers["route"]
	fmt.Println("choice  ", *answer.Choice)
	fmt.Println("options ", len(answer.Probabilities))
	fmt.Println("requests", answer.Requests)
	// Output:
	// choice   queue_77
	// options  200
	// requests 2
}

// -- read settings ------------------------------------------------------------

// Policy names a whole read, on the model or on one call. The three DE-2
// policies trade quality for latency: repeat-think (the default) thinks when it
// is unsure, repeat never does, and direct shows the question once instead of
// twice.
func ExampleDecisionModel_Decide_policy() {
	direct := "direct"
	model := exampleModel(exampleRead(2, 0))
	defer model.Close()

	decision, err := model.Decide(context.Background(), map[string]any{"text": "hi"},
		map[string]Question{"intent": NewChoice("Which applies?", []Option{
			{Key: "billing"}, {Key: "shipping"}})},
		CallOptions{Overrides: OptionOverrides{Policy: &direct}})
	if err != nil {
		panic(err)
	}
	fmt.Println("strategy   ", decision.Answers["intent"].Strategy)
	fmt.Println("temperature", decision.Answers["intent"].Temperature)
	fmt.Println("meta policy", decision.Meta["policy"])
	// Output:
	// strategy    direct
	// temperature 1.81
	// meta policy direct
}

// The same settings can be set on the model, and a call inherits whatever it
// does not override. Reasoning always follows a repeated read.
func ExampleNewDecisionModel_readSettings() {
	gate := 0.9
	budget := 256
	model, err := NewDecisionModel(DecisionModelConfig{
		BaseURL: "http://localhost:8000/v1",
		Model:   "shisa-ai/shisa-de-2",
		Overrides: OptionOverrides{
			ReasoningProb: &gate,
			ThinkBudget:   &budget,
		},
	})
	if err != nil {
		panic(err)
	}
	defer model.Close()

	fmt.Println(model.Policy())
	fmt.Println(model.Reasoning())
	fmt.Println(model.ThinkGate())
	fmt.Println(model.ThinkBudget())
	// Output:
	// repeat-think
	// true
	// 0.9
	// 256
}

// -- use a local server -------------------------------------------------------

// FromEndpoint points at a server you run yourself. An empty key is not the same
// as an unset one: it disables authentication instead of falling back to the
// environment, which is what a local server wants.
func ExampleFromEndpoint() {
	empty := ""
	model, err := FromEndpoint("http://127.0.0.1:8000/v1", "shisa-ai/shisa-de-2",
		DecisionModelConfig{APIKey: &empty})
	if err != nil {
		panic(err)
	}
	defer model.Close()

	fmt.Println(model.BaseURL)
	fmt.Println(model.Model)
	fmt.Println(model.Family)
	// Output:
	// http://127.0.0.1:8000
	// shisa-ai/shisa-de-2
	// de2
}

// A checkpoint your server names differently needs its family said out loud, and
// a tokenizer named, because the family decides how the model is asked and the
// tokenizer decides which codes are read.
func ExampleFromPretrained() {
	model, err := FromPretrained("my-de2-finetune", DecisionModelConfig{
		BaseURL:   "http://127.0.0.1:8000/v1",
		Family:    "de2",
		Tokenizer: "google/gemma-4-26B-A4B-it",
	})
	if err != nil {
		panic(err)
	}
	defer model.Close()

	fmt.Println(model.Family)
	fmt.Println(model.FamilySource)
	fmt.Println(model.FamilyExplicit)
	// Output:
	// de2
	// declared
	// true
}

// -- the command line's report ------------------------------------------------

// Health is what `shisa-de doctor` prints. The probe spends one question; the
// plain report only lists models and runs the boundary check.
func ExampleDecisionModel_Health() {
	model := exampleModel(exampleRead(2, 0))
	defer model.Close()

	report := model.Health(context.Background(), false)
	fmt.Println(report["model"])
	fmt.Println(report["models_status"])
	fmt.Println(report["model_listed"])
	fmt.Println(report["calibration_level"])
	fmt.Println(report["ok"])
	// Output:
	// shisa-ai/shisa-de-2
	// 200
	// true
	// checkpoint
	// true
}

// -- JSON ---------------------------------------------------------------------

// Decisions are plain Go values, so they marshal with encoding/json.
func ExampleDecision_Values() {
	model := exampleModel(exampleRead(2, 0))
	defer model.Close()

	decision, err := model.Classify(context.Background(), map[string]any{"text": "hi"},
		map[string]HeadSpec{"intent": {Options: []Option{{Key: "billing"}, {Key: "shipping"}}}},
		ClassifyOptions{})
	if err != nil {
		panic(err)
	}
	encoded, err := json.Marshal(decision.Values)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(encoded))
	// Output: {"intent":"billing"}
}
