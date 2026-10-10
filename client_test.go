package shisade

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// newStubModel is a DecisionModel over a server that answers with the recorded
// bodies in order, with MaxWorkers of 1 so the request order is the work order.
func newStubModel(t *testing.T, count int, bodies ...string) (*DecisionModel, *policyServer) {
	t.Helper()
	readout, server := newPolicyReadout(t, count, bodies...)
	model, err := NewDecisionModel(DecisionModelConfig{
		BaseURL:    "http://stub.invalid",
		Model:      "shisa-ai/shisa-de-2",
		Tokenizer:  "shisa-ai/shisa-de-2",
		Readout:    readout,
		MaxWorkers: 1,
	})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	return model, server
}

func modelsBody(ids ...string) string {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, fmt.Sprintf(`{"id":%q}`, id))
	}
	return fmt.Sprintf(`{"data":[%s]}`, strings.Join(entries, ","))
}

// TestAnswerValueAndLabel pins what the three answer types return.
func TestAnswerValueAndLabel(t *testing.T) {
	noul := Answer{Type: "noul", Noul: floatPointer(0.75)}
	if got := noul.Value(); got != 0.75 {
		t.Errorf("a noul's value is %v", got)
	}
	if got := noul.Label(); got != 0.75 {
		t.Errorf("a noul's label is %v", got)
	}
	choice := Answer{Type: "choice", Choice: stringPointer("billing")}
	if got := choice.Value(); got != "billing" {
		t.Errorf("a choice's value is %v", got)
	}
	if got := choice.Label(); got != "billing" {
		t.Errorf("a choice's label is %v", got)
	}
	score := Answer{Type: "score", Score: floatPointer(2.1), Level: stringPointer("medium"),
		Choice: stringPointer("2")}
	if got := score.Value(); got != 2.1 {
		t.Errorf("a score's value is %v, want the weighted position", got)
	}
	if got := score.Label(); got != "medium" {
		t.Errorf("a score's label is %v, want the level it sits on", got)
	}
	// A score with no level falls back to the position.
	bare := Answer{Type: "score", Score: floatPointer(1.5)}
	if got := bare.Label(); got != 1.5 {
		t.Errorf("a score with no level has label %v", got)
	}
	// A type that does not use a field yields nil, which is Python's None.
	if got := (Answer{Type: "choice"}).Value(); got != nil {
		t.Errorf("a choice with no option has value %v", got)
	}
	if got := (Answer{Type: "noul"}).Value(); got != nil {
		t.Errorf("a noul with no probability has value %v", got)
	}
}

// TestAnswerWireAndDict pins the two output shapes.
func TestAnswerWireAndDict(t *testing.T) {
	confidence := 0.8
	noul := Answer{Type: "noul", Noul: floatPointer(0.75), Probabilities: map[string]float64{"yes": 0.75, "no": 0.25},
		Confidence: &confidence}
	wire := noul.ToWire()
	if wire["type"] != "noul" || wire["noul"] != 0.75 {
		t.Errorf("a noul's wire shape is %v", wire)
	}
	if _, ok := wire["confidence"]; ok {
		t.Error("a noul's wire shape carries a confidence, which TypeSafe does not return")
	}
	if wire["probabilities"] != nil {
		t.Error("a noul's wire shape carries probabilities")
	}

	choice := Answer{Type: "choice", Choice: stringPointer("a"),
		Probabilities: map[string]float64{"a": 1}, Confidence: &confidence,
		ScoreSemantics: "option-softmax", Strategy: StrategyRepeat, Requests: 1}
	wire = choice.ToWire()
	if wire["choice"] != "a" {
		t.Errorf("a choice's wire shape is %v", wire)
	}
	if _, ok := wire["confidence"]; !ok {
		t.Error("a choice's wire shape dropped its confidence")
	}
	if _, ok := wire["strategy"]; ok {
		t.Error("an option-softmax choice's wire shape carries a strategy, which TypeSafe does not return")
	}
	// A choice whose semantics are not the ordinary softmax says so.
	overflow := choice
	overflow.ScoreSemantics = "conditional-on-finalists"
	wire = overflow.ToWire()
	if wire["strategy"] != StrategyRepeat || wire["score_semantics"] != "conditional-on-finalists" {
		t.Errorf("a conditional choice's wire shape is %v", wire)
	}
	if wire["calibrated"] != false {
		t.Error("a conditional choice reports itself as calibrated")
	}

	score := Answer{Type: "score", Score: floatPointer(1.5), Legend: map[string]string{"0": "low"},
		Probabilities: map[string]float64{"0": 1}, Confidence: &confidence}
	wire = score.ToWire()
	if wire["type"] != "score" || wire["legend"] == nil || wire["score"] == nil {
		t.Errorf("a score's wire shape is %v", wire)
	}

	// to_dict carries everything TypeSafe does not.
	full := Answer{Type: "choice", Choice: stringPointer("a"), Calibrated: true, Temperature: 1.89,
		Requests: 2, Probabilities: map[string]float64{"a": 1}, Confidence: &confidence,
		Strategy: StrategyRepeat, ScoreSemantics: "option-softmax", Stages: 1, LogicalReads: 1,
		MissingFromTop: []string{"B"}}
	dict := full.ToDict()
	for _, key := range []string{"type", "value", "confidence", "probabilities", "calibrated",
		"temperature", "requests", "strategy", "score_semantics", "stages", "logical_reads",
		"missing_from_top"} {
		if _, ok := dict[key]; !ok {
			t.Errorf("to_dict dropped %q: %v", key, dict)
		}
	}
	if _, ok := dict["thought_closed"]; ok {
		t.Error("to_dict carries a thought for a read that had none")
	}
	withThought := full
	closed := false
	withThought.ThoughtClosed = &closed
	withThought.ThoughtTokens = 3
	dict = withThought.ToDict()
	if dict["thought_tokens"] != 3 || dict["thought_closed"] != false {
		t.Errorf("to_dict lost the thought: %v", dict)
	}
}

