package main

import (
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"

	shisade "github.com/shisa-ai/shisa-de-go"
)

// The three option shapes the reference command line has and Go's `flag` does
// not.
//
// `argparse` distinguishes "not given" from "given the zero value", and the
// policy resolver needs that distinction: `--no-reasoning` and no flag at all
// are different reads. Go's `flag` cannot tell them apart for a bool, a float or
// an int, so each of these records whether it was set and hands the caller a
// pointer that is nil when it was not. The bool also carries the `--no-` form,
// which is `argparse.BooleanOptionalAction`.

// optionalBool is a flag that is `--name`, `--no-name`, or absent.
type optionalBool struct {
	set   bool
	value bool
}

func (o *optionalBool) String() string {
	if !o.set {
		return ""
	}
	return strconv.FormatBool(o.value)
}

func (o *optionalBool) pointer() *bool {
	if !o.set {
		return nil
	}
	return &o.value
}

// registerOptionalBool adds `--name` and `--no-name`, sharing one value.
//
// Both are `BoolFunc` rather than `Var`, because `flag` only lets a bare
// `--name` through for a bool flag and these two are what `argparse` calls
// `BooleanOptionalAction`: they are given with no argument.
func registerOptionalBool(flags *flag.FlagSet, name string, target *optionalBool, help string) {
	flags.BoolFunc(name, help, func(text string) error {
		value, err := strconv.ParseBool(text)
		if err != nil {
			return fmt.Errorf("expected a boolean, got %q", text)
		}
		target.set, target.value = true, value
		return nil
	})
	flags.BoolFunc("no-"+name, "the opposite of --"+name, func(text string) error {
		value, err := strconv.ParseBool(text)
		if err != nil {
			return fmt.Errorf("expected a boolean, got %q", text)
		}
		target.set, target.value = true, !value
		return nil
	})
}

// optionalFloat is a float flag that is nil when it was not given.
type optionalFloat struct {
	set   bool
	value float64
}

func (o *optionalFloat) String() string {
	if !o.set {
		return ""
	}
	return strconv.FormatFloat(o.value, 'g', -1, 64)
}

func (o *optionalFloat) Set(text string) error {
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return fmt.Errorf("expected a number, got %q", text)
	}
	o.set, o.value = true, value
	return nil
}

func (o *optionalFloat) pointer() *float64 {
	if !o.set {
		return nil
	}
	return &o.value
}

func registerOptionalFloat(flags *flag.FlagSet, name string, target *optionalFloat, help string) {
	flags.Var(target, name, help)
}

// optionalInt is an int flag that is nil when it was not given.
type optionalInt struct {
	set   bool
	value int
}

func (o *optionalInt) String() string {
	if !o.set {
		return ""
	}
	return strconv.Itoa(o.value)
}

func (o *optionalInt) Set(text string) error {
	value, err := strconv.Atoi(text)
	if err != nil {
		return fmt.Errorf("expected an integer, got %q", text)
	}
	o.set, o.value = true, value
	return nil
}

func (o *optionalInt) pointer() *int {
	if !o.set {
		return nil
	}
	return &o.value
}

func registerOptionalInt(flags *flag.FlagSet, name string, target *optionalInt, help string) {
	flags.Var(target, name, help)
}

// optionalStringValue is a string flag that is nil when it was not given.
//
// `--state` needs it: an absent `--state` means the built-in example, and
// `--state ""` is an empty state, which is a different question.
type optionalStringValue struct {
	set   bool
	value string
}

func (o *optionalStringValue) String() string { return o.value }

func (o *optionalStringValue) Set(text string) error {
	o.set, o.value = true, text
	return nil
}

func sortStrings(values []string) { sort.Strings(values) }

// commonFlags is the option list `commandUsage` prints. It is written from the
// same strings the flags are registered with, so the help cannot describe a
// flag that is not there, and every flag that is there is described.
func commonFlags(command string) string {
	lines := []string{
		"  --base-url URL          endpoint (default: $SHISA_DE_ENDPOINT, then the hosted endpoint)",
		"  --model ID              served model id (default: " + defaultModel + ")",
		"  --tokenizer SOURCE      tokenizer source (defaults to --model)",
		"  --calibration SPEC      a family (" + calibrationFamilies() + ") or a record file",
		"  --family NAME           the contract the served model is read through",
		"  --policy NAME           " + strings.Join(shisade.Policies, " | "),
		"  --reads NAME            " + strings.Join(shisade.ReadCounts, " | "),
		"  --reasoning / --no-reasoning",
		"  --reasoning-prob P      the think gate (default: " + shisade.PythonStr(shisade.ThinkGate) + ")",
		"  --compound / --no-compound",
		"                          a DE-1 read this port does not have; accepted and refused",
		fmt.Sprintf("  --reasoning-len N       the thought budget (default: %d)", shisade.ThinkBudget),
		"  --timeout SECONDS       one request's budget (default: 120)",
	}
	switch command {
	case "doctor":
		lines = append(lines,
			"  --json                  print the report as JSON",
			"  --probe                 also send one question to check the served read")
	case "ask":
		lines = append(lines,
			"  --state JSON|TEXT       the state; a built-in example by default",
			"  --image REFERENCE       local image path, HTTP(S) URL, or image data URL",
			fmt.Sprintf("  --image-top-logprobs N  image logprob limit (default: %d)",
				shisade.DefaultImageTopLogprobs),
			"  --labels a,b,c          comma separated labels",
			"  --prompt TEXT           the question to ask",
			"  --confidence            return the label with its confidence",
			"  --probabilities         return the full distribution")
	case "explain":
		lines = append(lines,
			"  --state JSON|TEXT       the state; a built-in example by default",
			"  --labels a,b,c          comma separated labels",
			"  --prompt TEXT           the question to ask")
	}
	return strings.Join(lines, "\n")
}
