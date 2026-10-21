// Command shisa-de is the command line for the DE-2 client: `doctor` checks an
// endpoint before you depend on it, `ask` is the one-line classification call,
// and `explain` prints every step of the readout for one question.
//
// The Python client's `shisa_de/cli.py` is the reference. What is deliberately
// absent here is the DE-1 read: the DE-1 branches of `explain`. `PORT.md`
// records it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	shisade "github.com/shisa-ai/shisa-de-go"
)

// defaultModel is the checkpoint the command line reads when nothing names one.
//
// It is the library's `DefaultModel` rather than a second copy of the id, so
// the two cannot disagree. It used to differ: the library defaulted to
// `shisa-ai/shisa-de-1` to match the Python client, which this port refuses, so
// a command whose zero-config form could only fail needed its own. DE-1 is
// retired now and the library defaults to DE-2, so the command line inherits.
const defaultModel = shisade.DefaultModel

// usageError is a bad invocation: the message is printed with the usage and the
// exit status is 2, which is what `argparse`'s `parser.error` does.
type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole command line, with the exit status as its return value so a
// test can drive it without a subprocess.
func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprint(stderr, topUsage())
		return 2
	}
	if argv[0] == "-h" || argv[0] == "--help" {
		fmt.Fprint(stdout, topUsage())
		return 0
	}

	command, rest := argv[0], argv[1:]
	switch command {
	case "doctor", "ask", "explain":
	default:
		fmt.Fprintf(stderr, "shisa-de: unknown command %q\n\n%s", command, topUsage())
		return 2
	}

	args, err := parse(command, rest)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, commandUsage(command))
			return 0
		}
		fmt.Fprintf(stderr, "shisa-de %s: %v\n\n%s", command, err, commandUsage(command))
		return 2
	}

	model, err := args.model()
	if err != nil {
		fmt.Fprintf(stderr, "shisa-de %s: %v\n\n%s", command, err, commandUsage(command))
		return 2
	}
	defer model.Close()

	switch command {
	case "doctor":
		return runDoctor(ctx, args, model, stdout, stderr)
	case "ask":
		return runAsk(ctx, args, model, stdout, stderr)
	default:
		return runExplain(ctx, args, model, stdout, stderr)
	}
}

// args is the parsed command line. One struct serves all three commands; a flag
// a command does not take is left at its zero value because it was never
// registered.
type args struct {
	command string

	baseURL     string
	modelID     string
	tokenizer   string
	calibration string
	family      string
	policy      string
	reads       string

	// The four choice flags above are read through their `set` flag as well as
	// their value, because `argparse` validates `choices` at parse time and
	// rejects an explicitly empty one. Without these, `--policy "$POLICY"` with
	// `POLICY` unset would quietly read the default policy.
	calibrationSet bool
	familySet      bool
	policySet      bool
	readsSet       bool
	reasoning      optionalBool
	reasoningP     optionalFloat
	reasoningN     optionalInt
	compound       optionalBool
	timeout        float64

	json             bool
	probe            bool
	state            string
	stateSet         bool
	labels           string
	prompt           string
	confidence       bool
	probabilities    bool
	image            string
	imageSet         bool
	imageTopLogprobs int
}

// newModel builds the client. It is a variable so a test can inject a readout
// with a stub tokenizer and a stub transport; the flags still go through
// `modelConfig`, which is what the tests that check the mapping exercise.
var newModel = shisade.NewDecisionModel

// model builds the client the flags describe.
func (a *args) model() (*shisade.DecisionModel, error) {
	config, err := a.modelConfig()
	if err != nil {
		return nil, err
	}
	return newModel(config)
}