// TestDecisionViews pins the typed views and the TypeSafe body.
func TestDecisionViews(t *testing.T) {
	decision := &Decision{
		Values: map[string]any{"a": "x"},
		Answers: map[string]Answer{
			"a": {Type: "choice", Choice: stringPointer("x")},
			"b": {Type: "noul", Noul: floatPointer(0.5)},
			"c": {Type: "score", Score: floatPointer(1)},
		},
		Usage: map[string]any{"input_tokens": 10, "output_tokens": 2},
		Meta:  map[string]any{"model": "shisa-ai/shisa-de-2"},
	}
	if len(decision.Choices()) != 1 || len(decision.Nouls()) != 1 || len(decision.Scores()) != 1 {
		t.Errorf("the typed views are %v %v %v",
			decision.Choices(), decision.Nouls(), decision.Scores())
	}
	wire := decision.ToWire()
	if wire["model"] != "shisa-ai/shisa-de-2" {
		t.Errorf("the wire body's model is %v", wire["model"])
	}
	usage, _ := wire["usage"].(map[string]any)
	if usage["input_tokens"] != 10 || usage["output_tokens"] != 2 {
		t.Errorf("the wire body's usage is %v", usage)
	}
	answers, _ := wire["answers"].(map[string]any)
	if len(answers) != 3 {
		t.Errorf("the wire body has %d answers", len(answers))
	}
	// An empty decision reports zeros rather than panicking.
	empty := &Decision{Meta: map[string]any{}, Usage: map[string]any{}}
	usage, _ = empty.ToWire()["usage"].(map[string]any)
	if usage["input_tokens"] != 0 {
		t.Errorf("an empty decision's usage is %v", usage)
	}
}

// TestQuestionFromHeadShorthands pins the label-set forms and the default
// question each one gets.
func TestQuestionFromHeadShorthands(t *testing.T) {
	cases := []struct {
		name         string
		spec         HeadSpec
		kind         string
		instructions string
		options      int
	}{
		{"options", HeadSpec{Options: []Option{{Key: "a", Description: "A"}, {Key: "b"}}},
			"choice", "What is the intent?", 2},
		{"options with instructions",
			HeadSpec{Options: []Option{{Key: "a"}, {Key: "b"}}, Instructions: "Which one?"},
			"choice", "Which one?", 2},
		{"levels", HeadSpec{Levels: []any{"low", "high"}}, "score", "What is the severity?", 2},
		{"levels with instructions", HeadSpec{Levels: []any{"low", "high"}, Instructions: "How bad?"},
			"score", "How bad?", 2},
		{"multi-label", HeadSpec{Options: []Option{{Key: "a"}, {Key: "b"}}, MultiLabel: true},
			"multi_label", "Is {label} one of the topic?", 0},
		{"a built question", HeadSpec{Question: NewNoul("Is it?")}, "noul", "Is it?", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head := "intent"
			if tc.kind == "score" {
				head = "severity"
			}
			if tc.kind == "multi_label" {
				head = "topic"
			}
			question, err := questionFromHead(head, tc.spec)
			if err != nil {
				t.Fatalf("questionFromHead: %v", err)
			}
			if question.Kind() != tc.kind {
				t.Errorf("kind = %q, want %q", question.Kind(), tc.kind)
			}
			if tc.kind != "multi_label" && question.Instructions() != tc.instructions {
				t.Errorf("instructions = %q, want %q", question.Instructions(), tc.instructions)
			}
			if tc.kind != "multi_label" && len(question.Options()) != tc.options {
				t.Errorf("%d options, want %d", len(question.Options()), tc.options)
			}
		})
	}
	// The default question replaces underscores with spaces.
	question, err := questionFromHead("customer_intent", HeadSpec{Options: []Option{{Key: "a"}, {Key: "b"}}})
	if err != nil {
		t.Fatalf("questionFromHead: %v", err)
	}
	if question.Instructions() != "What is the customer intent?" {
		t.Errorf("the default question is %q", question.Instructions())
	}
	// A multi-label head without a template uses its own default.
	multi, err := questionFromHead("topic", HeadSpec{Options: []Option{{Key: "a"}}, MultiLabel: true})
	if err != nil {
		t.Fatalf("questionFromHead: %v", err)
	}
	template := multi.(*multiLabelHead).template
	if template != "Is {label} one of the topic?" {
		t.Errorf("the default multi-label template is %q", template)
	}
	// The label is substituted where the template names it, and appended where
	// it does not.
	if got := noulFor(t, multi, "billing").Instructions(); got != "Is billing one of the topic?" {
		t.Errorf("the substituted question is %q", got)
	}
	plain, err := questionFromHead("topic", HeadSpec{Options: []Option{{Key: "a"}}, MultiLabel: true,
		Instructions: "Is it about"})
	if err != nil {
		t.Fatalf("questionFromHead: %v", err)
	}
	if got := noulFor(t, plain, "billing").Instructions(); got != "Is it about billing" {
		t.Errorf("the appended question is %q", got)
	}

	for _, tc := range []struct {
		name string
		spec HeadSpec
	}{
		{"no options", HeadSpec{}},
		{"an empty option list", HeadSpec{Options: []Option{}}},
		// Python's `{"labels": {k: v}, "multi_label": True}` reads one choice
		// over the described labels and ignores `multi_label`. This port refuses
		// the combination rather than reading the same input a second way and
		// dropping the descriptions.
		{"multi-label with descriptions", HeadSpec{
			Options:    []Option{{Key: "a", Description: "A"}, {Key: "b", Description: "B"}},
			MultiLabel: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := questionFromHead("intent", tc.spec); err == nil {
				t.Errorf("%s was accepted", tc.name)
			}
		})
	}
	// A multi-label head with no descriptions is the one it reads.
	multiOnly, err := questionFromHead("topic", HeadSpec{
		Options: []Option{{Key: "a"}, {Key: "b"}}, MultiLabel: true})
	if err != nil {
		t.Fatalf("a multi-label head with bare keys: %v", err)
	}
	if multiOnly.(*multiLabelHead).labels[0] != "a" {
		t.Errorf("the labels are %v", multiOnly.(*multiLabelHead).labels)
	}
}

// TestMultiLabelTemplateIsPythonFormat pins the template substitution against
// Python's `str.format(label=...)`.
//
// The template is the prompt, and the prompt is the answer, so a template with
// doubled braces or an unknown field must not render differently here than it
// does in the Python client.
func TestMultiLabelTemplateIsPythonFormat(t *testing.T) {
	cases := []struct{ template, label, want string }{
		{"Is {label} one of the topics?", "billing", "Is billing one of the topics?"},
		{"Is {label} a {{thing}}?", "billing", "Is billing a {thing}?"},
		{"Is {label} one of them? {label}", "billing", "Is billing one of them? billing"},
	}
	for _, tc := range cases {
		got, err := formatLabel(tc.template, tc.label)
		if err != nil {
			t.Errorf("formatLabel(%q): %v", tc.template, err)
			continue
		}
		if got != tc.want {
			t.Errorf("formatLabel(%q, %q) = %q, want %q", tc.template, tc.label, got, tc.want)
		}
	}
	// A field the template cannot fill is refused rather than passed through.
	for _, template := range []string{"Is {other}?", "Is {label", "Is label}", "Is {label!r}?"} {
		if _, err := formatLabel(template, "billing"); err == nil {
			t.Errorf("formatLabel(%q) was accepted", template)
		}
	}
}

