package shisade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	entries, ok := decision.Values["topic"].([]Object)
	if !ok || len(entries) != 1 {
		t.Fatalf("the confidence view is %v", decision.Values["topic"])
	}
	if label, _ := entries[0].Get("label"); label != "billing" {
		t.Errorf("the confidence view is %v", decision.Values["topic"])
	}
	// The key order is Python's, which is what the command line's stdout is
	// compared against.
	if got := objectKeys(entries[0]); !equalStrings(got, []string{"label", "confidence"}) {
		t.Errorf("the confidence view's keys are %v, want label then confidence", got)
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
	entries, ok = decision.Values["topic"].([]Object)
	if !ok || len(entries) != 1 {
		t.Fatalf("the probabilities view is %v", decision.Values["topic"])
	}
	if _, present := entries[0].Get("probabilities"); !present {
		t.Errorf("the probabilities view is %v", decision.Values["topic"])
	}
	// A noul's distribution is `yes` then `no`, whatever the option order was.
	distribution, _ := entries[0].Get("probabilities")
	ordered, ok := distribution.(Object)
	if !ok || !equalStrings(objectKeys(ordered), []string{"yes", "no"}) {
		t.Errorf("the multi-label distribution is %v, want yes then no", distribution)
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
	view, ok := decision.Values["route"].(Object)
	if !ok {
		t.Fatalf("the confidence view is %v", decision.Values["route"])
	}
	if label, _ := view.Get("label"); label != "billing" {
		t.Errorf("the confidence view is %v", decision.Values["route"])
	}
	if _, present := view.Get("confidence"); !present {
		t.Errorf("the confidence view is %v", decision.Values["route"])
	}
	if _, present := view.Get("probabilities"); present {
		t.Error("the confidence view carries probabilities")
	}
	if got := objectKeys(view); !equalStrings(got, []string{"label", "confidence"}) {
		t.Errorf("the confidence view's keys are %v, want label then confidence", got)
	}

	model, _ = newStubModel(t, 2, body)
	decision, err = model.Classify(context.Background(), map[string]any{"e": "x"}, heads,
		ClassifyOptions{IncludeProbabilities: true})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	view, ok = decision.Values["route"].(Object)
	if !ok {
		t.Fatalf("the probabilities view is %v", decision.Values["route"])
	}
	distribution, present := view.Get("probabilities")
	if !present {
		t.Errorf("the probabilities view is %v", decision.Values["route"])
	}
	// The distribution is keyed in the caller's option order, which is what
	// Python's dict literal gives and a Go map would not.
	ordered, ok := distribution.(Object)
	if !ok {
		t.Fatalf("the distribution is a %T, want an Object", distribution)
	}
	if got := objectKeys(ordered); !equalStrings(got, []string{"billing", "shipping"}) {
		t.Errorf("the distribution's keys are %v, want the option order", got)
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
		// The message has to carry the fix. It named the default while the default
		// was DE-1; DE-1 is retired, so it says what became of the name instead.
		if !strings.Contains(err.Error(), retiredDE1Model) {
			t.Errorf("%s: the error does not say what became of the DE-1 name: %v", model, err)
		}
		if !strings.Contains(err.Error(), DefaultModel) {
			t.Errorf("%s: the error does not name the checkpoint to pass: %v", model, err)
		}
	}
}

// TestDefaultModelIsReadable is the guard the zero config needs: the library's
// default has to be a checkpoint this port can actually read.
//
// It could not be while the default was the Python client's DE-1 id, and the
// test above asserted that failure rather than this fix. A default that cannot
// be constructed is not a default. Construction is lazy, so this needs no
// server and no tokenizer download.
func TestDefaultModelIsReadable(t *testing.T) {
	model, err := NewDecisionModel(DecisionModelConfig{})
	if err != nil {
		t.Fatalf("the zero config is not a working client: %v", err)
	}
	defer model.Close()
	if model.Model != DefaultModel {
		t.Errorf("the zero config reads %s, want %s", model.Model, DefaultModel)
	}
	if model.Family != FamilyDE2 {
		t.Errorf("the default model resolves to family %q, want %q", model.Family, FamilyDE2)
	}
	if model.ReadoutVersion != ReadoutVersionFor(FamilyDE2) {
		t.Errorf("the default readout is %s, want %s",
			model.ReadoutVersion, ReadoutVersionFor(FamilyDE2))
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
	// The strip is Python's `str.strip()`, which takes U+001C-U+001F and
	// `strings.TrimSpace` does not. This value is sent as the bearer token, so a
	// key the Python client trims and this port sent verbatim would be a
	// different `Authorization` header.
	t.Setenv(APIKeyEnvs[0], "\u001cfrom-the-first\u001f")
	if key := APIKeyFromEnv(); key == nil || *key != "from-the-first" {
		t.Errorf("a key behind U+001C is %v, want it stripped", key)
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
	// The slot summary is an `Object`, so its key order survives into
	// `doctor --json`: Python's report has the count first and the codes in
	// codebook order.
	slots, ok := report["slots"].(Object)
	if !ok || len(slots) != 3 || slots[0].Key != "count" || slots[0].Value != MaxCodes {
		t.Fatalf("the slot report is %v", report["slots"])
	}
	if slots[1].Key != "A" || slots[2].Key != "B" {
		t.Errorf("the slot report names %v and %v, want A then B", slots[1].Key, slots[2].Key)
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
		codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}),
		// The chat probe: `doctor --probe` also asks the chat endpoint how it
		// renders the system turn, because that is what an image question is
		// rendered with.
		`{"choices":[],"usage":{"prompt_tokens":0}}`)
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
	// A response with no prompt tokens cannot say how the server rendered the
	// system turn, and saying so is the whole report.
	if report["chat_system_render"] != ChatSystemUnverified {
		t.Errorf("the chat probe reports %v, want %q",
			report["chat_system_render"], ChatSystemUnverified)
	}
}

// TestHealthWithoutAProbeSkipsTheChatRequest pins that the chat probe is the
// probe's cost, not the report's: without `--probe` nothing is spent.
func TestHealthWithoutAProbeSkipsTheChatRequest(t *testing.T) {
	readout, server := newPolicyReadout(t, MaxCodes, modelsBody("shisa-ai/shisa-de-2"))
	model, err := NewDecisionModel(DecisionModelConfig{Model: "shisa-ai/shisa-de-2", Readout: readout})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	report := model.Health(context.Background(), false)
	if _, present := report["chat_system_render"]; present {
		t.Errorf("a report with no probe carries chat_system_render: %v",
			report["chat_system_render"])
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) != 1 {
		t.Errorf("the report sent %d requests, want the model listing only", len(server.requests))
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
// The parity gate compares through JSON, and a *float64 or *string marshals to
// the same value as the float64 or string behind it, so it cannot see this. A
// caller can: a pointer prints as an address, and
// `wire["confidence"].(float64)` panics while the identical assertion succeeds
// for a choice, whose confidence was dereferenced.
func TestCallerVisibleMapsHoldValuesNotPointers(t *testing.T) {
	confidence := 0.75
	answer := Answer{Type: "noul", Noul: floatPointer(0.6), Confidence: &confidence,
		Probabilities: map[string]float64{"yes": 0.6, "no": 0.4}}
	assertNoPointers(t, "a noul's ToWire", answer.ToWire())
	assertNoPointers(t, "a noul's ToDict", answer.ToDict())
	score := Answer{Type: "score", Score: floatPointer(1.5), Level: stringPointer("low"),
		Confidence: &confidence, Legend: map[string]string{"0": "low"}}
	assertNoPointers(t, "a score's ToWire", score.ToWire())
	assertNoPointers(t, "a score's ToDict", score.ToDict())

	// The classify views and the multi-label Raw block.
	model, _ := newStubModel(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	decision, err := model.Classify(context.Background(), map[string]any{"e": "x"},
		map[string]HeadSpec{"route": {Options: []Option{{Key: "billing"}, {Key: "shipping"}}}},
		ClassifyOptions{IncludeProbabilities: true})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	assertNoPointers(t, "a classify view", decision.Values["route"])

	model, _ = newStubModel(t, 2,
		codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}),
		codeReadBody(map[string]float64{"A": -3.0, "B": -0.1}))
	decision, err = model.Classify(context.Background(), map[string]any{"e": "x"},
		map[string]HeadSpec{"topic": {Options: []Option{{Key: "billing"}, {Key: "outage"}},
			MultiLabel: true, Instructions: "Is it {label}?"}}, ClassifyOptions{})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	assertNoPointers(t, "the multi-label raw block", decision.Raw["topic:billing"])

	// Decision.Raw for an ordinary head, without and with debug. The debug read
	// is where `prompt`, `top_logprobs`, `sampled` and `thought` all appear, and
	// the policy's raw block nests `components` under the head.
	model, _ = newStubModel(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	decision, err = model.Decide(context.Background(), map[string]any{"e": "x"},
		map[string]Question{"urgent": NewNoul("Is it urgent?")}, CallOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	assertNoPointers(t, "a raw head", decision.Raw["urgent"])

	readout, _ := newPolicyReadout(t, 2,
		codeReadBody(map[string]float64{"A": -0.9, "B": -1.0}),
		thinkBody(500, 501),
		codeReadBody(map[string]float64{"A": -1.2, "B": -0.5}))
	model, err = NewDecisionModel(DecisionModelConfig{
		BaseURL: "http://stub.invalid", Model: "shisa-ai/shisa-de-2",
		Tokenizer: "shisa-ai/shisa-de-2", Readout: readout,
	})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	decision, err = model.Decide(context.Background(), map[string]any{"e": "x"},
		map[string]Question{"urgent": NewNoul("Is it urgent?")}, CallOptions{Debug: true})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	assertNoPointers(t, "a debug raw head", decision.Raw["urgent"])

	// The image read's raw block, which is built by its own function rather than
	// by the policy's, and whose debug form is the one place `prompt` is nil and
	// `top_logprobs` is a map of numbers. A debug image decision is the only
	// thing that reaches both.
	model, _ = newStubModel(t, MaxCodes, chatBody(
		`{"token":"A","logprob":-0.1},{"token":"B","logprob":-1.0}`))
	decision, err = model.Classify(context.Background(), map[string]any{},
		map[string]HeadSpec{"intent": {Options: []Option{{Key: "spam"}, {Key: "ham"}}}},
		ClassifyOptions{CallOptions: CallOptions{Image: imageTestURL, Debug: true}})
	if err != nil {
		t.Fatalf("Classify with an image: %v", err)
	}
	assertNoPointers(t, "a debug image raw head", decision.Raw["intent"])
	if prompt, present := decision.Raw["intent"]["prompt"]; !present || prompt != nil {
		t.Errorf("an image read's debug prompt is %#v, want nil: the server rendered the "+
			"prompt, so there is no local one", prompt)
	}
	// The debug keys a caller reads, each one named: an image read has no local
	// prompt, its top-k is the server's, and `messages` and `system_render` are
	// what the server did with the turn the client could not render itself.
	for _, key := range []string{"prompt", "top_logprobs", "messages", "system_render"} {
		if _, present := decision.Raw["intent"][key]; !present {
			t.Errorf("a debug image raw head has no %q: %v", key, decision.Raw["intent"])
		}
	}
}

// assertNoPointers fails on any pointer inside a caller-visible value, at any
// depth.
//
// It walks maps and slices through reflection rather than switching on the types
// this package happens to use, because a `map[string]*float64` is a map of
// pointers and a walker that only looked at the map would miss it.
func assertNoPointers(t *testing.T, where string, value any) {
	t.Helper()
	if value == nil {
		return
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Map:
		for _, key := range reflected.MapKeys() {
			item := reflected.MapIndex(key)
			assertNoPointers(t, fmt.Sprintf("%s[%v]", where, key.Interface()), item.Interface())
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < reflected.Len(); index++ {
			assertNoPointers(t, fmt.Sprintf("%s[%d]", where, index),
				reflected.Index(index).Interface())
		}
	case reflect.Pointer:
		t.Errorf("%s is a %T; a caller would print an address and fail a type assertion",
			where, value)
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

// TestPythonQuoteMatchesPythonRepr replays Python's `repr()` for every sampled
// code point.
//
// `pythonQuote` is only reachable through a label: a level's text and a legend's
// values are the option keys, spelled with `repr` when they sit inside a
// container. The escaping rule is the interpreter's, so the expectations are
// generated from an interpreter rather than written down, and the sampling
// covers every printable range boundary plus all of the BMP's first three pages.
func TestPythonQuoteMatchesPythonRepr(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "python_repr.json"))
	if err != nil {
		t.Fatalf("reading the expectations: %v", err)
	}
	var fixture struct {
		Python        string            `json:"python"`
		PythonUnicode string            `json:"python_unicode"`
		Generated     string            `json:"generated"`
		CodePoints    int               `json:"code_points"`
		Reprs         map[string]string `json:"reprs"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("the expectations are not JSON: %v", err)
	}
	if fixture.PythonUnicode == "" || fixture.Generated == "" {
		t.Fatal("the expectations do not say which interpreter produced them")
	}
	if len(fixture.Reprs) != fixture.CodePoints {
		t.Errorf("the expectations hold %d reprs and claim %d code points",
			len(fixture.Reprs), fixture.CodePoints)
	}

	// The table and the expectations have to come from the same interpreter, or
	// a disagreement below is a Unicode version rather than a bug.
	table, err := pythonPrintableTable()
	if err != nil {
		t.Fatalf("the printable table did not load: %v", err)
	}
	if table.PythonUnicode != fixture.PythonUnicode {
		t.Errorf("the table is from Unicode %s and the expectations from %s",
			table.PythonUnicode, fixture.PythonUnicode)
	}

	bad := 0
	for character, want := range fixture.Reprs {
		runes := []rune(character)
		if len(runes) != 1 {
			t.Fatalf("an expectation key is %d runes, want 1", len(runes))
		}
		if got := pythonQuote(character); got != want {
			if bad < 20 {
				t.Errorf("pythonQuote(%#U) = %s, Python says %s", runes[0], got, want)
			}
			bad++
		}
	}
	if bad != 0 {
		t.Errorf("%d of %d sampled code points disagree", bad, len(fixture.Reprs))
	}
}

// TestPythonPrintableTableCoversPython pins the table's shape against the
// predicate it stands for.
//
// The table is what makes `pythonQuote` the interpreter's spelling rather than
// Go's near miss, and the one code point class the two languages cannot share is
// the surrogate: Go decodes a lone one out of a string as U+FFFD, so the table
// omits the range and the lookup must still answer for it.
func TestPythonPrintableTableCoversPython(t *testing.T) {
	table, err := pythonPrintableTable()
	if err != nil {
		t.Fatalf("the printable table did not load: %v", err)
	}
	if table.Python == "" || table.PythonUnicode == "" || table.Source == "" {
		t.Errorf("the table does not carry its provenance: %+v", table)
	}
	// The table is searched as sorted and disjoint; `pythonPrintableTable`
	// refuses anything else, so a load that succeeded is one that can be
	// searched.
	covered := 0
	for _, pair := range table.Printable {
		covered += pair[1] - pair[0] + 1
	}
	if covered < 0x10000 {
		t.Errorf("the table covers %d code points, which is not most of Unicode", covered)
	}
	if !isPythonPrintable(' ') {
		t.Error("the ASCII space is not printable; Python's isprintable() says it is")
	}
	for _, rune := range []rune{'a', 'é', '\u00a1', '\u4e2d', '\U0001f600'} {
		if !isPythonPrintable(rune) {
			t.Errorf("%#U is not printable", rune)
		}
	}
	for _, rune := range []rune{0, '\t', '\n', 0x7f, 0x9f, 0x200b, 0x2028, 0x2029, 0x00a0, 0xe000, 0xfeff} {
		if isPythonPrintable(rune) {
			t.Errorf("%#U is printable", rune)
		}
	}
	// U+FFFD is what Go decodes a lone surrogate to, and Python calls it
	// printable, so the omission of the surrogate range costs nothing here.
	if !isPythonPrintable('\ufffd') {
		t.Error("U+FFFD is not printable; Python's isprintable() says it is")
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

// -- the image read through the friendly layer --------------------------------

// TestClassifyImageIsOneDirectRead pins what the image path changes above the
// readout: one read, raw probabilities, and `input_type: image`.
func TestClassifyImageIsOneDirectRead(t *testing.T) {
	model, server := newStubModel(t, MaxCodes,
		chatBody(`{"token":"A","logprob":-0.1},{"token":"B","logprob":-1.0}`))

	decision, err := model.Classify(context.Background(), map[string]any{},
		map[string]HeadSpec{"intent": {Options: []Option{{Key: "spam"}, {Key: "ham"}}}},
		ClassifyOptions{IncludeProbabilities: true,
			CallOptions: CallOptions{Image: imageTestURL}})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	answer := decision.Answers["intent"]
	if answer.Choice == nil || *answer.Choice != "spam" {
		t.Errorf("the answer is %v, want spam", answer.Choice)
	}
	if answer.Requests != 1 || answer.LogicalReads != 1 {
		t.Errorf("the read spent %d requests and %d logical reads, want 1 and 1",
			answer.Requests, answer.LogicalReads)
	}
	// The image path has no fit in the shipped record, so a temperature of 1.0
	// leaves `Calibrated` false whatever was asked for. The caller's `Calibrated`
	// is still consulted -- TestClassifyImageHonoursAnExplicitCalibration pins
	// that -- it just has nothing to apply here.
	if answer.Calibrated || answer.Temperature != 1.0 {
		t.Errorf("an image answer is calibrated at temperature %v", answer.Temperature)
	}
	if got := decision.Meta["input_type"]; got != "image" {
		t.Errorf("input_type is %v, want image", got)
	}
	if got := decision.Usage["requests"]; got != 1 {
		t.Errorf("the call reports %v requests, want 1", got)
	}

	// The raw record is the flat read shape rather than the policy's staged one,
	// and it carries what the server did with the system turn.
	raw := decision.Raw["intent"]
	for _, key := range []string{"logprobs", "probabilities", "requests", "missing_from_top",
		"ranks", "prompt_tokens", "sampled", "options", "system_render"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("the raw record has no %q: %v", key, raw)
		}
	}
	if _, staged := raw["stages"]; staged {
		t.Errorf("the image raw record carries the policy's staged shape: %v", raw)
	}
	if got := len(server.requests); got != 1 {
		t.Errorf("the call sent %d requests, want 1", got)
	}
}

// TestClassifyImageKeepsTheLetterLimit pins that a choice above 26 options is
// refused rather than sent, and refused before any request.
//
// Overflow is the DE-1 mechanism for exactly this case, so it is also asked for
// here: the image path must refuse rather than balance chunks, whatever the
// caller requested. Python's `_run` refuses at the same point, before the
// overflow heads are even collected.
func TestClassifyImageKeepsTheLetterLimit(t *testing.T) {
	model, server := newStubModel(t, MaxCodes)
	options := make([]Option, MaxOptions+1)
	for index := range options {
		options[index] = Option{Key: fmt.Sprintf("option-%d", index)}
	}
	finalistTop1 := OverflowStrategies[0]
	_, err := model.Classify(context.Background(), map[string]any{},
		map[string]HeadSpec{"intent": {Options: options}},
		ClassifyOptions{CallOptions: CallOptions{Image: imageTestURL,
			Overrides: OptionOverrides{Overflow: &finalistTop1}}})
	if err == nil {
		t.Fatal("an image choice above the letter limit was accepted")
	}
	var questionError *QuestionError
	if !errors.As(err, &questionError) {
		t.Fatalf("got a %T, want a *QuestionError", err)
	}
	if !strings.Contains(err.Error(), "image choices above 26 options") {
		t.Errorf("the error is %v", err)
	}
	if got := len(server.requests); got != 0 {
		t.Errorf("a refused question sent %d requests", got)
	}
}

// TestClassifyImageFailsBeforeAnyRequest pins that an image the client cannot
// read is refused by the client, not by the server.
func TestClassifyImageFailsBeforeAnyRequest(t *testing.T) {
	model, server := newStubModel(t, MaxCodes)
	_, err := model.Classify(context.Background(), map[string]any{},
		map[string]HeadSpec{"intent": {Options: []Option{{Key: "spam"}, {Key: "ham"}}}},
		ClassifyOptions{CallOptions: CallOptions{Image: "photo.gif"}})
	if err == nil {
		t.Fatal("an unsupported image extension was accepted")
	}
	var imageError *ImageError
	if !errors.As(err, &imageError) {
		t.Fatalf("got a %T, want an *ImageError", err)
	}
	if got := len(server.requests); got != 0 {
		t.Errorf("a refused image sent %d requests", got)
	}
}

// TestDecideWithAnImage pins that the image reaches the typed-question call too,
// and that its `noul` shape comes back the same way.
func TestDecideWithAnImage(t *testing.T) {
	model, _ := newStubModel(t, MaxCodes,
		chatBody(`{"token":"A","logprob":-0.1},{"token":"B","logprob":-1.0}`))
	decision, err := model.Decide(context.Background(), map[string]any{},
		map[string]Question{"square": NewNoul("Is this a square?")},
		CallOptions{Image: imageTestURL})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	answer := decision.Answers["square"]
	if answer.Type != "noul" || answer.Noul == nil {
		t.Fatalf("the answer is %+v, want a noul", answer)
	}
	if *answer.Noul <= 0.5 {
		t.Errorf("the answer is %v, want the peaked option", *answer.Noul)
	}
	if answer.Calibrated {
		t.Errorf("an image answer is calibrated at temperature %v", answer.Temperature)
	}
}

// TestClassifyImageHonoursAnExplicitCalibration pins both halves of the image
// path's calibration rule.
//
// The default is off because the shipped record has no fit for that read. An
// explicit `Calibrated` is still consulted, which is what the Python client does
// (`calibrated = image_url is None` only when the caller passed None), so a
// record that does carry an image fit is applied when it is asked for and left
// alone when it is not. The fit is supplied here rather than taken from the
// shipped record, because no shipped record has one.
func TestClassifyImageHonoursAnExplicitCalibration(t *testing.T) {
	calibration := Calibration{
		Temperatures:   map[string]float64{"choice": 2.0, "noul": 2.0},
		Reads:          map[string]map[string]float64{"image": {"choice": 2.0, "noul": 2.0}},
		ReadoutVersion: ReadoutVersionFor(FamilyDE2),
		Model:          "shisa-ai/shisa-de-2",
		Family:         FamilyDE2,
	}
	newModel := func(t *testing.T) *DecisionModel {
		t.Helper()
		readout, _ := newPolicyReadout(t, MaxCodes,
			chatBody(`{"token":"A","logprob":-0.1},{"token":"B","logprob":-1.0}`))
		model, err := NewDecisionModel(DecisionModelConfig{
			BaseURL: "http://stub.invalid", Model: "shisa-ai/shisa-de-2",
			Tokenizer: "shisa-ai/shisa-de-2", Calibration: &calibration, Readout: readout,
		})
		if err != nil {
			t.Fatalf("NewDecisionModel: %v", err)
		}
		return model
	}
	ask := func(t *testing.T, model *DecisionModel, options CallOptions) Answer {
		t.Helper()
		options.Image = imageTestURL
		decision, err := model.Classify(context.Background(), map[string]any{},
			map[string]HeadSpec{"intent": {Options: []Option{{Key: "spam"}, {Key: "ham"}}}},
			ClassifyOptions{CallOptions: options})
		if err != nil {
			t.Fatalf("Classify: %v", err)
		}
		return decision.Answers["intent"]
	}

	// The default: the fit exists and is not applied.
	plain := ask(t, newModel(t), CallOptions{})
	if plain.Calibrated || plain.Temperature != 1.0 {
		t.Errorf("an image answer was calibrated without being asked: calibrated=%v "+
			"temperature=%v", plain.Calibrated, plain.Temperature)
	}

	// The override: the same call, asked for.
	asked := ask(t, newModel(t), CallOptions{Calibrated: boolPointer(true)})
	if !asked.Calibrated || asked.Temperature != 2.0 {
		t.Errorf("an explicit Calibrated was ignored with an image: calibrated=%v "+
			"temperature=%v", asked.Calibrated, asked.Temperature)
	}
	if asked.Choice == nil || plain.Choice == nil || *asked.Choice != *plain.Choice {
		t.Errorf("the fit changed the choice from %v to %v, so this test is not "+
			"distinguishing the two cases", plain.Choice, asked.Choice)
	}
	// The distribution moved, which is what makes the assertion above an
	// observation rather than a report of the flag.
	if asked.Probabilities["spam"] == plain.Probabilities["spam"] {
		t.Errorf("the tempered distribution is identical to the raw one: %v",
			asked.Probabilities)
	}
}

// TestImageTopLogprobsReachesTheReadout pins the one thing the config field does.
func TestImageTopLogprobsReachesTheReadout(t *testing.T) {
	readout, server := newPolicyReadout(t, MaxCodes,
		chatBody(`{"token":"A","logprob":-0.1},{"token":"B","logprob":-1.0}`))
	model, err := NewDecisionModel(DecisionModelConfig{
		Model: "shisa-ai/shisa-de-2", Readout: readout, ImageTopLogprobs: 64,
	})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	if _, err := model.Classify(context.Background(), map[string]any{},
		map[string]HeadSpec{"intent": {Options: []Option{{Key: "spam"}, {Key: "ham"}}}},
		ClassifyOptions{CallOptions: CallOptions{Image: imageTestURL}}); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) != 1 {
		t.Fatalf("the call sent %d requests, want 1", len(server.requests))
	}
	if server.requests[0]["top_logprobs"] != float64(64) {
		t.Errorf("the request asked for %v, want 64", server.requests[0]["top_logprobs"])
	}
	// A non-positive limit is refused at construction rather than at the read.
	if _, err := NewDecisionModel(DecisionModelConfig{
		Model: "shisa-ai/shisa-de-2", Readout: readout, ImageTopLogprobs: -1,
	}); err == nil {
		t.Error("a negative image logprob limit was accepted")
	}
}

// objectKeys is an Object's keys in order, for a test that pins the order rather
// than only the contents.
func objectKeys(object Object) []string {
	keys := make([]string, 0, len(object))
	for _, member := range object {
		keys = append(keys, member.Key)
	}
	return keys
}

// TestOrderedProbabilities pins the reader the writers use, including the two
// cases no client path produces: an Answer a caller built itself, and one whose
// keys are set incompletely.
func TestOrderedProbabilities(t *testing.T) {
	// No order to reproduce: sorted, which is stable rather than random.
	built := Answer{Probabilities: map[string]float64{"no": 0.4, "yes": 0.6}}
	if got := objectKeys(built.OrderedProbabilities()); !equalStrings(got, []string{"no", "yes"}) {
		t.Errorf("a hand-built answer's keys are %v, want them sorted", got)
	}
	// An order that names only some of the keys: the order is lost for the rest
	// and no value is.
	partial := Answer{
		Probabilities:   map[string]float64{"b": 0.5, "a": 0.3, "c": 0.2},
		ProbabilityKeys: []string{"b"},
	}
	got := objectKeys(partial.OrderedProbabilities())
	if !equalStrings(got, []string{"b", "a", "c"}) {
		t.Errorf("a partial order gives %v, want b then the rest sorted", got)
	}
	if len(partial.OrderedProbabilities()) != 3 {
		t.Errorf("a partial order dropped a value: %v", partial.OrderedProbabilities())
	}
	// A key the distribution does not have is skipped rather than written as a
	// zero.
	extra := Answer{
		Probabilities:   map[string]float64{"a": 0.5},
		ProbabilityKeys: []string{"a", "gone"},
	}
	if members := extra.OrderedProbabilities(); len(members) != 1 {
		t.Errorf("a stale key produced %v", members)
	}
}

// TestTheClientAppliesTheRecordOfTheCheckpointItServes pins that the record is
// chosen from the served checkpoint rather than from the family, and that a
// checkpoint with no record is reported and left raw rather than tempered with
// another checkpoint's fit.
func TestTheClientAppliesTheRecordOfTheCheckpointItServes(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		tokenizer string
		want      string
		level     ApplicabilityLevel
		applied   bool
	}{
		{"DE-2", "shisa-ai/shisa-de-2", "", "shisa-ai/shisa-de-2", ApplicabilityCheckpoint, true},
		{"DE-2.1", "shisa-ai/shisa-de-2.1", "", "shisa-ai/shisa-de-2.1", ApplicabilityCheckpoint, true},
		{"a quantized DE-2.1 repo", "shisa-ai/shisa-de-2.1-FP8-dynamic", "",
			"shisa-ai/shisa-de-2.1", ApplicabilityCheckpoint, true},
		{"a local DE-2.1 directory", "/models/shisa-de-2.1", "",
			"shisa-ai/shisa-de-2.1", ApplicabilityCheckpoint, true},
		{"an alias with a DE-2.1 tokenizer", "alias", "shisa-ai/shisa-de-2.1",
			"shisa-ai/shisa-de-2.1", ApplicabilityCheckpoint, true},
		{"a training arm", "de2-some-arm", "",
			"shisa-ai/shisa-de-2", ApplicabilityFamily, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readout, _ := newPolicyReadout(t, 1)
			model, err := NewDecisionModel(DecisionModelConfig{
				BaseURL: "http://stub.invalid", Model: tc.model, Tokenizer: tc.tokenizer,
				Readout: readout, MaxWorkers: 1,
			})
			if err != nil {
				t.Fatalf("NewDecisionModel: %v", err)
			}
			defer model.Close()
			if model.Calibration.Model != tc.want {
				t.Errorf("serving %s selected the record for %s, want %s",
					tc.model, model.Calibration.Model, tc.want)
			}
			if model.CalibrationLevel != tc.level {
				t.Errorf("serving %s is %q, want %q", tc.model, model.CalibrationLevel, tc.level)
			}
			if model.CalibrationApplied != tc.applied {
				t.Errorf("serving %s applied=%v, want %v", tc.model, model.CalibrationApplied, tc.applied)
			}
			// A record that is not applied is reported, never silent.
			warned := len(model.Warnings) != 0
			if warned == tc.applied {
				t.Errorf("serving %s warned=%v with applied=%v", tc.model, warned, tc.applied)
			}
		})
	}
}

// TestDE21IsTemperedWithItsOwnFit pins the number a caller actually gets: the
// same raw probability read from DE-2 and from DE-2.1 comes back different,
// because the records differ and DE-2's fit over-corrects DE-2.1.
func TestDE21IsTemperedWithItsOwnFit(t *testing.T) {
	de21, err := CalibrationForCheckpoint(FamilyDE2, "shisa-ai/shisa-de-2.1", "")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint(2.1): %v", err)
	}
	de2, err := CalibrationForCheckpoint(FamilyDE2, "shisa-ai/shisa-de-2", "")
	if err != nil {
		t.Fatalf("CalibrationForCheckpoint(de2): %v", err)
	}
	raw := map[string]float64{"A": 0.8, "B": 0.2}
	withDE21 := TemperDistribution(raw, de21.TemperatureFor("choice", "direct"))
	withDE2 := TemperDistribution(raw, de2.TemperatureFor("choice", "direct"))
	if math.Abs(withDE21["A"]-withDE2["A"]) < 1e-6 {
		t.Fatalf("both records temper 0.8 to %v", withDE21["A"])
	}
	// DE-2.1 is less overconfident, so its temperature is closer to 1 and its
	// tempered probability is closer to the raw one.
	if withDE21["A"] <= withDE2["A"] || withDE21["A"] > raw["A"] {
		t.Errorf("DE-2.1 tempers 0.8 to %v and DE-2 to %v; want the DE-2.1 value between 0.8 and the DE-2 value",
			withDE21["A"], withDE2["A"])
	}
}

// TestADisagreeingTokenizerIsReported pins the rule for two names that name
// different checkpoints: the model id decides, because a tokenizer is not a
// statement about weights -- DE-2.1 is served with DE-2's tokenizer, so the
// reverse disagreement is a correct configuration -- and the disagreement is
// said out loud when the choice was this client's to make.
//
// A supplied record is the caller's assertion, so the model id did not choose
// it and there is nothing to report: that is the documented alias remedy.
func TestADisagreeingTokenizerIsReported(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		tokenizer string
		supplied  string // the checkpoint whose record is passed, if any
		want      string
		warns     bool
	}{
		{name: "the alias case", model: "shisa-ai/shisa-de-2", tokenizer: "shisa-ai/shisa-de-2.1",
			want: "shisa-ai/shisa-de-2", warns: true},
		{name: "2.1 served with DE-2's tokenizer", model: "shisa-ai/shisa-de-2.1", tokenizer: "shisa-ai/shisa-de-2",
			want: "shisa-ai/shisa-de-2.1", warns: true},
		{name: "the same checkpoint twice", model: "shisa-ai/shisa-de-2.1", tokenizer: "shisa-ai/shisa-de-2.1-FP8-dynamic",
			want: "shisa-ai/shisa-de-2.1", warns: false},
		{name: "the alias remedy", model: "shisa-ai/shisa-de-2", tokenizer: "shisa-ai/shisa-de-2.1",
			supplied: "shisa-ai/shisa-de-2.1", want: "shisa-ai/shisa-de-2.1", warns: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readout, _ := newPolicyReadout(t, 1)
			config := DecisionModelConfig{
				BaseURL: "http://stub.invalid", Model: tc.model, Tokenizer: tc.tokenizer,
				Readout: readout, MaxWorkers: 1,
			}
			if tc.supplied != "" {
				record, err := CalibrationForCheckpoint(FamilyDE2, tc.supplied, "")
				if err != nil {
					t.Fatalf("CalibrationForCheckpoint: %v", err)
				}
				config.Calibration = &record
			}
			model, err := NewDecisionModel(config)
			if err != nil {
				t.Fatalf("NewDecisionModel: %v", err)
			}
			defer model.Close()
			if model.Calibration.Model != tc.want {
				t.Errorf("serving %s with tokenizer %s chose %s, want %s",
					tc.model, tc.tokenizer, model.Calibration.Model, tc.want)
			}
			// The model id decided, or the caller did, so the record is applied.
			if !model.CalibrationApplied || model.CalibrationLevel != ApplicabilityCheckpoint {
				t.Errorf("%s is %q applied=%v", tc.model, model.CalibrationLevel, model.CalibrationApplied)
			}
			reported := false
			for _, warning := range model.Warnings {
				if strings.Contains(warning, "the model id names") {
					reported = true
				}
			}
			if reported != tc.warns {
				t.Errorf("the disagreement was reported=%v, want %v: %v", reported, tc.warns, model.Warnings)
			}
		})
	}
}

// TestACustomFitIsNotSecondGuessed pins that the family-record warning is about
// the family's record only. A fit of the caller's own on an uncovered checkpoint
// is their judgement, and telling them to discard it would be wrong.
func TestACustomFitIsNotSecondGuessed(t *testing.T) {
	custom, err := CalibrationFor(FamilyDE2)
	if err != nil {
		t.Fatalf("CalibrationFor: %v", err)
	}
	custom.Model = "my-own-fit"
	custom.FittedOn = "a fit of my own"
	readout, _ := newPolicyReadout(t, 1)
	// The served checkpoint has its own record and the supplied one is neither
	// it nor the family's, so this is the branch the guard is about: the level is
	// `family` and only the "is it the family's record" test keeps it quiet.
	model, err := NewDecisionModel(DecisionModelConfig{
		BaseURL: "http://stub.invalid", Model: "shisa-ai/shisa-de-2.1",
		Readout: readout, MaxWorkers: 1, Calibration: &custom,
	})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	defer model.Close()
	if !model.CalibrationApplied {
		t.Error("a supplied record was not applied")
	}
	if model.CalibrationLevel != ApplicabilityFamily {
		t.Fatalf("the supplied record is %q, want %q so the guard is exercised",
			model.CalibrationLevel, ApplicabilityFamily)
	}
	for _, warning := range model.Warnings {
		if strings.Contains(warning, "own record") {
			t.Errorf("a custom fit was second-guessed: %s", warning)
		}
	}
}

// TestAFamilyRecordOnACheckpointWithItsOwnRecordIsReported pins the second way a
// wrong fit arrives: a caller passes a family's record, which was one fit before
// a family had two. A supplied record is still applied as given -- it is the
// caller's assertion -- but naming the family no longer names one fit, so the
// answer says so.
func TestAFamilyRecordOnACheckpointWithItsOwnRecordIsReported(t *testing.T) {
	family, err := CalibrationFor(FamilyDE2)
	if err != nil {
		t.Fatalf("CalibrationFor: %v", err)
	}
	readout, _ := newPolicyReadout(t, 1)
	model, err := NewDecisionModel(DecisionModelConfig{
		BaseURL: "http://stub.invalid", Model: "shisa-ai/shisa-de-2.1",
		Readout: readout, MaxWorkers: 1, Calibration: &family,
	})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	defer model.Close()
	if model.Calibration.Model != "shisa-ai/shisa-de-2" {
		t.Errorf("the supplied record is %s", model.Calibration.Model)
	}
	if !model.CalibrationApplied {
		t.Error("a supplied record was not applied")
	}
	found := false
	for _, warning := range model.Warnings {
		if strings.Contains(warning, "shisa-ai/shisa-de-2.1") && strings.Contains(warning, "own record") {
			found = true
		}
	}
	if !found {
		t.Errorf("a family record on a checkpoint that has its own was not reported: %v", model.Warnings)
	}
	// The record for the served checkpoint is the one that should have been used.
	if got, err := CalibrationForCheckpoint(FamilyDE2, "shisa-ai/shisa-de-2.1", ""); err != nil || got.Model != "shisa-ai/shisa-de-2.1" {
		t.Fatalf("CalibrationForCheckpoint = %q, %v", got.Model, err)
	}

	// The same mistake through the tokenizer: DE-2.1 served with DE-2's tokenizer
	// reaches `checkpoint` level for the DE-2 record, so a guard on the level
	// would stay quiet while DE-2's fit is applied to DE-2.1.
	readout2, _ := newPolicyReadout(t, 1)
	shared, err := NewDecisionModel(DecisionModelConfig{
		BaseURL: "http://stub.invalid", Model: "shisa-ai/shisa-de-2.1",
		Tokenizer: "shisa-ai/shisa-de-2", Readout: readout2, MaxWorkers: 1,
		Calibration: &family,
	})
	if err != nil {
		t.Fatalf("NewDecisionModel: %v", err)
	}
	defer shared.Close()
	if shared.CalibrationLevel != ApplicabilityCheckpoint {
		t.Errorf("the shared-tokenizer record is %q, the case is not exercised", shared.CalibrationLevel)
	}
	found = false
	for _, warning := range shared.Warnings {
		if strings.Contains(warning, "own record") {
			found = true
		}
	}
	if !found {
		t.Errorf("a family record on a DE-2.1 endpoint sharing DE-2's tokenizer was not reported: %v",
			shared.Warnings)
	}
}
