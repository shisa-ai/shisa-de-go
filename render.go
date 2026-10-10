package shisade

import "strings"

// RenderOptions selects which prompt to render for one question.
type RenderOptions struct {
	// Repeat is how many times the user turn is written: 1, or 2 for the DE-2
	// repeated read. Anything below 1 is treated as 1.
	Repeat int
	// Thinking renders the prompt a thought is generated from: the thinking
	// system line, the template's thinking marker at the top of the system turn,
	// and no answer prefix, so the model opens and writes the thought itself.
	Thinking bool
	// OptionLimit is the option limit of the contract in use. Zero means
	// MaxOptions, the letter limit.
	OptionLimit int
}

// The pinned chat template.
//
// The Python client renders with `tokenizer.apply_chat_template`, which runs the
// checkpoint's own Jinja template. This port renders the two message shapes the
// readout uses, and only those, because a Jinja engine is a large dependency for
// six fixed fragments. The render is byte-identical for the system-plus-user
// shape with and without thinking, which is the whole of what the readout sends.
//
// It is pinned to the Gemma 4 template `shisa-ai/shisa-de-2` carries, including
// the literal `bos_token` the template's `{{- bos_token -}}` expands to.
// `bos_token` is a tokenizer-config value rather than part of the template, so a
// checkpoint with the same template and a different one would differ in the
// prompt's first token; the M3 read resolves it from the tokenizer and checks it
// against this literal. A checkpoint with a different template needs this
// function changed.
//
// What guards the pin: TestRenderParity, which compares 33 rendered prompts
// byte for byte against the Python client, is the guard for the render itself.
// The runtime checks catch something else -- a client/server tokenizer
// mismatch. Every read checks that the prompt ends on AnswerPrefix, and the
// first read checks the server's prompt-token count against this client's; both
// compare two tokenizations of the same bytes the client sent, so neither can
// see a render that disagrees with the checkpoint's Jinja template.
const (
	templateBOS       = "<bos>"
	templateTurnOpen  = "<|turn>"
	templateTurnClose = "<turn|>"
	templateThinkMark = "<|think|>\n"
	templateThought   = "<|channel>thought\n"
	templateRoleEnd   = "\n"
)

// Payload is the user JSON object one question is rendered from.
//
// It returns the object and the option pairs it was built from, in presentation
// order. The text readout renders the object into the prompt itself; an image
// read sends it as the text part beside the image and lets the server render.
func Payload(state any, question Question, optionLimit int) (Object, []Option, error) {
	if optionLimit <= 0 {
		optionLimit = MaxOptions
	}
	if err := question.Validate(optionLimit); err != nil {
		return nil, nil, err
	}
	options := question.Options()
	codes, err := CodesFor(len(options))
	if err != nil {
		return nil, nil, err
	}
	wireOptions := make([]any, len(options))
	for i, option := range options {
		wireOptions[i] = Object{
			{Key: "letter", Value: codes[i]},
			{Key: "description", Value: option.Description},
		}
	}
	payload := Object{
		{Key: "evidence", Value: state},
		{Key: "criterion", Value: question.Instructions()},
		{Key: "options", Value: wireOptions},
	}
	return payload, options, nil
}

// Render renders one question against one state into the prompt the model sees.
//
// The defaults are the DE-1 scaffold. DE-2 passes Repeat 2 to write the user turn
// twice, Thinking for the prompt a thought is generated from, and an OptionLimit
// of MaxCodes to admit codebook-width questions.
//
// The user turn is serialized the way Python's json.dumps does, not the way
// encoding/json does; see PythonJSON for why that is not a detail.
func Render(state any, question Question, options RenderOptions) (string, error) {
	payload, _, err := Payload(state, question, options.OptionLimit)
	if err != nil {
		return "", err
	}
	user, err := PythonJSON(payload)
	if err != nil {
		return "", &ReadoutError{Message: "the state cannot be rendered as the user turn", Err: err}
	}
	if options.Repeat > 1 {
		copies := make([]string, options.Repeat)
		for i := range copies {
			copies[i] = user
		}
		user = strings.Join(copies, InputRepeat)
	}

	var prompt strings.Builder
	prompt.WriteString(templateBOS)
	prompt.WriteString(templateTurnOpen)
	prompt.WriteString("system" + templateRoleEnd)
	if options.Thinking {
		prompt.WriteString(templateThinkMark)
		prompt.WriteString(ThinkSystem)
	} else {
		prompt.WriteString(DirectSystem)
	}
	prompt.WriteString(templateTurnClose + templateRoleEnd)
	prompt.WriteString(templateTurnOpen)
	prompt.WriteString("user" + templateRoleEnd)
	prompt.WriteString(user)
	prompt.WriteString(templateTurnClose + templateRoleEnd)
	prompt.WriteString(templateTurnOpen)
	prompt.WriteString("model" + templateRoleEnd)
	if !options.Thinking {
		prompt.WriteString(templateThought)
		prompt.WriteString(AnswerPrefix)
	}
	return prompt.String(), nil
}