// TestNewDecisionModelForwardsTheDeclaredFamily pins that a declaration reaches
// the readout, not only the client.
//
// The client resolves the family to decide whether it can read the checkpoint at
// all; the readout resolves it again, and a declaration the readout never sees
// leaves the two layers free to disagree. A DE-2 checkpoint served under a
// neutral alias with a DE-1-named tokenizer is the case that tells them apart:
// the client's own resolver accepts the declaration, and without forwarding it
// the readout's resolver names DE-1 and refuses.
func TestNewDecisionModelForwardsTheDeclaredFamily(t *testing.T) {
	_, err := NewDecisionModel(DecisionModelConfig{
		BaseURL: "http://stub.invalid", Model: "my-checkpoint",
		Tokenizer: "shisa-ai/shisa-de-1", Family: FamilyDE2,
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("a declared DE-2 family with a DE-1 tokenizer was refused: %v", err)
	}
	// Without the declaration the tokenizer slug is what names the family, and
	// this port cannot read it.
	_, err = NewDecisionModel(DecisionModelConfig{
		BaseURL: "http://stub.invalid", Model: "my-checkpoint",
		Tokenizer: "shisa-ai/shisa-de-1", HTTPClient: http.DefaultClient,
	})
	if err == nil {
		t.Fatal("a DE-1 tokenizer named the family and was still accepted")
	}
	var unsupported *UnsupportedFamilyError
	if !errors.As(err, &unsupported) {
		t.Errorf("the error is %v, want an UnsupportedFamilyError", err)
	}
}

// TestMultiLabelThreshold pins the cutoff, which is 0.5 unless the head says
// otherwise.
func TestMultiLabelThreshold(t *testing.T) {
	if got := multiLabelThreshold(HeadSpec{}); got != 0.5 {
		t.Errorf("the default threshold is %v, want 0.5", got)
	}
	threshold := 0.8
	if got := multiLabelThreshold(HeadSpec{Threshold: &threshold}); got != 0.8 {
		t.Errorf("the threshold is %v, want 0.8", got)
	}
}

// TestClassifyMultiLabel pins the multi-label view: one noul per label, and the
// labels at or above the threshold come back.
func TestClassifyMultiLabel(t *testing.T) {
	// Two nouls, one above and one below the default cutoff.
	model, server := newStubModel(t, 2,
		codeReadBody(map[string]float64{"A": -0.1, "B": -3.0}),
		codeReadBody(map[string]float64{"A": -3.0, "B": -0.1}))
	decision, err := model.Classify(context.Background(), map[string]any{"e": "x"},
		map[string]HeadSpec{"topic": {Options: []Option{{Key: "billing"}, {Key: "outage"}},
			MultiLabel: true, Instructions: "Is it {label}?"}}, ClassifyOptions{})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	labels, ok := decision.Values["topic"].([]string)
	if !ok {
		t.Fatalf("the multi-label value is %T, want a list of labels", decision.Values["topic"])
	}
	if len(labels) != 1 || labels[0] != "billing" {
		t.Errorf("the labels above the cutoff are %v, want [billing]", labels)
	}
	// The per-label answers are removed from the answer map and kept in Raw.
	if _, ok := decision.Answers["topic:billing"]; ok {
		t.Error("the per-label answer is still in the answer map")
	}
	if _, ok := decision.Raw["topic:billing"]; !ok {
		t.Error("the per-label answer is not in Raw")
	}
	if server.at != 2 {
		t.Errorf("the call spent %d requests, want 2", server.at)
	}
	// The confidence view keeps the label and the confidence.
	model, _ = newStubModel(t, 2,
		codeReadBody(map[string]float64{"A": -0.1, "B": -3.0}),
		codeReadBody(map[string]float64{"A": -3.0, "B": -0.1}))
	decision, err = model.Classify(context.Background(), map[string]any{"e": "x"},
		map[string]HeadSpec{"topic": {Options: []Option{{Key: "billing"}, {Key: "outage"}},
			MultiLabel: true, Instructions: "Is it {label}?"}},
		ClassifyOptions{IncludeConfidence: true})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	entries, ok := decision.Values["topic"].([]map[string]any)
	if !ok || len(entries) != 1 || entries[0]["label"] != "billing" {
		t.Errorf("the confidence view is %v", decision.Values["topic"])
	}
	// The probabilities view adds the distribution.
	model, _ = newStubModel(t, 2,
		codeReadBody(map[string]float64{"A": -0.1, "B": -3.0}),
		codeReadBody(map[string]float64{"A": -3.0, "B": -0.1}))
	decision, err = model.Classify(context.Background(), map[string]any{"e": "x"},
		map[string]HeadSpec{"topic": {Options: []Option{{Key: "billing"}, {Key: "outage"}},
			MultiLabel: true, Instructions: "Is it {label}?"}},
		ClassifyOptions{IncludeProbabilities: true})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	entries, ok = decision.Values["topic"].([]map[string]any)
	if !ok || len(entries) != 1 || entries[0]["probabilities"] == nil {
		t.Errorf("the probabilities view is %v", decision.Values["topic"])
	}
	// A threshold above the answer's probability drops the label.
	high := 0.99
	model, _ = newStubModel(t, 2,
		codeReadBody(map[string]float64{"A": -0.1, "B": -3.0}),
		codeReadBody(map[string]float64{"A": -3.0, "B": -0.1}))
	decision, err = model.Classify(context.Background(), map[string]any{"e": "x"},
		map[string]HeadSpec{"topic": {Options: []Option{{Key: "billing"}, {Key: "outage"}},
			MultiLabel: true, Instructions: "Is it {label}?", Threshold: &high}}, ClassifyOptions{})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if labels := decision.Values["topic"].([]string); len(labels) != 0 {
		t.Errorf("a threshold of 0.99 still returned %v", labels)
	}
}

// TestClassifyViews pins the ordinary head's three views.
func TestClassifyViews(t *testing.T) {
	body := codeReadBody(map[string]float64{"A": -0.1, "B": -2.0})
	heads := map[string]HeadSpec{"route": {Options: []Option{
		{Key: "billing", Description: "Charges"}, {Key: "shipping", Description: "Delivery"}}}}

	model, _ := newStubModel(t, 2, body)
	decision, err := model.Classify(context.Background(), map[string]any{"e": "x"}, heads, ClassifyOptions{})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if decision.Values["route"] != "billing" {
		t.Errorf("the plain view is %v", decision.Values["route"])
	}
	if _, ok := decision.Values["route"].(string); !ok {
		t.Errorf("the plain view is a %T, want a string", decision.Values["route"])
	}
	if decision.Answers["route"].Choice == nil || *decision.Answers["route"].Choice != "billing" {
		t.Errorf("the answer is %v", decision.Answers["route"])
	}

	model, _ = newStubModel(t, 2, body)
	decision, err = model.Classify(context.Background(), map[string]any{"e": "x"}, heads,
		ClassifyOptions{IncludeConfidence: true})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	view, ok := decision.Values["route"].(map[string]any)
	if !ok || view["label"] != "billing" || view["confidence"] == nil {
		t.Errorf("the confidence view is %v", decision.Values["route"])
	}
	if _, ok := view["probabilities"]; ok {
		t.Error("the confidence view carries probabilities")
	}

	model, _ = newStubModel(t, 2, body)
	decision, err = model.Classify(context.Background(), map[string]any{"e": "x"}, heads,
		ClassifyOptions{IncludeProbabilities: true})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	view, ok = decision.Values["route"].(map[string]any)
	if !ok || view["probabilities"] == nil {
		t.Errorf("the probabilities view is %v", decision.Values["route"])
	}
}

// TestDecideReturnsThePrimaryValue pins that Decide's map holds the value rather
// than the label.
func TestDecideReturnsThePrimaryValue(t *testing.T) {
	model, _ := newStubModel(t, 5, codeReadBody(map[string]float64{
		"A": -0.1, "B": -2.0, "C": -3.0, "D": -4.0, "E": -5.0}))
	decision, err := model.Decide(context.Background(), map[string]any{"e": "x"},
		map[string]Question{"severity": NewScore("How bad?", []any{"none", "low", "high", "severe", "critical"})},
		CallOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	value, ok := decision.Values["severity"].(float64)
	if !ok {
		t.Fatalf("the score's value is %T, want a number", decision.Values["severity"])
	}
	if value < 0 || value > 4 {
		t.Errorf("the weighted position is %v, want a position on the five levels", value)
	}
	// The label is the level, which is what Classify returns and Decide does
	// not; the position above is the value.
	if got := decision.Answers["severity"].Label(); got != "none" {
		t.Errorf("the label is %v, want the level the argmax sits on", got)
	}
	// A score's probabilities are keyed by position and its legend maps them to
	// the level text.
	answer := decision.Answers["severity"]
	if len(answer.Probabilities) != 5 || answer.Legend["0"] != "none" {
		t.Errorf("the score's distribution is %v with legend %v", answer.Probabilities, answer.Legend)
	}
}

// TestDecideRefusesProbabilityWithReasoning pins the one combination the Python
// client refuses before spending anything.
func TestDecideRefusesProbabilityWithReasoning(t *testing.T) {
	model, server := newStubModel(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	_, err := model.Decide(context.Background(), map[string]any{"e": "x"},
		map[string]Question{"urgent": NewNoul("Is it urgent?")},
		CallOptions{Probability: true, Overrides: OptionOverrides{Reasoning: boolPointer(true)}})
	if err == nil {
		t.Fatal("probability with reasoning was accepted")
	}
	if !strings.Contains(err.Error(), "cannot be combined with reasoning=true") {
		t.Errorf("the error is %v", err)
	}
	if server.at != 0 {
		t.Errorf("a refused call spent %d requests", server.at)
	}
}

// TestDecideValidatesBeforeSending pins that every head is checked before any
// request goes out, so one bad head does not leave half a call on the wire.
func TestDecideValidatesBeforeSending(t *testing.T) {
	model, server := newStubModel(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	_, err := model.Decide(context.Background(), map[string]any{"e": "x"},
		map[string]Question{
			"good": NewNoul("Is it urgent?"),
			"bad":  NewChoice("Which?", nil),
		}, CallOptions{})
	if err == nil {
		t.Fatal("a head with no options was accepted")
	}
	if server.at != 0 {
		t.Errorf("a refused call spent %d requests", server.at)
	}
}

// TestSystemOneIsDecide pins the second name for the full call.
func TestSystemOneIsDecide(t *testing.T) {
	model, _ := newStubModel(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	decision, err := model.SystemOne(context.Background(), map[string]any{"e": "x"},
		map[string]Question{"urgent": NewNoul("Is it urgent?")}, CallOptions{})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if len(decision.Answers) != 1 {
		t.Errorf("SystemOne returned %d answers", len(decision.Answers))
	}
}

// TestDecisionModelRefusesDE1 pins the DE-2-only guard at the friendly layer.
func TestDecisionModelRefusesDE1(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	for _, model := range []string{"shisa-ai/shisa-de-1", "de-1", "de1-cont-lr1e5"} {
		_, err := NewDecisionModel(DecisionModelConfig{Model: model, Readout: readout})
		if err == nil {
			t.Errorf("%s was accepted", model)
			continue
		}
		var unsupported *UnsupportedFamilyError
		if !errors.As(err, &unsupported) {
			t.Errorf("%s: the error is %v, want an UnsupportedFamilyError", model, err)
			continue
		}
		if unsupported.Family != FamilyDE1 {
			t.Errorf("%s: the error names family %q", model, unsupported.Family)
		}
		// The message has to carry the fix: the default model id is DE-1, so a
		// zero config lands here rather than on a working client.
		if !strings.Contains(err.Error(), "Pass a DE-2") {
			t.Errorf("%s: the error does not say what to do: %v", model, err)
		}
	}
	// The default model id is DE-1, so the zero config is this error and not a
	// working client.
	if _, err := NewDecisionModel(DecisionModelConfig{}); err == nil {
		t.Error("a zero config produced a client")
	}
}

// TestDecisionModelFamilyAssumption pins that an assumed family is reported as
// an assumption and not as a detection.
func TestDecisionModelFamilyAssumption(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	model, err := NewDecisionModel(DecisionModelConfig{Model: "my-checkpoint",
		Tokenizer: "my-tokenizer", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	if model.FamilyExplicit {
		t.Error("an assumed family reports itself as explicit")
	}
	if model.FamilySource != SourceAssumed {
		t.Errorf("the family source is %q, want %q", model.FamilySource, SourceAssumed)
	}
	if model.Family != FamilyDE2 {
		t.Errorf("the assumed family is %q", model.Family)
	}
	if !hasWarning(model, "names neither DE-1 nor DE-2") {
		t.Errorf("the assumption was not recorded as a warning: %v", model.Warnings)
	}
	// A tokenizer source that names a family is a detection, not an assumption.
	model, err = NewDecisionModel(DecisionModelConfig{Model: "my-checkpoint",
		Tokenizer: "shisa-ai/shisa-de-2", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	if !model.FamilyExplicit || model.FamilySource != SourceTokenizer {
		t.Errorf("a tokenizer-named family reports source %q, explicit %v",
			model.FamilySource, model.FamilyExplicit)
	}
	// An explicit declaration is reported as one.
	model, err = NewDecisionModel(DecisionModelConfig{Model: "my-checkpoint",
		Family: FamilyDE2, Tokenizer: "my-tokenizer", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	if !model.FamilyExplicit || model.FamilySource != SourceDeclared {
		t.Errorf("a declared family reports source %q, explicit %v",
			model.FamilySource, model.FamilyExplicit)
	}
	if hasWarning(model, "names neither DE-1 nor DE-2") {
		t.Errorf("a declared family still produced the assumption warning: %v", model.Warnings)
	}
}

// TestDecisionModelCalibration pins which records are applied and which are
// merely held.
func TestDecisionModelCalibration(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))

	// The fitted checkpoint: applied.
	model, err := NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	if model.CalibrationLevel != ApplicabilityCheckpoint || !model.CalibrationApplied {
		t.Errorf("the fitted checkpoint reports level %q applied %v",
			model.CalibrationLevel, model.CalibrationApplied)
	}
	if len(model.Warnings) != 0 {
		t.Errorf("the fitted checkpoint produced warnings: %v", model.Warnings)
	}
	_ = model

	// Another checkpoint of the same family: held, not applied, and the caller
	// is told why.
	model, err = NewDecisionModel(DecisionModelConfig{Model: "de2-candidate-lr5e5",
		Tokenizer: "de2-candidate-lr5e5", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	if model.CalibrationLevel != ApplicabilityFamily || model.CalibrationApplied {
		t.Errorf("another checkpoint reports level %q applied %v",
			model.CalibrationLevel, model.CalibrationApplied)
	}
	if !hasWarning(model, "not applied") {
		t.Errorf("the unapplied record was not recorded as a warning: %v", model.Warnings)
	}

	// A record the caller passes is their assertion and is applied as given.
	supplied := Calibration{
		Temperatures:   map[string]float64{"choice": 2.0, "noul": 2.0},
		ReadoutVersion: ReadoutVersionFor(FamilyDE2),
		Model:          "some-other-checkpoint",
		ServingShape:   "test",
		Family:         FamilyDE2,
	}
	model, err = NewDecisionModel(DecisionModelConfig{Model: "de2-candidate-lr5e5",
		Tokenizer: "de2-candidate-lr5e5", Calibration: &supplied, Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	if model.CalibrationLevel != ApplicabilityFamily || !model.CalibrationApplied {
		t.Errorf("a supplied record reports level %q applied %v",
			model.CalibrationLevel, model.CalibrationApplied)
	}
	if model.Calibration.ID() != supplied.ID() {
		t.Errorf("the client holds %q, the caller supplied %q", model.Calibration.ID(), supplied.ID())
	}
}

// TestDecisionModelRefusesABadMaxLogprobs pins the one numeric argument the
// Python client validates in the constructor.
func TestDecisionModelRefusesABadMaxLogprobs(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	if _, err := NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2",
		MaxLogprobs: -1, Readout: readout}); err == nil {
		t.Error("a negative max_logprobs was accepted")
	}
	// Zero means the default, not "ask for no logprobs".
	model, err := NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	if model.maxWorkers != 8 {
		t.Errorf("the default worker count is %d, want 8", model.maxWorkers)
	}
	model, err = NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2",
		MaxWorkers: -3, Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	if model.maxWorkers != 1 {
		t.Errorf("a negative worker count became %d, want 1", model.maxWorkers)
	}
}

// TestAPIKeyFromEnv pins the key's two environment variables and the trimming.
func TestAPIKeyFromEnv(t *testing.T) {
	for _, name := range APIKeyEnvs {
		t.Setenv(name, "")
	}
	if key := APIKeyFromEnv(); key != nil {
		t.Errorf("APIKeyFromEnv returned %q with nothing set", *key)
	}
	t.Setenv(APIKeyEnvs[1], "  from-the-second  ")
	key := APIKeyFromEnv()
	if key == nil || *key != "from-the-second" {
		t.Errorf("APIKeyFromEnv returned %v, want the trimmed second variable", key)
	}
	// The first variable wins.
	t.Setenv(APIKeyEnvs[0], "from-the-first")
	if key := APIKeyFromEnv(); key == nil || *key != "from-the-first" {
		t.Errorf("APIKeyFromEnv returned %v, want the first variable", key)
	}
	// A blank first variable falls through rather than winning with nothing.
	t.Setenv(APIKeyEnvs[0], "   ")
	if key := APIKeyFromEnv(); key == nil || *key != "from-the-second" {
		t.Errorf("a blank first variable did not fall through: %v", key)
	}
	// An explicit key beats the environment.
	readout, _ := newPolicyReadout(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	explicit := "explicit"
	if _, err := NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2",
		APIKey: &explicit, Readout: readout}); err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
}

// TestHealthOffline pins the report's shape and the two things it can fail on
// without spending a read.
func TestHealthOffline(t *testing.T) {
	readout, _ := newPolicyReadout(t, MaxCodes,
		modelsBody("shisa-ai/shisa-de-2", "other"))
	model, err := NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	report := model.Health(context.Background(), false)
	if report["ok"] != true {
		t.Errorf("a healthy client reports %v (boundary %v)", report["ok"], report["boundary_check"])
	}
	if report["boundary_check"] != "passed" {
		t.Errorf("the boundary check is %v", report["boundary_check"])
	}
	if report["model_listed"] != true {
		t.Errorf("the model listing is %v", report["model_listed"])
	}
	if report["calibration_level"] != string(ApplicabilityCheckpoint) {
		t.Errorf("the calibration level is %v", report["calibration_level"])
	}
	if report["readout_version"] != ReadoutVersionFor(FamilyDE2) {
		t.Errorf("the readout version is %v", report["readout_version"])
	}
	if report["policy"] != "repeat-think" || report["reads"] != "double" {
		t.Errorf("the policy is %v/%v", report["policy"], report["reads"])
	}
	// The report's keys are the Python client's, minus the two that only the
	// image path produces.
	for _, key := range []string{"base_url", "model", "model_family", "family_source",
		"family_explicit", "readout_version", "policy", "reads", "reasoning", "compound",
		"calibration", "calibration_model", "calibration_readout_version",
		"calibration_serving_shape", "calibration_level", "calibration_applied", "ok",
		"calibration_match", "models_status", "served_models", "model_listed",
		"boundary_check", "slots"} {
		if _, ok := report[key]; !ok {
			t.Errorf("the report is missing %q: %v", key, report)
		}
	}
	if _, ok := report["chat_system_render"]; ok {
		t.Error("the report carries the image path's chat render probe")
	}
	slots, ok := report["slots"].(map[string]any)
	if !ok || slots["count"] != MaxCodes {
		t.Errorf("the slot report is %v", report["slots"])
	}

	// A model the endpoint does not list fails the report.
	readout, _ = newPolicyReadout(t, MaxCodes, modelsBody("something-else"))
	model, err = NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	if report := model.Health(context.Background(), false); report["ok"] != false {
		t.Errorf("an unlisted model reports ok=%v", report["ok"])
	}

	// A record that does not match the served checkpoint fails the report even
	// when the endpoint answers, because a threshold fitted elsewhere is a
	// silent failure.
	readout, _ = newPolicyReadout(t, MaxCodes, modelsBody("shisa-ai/shisa-de-2"))
	model, err = NewDecisionModel(DecisionModelConfig{Model: "de2-candidate-lr5e5",
		Tokenizer: "de2-candidate-lr5e5", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	report = model.Health(context.Background(), false)
	if report["ok"] != false || report["calibration_match"] != true {
		t.Errorf("a same-family record reports ok=%v match=%v", report["ok"], report["calibration_match"])
	}
	if report["calibration_note"] == nil {
		t.Error("a same-family record did not explain itself")
	}

	// An endpoint that refuses the listing is reported rather than fatal.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"nope"}}`)
	}))
	t.Cleanup(server.Close)
	readout, err = NewReadout(ReadoutConfig{BaseURL: server.URL, Model: "de2-test",
		HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewReadout: %v", err)
	}
	readout.SetTokenizer(codeTokenizer(t, MaxCodes))
	model, err = NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	report = model.Health(context.Background(), false)
	if status, _ := report["models_status"].(string); !strings.HasPrefix(status, "error:") {
		t.Errorf("a refused listing reports %v", report["models_status"])
	}
}

// TestHealthProbe pins that a probe spends a read and reports it.
func TestHealthProbe(t *testing.T) {
	readout, _ := newPolicyReadout(t, MaxCodes,
		modelsBody("shisa-ai/shisa-de-2"),
		codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	model, err := NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	report := model.Health(context.Background(), true)
	probe, _ := report["read_probe"].(string)
	if !strings.HasPrefix(probe, "passed") {
		t.Errorf("the read probe reports %v", report["read_probe"])
	}
	if !strings.Contains(probe, StrategyRepeat) {
		t.Errorf("the read probe does not name the strategy it used: %v", probe)
	}
}

// TestDecisionModelClose pins that closing is safe on a built model.
func TestDecisionModelClose(t *testing.T) {
	model, _ := newStubModel(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	model.Close()
	// A zero model has no readout; closing must not panic.
	(&DecisionModel{}).Close()
}

// TestCallerVisibleMapsHoldValuesNotPointers pins the Go type in every map a
// caller reads.
//
// The parity gate compares through JSON, and a *float64 marshals to the same
// number as a float64, so it cannot see this. A caller can: a pointer prints as
// an address, and `wire["confidence"].(float64)` panics while the identical
// assertion succeeds for a choice, whose confidence was dereferenced.
func TestCallerVisibleMapsHoldValuesNotPointers(t *testing.T) {
	confidence := 0.75
	answer := Answer{Type: "noul", Noul: floatPointer(0.6), Confidence: &confidence,
		Probabilities: map[string]float64{"yes": 0.6, "no": 0.4}}
	for name, value := range answer.ToWire() {
		assertNotAPointer(t, "ToWire", name, value)
	}
	for name, value := range answer.ToDict() {
		assertNotAPointer(t, "ToDict", name, value)
	}
	score := Answer{Type: "score", Score: floatPointer(1.5), Level: stringPointer("low"),
		Confidence: &confidence, Legend: map[string]string{"0": "low"}}
	for name, value := range score.ToWire() {
		assertNotAPointer(t, "a score's ToWire", name, value)
	}

	// The classify views and the multi-label Raw block.
	model, _ := newStubModel(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	decision, err := model.Classify(context.Background(), map[string]any{"e": "x"},
		map[string]HeadSpec{"route": {Options: []Option{{Key: "billing"}, {Key: "shipping"}}}},
		ClassifyOptions{IncludeProbabilities: true})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	for name, value := range decision.Values["route"].(map[string]any) {
		assertNotAPointer(t, "a classify view", name, value)
	}

	model, _ = newStubModel(t, 2,
		codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}),
		codeReadBody(map[string]float64{"A": -3.0, "B": -0.1}))
	decision, err = model.Classify(context.Background(), map[string]any{"e": "x"},
		map[string]HeadSpec{"topic": {Options: []Option{{Key: "billing"}, {Key: "outage"}},
			MultiLabel: true, Instructions: "Is it {label}?"}}, ClassifyOptions{})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	for name, value := range decision.Raw["topic:billing"] {
		assertNotAPointer(t, "the multi-label raw block", name, value)
	}
}

// assertNotAPointer fails when a map value a caller reads is a pointer to
// something rather than the something.
func assertNotAPointer(t *testing.T, where, name string, value any) {
	t.Helper()
	if value == nil {
		return
	}
	reflected := reflect.ValueOf(value)
	if reflected.Kind() == reflect.Pointer {
		t.Errorf("%s[%q] is a %T; a caller would print an address and fail a type assertion",
			where, name, value)
	}
}

// TestPythonStr pins Python's `str()` for the values a level can hold.
func TestPythonStr(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{nil, "None"},
		{"medium", "medium"},
		{true, "True"},
		{false, "False"},
		{3, "3"},
		{int64(4), "4"},
		{3.0, "3.0"},
		{1.5, "1.5"},
		{float32(2.5), "2.5"},
		// Python's `repr` writes `inf` and `nan`; `json.dumps` writes
		// `Infinity` and `NaN`. These are the `repr` spellings.
		{math.Inf(1), "inf"},
		{math.Inf(-1), "-inf"},
		{math.NaN(), "nan"},
		// A container is spelled with `repr` for its elements.
		{[]any{"low", nil}, "['low', None]"},
		{map[string]any{"a": 1}, "{'a': 1}"},
		{[]any{true, 1.5}, "[True, 1.5]"},
		{[]any{"it's"}, `["it's"]`},
	}
	for _, tc := range cases {
		if got := pythonStr(tc.value); got != tc.want {
			t.Errorf("pythonStr(%#v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

// TestScoreLegendUsesPythonStr pins that a numeric level is spelled the way
// Python spells it, so the legend and the label agree across the two clients.
func TestScoreLegendUsesPythonStr(t *testing.T) {
	model, _ := newStubModel(t, 3, codeReadBody(map[string]float64{"A": -0.1, "B": -2.0, "C": -3.0}))
	decision, err := model.Decide(context.Background(), map[string]any{"e": "x"},
		map[string]Question{"level": NewScore("How bad?", []any{1, 2.5, "high"})}, CallOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	answer := decision.Answers["level"]
	if answer.Legend["0"] != "1" || answer.Legend["1"] != "2.5" || answer.Legend["2"] != "high" {
		t.Errorf("the legend is %v", answer.Legend)
	}
	if answer.Level == nil || *answer.Level != "1" {
		t.Errorf("the level is %v", answer.Level)
	}
}

// TestDecisionModelOverridesPerCall pins that a call's overrides do not leak
// into the model's defaults.
func TestDecisionModelOverridesPerCall(t *testing.T) {
	body := codeReadBody(map[string]float64{"A": -0.1, "B": -1.0})
	model, _ := newStubModel(t, 2, body, body)
	policy := "direct"
	decision, err := model.Decide(context.Background(), map[string]any{"e": "x"},
		map[string]Question{"urgent": NewNoul("Is it urgent?")},
		CallOptions{Overrides: OptionOverrides{Policy: &policy}})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Meta["policy"] != "direct" {
		t.Errorf("the call's policy is %v", decision.Meta["policy"])
	}
	if decision.Answers["urgent"].Strategy != StrategyDirect {
		t.Errorf("the answer's strategy is %q", decision.Answers["urgent"].Strategy)
	}
	if model.Options.Policy != "repeat-think" {
		t.Errorf("the call changed the model's default policy to %q", model.Options.Policy)
	}
	// A later call without overrides uses the model's default again.
	decision, err = model.Decide(context.Background(), map[string]any{"e": "x"},
		map[string]Question{"urgent": NewNoul("Is it urgent?")}, CallOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Meta["policy"] != "repeat-think" {
		t.Errorf("the second call's policy is %v", decision.Meta["policy"])
	}
}

// TestDecideConcurrentHeads pins that several heads are read and that the
// bounded pool does not reorder or drop any.
func TestDecideConcurrentHeads(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2,
		codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}),
		codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}),
		codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	model, err := NewDecisionModel(DecisionModelConfig{
		BaseURL: "http://stub.invalid", Model: "shisa-ai/shisa-de-2",
		Tokenizer: "shisa-ai/shisa-de-2", Readout: readout, MaxWorkers: 3,
	})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	decision, err := model.Decide(context.Background(), map[string]any{"e": "x"},
		map[string]Question{
			"a": NewNoul("Is A?"), "b": NewNoul("Is B?"), "c": NewNoul("Is C?"),
		}, CallOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(decision.Answers) != 3 {
		t.Errorf("the call returned %d answers, want 3", len(decision.Answers))
	}
	for _, head := range []string{"a", "b", "c"} {
		if _, ok := decision.Answers[head]; !ok {
			t.Errorf("head %q is missing", head)
		}
	}
	usage := decision.Usage
	if usage["requests"] != 3 || usage["logical_reads"] != 3 {
		t.Errorf("the usage block is %v", usage)
	}
}

// TestDecideHeadOrderIsDeterministic pins that the work order does not depend on
// Go's map iteration, so the recorded fixtures replay.
func TestDecideHeadOrderIsDeterministic(t *testing.T) {
	for attempt := 0; attempt < 8; attempt++ {
		readout, server := newPolicyReadout(t, 2,
			codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}),
			codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
		model, err := NewDecisionModel(DecisionModelConfig{
			BaseURL: "http://stub.invalid", Model: "shisa-ai/shisa-de-2",
			Tokenizer: "shisa-ai/shisa-de-2", Readout: readout, MaxWorkers: 1,
		})
		if err != nil {
			t.Fatalf("NewDecisionModel: %v", err)
		}
		if _, err := model.Decide(context.Background(), map[string]any{"e": "x"},
			map[string]Question{"alpha": NewNoul("Is A?"), "beta": NewNoul("Is B?")},
			CallOptions{}); err != nil {
			t.Fatalf("Decide: %v", err)
		}
		first, _ := server.requests[0]["prompt"].(string)
		if !strings.Contains(first, "Is A?") {
			t.Fatalf("attempt %d sent %q first, want the alphabetically first head", attempt, truncate(first))
		}
	}
}

// TestDecideUncalibratedWhenNoRecordCoversTheRead pins that a read with no fit
// stays raw even when the record has one for another read.
//
// There are two ways that happens, and they are different rules. A record with
// per-read fits may simply have none for the read in question; a record with no
// per-read fits at all was fitted on a read before any thought, so every read
// after one is outside it. The second is the `covered` rule, and a record that
// carries a top-level temperature is the only kind that can tell whether the
// rule is there: without it, that temperature would be applied to the
// post-thought read and the answer would report itself as calibrated.
func TestDecideUncalibratedWhenNoRecordCoversTheRead(t *testing.T) {
	cases := []struct {
		name        string
		calibration Calibration
	}{
		{"a per-read record with no fit for the read that followed the thought", Calibration{
			Temperatures:   map[string]float64{"choice": 2.0, "noul": 2.0},
			Reads:          map[string]map[string]float64{"direct": {"choice": 2.0, "noul": 2.0}},
			ReadoutVersion: ReadoutVersionFor(FamilyDE2),
			Model:          "shisa-ai/shisa-de-2",
			Family:         FamilyDE2,
		}},
		{"a record with no per-read fits at all", Calibration{
			Temperatures:   map[string]float64{"choice": 2.0, "noul": 2.0},
			ReadoutVersion: ReadoutVersionFor(FamilyDE2),
			Model:          "shisa-ai/shisa-de-2",
			Family:         FamilyDE2,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calibration := tc.calibration
			readout, _ := newPolicyReadout(t, 2,
				codeReadBody(map[string]float64{"A": -0.9, "B": -1.0}),
				thinkBody(500, 501),
				codeReadBody(map[string]float64{"A": -1.2, "B": -0.5}))
			model, err := NewDecisionModel(DecisionModelConfig{
				BaseURL: "http://stub.invalid", Model: "shisa-ai/shisa-de-2",
				Tokenizer: "shisa-ai/shisa-de-2", Calibration: &calibration, Readout: readout,
			})
			if err != nil {
				t.Fatalf("NewDecisionModel: %v", err)
			}
			decision, err := model.Decide(context.Background(), map[string]any{"e": "x"},
				map[string]Question{"urgent": NewNoul("Is it urgent?")}, CallOptions{})
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			answer := decision.Answers["urgent"]
			if answer.Strategy != StrategyRepeatThink {
				t.Fatalf("the read did not think: %q", answer.Strategy)
			}
			if answer.Calibrated || answer.Temperature != 1.0 {
				t.Errorf("an answer read after a thought was tempered by a record that "+
					"does not describe that read: calibrated=%v temperature=%v",
					answer.Calibrated, answer.Temperature)
			}
		})
	}
}

// TestDecisionModelMetaCarriesProvenance pins the block a caller reads to tell
// which read produced a number.
func TestDecisionModelMetaCarriesProvenance(t *testing.T) {
	model, _ := newStubModel(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	decision, err := model.Decide(context.Background(), map[string]any{"e": "x"},
		map[string]Question{"urgent": NewNoul("Is it urgent?")}, CallOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	for _, key := range []string{"model", "base_url", "family", "readout_version", "policy",
		"probability", "input_type", "calibration", "calibrated", "calibration_by_head",
		"strategy_by_head", "think_gate", "think_budget", "think_option_cap"} {
		if _, ok := decision.Meta[key]; !ok {
			t.Errorf("the meta block is missing %q: %v", key, decision.Meta)
		}
	}
	if decision.Meta["family"] != string(FamilyDE2) {
		t.Errorf("the meta family is %v", decision.Meta["family"])
	}
	if decision.Meta["think_gate"] != ThinkGate || decision.Meta["think_budget"] != ThinkBudget {
		t.Errorf("the meta gate and budget are %v and %v",
			decision.Meta["think_gate"], decision.Meta["think_budget"])
	}
	if decision.Meta["think_option_cap"] != ThinkOptionCap {
		t.Errorf("the meta option cap is %v", decision.Meta["think_option_cap"])
	}
	byHead, _ := decision.Meta["calibration_by_head"].(map[string]bool)
	if byHead["urgent"] != true {
		t.Errorf("the per-head calibration block is %v", decision.Meta["calibration_by_head"])
	}
}

// TestDecideProbabilitySkipsTheThought pins that probability=True stops at the
// repeated read without changing the return shape.
func TestDecideProbabilitySkipsTheThought(t *testing.T) {
	readout, server := newPolicyReadout(t, 2,
		codeReadBody(map[string]float64{"A": -0.9, "B": -1.0}))
	model, err := NewDecisionModel(DecisionModelConfig{
		BaseURL: "http://stub.invalid", Model: "shisa-ai/shisa-de-2",
		Tokenizer: "shisa-ai/shisa-de-2", Readout: readout,
	})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	decision, err := model.Decide(context.Background(), map[string]any{"e": "x"},
		map[string]Question{"urgent": NewNoul("Is it urgent?")}, CallOptions{Probability: true})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	answer := decision.Answers["urgent"]
	if answer.Strategy != StrategyRepeat {
		t.Errorf("strategy = %q, want %q", answer.Strategy, StrategyRepeat)
	}
	if answer.Requests != 1 || server.at != 1 {
		t.Errorf("a probability call spent %d requests", server.at)
	}
	if answer.Calibrated != true {
		t.Error("probability=True changed the calibration")
	}
	if decision.Meta["probability"] != true {
		t.Errorf("the meta does not record the probability flag: %v", decision.Meta["probability"])
	}
	// The answer is still a full noul, not a bare number.
	if answer.Noul == nil || answer.Confidence == nil || len(answer.Probabilities) != 2 {
		t.Errorf("the answer shape changed: %v", answer)
	}
}

// TestDecisionModelZeroValueReadoutIsReported pins that a client with no readout
// reports rather than panicking.
func TestDecisionModelZeroValueReadoutIsReported(t *testing.T) {
	model := &DecisionModel{}
	report := model.Health(context.Background(), false)
	if status, _ := report["models_status"].(string); !strings.HasPrefix(status, "error:") {
		t.Errorf("a zero client's listing reports %v", report["models_status"])
	}
	if report["ok"] != false {
		t.Errorf("a zero client reports ok=%v", report["ok"])
	}
}

func floatPointer(value float64) *float64 { return &value }

// noulFor is the multi-label template substitution, with the error surfaced.
func noulFor(t *testing.T, question Question, label string) Noul {
	t.Helper()
	multi, ok := question.(*multiLabelHead)
	if !ok {
		t.Fatalf("the question is a %T, want a multi-label head", question)
	}
	noul, err := multi.noulFor(label)
	if err != nil {
		t.Fatalf("noulFor(%q): %v", label, err)
	}
	return noul
}

// hasWarning reports whether the client recorded a warning containing text.
func hasWarning(model *DecisionModel, text string) bool {
	for _, warning := range model.Warnings {
		if strings.Contains(warning, text) {
			return true
		}
	}
	return false
}
