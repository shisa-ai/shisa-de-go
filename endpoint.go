package shisade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
)

// ReadoutHTTPError is a request the endpoint answered with an error status.
//
// It carries the status and the first part of the body, because the only useful
// thing about a rejected request is what the server said about it. The DE-2 code
// read uses Status to notice a server that does not know `logprob_token_ids` and
// ask again without it.
//
// It is also a ReadoutError, as it is in the Python client
// (`ReadoutHTTPError(ReadoutError)`): a caller that handles a failed read
// handles a rejected request too, without having to name both types.
type ReadoutHTTPError struct {
	*ReadoutError
	Status int
	Detail string
}

// Error is spelled out rather than promoted, so a ReadoutHTTPError built without
// its embedded error reports a bad value instead of panicking on a nil pointer.
// The only construction inside this package sets the embed.
func (e *ReadoutHTTPError) Error() string {
	if e == nil {
		return "shisade: <nil ReadoutHTTPError>"
	}
	if e.ReadoutError == nil {
		return fmt.Sprintf("shisade: HTTP %d: %s", e.Status, e.Detail)
	}
	return e.ReadoutError.Error()
}

// Unwrap is what makes an HTTP failure a ReadoutError as well. errors.As finds
// the embedded error through it. A nil embed unwraps to nothing rather than to a
// typed nil, which errors.As would otherwise match and hand back as a
// *ReadoutError that is nil.
func (e *ReadoutHTTPError) Unwrap() error {
	if e == nil || e.ReadoutError == nil {
		return nil
	}
	return e.ReadoutError
}

// The wire shapes of one /v1/completions call.
//
// The request is built from this struct rather than from a map so the fields a
// given read does not use are absent rather than null: a server that rejects
// `logprob_token_ids` must not also be sent `stop_token_ids`, and `prompt` is
// either the rendered text or, for the read that follows a thought, token ids.

type completionRequest struct {
	Model           string  `json:"model"`
	Prompt          any     `json:"prompt"`
	MaxTokens       int     `json:"max_tokens"`
	Temperature     float64 `json:"temperature"`
	Logprobs        *int    `json:"logprobs,omitempty"`
	LogprobTokenIDs []int   `json:"logprob_token_ids,omitempty"`
	// ReturnTokensAsTokenIDs keys the returned logprobs by `token_id:<id>`, so a
	// code is matched by id rather than by decoded text.
	ReturnTokensAsTokenIDs *bool `json:"return_tokens_as_token_ids,omitempty"`
	// PromptLogprobs is the DE-1 fallback: 0 asks for the logprob of the prompt's
	// own tokens, which is how a code missing from the top-k is read.
	PromptLogprobs *int `json:"prompt_logprobs,omitempty"`
	// StopTokenIDs stops the thought at the token the answer is read after.
	StopTokenIDs []int `json:"stop_token_ids,omitempty"`
	// ReturnTokenIDs returns the generated ids, which is what lets the read after
	// a thought continue it token for token instead of re-tokenizing its text.
	ReturnTokenIDs *bool `json:"return_token_ids,omitempty"`
}

type completionResponse struct {
	Choices []completionChoice `json:"choices"`
	Usage   completionUsage    `json:"usage"`
}

// The wire shapes of one /v1/chat/completions call.
//
// The image readout cannot render its prompt: only the server can expand an
// image placeholder into the image tokens the checkpoint expects. So it posts
// messages and lets the server apply the chat template, which is why the two
// reads do not share a request type -- the fields a given call does not use are
// absent rather than null, and the probe sends no logprobs while the image read
// sends no `stop_token_ids`.
type chatRequest struct {
	Model              string             `json:"model"`
	Messages           []Object           `json:"messages"`
	MaxTokens          int                `json:"max_tokens"`
	Temperature        float64            `json:"temperature"`
	Logprobs           *bool              `json:"logprobs,omitempty"`
	TopLogprobs        *int               `json:"top_logprobs,omitempty"`
	PromptLogprobs     *int               `json:"prompt_logprobs,omitempty"`
	ChatTemplateKwargs chatTemplateKwargs `json:"chat_template_kwargs"`
}