// modelConfig is the flags as a `DecisionModelConfig`.
func (a *args) modelConfig() (shisade.DecisionModelConfig, error) {
	config := shisade.DecisionModelConfig{
		BaseURL:          a.baseURL,
		Model:            a.modelID,
		Tokenizer:        a.tokenizer,
		Timeout:          time.Duration(a.timeout * float64(time.Second)),
		ImageTopLogprobs: a.imageTopLogprobs,
	}
	// The family is an exact choice, which is what `argparse`'s `choices` is:
	// `--family DE-2` is a bad invocation, not a spelling to normalize.
	if a.familySet {
		if !contains(shisade.Families, a.family) {
			return shisade.DecisionModelConfig{}, &usageError{fmt.Sprintf(
				"unknown family %q; expected one of %s",
				a.family, familyNames())}
		}
		config.Family = shisade.Family(a.family)
	}
	if a.calibrationSet && a.calibration != "auto" {
		if a.calibration == "" {
			return shisade.DecisionModelConfig{}, &usageError{fmt.Sprintf(
				"--calibration '' is neither a family (%s), a released checkpoint (%s) nor a record file",
				familyNames(), checkpointNames())}
		}
		record, _, err := shisade.ResolveCalibration(a.calibration)
		if err != nil {
			return shisade.DecisionModelConfig{}, err
		}
		config.Calibration = &record
	}
	config.Overrides = shisade.OptionOverrides{
		Reasoning: a.reasoning.pointer(),
		Compound:  a.compound.pointer(),
	}
	if a.readsSet {
		if !contains(shisade.ReadCounts, a.reads) {
			return shisade.DecisionModelConfig{}, &usageError{fmt.Sprintf(
				"unknown reads %q; expected one of %s",
				a.reads, strings.Join(shisade.ReadCounts, ", "))}
		}
		reads := a.reads
		config.Overrides.Reads = &reads
	}
	if a.policySet {
		if !contains(shisade.Policies, a.policy) {
			return shisade.DecisionModelConfig{}, &usageError{fmt.Sprintf(
				"unknown policy %q; expected one of %s",
				a.policy, strings.Join(shisade.Policies, ", "))}
		}
		policy := a.policy
		config.Overrides.Policy = &policy
	}
	if gate := a.reasoningP.pointer(); gate != nil {
		config.Overrides.ReasoningProb = gate
	}
	if length := a.reasoningN.pointer(); length != nil {
		config.Overrides.ReasoningLen = length
	}
	return config, nil
}

// parse reads one command's flags. Unknown flags, a missing value and a bad
// choice are all errors, which is what `argparse` does.
func parse(command string, argv []string) (*args, error) {
	parsed := &args{
		command:   command,
		modelID:   defaultModel,
		reasoning: optionalBool{},
		timeout:   120,
		// The Python command line reads this with
		// `getattr(args, "image_top_logprobs", 20)`, so every command has the
		// default even though only `ask` registers the flag.
		imageTopLogprobs: shisade.DefaultImageTopLogprobs,
	}
	// A nil `--state` is a real value here: it means "use the built-in example",
	// and the empty string is a state like any other. `flag` cannot tell the two
	// apart, so the flag is recorded through a `flag.Value`.
	state := optionalStringValue{}
	// `--image` needs the same treatment: an absent `--image` is a text read and
	// `--image ""` is an unusable reference, which the Python command line
	// reports as `image is empty` rather than answering the text question.
	image := optionalStringValue{}
	labels := optionalStringValue{}
	calibration := optionalStringValue{}
	family := optionalStringValue{}
	policy := optionalStringValue{}
	reads := optionalStringValue{}

	flags := flag.NewFlagSet("shisa-de "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}

	flags.StringVar(&parsed.baseURL, "base-url", "",
		"endpoint (default: $SHISA_DE_ENDPOINT, then the hosted endpoint)")
	flags.StringVar(&parsed.modelID, "model", defaultModel, "served model id")
	flags.StringVar(&parsed.tokenizer, "tokenizer", "", "tokenizer source (defaults to --model)")
	flags.Var(&calibration, "calibration",
		"calibration record: a family ("+calibrationFamilies()+"), a released checkpoint ("+
			checkpointNames()+") or a path to a record. "+
			"Default: chosen from the served model id")
	flags.Var(&family, "family", "the contract the served model is read through. "+
		"Default: taken from the model id, then the tokenizer, otherwise assumed to be de2")
	flags.Var(&policy, "policy", "the DE-2 read. Default: repeat-think")
	flags.Var(&reads, "reads", "how often the question is written. Default: double")
	registerOptionalBool(flags, "reasoning", &parsed.reasoning,
		"think when the read is unsure, then read again. Default: on")
	registerOptionalFloat(flags, "reasoning-prob", &parsed.reasoningP,
		fmt.Sprintf("think when the top probability is below P (default: %s)",
			shisade.PythonStr(shisade.ThinkGate)))
	registerOptionalInt(flags, "reasoning-len", &parsed.reasoningN,
		fmt.Sprintf("the most tokens a thought may run to (default: %d)", shisade.ThinkBudget))
	registerOptionalBool(flags, "compound", &parsed.compound,
		"read a DE-1 choice above 26 options in two rounds; this port has no DE-1 read, "+
			"so the flag is accepted and refused")
	flags.Float64Var(&parsed.timeout, "timeout", 120, "seconds before one request gives up")

	switch command {
	case "doctor":
		flags.BoolVar(&parsed.json, "json", false, "print the report as JSON")
		flags.BoolVar(&parsed.probe, "probe", false,
			"also send one question to check the served read")
	case "ask":
		flags.Var(&state, "state", "JSON state, or plain text; a built-in example by default")
		flags.StringVar(&parsed.prompt, "prompt", "", "the question to ask (default: derived from the head name)")
		flags.Var(&labels, "labels", "comma separated labels")
		flags.Var(&image, "image", "local image path, HTTP(S) URL, or image data URL")
		flags.IntVar(&parsed.imageTopLogprobs, "image-top-logprobs",
			shisade.DefaultImageTopLogprobs,
			"image logprob limit (must be allowed by the server)")
		flags.BoolVar(&parsed.confidence, "confidence", false, "return the label with its confidence")
		flags.BoolVar(&parsed.probabilities, "probabilities", false, "return the full distribution")
	case "explain":
		flags.Var(&state, "state", "JSON state, or plain text; a built-in example by default")
		flags.Var(&labels, "labels", "comma separated labels")
		flags.StringVar(&parsed.prompt, "prompt", "", "the question to ask")
	}

	if err := flags.Parse(argv); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	parsed.state, parsed.stateSet = state.value, state.set
	parsed.image, parsed.imageSet = image.value, image.set
	parsed.labels = labels.value
	parsed.calibration, parsed.calibrationSet = calibration.value, calibration.set
	parsed.family, parsed.familySet = family.value, family.set
	parsed.policy, parsed.policySet = policy.value, policy.set
	parsed.reads, parsed.readsSet = reads.value, reads.set
	return parsed, nil
}

