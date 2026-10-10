package shisade

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// keyedOptions is n options with distinct keys, for the tests that only care
// about the option count. A repeated key is refused, so a slice of zero-valued
// Options is no longer a stand-in for "n options".
func keyedOptions(n int) []Option {
	options := make([]Option, n)
	for index := range options {
		options[index] = Option{Key: fmt.Sprintf("o%02d", index), Description: fmt.Sprintf("Option %d", index)}
	}
	return options
}

// TestQuestionRefusesARepeatedKey pins the one validation rule Python cannot
// reach.
//
// Python's criteria are a dict, so a repeated key cannot survive into a read.
// This port takes a slice, because a Go map has no order and the order is what
// assigns the answer codes. Two options with one key would be scored from two
// codes and then written into one probability, so the returned distribution
// would no longer sum to one and the confidence would describe a different
// distribution than the caller receives.
func TestQuestionRefusesARepeatedKey(t *testing.T) {
	duplicate := []Option{{Key: "billing", Description: "Charges"}, {Key: "billing", Description: "Also charges"}}
	var questionErr *QuestionError

	err := NewChoice("Which queue?", duplicate).Validate(MaxOptions)
	if !errors.As(err, &questionErr) {
		t.Fatalf("a repeated choice key returned %v, want *QuestionError", err)
	}
	if !strings.Contains(err.Error(), "billing") {
		t.Errorf("the error does not name the key: %v", err)
	}
	// An empty key is a key, and two of them collide too.
	if err := NewChoice("Which queue?", []Option{{}, {}}).Validate(MaxOptions); err == nil {
		t.Error("two empty keys were accepted")
	}
	// Distinct keys are fine, and the score's positional keys and the noul's
	// yes/no never collide.
	if err := NewChoice("Which queue?", keyedOptions(3)).Validate(MaxOptions); err != nil {
		t.Errorf("three distinct keys returned %v", err)
	}
	if err := NewNoul("Is it?").Validate(MaxOptions); err != nil {
		t.Errorf("a noul returned %v", err)
	}
	if err := NewScore("How bad?", []any{"a", "a"}).Validate(MaxOptions); err != nil {
		t.Errorf("a score with two equal levels returned %v; the levels are not the keys", err)
	}
}

// TestClassifyRefusesARepeatedKeyBeforeSending pins that the refusal happens in
// the validation pass, so a bad head costs nothing.
func TestClassifyRefusesARepeatedKeyBeforeSending(t *testing.T) {
	model, server := newStubModel(t, 2, codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	_, err := model.Classify(context.Background(), map[string]any{"e": "x"},
		map[string]HeadSpec{"route": {Options: []Option{
			{Key: "billing"}, {Key: "billing"},
		}}}, ClassifyOptions{})
	if err == nil {
		t.Fatal("a repeated label was accepted")
	}
	if server.at != 0 {
		t.Errorf("a refused head spent %d requests", server.at)
	}
}

// TestMultiLabelRefusesARepeatedLabel pins the same rule on the one path a
// choice's validation does not cover.
//
// A multi-label head becomes one noul per label under the key `head:label`, so a
// repeated label reads the same question twice under one key and the second
// answer overwrites the first. Python pops the key once and then raises a
// `KeyError`; this port refuses it before anything is sent. Without the refusal
// the second read's answer is written, the first is dropped from the view, and
// `Raw["head:label"]` is left as the zero answer, silently.
func TestMultiLabelRefusesARepeatedLabel(t *testing.T) {
	model, server := newStubModel(t, 2,
		codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}),
		codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}))
	_, err := model.Classify(context.Background(), map[string]any{"e": "x"},
		map[string]HeadSpec{"topic": {Options: []Option{
			{Key: "billing"}, {Key: "billing"},
		}, MultiLabel: true, Instructions: "Is it {label}?"}}, ClassifyOptions{})
	if err == nil {
		t.Fatal("a repeated multi-label label was accepted")
	}
	var questionErr *QuestionError
	if !errors.As(err, &questionErr) {
		t.Errorf("the error is %v, want a *QuestionError", err)
	}
	if !strings.Contains(err.Error(), "billing") {
		t.Errorf("the error does not name the label: %v", err)
	}
	if server.at != 0 {
		t.Errorf("a refused head spent %d requests", server.at)
	}
	// Distinct labels are the head this reads, and the two reads do not collide.
	model, server = newStubModel(t, 2,
		codeReadBody(map[string]float64{"A": -0.1, "B": -1.0}),
		codeReadBody(map[string]float64{"A": -3.0, "B": -0.1}))
	decision, err := model.Classify(context.Background(), map[string]any{"e": "x"},
		map[string]HeadSpec{"topic": {Options: []Option{
			{Key: "billing"}, {Key: "outage"},
		}, MultiLabel: true, Instructions: "Is it {label}?"}}, ClassifyOptions{})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(decision.Raw) != 2 {
		t.Errorf("two labels produced %d raw entries: %v", len(decision.Raw), decision.Raw)
	}
	if server.at != 2 {
		t.Errorf("two labels spent %d requests, want 2", server.at)
	}
}