// chatTemplateKwargs is the one template flag every read here sends. The
// thinking read is a separate request shape on the completions endpoint, so no
// chat request in this client turns it on.
type chatTemplateKwargs struct {
	EnableThinking bool `json:"enable_thinking"`
}

type chatResponse struct {
	Choices []chatChoice    `json:"choices"`
	Usage   completionUsage `json:"usage"`
	// PromptLogprobs has one entry per prompt token, and the first is null. Each
	// entry maps the token's key to its logprob, rank and decoded text;
	// `prompt_logprobs: 0` returns only the prompt's own token at each position.
	//
	// This is what the image readout has instead of a locally tokenized prompt:
	// the last entry names the token the distribution was read after, and the
	// entries after the system line say whether the server rendered that line the
	// way the text scaffold does.
	PromptLogprobs []map[string]*chatPromptEntry `json:"prompt_logprobs"`
}

type chatPromptEntry struct {
	Logprob *float64 `json:"logprob"`
	Rank    *int     `json:"rank"`
	// DecodedToken is the server's own reading of the key, which the boundary
	// check compares against the locally resolved terminator. It is a pointer so
	// an entry that omits the field, or sends null, is distinguishable from one
	// that sends the empty string: the Python client reads it with `.get`, which
	// is None in the first two cases, and the refusal names what arrived.
	DecodedToken *string `json:"decoded_token"`
}

// chatChoice is a chat response's choice.
//
// The logprob and message fields are kept as raw JSON rather than decoded into
// structs, because the Python client validates the answer position entry by
// entry and prints the offending entry when one is malformed. A typed struct
// would reject the whole response in the decoder instead, and the message would
// say the endpoint did not return JSON rather than which entry was wrong. The
// raw bytes are read back with DecodeJSON, so an integer stays an integer and a
// key keeps its position, which is how `repr()` spells the entry.
type chatChoice struct {
	Message  map[string]json.RawMessage `json:"message"`
	Logprobs *struct {
		// One object per generated token; the image readout reads position 0.
		Content []map[string]json.RawMessage `json:"content"`
	} `json:"logprobs"`
}

// contentTopLogprobs is the answer position's token distribution from a chat
// response.
//
// A chat response carries top logprobs as a list of token objects, one per
// generated token, where the completions endpoint carries a map at the same
// position. Only the entries that are well formed take part; a malformed one is
// an error rather than a value to skip, because skipping it would answer from a
// distribution the server did not send.
func contentTopLogprobs(choice chatChoice) (map[string]float64, error) {
	if choice.Logprobs == nil || len(choice.Logprobs.Content) == 0 {
		return nil, &ReadoutError{Message: "the chat response carries no logprobs.content, " +
			"so there is no answer distribution to read"}
	}
	raw, present := choice.Logprobs.Content[0]["top_logprobs"]
	if !present {
		return nil, &ReadoutError{Message: "the chat response's logprobs.content[0] carries no " +
			"top_logprobs, so there is no answer distribution to read"}
	}
	decoded, err := DecodeJSON(raw)
	if err != nil {
		return nil, &ReadoutError{Message: "the chat response's top_logprobs is not readable",
			Err: err}
	}
	entries, ok := decoded.([]any)
	if !ok || len(entries) == 0 {
		return nil, &ReadoutError{Message: "the chat response's logprobs.content[0] carries no " +
			"top_logprobs, so there is no answer distribution to read"}
	}
	top := make(map[string]float64, len(entries))
	for _, entry := range entries {
		token, logprob, ok := chatLogprobEntry(entry)
		if !ok {
			return nil, &ReadoutError{Message: fmt.Sprintf(
				"the chat response has a malformed top_logprobs entry: %s", pythonRepr(entry, true))}
		}
		// First occurrence wins: a repeated token cannot carry two logprobs.
		if _, seen := top[token]; !seen {
			top[token] = logprob
		}
	}
	return top, nil
}