// contains is the membership test `argparse`'s `choices` is. It is generic
// because the family list is `[]shisade.Family` and the others are `[]string`.
func contains[T ~string](values []T, wanted string) bool {
	for _, value := range values {
		if string(value) == wanted {
			return true
		}
	}
	return false
}

func calibrationFamilies() string {
	names := make([]string, 0, len(shisade.CalibrationFiles))
	for family := range shisade.CalibrationFiles {
		names = append(names, string(family))
	}
	sortStrings(names)
	return strings.Join(names, ", ")
}

// checkpointNames is the released checkpoints a calibration record ships for.
func checkpointNames() string {
	names := make([]string, 0, len(shisade.CheckpointCalibrationFiles))
	for checkpoint := range shisade.CheckpointCalibrationFiles {
		names = append(names, checkpoint)
	}
	sortStrings(names)
	return strings.Join(names, ", ")
}

// familyNames is `FAMILIES` as the reference command line writes it.
func familyNames() string {
	names := make([]string, 0, len(shisade.Families))
	for _, family := range shisade.Families {
		names = append(names, string(family))
	}
	return strings.Join(names, ", ")
}

// topUsage and commandUsage are written by hand rather than by `flag`, because
// `flag`'s own printer does not show the `--no-` forms and prints the defaults
// in a different style from the reference command line.
func topUsage() string {
	return `usage: shisa-de <command> [options]

commands:
  doctor   check an endpoint, the served model, and the answer boundary
  ask      classify a state against a label set
  explain  print every step of the readout for one question

Run "shisa-de <command> --help" for a command's options.
`
}

func commandUsage(command string) string {
	var description string
	switch command {
	case "doctor":
		description = "check an endpoint, the served model, and the answer boundary"
	case "ask":
		description = "classify a state against a label set"
	default:
		description = "print every step of the readout for one question"
	}
	return fmt.Sprintf(`usage: shisa-de %s [options]

%s

options:
%s
`, command, description, commonFlags(command))
}
