package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	shisade "github.com/shisa-ai/shisa-de-go"
)

// rule is the line the walkthrough puts under each heading.
const rule = "------------------------------------------------------------------------"

// runExplain is the Python command line's `explain` branch, with its DE-1
// branch removed: this port has one contract, so the first read of the DE-2
// policy is the only read there is.
//
// A thought, when the gate asks for one, follows the read shown here and is not
// shown; the last line says whether one would.
func runExplain(ctx context.Context, args *args, model *shisade.DecisionModel,
	stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "shisa-de explain: %v\n", err)
		return 1
	}
	state, err := stateFrom(args.state, args.stateSet)
	if err != nil {
		fmt.Fprintf(stderr, "shisa-de explain: %v\n", err)
		return 2
	}
	readout := model.Readout
	if readout == nil {
		fmt.Fprintln(stderr, "shisa-de explain: the model has no readout")
		return 1
	}

	head, labels := labelsFrom(args.labels)
	instructions := args.prompt
	if instructions == "" {
		instructions = defaultQuestion(head)
	}
	question := shisade.NewChoice(instructions, optionsFromLabels(labels))
	options := question.Options()

	// The first read of the policy. `repeat-think` and `repeat` write the user
	// turn twice; `direct` writes it once.
	repeat := 2
	if model.Policy() == "direct" {
		repeat = 1
	}
	prompt, err := readout.Render(state, question, shisade.RenderOptions{
		Repeat: repeat, OptionLimit: shisade.MaxCodes,
	})
	if err != nil {
		return fail(err)
	}
	slots, err := readout.CodeSlots(len(options))
	if err != nil {
		return fail(err)
	}
	tokenizer, err := readout.EnsureTokenizer()
	if err != nil {
		return fail(err)
	}
	// The boundary check is the point of the walkthrough: it is what says the
	// codes were read from the position the answer is written at.
	if err := readout.CheckCodeBoundary(tokenizer.Encode(prompt), slots); err != nil {
		return fail(err)
	}

	// The read is taken before anything is printed, so a question the readout
	// cannot read reports an error rather than half a walkthrough.
	read, err := readout.ReadCodes(ctx, prompt, slots, true)
	if err != nil {
		return fail(err)
	}
	// The body the read sent, in the order `json.dumps` would write it, which is
	// the order the curl line shows.
	body := shisade.Object{
		{Key: "model", Value: model.Model},
		{Key: "prompt", Value: prompt},
		{Key: "max_tokens", Value: 1},
		{Key: "temperature", Value: 0},
		{Key: "logprobs", Value: readout.MaxLogprobs()},
	}
	ids := make([]any, 0, len(slots))
	for _, slot := range slots {
		ids = append(ids, slot.TokenID)
	}
	body = append(body,
		shisade.Member{Key: "return_tokens_as_token_ids", Value: true},
		shisade.Member{Key: "logprob_token_ids", Value: ids})

	fmt.Fprintln(stdout, "1. The prompt the model receives")
	fmt.Fprintln(stdout, rule)
	fmt.Fprintln(stdout, prompt)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "2. The answer slots")
	fmt.Fprintln(stdout, rule)
	for _, slot := range slots {
		fmt.Fprintf(stdout, "   %s  token %6d\n", slot.Code, slot.TokenID)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "3. One request: max_tokens 1, temperature 0, the %d answer codes by token id\n",
		len(slots))
	fmt.Fprintln(stdout, rule)
	fmt.Fprintf(stdout, "   POST %s/v1/completions\n", model.BaseURL)
	fmt.Fprintf(stdout, "   prompt tokens %d, requests %d, sampled %s\n",
		read.PromptTokens, read.Requests, reprOptionalString(read.Sampled))
	marks := make(map[string]string, len(slots)*2)
	for _, slot := range slots {
		marks[slot.Code] = slot.Code
		marks[fmt.Sprintf("token_id:%d", slot.TokenID)] = slot.Code
	}
	for _, entry := range sortedTopLogprobs(read.TopLogprobs) {
		marker := ""
		if code, ok := marks[entry.token]; ok {
			marker = "  <- answer slot " + code
		}
		fmt.Fprintf(stdout, "   %18s  %9.4f%s\n",
			shisade.PythonRepr(entry.token), entry.logprob, marker)
	}
	if len(read.MissingFromTop) != 0 {
		fmt.Fprintf(stdout, "   fallback requests for codes not returned: %s\n",
			strings.Join(read.MissingFromTop, ", "))
	}
	fmt.Fprintln(stdout)

	fmt.Fprintln(stdout, "4. The distribution over the option letters")
	fmt.Fprintln(stdout, rule)
	for index, slot := range slots {
		fmt.Fprintf(stdout, "   %7.4f  %s  %s  (%s)\n",
			read.Probabilities[slot.Code], slot.Code, options[index].Key,
			shisade.PythonStr(options[index].Description))
	}
	best, top, err := bestCode(read, slots)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "   answer: %s (%.4f)\n", best, read.Probabilities[best])
	fmt.Fprintln(stdout)

	fmt.Fprintln(stdout, "5. The same request without Python")
	fmt.Fprintln(stdout, rule)
	encoded, err := shisade.PythonJSONWith(body, shisade.PythonJSONOptions{EnsureASCII: true})
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "curl -s %s \\\n", shlexQuote(model.BaseURL+"/v1/completions"))
	fmt.Fprintln(stdout, "  -H 'Content-Type: application/json' \\")
	if apiKeyFromEnv() != "" {
		fmt.Fprintln(stdout, `  -H "Authorization: Bearer ${SHISA_DE_API_KEY:-$SHISA_API_KEY}" \`)
	}
	fmt.Fprintf(stdout, "  -d %s\n", shlexQuote(encoded))
	fmt.Fprintln(stdout)

	wouldThink := model.Policy() == "repeat-think" && top < model.ThinkGate() &&
		len(options) <= shisade.ThinkOptionCap
	fmt.Fprintf(stdout, "readout %s, policy %s; %d codes available, %d used\n",
		model.ReadoutVersion, model.Policy(), shisade.MaxCodes, len(options))
	if model.Policy() != "direct" {
		fmt.Fprintf(stdout, "the user turn is written twice, joined by %s\n",
			shisade.PythonRepr(shisade.InputRepeat))
	}
	if model.Policy() == "repeat-think" {
		verdict := "this read is the answer"
		if wouldThink {
			verdict = "a thought would follow this read"
		}
		fmt.Fprintf(stdout, "top probability %.4f against the gate %v: %s\n",
			top, model.ThinkGate(), verdict)
	}
	return 0
}

// reprOptionalString is `repr` of a `str | None`, which is what `!r` does to the
// read's `sampled` field.
func reprOptionalString(value *string) string {
	if value == nil {
		return "None"
	}
	return shisade.PythonRepr(*value)
}

// topLogprob is one entry of the debug top-k.
type topLogprob struct {
	token   string
	logprob float64
}

// sortedTopLogprobs orders the debug top-k by descending logprob.
//
// Python's `sorted(..., key=lambda kv: -kv[1])` raises `TypeError` on a null
// logprob, which a server can send and the readout keeps. Here a null sorts
// last, because a walkthrough that dies on a server's null is worse than one
// that shows it.
func sortedTopLogprobs(values map[string]*float64) []topLogprob {
	entries := make([]topLogprob, 0, len(values))
	for token, logprob := range values {
		entry := topLogprob{token: token, logprob: 0}
		if logprob != nil {
			entry.logprob = *logprob
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := values[entries[i].token], values[entries[j].token]
		switch {
		case left == nil && right == nil:
			return entries[i].token < entries[j].token
		case left == nil:
			return false
		case right == nil:
			return true
		}
		if *left != *right {
			return *left > *right
		}
		// A tie keeps Python's order, which is the map's insertion order and is
		// not recoverable here; the token sorts, so the output is stable.
		return entries[i].token < entries[j].token
	})
	return entries
}

// bestCode is the highest-probability slot and that probability.
//
// Python's `read.answer()` is `max(probabilities, key=probabilities.get)`, which
// returns the first maximum in insertion order, and `read_codes` inserts in slot
// order. Iterating the slots in that order and skipping a code the read did not
// return is the same choice, and it is why this does not read a missing code as
// a probability of zero.
func bestCode(read shisade.CodeRead, slots []shisade.Slot) (string, float64, error) {
	best, top := "", 0.0
	for _, slot := range slots {
		value, ok := read.Probabilities[slot.Code]
		if !ok {
			continue
		}
		if best == "" || value > top {
			best, top = slot.Code, value
		}
	}
	if best == "" {
		return "", 0, fmt.Errorf("no code came back with a probability, so there is no answer to show")
	}
	return best, top, nil
}

// apiKeyFromEnv is the Python client's `_api_key_from_env`: the first of the
// client's variables that holds something but whitespace.
func apiKeyFromEnv() string {
	for _, name := range shisade.APIKeyEnvs {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// shlexQuote is Python's `shlex.quote`, which is what the curl line is written
// with so it can be pasted into a shell.
//
// A string of the shell-safe characters is left alone; anything else is wrapped
// in single quotes with each embedded single quote closed, escaped and reopened.
func shlexQuote(text string) string {
	if text == "" {
		return "''"
	}
	safe := true
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_@%+=:,./-", r):
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return text
	}
	return "'" + strings.ReplaceAll(text, "'", `'"'"'`) + "'"
}