// chatLogprobEntry reads one `{token, logprob}` entry, or reports that it is not
// one.
//
// A bool is refused explicitly: Python's `isinstance(True, (int, float))` is
// true, so a client that trusted the type would read `true` as 1.0.
func chatLogprobEntry(entry any) (string, float64, bool) {
	object, ok := entry.(Object)
	if !ok {
		return "", 0, false
	}
	tokenValue, _ := object.Get("token")
	token, ok := tokenValue.(string)
	if !ok {
		return "", 0, false
	}
	logprobValue, _ := object.Get("logprob")
	number, ok := logprobValue.(json.Number)
	if !ok {
		return "", 0, false
	}
	logprob, err := strconv.ParseFloat(string(number), 64)
	if err != nil || math.IsInf(logprob, 0) || math.IsNaN(logprob) {
		return "", 0, false
	}
	return token, logprob, true
}

// firstChatChoice is the choice a chat response must carry, or an error naming
// what the response had instead.
//
// The message is the Python client's `_first_choice` verbatim, with no suffix
// naming the read: both chat reads send the same request shape, and the Python
// client does not distinguish them here either.
func firstChatChoice(response chatResponse) (chatChoice, error) {
	if len(response.Choices) == 0 {
		return chatChoice{}, &ReadoutError{Message: "the chat response carries no choices"}
	}
	return response.Choices[0], nil
}

type completionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type completionChoice struct {
	// Text is a pointer so a response that omits `text`, or sends null, is
	// distinguishable from one that sends the empty string. The Python client
	// reads the same field with `.get("text")`, which is None in that case, and
	// `Decision.Raw` carries the difference.
	Text *string `json:"text"`
	// TokenIDs is a pointer so a response that omits the field, or sends null,
	// is distinguishable from one that sends an empty list. The thinking read
	// needs the tokens themselves and errors on the former, but an empty list is
	// a real answer: the model produced no thought before closing it.
	TokenIDs *TokenIDList `json:"token_ids"`
	Logprobs *struct {
		// One map per generated position; this client reads position 0. With
		// `return_tokens_as_token_ids` the keys are `token_id:<id>`, otherwise
		// the decoded token text. A null value means the server did not report
		// that entry.
		TopLogprobs []map[string]*float64 `json:"top_logprobs"`
	} `json:"logprobs"`
	// PromptLogprobs has one entry per prompt token, and the first is null. Each
	// entry maps the token's key to its logprob and rank; `prompt_logprobs: 0`
	// returns only the prompt's own token at each position.
	PromptLogprobs []map[string]*promptLogprobEntry `json:"prompt_logprobs"`
}

type promptLogprobEntry struct {
	Logprob *float64 `json:"logprob"`
	Rank    *int     `json:"rank"`
}

// TokenIDList is a `token_ids` array in which every element has to be a JSON
// integer.
//
// Go's decoder accepts `null` into an int element and leaves the zero value, so
// a plain `[]int` turns `[null, 101]` into a thought holding token 0 and marked
// closed -- an invented token the server never produced, continued as if it had.
// The Python client rejects any element that is not an integer, and so does
// this.
type TokenIDList []int

func (l *TokenIDList) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	ids := make([]int, len(raw))
	for i, item := range raw {
		var id *int
		if err := json.Unmarshal(item, &id); err != nil || id == nil {
			return fmt.Errorf("token_ids[%d] is not an integer: %s", i, item)
		}
		ids[i] = *id
	}
	*l = ids
	return nil
}

// topLogprobs is the first generated position's logprobs, or an empty map when
// the response carried none.
func (c completionChoice) topLogprobs() map[string]*float64 {
	if c.Logprobs == nil || len(c.Logprobs.TopLogprobs) == 0 || c.Logprobs.TopLogprobs[0] == nil {
		return map[string]*float64{}
	}
	return c.Logprobs.TopLogprobs[0]
}

