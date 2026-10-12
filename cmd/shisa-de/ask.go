package main

import (
	"context"
	"fmt"
	"io"

	shisade "github.com/shisa-ai/shisa-de-go"
)

// runAsk is the Python command line's `ask` branch: one label set, one
// classification, printed as `json.dumps(dict(result), indent=2,
// ensure_ascii=False)`.
//
// `dict(result)` is the head-to-value mapping, which is `Decision.Values`. The
// typed answers with their distributions and request accounting are not printed;
// `explain` is the command for those.
func runAsk(ctx context.Context, args *args, model *shisade.DecisionModel,
	stdout, stderr io.Writer) int {
	// An explicitly empty `--image` is an unusable reference, not "no image". The
	// Python command line passes whatever it was given to `prepare_image`, which
	// reports it as `image is empty` rather than answering the text question.
	// `CallOptions.Image` cannot express the difference — empty means no image
	// there — so the flag records whether it was given, and the message comes from
	// the library rather than from a second copy of it here.
	if args.imageSet && args.image == "" {
		if _, err := shisade.PrepareImage(args.image); err != nil {
			fmt.Fprintf(stderr, "shisa-de ask: %v\n", err)
			return 1
		}
	}
	state := stateFrom(args.state, args.stateSet)
	if args.image != "" && !args.stateSet {
		// An image question with no state asks about the image alone, which is
		// what the Python command line's `{} if args.image and args.state is
		// None else _state_from(args.state)` does.
		state = shisade.Object{}
	}
	head, labels := labelsFrom(args.labels)
	spec := shisade.HeadSpec{Options: optionsFromLabels(labels)}
	if args.prompt != "" {
		spec.Instructions = args.prompt
	}
	decision, err := model.Classify(ctx, state, map[string]shisade.HeadSpec{head: spec},
		shisade.ClassifyOptions{
			IncludeConfidence:    args.confidence,
			IncludeProbabilities: args.probabilities,
			CallOptions:          shisade.CallOptions{Image: args.image},
		})
	if err != nil {
		fmt.Fprintf(stderr, "shisa-de ask: %v\n", err)
		return 1
	}
	indent := "  "
	text, err := shisade.PythonJSONWith(decision.Values, shisade.PythonJSONOptions{Indent: &indent})
	if err != nil {
		fmt.Fprintf(stderr, "shisa-de ask: the answer is not JSON: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, text)
	return 0
}
