package main

import (
	"strings"

	shisade "github.com/shisa-ai/shisa-de-go"
)

// defaultState is the built-in example `ask` and `explain` use when no
// `--state` is given.
const (
	defaultTicket = "Order 4812 was marked delivered on Monday. The customer says the parcel " +
		"never arrived and tracking has not updated since Friday."
	defaultAccount = "customer since 2021, no prior claims"
)

// defaultLabels are the head and labels the command line uses when no
// `--labels` is given.
var defaultLabels = []string{"order_status", "refund_request", "cancel_order", "speak_to_human"}

// stateFrom is the Python client's `_state_from`.
//
// An absent `--state` is the built-in example. Text that starts with `{` or `[`
// is parsed as JSON, and text that does not parse is the state itself rather
// than an error: the readout accepts any JSON value as evidence, including a
// bare string.
//
// The parse is `DecodeJSON` rather than `encoding/json`, because the difference
// is visible: a plain unmarshal turns every number into a float64, and the state
// is serialized back into the prompt, where `3` and `3.0` are different tokens.
func stateFrom(text string, given bool) (any, error) {
	if !given {
		// An `Object` rather than a map: the evidence is serialized into the
		// prompt, and Python's `json.dumps` writes a dict in insertion order
		// while a Go map has none. The order is part of the bytes the model
		// sees, so the example is written the way the Python command line
		// writes it.
		return shisade.Object{
			{Key: "ticket", Value: defaultTicket},
			{Key: "account", Value: defaultAccount},
		}, nil
	}
	stripped := strings.TrimSpace(text)
	if strings.HasPrefix(stripped, "{") || strings.HasPrefix(stripped, "[") {
		value, err := shisade.DecodeJSON([]byte(stripped))
		if err == nil {
			return value, nil
		}
		// A `{`-prefixed string that is not JSON is a state like any other,
		// which is what `json.JSONDecodeError` gives the Python command line.
	}
	return text, nil
}

// labelsFrom is the Python client's `_labels_from`: one head named `intent`,
// with the comma separated labels, or the built-in set.
func labelsFrom(spec string) (string, []string) {
	if spec == "" {
		return "intent", append([]string(nil), defaultLabels...)
	}
	labels := make([]string, 0, strings.Count(spec, ",")+1)
	for _, part := range strings.Split(spec, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			labels = append(labels, trimmed)
		}
	}
	return "intent", labels
}

// optionsFromLabels is a label list as a choice's criteria: the label, and no
// description, which the renderer shows as the label.
func optionsFromLabels(labels []string) []shisade.Option {
	options := make([]shisade.Option, 0, len(labels))
	for _, label := range labels {
		options = append(options, shisade.Option{Key: label})
	}
	return options
}

// defaultQuestion is the Python client's `_default_question`.
func defaultQuestion(head string) string {
	return "What is the " + strings.ReplaceAll(head, "_", " ") + "?"
}