// lastPromptLogprobEntry is the entry for the last prompt token, which is the
// answer boundary: the fallback appends the code to the prompt, so the code's
// logprob is the prompt's own last token.
func (c completionChoice) lastPromptLogprobEntry() map[string]*promptLogprobEntry {
	if len(c.PromptLogprobs) == 0 {
		return nil
	}
	return c.PromptLogprobs[len(c.PromptLogprobs)-1]
}

// postJSON sends one request to the endpoint and decodes the JSON object it
// returns into `into`.
//
// A transport failure, an error status, a non-JSON body and a JSON value that is
// not an object are all ReadoutError or ReadoutHTTPError, so a caller never has
// to tell a network problem from a protocol one by the concrete error type.
func postJSON(ctx context.Context, client *http.Client, url string, headers http.Header, body any, into any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return &ReadoutError{Message: "the request body is not JSON", Err: err}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return &ReadoutError{Message: fmt.Sprintf("building the request to %s", url), Err: err}
	}
	return doJSON(client, request, headers, into)
}

// getJSON sends one GET and decodes the JSON object it returns.
func getJSON(ctx context.Context, client *http.Client, url string, headers http.Header, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &ReadoutError{Message: fmt.Sprintf("building the request to %s", url), Err: err}
	}
	return doJSON(client, request, headers, into)
}

func doJSON(client *http.Client, request *http.Request, headers http.Header, into any) error {
	return doJSONLimit(client, request, headers, into, maxResponseBytes)
}

// doJSONLimit is doJSON with the response cap given, so a test can exercise the
// truncation without moving 64 MB over a socket.
func doJSONLimit(client *http.Client, request *http.Request, headers http.Header, into any,
	limit int64) error {
	url := request.URL.String()
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return &ReadoutError{Message: fmt.Sprintf("request to %s failed", url), Err: err}
	}
	defer response.Body.Close()
	// The body is capped so a server that streams an error page cannot exhaust
	// memory here; the Python client keeps the first 400 characters of it.
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit))
	if err != nil {
		return &ReadoutError{Message: fmt.Sprintf("reading the response from %s", url), Err: err}
	}
	if response.StatusCode >= 400 {
		// The first 400 characters, as the Python client keeps them: it slices
		// the decoded body, so this slices runes rather than bytes and does not
		// split one. The cap is what stops an error page from filling memory.
		detail := string(raw)
		if runes := []rune(detail); len(runes) > 400 {
			detail = string(runes[:400])
		}
		return &ReadoutHTTPError{
			ReadoutError: &ReadoutError{Message: fmt.Sprintf(
				"%s returned HTTP %d: %s", url, response.StatusCode, detail)},
			Status: response.StatusCode,
			Detail: detail,
		}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return &ReadoutError{
			Message: fmt.Sprintf("%s did not return a JSON object", url),
			Err:     err,
		}
	}
	return nil
}

// maxResponseBytes caps one response body, so a server that streams an error
// page cannot exhaust memory here. It has to clear the largest honest response,
// which is not one position of top-logprobs but the `prompt_logprobs: 0`
// fallback: that returns one entry per prompt token, so a very long prompt on a
// server without `logprob_token_ids` is the big case. At roughly 40 bytes an
// entry, 64 MB covers a prompt of well over a million tokens, and the pinned
// DE-2 prompts are about 8,500. The Python client reads whatever arrives.
const maxResponseBytes = 64 << 20

// firstChoice is the choice a completion must carry, or an error naming what the
// response had instead.
func firstChoice(response completionResponse, what string) (completionChoice, error) {
	if len(response.Choices) == 0 {
		return completionChoice{}, &ReadoutError{Message: fmt.Sprintf(
			"the server returned no choices for %s", what)}
	}
	return response.Choices[0], nil
}
