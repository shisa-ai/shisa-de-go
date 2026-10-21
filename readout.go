package shisade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shisa-ai/shisa-de-go/tokenizer"
)

// The measured scaffold. These strings are part of the readout identity, not
// preferences: changing one changes the answers, so it is a readout version
// bump and a readout-page update in the same commit.

// DirectSystem is the system line the measured scaffold uses.
const DirectSystem = "Apply the supplied criterion to the supplied evidence. " +
	"Choose exactly one listed option. Respond with only its uppercase letter, " +
	"with no explanation or reasoning."

// ThinkSystem is the system line for the thinking read: the direct line with the
// ban on reasoning replaced by a request for it.
const ThinkSystem = "Apply the supplied criterion to the supplied evidence. " +
	"Choose exactly one listed option. Reason through the question step by step " +
	"before you answer. When your reasoning is complete, respond with only its " +
	"uppercase letter."

// InputRepeat is the line between the two copies of the user turn in the
// repeated read: two newlines, the sentence, one newline.
const InputRepeat = "\n\nRead the same input again before answering:\n"

// AnswerPrefix is the generation-prompt terminator the checkpoint's chat
// template ends on, so the answer code is the token after it. The image readout
// cannot render the prompt itself, so it confirms the answer boundary by
// requiring the server's last prompt token to be this one.
const AnswerPrefix = "<channel|>"

// Readout identities. A readout version identifies one family's
// answer-producing contract, not the client build or the contents of a file.
const (
	// DE1ReadoutVersion is the DE-1 contract. This client does not implement it.
	DE1ReadoutVersion = "de1-letter-slots-v3"
	// DE2ReadoutVersion is the DE-2 contract, specified in docs/READOUT-DE2.md.
	DE2ReadoutVersion = "de2-codebook-v1"
)

// ReadoutVersionFor is the one place the current readout version per family
// lives. Do not define a copy elsewhere.
func ReadoutVersionFor(family Family) string {
	switch family {
	case FamilyDE1:
		return DE1ReadoutVersion
	case FamilyDE2:
		return DE2ReadoutVersion
	}
	return ""
}

// calibrationCompatible lists, for each current readout version, the earlier
// versions whose direct text read is answer-identical to it, so a calibration
// fitted against one of them still describes the answers this client returns. A
// version bump that changes the direct text read must not list its predecessors
// here.
//
// The DE-1 entry mirrors the shared source of truth. It is inert here: this
// port implements DE-2 only, and RequireDE2 refuses a DE-1 model before a
// calibration can be selected.
var calibrationCompatible = map[string][]string{
	DE1ReadoutVersion: {"de1-letter-slots-v1", "de1-letter-slots-v2"},
	DE2ReadoutVersion: {},
}

// CalibrationCompatibleWith reports whether a calibration fitted against one
// readout version applies under another.
func CalibrationCompatibleWith(fitted, current string) bool {
	if fitted == current {
		return true
	}
	for _, version := range calibrationCompatible[current] {
		if version == fitted {
			return true
		}
	}
	return false
}

// Softmax turns logprobs into a distribution over the same keys.
//
// Only the keys given take part: tokens outside the option codes are discarded
// even when they outrank a valid option.
//
// The sum runs in a fixed order -- shortest key first, then lexicographic,
// which is the codebook's own order for a codebook-ordered prefix -- so the
// result is reproducible. A Go map iterates in a randomized order, and summing
// floats in a different order changes the last bits.
//
// The order is the one the Python client sums in when the codes are given in
// codebook order, which is how `ReadCodes` gives them: it rebuilds `logprobs`
// in option order before normalizing. It is not the same sum in general --
// Python preserves whatever order the caller's slot list had, and this sorts --
// and it is not bit-identical on CPython 3.12+, where `sum()` is
// Neumaier-compensated. The difference is a relative 1e-16 or so, which moves a
// probability by far less than the 0.7 gate's distance from any measured value,
// but it is not zero and it is not a guarantee about the gate.
//
// The returned map carries no order, so a caller that needs the option order
// keeps its own list of codes.
func Softmax(logprobs map[string]float64) (map[string]float64, error) {
	if len(logprobs) == 0 {
		return nil, &ReadoutError{Message: "no code logprobs to normalize"}
	}
	keys := make([]string, 0, len(logprobs))
	for key := range logprobs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) < len(keys[j])
		}
		return keys[i] < keys[j]
	})
	peak := math.Inf(-1)
	for _, key := range keys {
		if value := logprobs[key]; value > peak {
			peak = value
		}
	}
	weights := make(map[string]float64, len(logprobs))
	total := 0.0
	for _, key := range keys {
		weight := math.Exp(logprobs[key] - peak)
		weights[key] = weight
		total += weight
	}
	probabilities := make(map[string]float64, len(logprobs))
	for _, key := range keys {
		probabilities[key] = weights[key] / total
	}
	return probabilities, nil
}

// CandidateIDLimit is the most token ids vLLM accepts in one
// `logprob_token_ids` list. A question with more options sends the same prompt
// once per block of this many, in codebook order; the logprobs are the server's
// raw ones over the whole vocabulary, so the blocks combine under one softmax.
const CandidateIDLimit = 128

// DefaultMaxLogprobs is the `logprobs` value a code read sends. It only has to
// be set beside `logprob_token_ids` for vLLM to accept the request; the named
// ids come back whatever their rank.
const DefaultMaxLogprobs = 20

// Tokenizer is the part of a tokenizer the readout needs: text to ids, and ids
// back to text for the single-token checks. *tokenizer.Tokenizer satisfies it.
type Tokenizer interface {
	Encode(text string) []int
	Decode(ids []int) string
}

// Slot is one answer position: the code, its token id, and the text the code is
// written as. The Python client calls the field `letter`, because DE-1 and DE-2
// share the type; here it is always a codebook code.
type Slot struct {
	Code    string
	TokenID int
	Text    string
}

// CodeRead is what one code read cost and returned. It is the DE-2 form of the
// Python client's LetterRead.
type CodeRead struct {
	// ServedModel is the model id the server reported for this read, or "" when it
	// reported none. It is the only evidence a response carries about which
	// checkpoint answered, so a caller that holds per-checkpoint records reads it
	// before deciding which one applies.
	ServedModel string
	// Logprobs is the logprob of every option code, in option order.
	Logprobs map[string]float64
	// Probabilities is the softmax over those codes only: tokens outside the
	// option list are discarded even when they outrank a valid option.
	Probabilities map[string]float64
	// Requests is the HTTP requests the read took: one per block of 128 codes,
	// plus one per code that needed the fallback. A request the server rejected
	// before answering is not counted, matching the Python client.
	Requests int
	// PromptTokens is the sum of the server's `usage.prompt_tokens` over those
	// requests, so a two-block read counts the prompt twice.
	PromptTokens int
	// MissingFromTop lists the codes the top-k did not carry and the fallback
	// had to fetch, in option order.
	MissingFromTop []string
	// Ranks is the rank the fallback reported for a code, when it reported one.
	Ranks map[string]int
	// Sampled is the first request's `choices[0].text`, or nil when the server
	// sent no `text`. The Python client's field is `str | None` for the same
	// reason.
	Sampled *string
	// Prompt is the rendered text, only when the read was asked for debug
	// detail. A read after a thought has no text prompt to keep.
	Prompt string
	// TopLogprobs is every entry of the first generated position's top-k, only
	// with debug. A nil value is a null the server returned.
	TopLogprobs map[string]*float64
	// Messages is the multimodal messages an image read sent, only when the read
	// was asked for debug detail. The server renders that prompt, so there is no
	// local Prompt to keep; the messages carry the image URL or its bytes.
	Messages []Object
	// SystemRender is image reads only: whether the server rendered the system
	// turn the way the text scaffold does. Empty on every other read, which is
	// the Python client's None.
	SystemRender string
}

// Thought is one bounded thought: the prompt it followed and the tokens
// generated.
type Thought struct {
	// PromptIDs is the thinking prompt's tokens.
	PromptIDs []int
	// TokenIDs is the thought, with the closing token removed when the model
	// produced one.
	TokenIDs []int
	// Closed reports whether the model closed the thought itself. A thought the
	// budget cut is not closed, and the caller closes it.
	Closed bool
	// PromptTokens is the server's `usage.prompt_tokens` for the request.
	PromptTokens int
	// Text is the generated text, only with debug, or nil when the server sent
	// no `text`.
	Text *string
}

// renderedPrompt is a rendered prompt in either of the two forms the code read
// accepts: the text, or token ids for the read that follows a thought, where the
// thought has to be continued token for token.
type renderedPrompt struct {
	text string
	ids  []int
}

func textPrompt(text string) renderedPrompt { return renderedPrompt{text: text} }

func idPrompt(ids []int) renderedPrompt {
	// A copy, because the whole read sends this list: the Python client does
	// `list(prompt)` for the id form, so a caller that reuses its buffer while a
	// read is in flight cannot change the prompt the later blocks send.
	copied := make([]int, len(ids))
	copy(copied, ids)
	return renderedPrompt{ids: copied}
}

// isText reports which form this is. The id form is the one with a non-nil
// slice, so an empty prompt is still unambiguously the id form.
func (p renderedPrompt) isText() bool { return p.ids == nil }

// wire is the JSON value the request carries: a string, or a list of ids.
func (p renderedPrompt) wire() any {
	if p.isText() {
		return p.text
	}
	return p.ids
}

func (p renderedPrompt) tokenIDs(tok Tokenizer) []int {
	if p.isText() {
		return tok.Encode(p.text)
	}
	return p.ids
}

// appended is the prompt the fallback sends: the code as text after a text
// prompt, or the code's token id after a token-id prompt.
func (p renderedPrompt) appended(slot Slot) renderedPrompt {
	if p.isText() {
		return renderedPrompt{text: p.text + slot.Code}
	}
	ids := make([]int, len(p.ids), len(p.ids)+1)
	copy(ids, p.ids)
	return renderedPrompt{ids: append(ids, slot.TokenID)}
}

// ReadoutConfig configures a Readout.
type ReadoutConfig struct {
	// BaseURL is the endpoint root, with or without a trailing slash. The
	// requests go to BaseURL + "/v1/completions".
	BaseURL string
	// Model is the served model id sent in every request.
	Model string
	// Tokenizer is the tokenizer source the client tokenizes with, usually the
	// checkpoint the server serves. Empty means Model.
	Tokenizer string
	// TokenizerRevision pins the snapshot when the tokenizer is fetched.
	TokenizerRevision string
	// LocalFilesOnly refuses to fetch a tokenizer from the hub. A source that is
	// neither a readable file nor already cached is an error rather than a
	// download, as the Python client's `local_files_only=True` does.
	LocalFilesOnly bool
	// Family declares which DE contract to read through, overriding what the
	// model id and the tokenizer source say. Empty means "not declared", so
	// resolution falls through to those. This client implements DE-2 only, so a
	// declaration that is not DE-2 is refused.
	Family string
	// Timeout bounds one request. Zero means DefaultTimeout.
	Timeout time.Duration
	// SystemPrompt is the direct read's system line. Empty means DirectSystem.
	SystemPrompt string
	// MaxLogprobs is the `logprobs` value a code read sends. Zero means
	// DefaultMaxLogprobs.
	MaxLogprobs int
	// APIKey is sent as a bearer token when non-empty.
	APIKey string
	// HTTPClient is the transport, which is what the offline tests replace.
	// Nil builds one from Timeout.
	HTTPClient *http.Client
}

// DefaultTimeout bounds one request when the config does not.
const DefaultTimeout = 120 * time.Second

// Readout reads the DE-2 codebook distribution at the end of a rendered prompt.
//
// It is safe for concurrent use. The tokenizer is loaded on first use and, like
// the Python client, a failed load is retried on the next call rather than
// remembered.
//
// NewReadout is the only supported constructor: the zero value has no endpoint
// and no HTTP client, and every method that reads or spends a request returns an
// error rather than pretending to read something. `Render` and `ResolvedFamily`
// need neither, so they still answer on a zero value.
type Readout struct {
	baseURL           string
	model             string
	tokenizerSource   string
	tokenizerRevision string
	localFilesOnly    bool
	systemPrompt      string
	maxLogprobs       int
	client            *http.Client
	headers           http.Header
	family            Family
	familySource      FamilySource

	mu sync.Mutex
	// tokenizer, slots and boundaryChecked are the lazily built state, all
	// under mu so concurrent reads can share them.
	tokenizer Tokenizer
	slots     map[string]Slot
	// boundaryChecked records, per code, the token id whose boundary was
	// approved. The id is part of the record: an approval is a statement about
	// one code *and* one token id, and a caller can hand `ReadCodes` a Slot
	// whose Code is approved and whose TokenID is not.
	boundaryChecked map[string]int
	// tokenizerGeneration counts tokenizer swaps, so a read that captured a
	// tokenizer can tell whether the readout still holds it. A read that lost
	// the race re-checks its codes without caching them, rather than leaving
	// approvals for a tokenizer it no longer has.
	tokenizerGeneration uint64
	// candidateIDs is cleared the first time a server rejects
	// `logprob_token_ids`, after which codes are read from the top-k with the
	// per-code fallback.
	candidateIDs bool
}

// NewReadout builds a readout. Nothing is fetched until the first read.
//
// The DE contract is resolved here, before anything is loaded or sent, and
// anything but DE-2 is refused: this client implements one contract, and
// reading a checkpoint through the other one returns answers nobody has
// measured. The Python client makes the same decision in `DecisionModel`, one
// layer up; here it is in the readout too, because a caller can hold a Readout
// directly and it is the thing that spends requests.
func NewReadout(config ReadoutConfig) (*Readout, error) {
	if strings.TrimSpace(config.BaseURL) == "" {
		return nil, &ReadoutError{Message: "the readout needs a base URL"}
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, &ReadoutError{Message: "the readout needs a model id"}
	}
	familySource, err := RequireDE2(config.Model, config.Tokenizer, config.Family)
	if err != nil {
		return nil, err
	}
	source := config.Tokenizer
	if source == "" {
		source = config.Model
	}
	if config.TokenizerRevision != "" {
		source += "@" + config.TokenizerRevision
	}
	client := config.HTTPClient
	if client == nil {
		timeout := config.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		client = &http.Client{Timeout: timeout}
	}
	maxLogprobs := config.MaxLogprobs
	if maxLogprobs <= 0 {
		maxLogprobs = DefaultMaxLogprobs
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	if config.APIKey != "" {
		headers.Set("Authorization", "Bearer "+config.APIKey)
	}
	return &Readout{
		baseURL:           strings.TrimRight(config.BaseURL, "/"),
		model:             config.Model,
		tokenizerSource:   source,
		tokenizerRevision: config.TokenizerRevision,
		localFilesOnly:    config.LocalFilesOnly,
		systemPrompt:      config.SystemPrompt,
		maxLogprobs:       maxLogprobs,
		client:            client,
		headers:           headers,
		family:            FamilyDE2,
		familySource:      familySource,
		slots:             map[string]Slot{},
		boundaryChecked:   map[string]int{},
		candidateIDs:      true,
	}, nil
}

// ResolvedFamily is the contract this readout reads through and where that came
// from. It is always DE-2; the source says whether the model id, the tokenizer
// source or a declaration named it, or whether it was assumed.
func (r *Readout) ResolvedFamily() (Family, FamilySource) {
	return r.family, r.familySource
}

// SetTokenizer installs a tokenizer instead of loading the source's. The offline
// tests use it; a caller normally lets the readout load on first use.
//
// Every answer id and boundary approval resolved from the previous tokenizer is
// discarded, because they describe that tokenizer: keeping them would let a
// read tokenize the prompt with one and validate the codes against another. A
// read already in flight keeps the tokenizer it started with and does not
// approve anything for the new one, so it stays consistent either way.
func (r *Readout) SetTokenizer(t Tokenizer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokenizer = t
	r.slots = map[string]Slot{}
	r.boundaryChecked = map[string]int{}
	r.tokenizerGeneration++
}

// ensureTokenizerLocked returns the tokenizer, loading it once. The caller holds
// mu, so concurrent first calls serialize on one load, and a failed load leaves
// nothing behind to be retried.
func (r *Readout) ensureTokenizerLocked() (Tokenizer, error) {
	if r.tokenizer != nil {
		return r.tokenizer, nil
	}
	loaded, err := tokenizer.LoadWith(r.tokenizerSource, tokenizer.ResolveOptions{
		LocalFilesOnly: r.localFilesOnly,
	})
	if err != nil {
		return nil, &ReadoutError{
			Message: fmt.Sprintf("loading the tokenizer %q", r.tokenizerSource), Err: err}
	}
	r.tokenizer = loaded
	return r.tokenizer, nil
}

func (r *Readout) ensureTokenizer() (Tokenizer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensureTokenizerLocked()
}

// tokenizerSnapshot returns the tokenizer and the generation it belongs to, so a
// read can use one tokenizer throughout and know whether it is still the
// readout's.
// Close releases the readout's idle connections.
//
// The Python client's `close` closes its httpx client. Go's http.Client is
// shared and reusable, so this drops only the pooled connections; the readout
// stays usable and a later read opens new ones.
func (r *Readout) Close() {
	if r == nil || r.client == nil {
		return
	}
	r.client.CloseIdleConnections()
}

// EnsureTokenizer returns the readout's tokenizer, loading it on first use.
//
// The Python client exposes the same method. It is here for callers that need to
// tokenize a prompt themselves, such as a health check that verifies the answer
// boundary before spending a request.
func (r *Readout) EnsureTokenizer() (Tokenizer, error) { return r.ensureTokenizer() }

// MaxLogprobs is the `logprobs` value a code read sends, resolved from the
// config. The command line prints it and puts it in the request body it shows.
func (r *Readout) MaxLogprobs() int { return r.maxLogprobs }

func (r *Readout) tokenizerSnapshot() (Tokenizer, uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tok, err := r.ensureTokenizerLocked()
	if err != nil {
		return nil, 0, err
	}
	return tok, r.tokenizerGeneration, nil
}

// Slot resolves one answer code to its token.
//
// A code must be exactly one token for the readout to work, and the ids are
// checkpoint-specific, so they are resolved from the tokenizer the client
// tokenizes with rather than read from the codebook's audit.
func (r *Readout) Slot(code string) (Slot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.slotLocked(code)
}

func (r *Readout) slotLocked(code string) (Slot, error) {
	if slot, ok := r.slots[code]; ok {
		return slot, nil
	}
	tok, err := r.ensureTokenizerLocked()
	if err != nil {
		return Slot{}, err
	}
	slot, err := slotFor(tok, code, r.tokenizerSource)
	if err != nil {
		return Slot{}, err
	}
	r.slots[code] = slot
	return slot, nil
}

// slotFor resolves one code against one tokenizer, so a read that already holds
// a tokenizer keeps using it rather than picking up a replacement part way
// through.
func slotFor(tok Tokenizer, code, source string) (Slot, error) {
	ids := tok.Encode(code)
	if len(ids) != 1 || tok.Decode(ids) != code {
		// Python's shared slot resolver says "answer letter ... read with letter
		// slots"; this is the DE-2 rewording of it, because the contract's slots
		// are codes. The quoting is still Python's `!r`.
		return Slot{}, &ReadoutError{Message: fmt.Sprintf(
			"answer code %s is not one token for tokenizer %s; this checkpoint cannot be "+
				"read with the codebook", pythonQuote(code), pythonQuote(source))}
	}
	return Slot{Code: code, TokenID: ids[0], Text: code}, nil
}

// codeSlotsFor is [Readout.CodeSlots] against one tokenizer.
//
// A slot resolved from a tokenizer the readout has since replaced is still the
// code it names, so it is used; it is only written to the cache when the
// tokenizer is still the readout's, which is the rule [Readout.checkCodeBoundaryFor]
// follows. That is what keeps a replacement from leaving the cache holding ids
// from two tokenizers.
func (r *Readout) codeSlotsFor(tok Tokenizer, generation uint64, count int) ([]Slot, error) {
	codes, err := CodesFor(count)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.tokenizerGeneration == generation
	slots := make([]Slot, len(codes))
	for i, code := range codes {
		if current {
			if slot, ok := r.slots[code]; ok {
				slots[i] = slot
				continue
			}
		}
		slot, err := slotFor(tok, code, r.tokenizerSource)
		if err != nil {
			return nil, err
		}
		if current {
			r.slots[code] = slot
		}
		slots[i] = slot
	}
	return slots, nil
}

// CodeSlots resolves the first count codes of the codebook, in codebook order.
func (r *Readout) CodeSlots(count int) ([]Slot, error) {
	codes, err := CodesFor(count)
	if err != nil {
		return nil, err
	}
	// One lock for the whole list, so a concurrent SetTokenizer cannot leave
	// half the slots resolved from one tokenizer and half from another.
	r.mu.Lock()
	defer r.mu.Unlock()
	slots := make([]Slot, len(codes))
	for i, code := range codes {
		slot, err := r.slotLocked(code)
		if err != nil {
			return nil, err
		}
		slots[i] = slot
	}
	return slots, nil
}

// AnswerPrefixID is the token id of the generation-prompt terminator, resolved
// locally. It has to be exactly one token for the boundary check to mean
// anything, which is the same requirement the answer codes carry.
func (r *Readout) AnswerPrefixID() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tok, err := r.ensureTokenizerLocked()
	if err != nil {
		return 0, err
	}
	return r.answerPrefixIDFor(tok)
}

// answerPrefixIDFor resolves the terminator against one tokenizer, so a read
// that already holds a tokenizer keeps using it rather than picking up a
// replacement part way through.
func (r *Readout) answerPrefixIDFor(tok Tokenizer) (int, error) {
	ids := tok.Encode(AnswerPrefix)
	if len(ids) != 1 || tok.Decode(ids) != AnswerPrefix {
		return 0, &ReadoutError{Message: fmt.Sprintf(
			"the generation-prompt terminator %s is not one token for tokenizer %s, so the "+
				"image readout cannot confirm the answer boundary with this tokenizer",
			pythonQuote(AnswerPrefix), pythonQuote(r.tokenizerSource))}
	}
	return ids[0], nil
}

// CheckCodeBoundary verifies that the prompt ends where the answer is read and
// that each code appended to that terminator adds exactly its own token.
//
// The terminator is a control token, so what follows it tokenizes the same
// whatever precedes it. That is what lets the per-code half be checked once per
// code instead of once per code per question: a 256-option question would
// otherwise re-tokenize its whole prompt 256 times.
func (r *Readout) CheckCodeBoundary(promptIDs []int, slots []Slot) error {
	tok, generation, err := r.tokenizerSnapshot()
	if err != nil {
		return err
	}
	return r.checkCodeBoundaryFor(tok, generation, promptIDs, slots)
}

// checkCodeBoundaryFor checks against one tokenizer. It takes the lock for the
// approval cache but not for the tokenizer, which the caller already holds, so a
// whole read stays on one tokenizer even if another goroutine swaps it.
//
// The cache is only read and only written when the readout still holds the
// tokenizer the caller passed. A read that lost a SetTokenizer race re-checks
// its codes and caches nothing, so it cannot leave approvals that a later read
// under the new tokenizer would trust.
func (r *Readout) checkCodeBoundaryFor(tok Tokenizer, generation uint64, promptIDs []int,
	slots []Slot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.tokenizerGeneration == generation
	prefixID, err := r.answerPrefixIDFor(tok)
	if err != nil {
		return err
	}
	if len(promptIDs) == 0 || promptIDs[len(promptIDs)-1] != prefixID {
		return &ReadoutError{Message: fmt.Sprintf(
			"the prompt does not end on %s (token %d), so the answer is not at the first "+
				"generated position. The tokenizer (%s) does not match the served model (%s).",
			pythonQuote(AnswerPrefix), prefixID,
			pythonQuote(r.tokenizerSource), pythonQuote(r.model))}
	}
	for _, slot := range slots {
		if current {
			if approved, ok := r.boundaryChecked[slot.Code]; ok && approved == slot.TokenID {
				continue
			}
		}
		combined := tok.Encode(AnswerPrefix + slot.Code)
		if len(combined) != 2 || combined[0] != prefixID || combined[1] != slot.TokenID {
			return &ReadoutError{Message: fmt.Sprintf(
				"the answer boundary moves for %s: appended to %s it does not tokenize as "+
					"token %d. The tokenizer (%s) does not match the served model (%s).",
				pythonQuote(slot.Code), pythonQuote(AnswerPrefix), slot.TokenID,
				pythonQuote(r.tokenizerSource), pythonQuote(r.model))}
		}
		if current {
			r.boundaryChecked[slot.Code] = slot.TokenID
		}
	}
	return nil
}

// checkPromptTokens requires the server to have counted the prompt the way the
// client tokenized it. A server that reports nothing is tolerated, because not
// every OpenAI-compatible server fills `usage` in.
func (r *Readout) checkPromptTokens(response completionResponse, expected int) (int, error) {
	reported := response.Usage.PromptTokens
	if reported != 0 && reported != expected {
		return reported, &ReadoutError{Message: fmt.Sprintf(
			"the server counted %d prompt tokens where the client tokenized %d: the "+
				"tokenizer (%s) does not match the served model (%s)",
			reported, expected, pythonQuote(r.tokenizerSource), pythonQuote(r.model))}
	}
	return reported, nil
}

// Render renders one question against one state into the prompt the model sees,
// with this readout's system line.
//
// `options.SystemPrompt` means the readout's configured line when it is empty,
// which is not what the package-level Render means by the same field: there,
// empty is DirectSystem. A readout built with a custom line therefore cannot ask
// for DirectSystem through this method; call the package function for that.
func (r *Readout) Render(state any, question Question, options RenderOptions) (string, error) {
	if options.SystemPrompt == "" {
		options.SystemPrompt = r.systemPrompt
	}
	return Render(state, question, options)
}

// ReadCodes reads the distribution over slots at the end of a text prompt, by
// token id.
//
// One request names every candidate in `logprob_token_ids`, so each code's
// logprob comes back whatever its rank and no code needs the fallback. A server
// without that field is read from its top-k with one fallback request per
// missing code, and is not asked for the field again.
func (r *Readout) ReadCodes(ctx context.Context, prompt string, slots []Slot, debug bool) (CodeRead, error) {
	return r.readCodes(ctx, textPrompt(prompt), slots, debug)
}

// ReadCodesAfterThought is the read that follows a thought: the same code read,
// over the thinking prompt's tokens continued by the thought's tokens and one
// closing terminator. The thought has to be continued token for token, which is
// why this form takes ids rather than text.
func (r *Readout) ReadCodesAfterThought(ctx context.Context, promptIDs []int, slots []Slot, debug bool) (CodeRead, error) {
	return r.readCodes(ctx, idPrompt(promptIDs), slots, debug)
}

func (r *Readout) readCodes(ctx context.Context, prompt renderedPrompt, slots []Slot, debug bool) (CodeRead, error) {
	tok, generation, err := r.tokenizerSnapshot()
	if err != nil {
		return CodeRead{}, err
	}
	promptIDs := prompt.tokenIDs(tok)
	if err := r.checkCodeBoundaryFor(tok, generation, promptIDs, slots); err != nil {
		return CodeRead{}, err
	}

	result := CodeRead{
		Logprobs:       map[string]float64{},
		Ranks:          map[string]int{},
		MissingFromTop: []string{},
		TopLogprobs:    map[string]*float64{},
	}
	// Every request in the read names the same model, so the first response that
	// reports an id settles it for the whole read.
	noteServed := func(served string) {
		if result.ServedModel == "" && served != "" {
			result.ServedModel = served
		}
	}
	if debug && prompt.isText() {
		result.Prompt = prompt.text
	}
	for start := 0; start < len(slots); {
		useCandidateIDs := r.usesCandidateIDs()
		block := slots
		if useCandidateIDs {
			// vLLM caps the list at 128 ids, so a wider question reads its
			// candidates in consecutive blocks over the same prompt.
			block = slots[start:]
			if len(block) > CandidateIDLimit {
				block = block[:CandidateIDLimit]
			}
		}
		logprobs := r.maxLogprobs
		withTokenIDs := true
		body := completionRequest{
			Model:                  r.model,
			Prompt:                 prompt.wire(),
			MaxTokens:              1,
			Temperature:            0,
			Logprobs:               &logprobs,
			ReturnTokensAsTokenIDs: &withTokenIDs,
		}
		if useCandidateIDs {
			body.LogprobTokenIDs = make([]int, len(block))
			for i, slot := range block {
				body.LogprobTokenIDs[i] = slot.TokenID
			}
		}
		var response completionResponse
		err := r.completions(ctx, body, &response)
		if err != nil {
			var httpError *ReadoutHTTPError
			if useCandidateIDs && errors.As(err, &httpError) &&
				(httpError.Status == 400 || httpError.Status == 422) &&
				strings.Contains(httpError.Detail, "logprob_token_ids") {
				// The server does not know the field. Stop sending it, and ask
				// the same block again without it.
				r.disableCandidateIDs()
				continue
			}
			return CodeRead{}, err
		}
		noteServed(response.Model)
		choice, err := firstChoice(response, "the code read")
		if err != nil {
			return CodeRead{}, err
		}
		top := choice.topLogprobs()
		result.Requests++
		counted, err := r.checkPromptTokens(response, len(promptIDs))
		if err != nil {
			return CodeRead{}, err
		}
		result.PromptTokens += counted
		if start == 0 {
			result.Sampled = choice.Text
		}
		if debug {
			for key, value := range top {
				result.TopLogprobs[key] = value
			}
		}
		for _, slot := range block {
			// The Python client looks the id key up with a default of the text
			// key, so an id key that is present but null does not fall through
			// to the text key. A server that ignores `return_tokens_as_token_ids`
			// keys the map by decoded text and has no id key at all.
			value, present := top["token_id:"+strconv.Itoa(slot.TokenID)]
			if !present {
				value = top[slot.Text]
			}
			if value != nil {
				result.Logprobs[slot.Code] = *value
			}
		}
		start += len(block)
	}

	var missing []Slot
	for _, slot := range slots {
		if _, ok := result.Logprobs[slot.Code]; !ok {
			missing = append(missing, slot)
		}
	}
	for _, slot := range missing {
		result.MissingFromTop = append(result.MissingFromTop, slot.Code)
	}
	for _, slot := range missing {
		logprob, rank, promptTokens, err := r.letterLogprob(ctx, prompt, slot)
		if err != nil {
			return CodeRead{}, err
		}
		result.Requests++
		result.PromptTokens += promptTokens
		if logprob == nil {
			return CodeRead{}, &ReadoutError{Message: fmt.Sprintf(
				"code %s was not returned for its token id and the prompt_logprobs fallback "+
					"returned nothing for it", pythonQuote(slot.Code))}
		}
		result.Logprobs[slot.Code] = *logprob
		if rank != nil {
			result.Ranks[slot.Code] = *rank
		}
	}

	// Option order, not arrival order: the fallback fills gaps out of sequence.
	ordered := make(map[string]float64, len(slots))
	for _, slot := range slots {
		ordered[slot.Code] = result.Logprobs[slot.Code]
	}
	result.Logprobs = ordered
	probabilities, err := Softmax(result.Logprobs)
	if err != nil {
		return CodeRead{}, err
	}
	result.Probabilities = probabilities
	return result, nil
}

// letterLogprob is one fallback request: read a single code's logprob at the
// answer boundary, by appending the code to the prompt and asking for the
// prompt's own last-token logprob.
func (r *Readout) letterLogprob(ctx context.Context, prompt renderedPrompt, slot Slot) (*float64, *int, int, error) {
	promptLogprobs := 0
	body := completionRequest{
		Model:          r.model,
		Prompt:         prompt.appended(slot).wire(),
		MaxTokens:      1,
		Temperature:    0,
		PromptLogprobs: &promptLogprobs,
	}
	var response completionResponse
	if err := r.completions(ctx, body, &response); err != nil {
		return nil, nil, 0, err
	}
	choice, err := firstChoice(response, "the code fallback")
	if err != nil {
		return nil, nil, 0, err
	}
	promptTokens := response.Usage.PromptTokens
	entries := choice.lastPromptLogprobEntry()
	if len(entries) == 0 {
		return nil, nil, promptTokens, nil
	}
	if len(entries) > 1 {
		// `prompt_logprobs: 0` returns the prompt's own token and nothing else,
		// so more than one entry is a server this client cannot read that way.
		// Picking one would score a token nobody asked about.
		return nil, nil, promptTokens, &ReadoutError{Message: fmt.Sprintf(
			"the prompt_logprobs fallback for code %s returned %d entries where "+
				"prompt_logprobs: 0 returns one", pythonQuote(slot.Code), len(entries))}
	}
	for _, entry := range entries {
		if entry == nil {
			// A null entry at the answer boundary is not a logprob. Go's
			// decoder turns `null` into a nil map entry, and reading through it
			// would panic in a library that promises errors.
			return nil, nil, promptTokens, &ReadoutError{Message: fmt.Sprintf(
				"the prompt_logprobs fallback for code %s returned a null entry at the "+
					"answer boundary", pythonQuote(slot.Code))}
		}
		return entry.Logprob, entry.Rank, promptTokens, nil
	}
	return nil, nil, promptTokens, nil
}

// Think generates one greedy thought of at most budget tokens.
//
// prompt is a Render(..., Thinking: true) prompt. Generation stops at the token
// that closes the thought, which is the token the answer is read after, or at
// the budget. The tokens come back as ids so the read that follows continues
// exactly what the model produced; a thought the budget cut is returned unclosed
// and the caller closes it.
func (r *Readout) Think(ctx context.Context, prompt string, budget int, debug bool) (Thought, error) {
	if budget < 1 {
		return Thought{}, &ReadoutError{Message: fmt.Sprintf(
			"the thinking budget must be a positive integer, got %d", budget)}
	}
	tok, err := r.ensureTokenizer()
	if err != nil {
		return Thought{}, err
	}
	closeID, err := r.answerPrefixIDFor(tok)
	if err != nil {
		return Thought{}, err
	}
	promptIDs := tok.Encode(prompt)
	withTokenIDs := true
	body := completionRequest{
		Model:          r.model,
		Prompt:         prompt,
		MaxTokens:      budget,
		Temperature:    0,
		StopTokenIDs:   []int{closeID},
		ReturnTokenIDs: &withTokenIDs,
	}
	var response completionResponse
	if err := r.completions(ctx, body, &response); err != nil {
		return Thought{}, err
	}
	choice, err := firstChoice(response, "the thinking read")
	if err != nil {
		return Thought{}, err
	}
	if choice.TokenIDs == nil {
		return Thought{}, &ReadoutError{Message: "the completion carries no token_ids, so the " +
			"thought cannot be continued token for token; the thinking read needs a server " +
			"that honours return_token_ids"}
	}
	generated := make([]int, len(*choice.TokenIDs))
	copy(generated, *choice.TokenIDs)
	closed := false
	for i, id := range generated {
		if id == closeID {
			generated = generated[:i]
			closed = true
			break
		}
	}
	promptTokens, err := r.checkPromptTokens(response, len(promptIDs))
	if err != nil {
		return Thought{}, err
	}
	thought := Thought{
		PromptIDs:    promptIDs,
		TokenIDs:     generated,
		Closed:       closed,
		PromptTokens: promptTokens,
	}
	if debug {
		thought.Text = choice.Text
	}
	return thought, nil
}

// The verdicts one image read records for the server's rendering of the system
// turn, and the prefix the probe reports for a server it could not ask.
const (
	// ChatSystemString means the server rendered the system turn the way the
	// text scaffold does.
	ChatSystemString = "string"
	// ChatSystemDiffers means it did not. vLLM hands Gemma 4's chat template
	// every message of an image request as content parts, and the template
	// writes a space after a system message that arrives that way, so this is
	// what a stock vLLM reports.
	ChatSystemDiffers = "differs"
	// ChatSystemUnverified means the response did not carry enough prompt tokens
	// to tell.
	ChatSystemUnverified = "unverified"
)

// DefaultImageTopLogprobs is the `top_logprobs` an image read sends when the
// caller does not choose one. The server has to allow the limit, and the image
// readout has no fallback, so every option has to fit inside it.
const DefaultImageTopLogprobs = 20

// ImageReadOptions are the settings one image read takes.
type ImageReadOptions struct {
	// TopLogprobs is the `top_logprobs` the server is asked for. Zero means
	// DefaultImageTopLogprobs. It must be at least the option count: the image
	// readout has no fallback, so an option outside the returned top-k is an
	// error rather than a second request.
	TopLogprobs int
	// Debug keeps the sent messages and the whole returned top-k in the result.
	Debug bool
	// RequireStringSystem turns a system turn the server rendered differently
	// into an error. `DecisionModel` does not set it: a stock vLLM renders the
	// space on every image request, and rejects images outright when told to pass
	// strings, so the strict form is for a server whose template has been fixed.
	RequireStringSystem bool
}

// ReadImage reads one question's answer codes with one image attached.
//
// The image is sent as an `image_url` content part and the question as the JSON
// object `Render` would have rendered, because only the server can turn the
// image into image tokens. `imageURL` must already be a URL: a local file name
// is rejected rather than forwarded, so call [PrepareImage] first.
//
// Unlike [Readout.ReadCodes], there is no code fallback. Appending a code to a
// chat request does not reproduce the answer boundary, so a code outside the
// returned top-k is an error instead of costing a second request, and an
// incomplete distribution is never renormalized into an answer.
//
// The read records, as `SystemRender`, whether the server rendered the system
// turn the way the text scaffold does.
func (r *Readout) ReadImage(ctx context.Context, state any, question Question, imageURL string,
	options ImageReadOptions) (CodeRead, error) {
	url, err := ValidateImageURL(imageURL)
	if err != nil {
		return CodeRead{}, err
	}
	topLogprobs := options.TopLogprobs
	if topLogprobs == 0 {
		topLogprobs = DefaultImageTopLogprobs
	}
	if topLogprobs < 1 {
		return CodeRead{}, &ReadoutError{Message: fmt.Sprintf(
			"top_logprobs must be a positive integer, got %d", topLogprobs)}
	}
	// One tokenizer for the whole read: the slots, the terminator and the
	// system-render comparison all have to come from the same one, or a
	// concurrent [Readout.SetTokenizer] could have this read confirm the boundary
	// with one tokenizer and compare the server's rendering with another.
	tok, generation, err := r.tokenizerSnapshot()
	if err != nil {
		return CodeRead{}, err
	}
	// The image readout keeps the letter limit: 26 options, the same as the DE-1
	// scaffold, because it is a chat request the server renders rather than a
	// codebook read.
	payload, options_, err := Payload(state, question, MaxOptions)
	if err != nil {
		return CodeRead{}, err
	}
	slots, err := r.codeSlotsFor(tok, generation, len(options_))
	if err != nil {
		return CodeRead{}, err
	}
	if len(slots) > topLogprobs {
		return CodeRead{}, &ReadoutError{Message: fmt.Sprintf(
			"%d options cannot be read from a top-%d distribution: the server returns at most "+
				"%d tokens, so at least %d option letters would be missing and the image "+
				"readout has no letter fallback; pass top_logprobs=%d or higher and serve the "+
				"model with --max-logprobs %d or higher",
			len(slots), topLogprobs, topLogprobs, len(slots)-topLogprobs, len(slots), len(slots))}
	}
	prefixID, err := r.answerPrefixIDFor(tok)
	if err != nil {
		return CodeRead{}, err
	}
	user, err := PythonJSON(payload)
	if err != nil {
		return CodeRead{}, &ReadoutError{Message: "the state cannot be rendered as the user turn", Err: err}
	}
	messages := imageMessages(r.systemLine(), url, user)
	true_, zero := true, 0
	body := chatRequest{
		Model:              r.model,
		Messages:           messages,
		MaxTokens:          1,
		Temperature:        0,
		Logprobs:           &true_,
		TopLogprobs:        &topLogprobs,
		PromptLogprobs:     &zero,
		ChatTemplateKwargs: chatTemplateKwargs{EnableThinking: false},
	}
	var response chatResponse
	if err := r.chatCompletions(ctx, body, &response); err != nil {
		return CodeRead{}, err
	}
	choice, err := firstChatChoice(response)
	if err != nil {
		return CodeRead{}, err
	}
	if err := checkImageBoundary(response, prefixID); err != nil {
		return CodeRead{}, err
	}
	systemRender, err := r.systemRenderFor(tok, response, state, question)
	if err != nil {
		return CodeRead{}, err
	}
	if options.RequireStringSystem && systemRender != ChatSystemString {
		return CodeRead{}, &ReadoutError{Message: fmt.Sprintf(
			"the server did not render the system turn the way the text scaffold does "+
				"(system render: %s). Gemma 4's chat template writes a space after a system "+
				"message that arrives as content parts, which is how vLLM passes every message "+
				"of an image request", systemRender)}
	}
	top, err := contentTopLogprobs(choice)
	if err != nil {
		return CodeRead{}, err
	}
	missing := make([]string, 0, len(slots))
	for _, slot := range slots {
		if _, ok := top[slot.Text]; !ok {
			missing = append(missing, slot.Code)
		}
	}
	if len(missing) != 0 {
		return CodeRead{}, &ReadoutError{Message: fmt.Sprintf(
			"option letters %s are missing from the returned top %d logprobs and the image "+
				"readout has no letter fallback; raise the server's --max-logprobs above %d or "+
				"pass a higher top_logprobs", strings.Join(missing, ""), topLogprobs, topLogprobs)}
	}
	logprobs := make(map[string]float64, len(slots))
	for _, slot := range slots {
		logprobs[slot.Code] = top[slot.Text]
	}
	probabilities, err := Softmax(logprobs)
	if err != nil {
		return CodeRead{}, err
	}
	result := CodeRead{
		ServedModel:    response.Model,
		Logprobs:       logprobs,
		Probabilities:  probabilities,
		Requests:       1,
		PromptTokens:   response.Usage.PromptTokens,
		MissingFromTop: []string{},
		Ranks:          map[string]int{},
		TopLogprobs:    map[string]*float64{},
		SystemRender:   systemRender,
	}
	if raw, present := choice.Message["content"]; present {
		// A null or a non-string content leaves this nil, which is the Python
		// client's `isinstance(sampled, str)`.
		var text *string
		if err := json.Unmarshal(raw, &text); err == nil && text != nil {
			result.Sampled = text
		}
	}
	if options.Debug {
		for key, value := range top {
			entry := value
			result.TopLogprobs[key] = &entry
		}
		result.Messages = messages
	}
	return result, nil
}

// imageMessages is the multimodal messages the server renders: the image, then
// the JSON. There is no repeated user turn here -- the repeat is part of the
// DE-2 text scaffold, and the server renders this prompt from what it is given.
func imageMessages(systemPrompt, imageURL, user string) []Object {
	return []Object{
		{{Key: "role", Value: "system"}, {Key: "content", Value: systemPrompt}},
		{{Key: "role", Value: "user"}, {Key: "content", Value: []any{
			Object{{Key: "type", Value: "image_url"},
				{Key: "image_url", Value: Object{{Key: "url", Value: imageURL}}}},
			Object{{Key: "type", Value: "text"}, {Key: "text", Value: user}},
		}}},
	}
}

// systemLine is the system prompt this readout renders with. An empty
// `SystemPrompt` means [DirectSystem], the same substitution [Render] makes, so
// the image read and the chat probe send the line the text scaffold uses.
func (r *Readout) systemLine() string {
	if r.systemPrompt == "" {
		return DirectSystem
	}
	return r.systemPrompt
}

// checkImageBoundary confirms the server read the distribution at the answer
// boundary.
//
// [Readout.CheckCodeBoundary] does this for the text path by tokenizing the
// prompt it rendered itself. The image readout has no local prompt to tokenize,
// so the substitute is the server's last prompt token: it must be the exact
// token the local tokenizer resolves the generation-prompt terminator to.
func checkImageBoundary(response chatResponse, prefixID int) error {
	if len(response.PromptLogprobs) == 0 {
		return &ReadoutError{Message: "the chat response carries no root-level prompt_logprobs, " +
			"so the answer boundary cannot be confirmed; the server must return prompt_logprobs"}
	}
	last := response.PromptLogprobs[len(response.PromptLogprobs)-1]
	if len(last) == 0 {
		return &ReadoutError{Message: "the chat response's prompt_logprobs carries no token at " +
			"the answer boundary"}
	}
	key, entry := boundaryEntry(last, prefixID)
	// Python reads the entry with `.get("decoded_token")`, so an entry that
	// omits it, or is not an object, is None rather than the empty string.
	decoded := "None"
	if entry != nil && entry.DecodedToken != nil {
		decoded = pythonQuote(*entry.DecodedToken)
	}
	// The comparison is on the key's text, which is how the Python client
	// compares it: a key that is not a token id at all is reported as itself
	// rather than as an id.
	if key != strconv.Itoa(prefixID) || entry == nil || entry.DecodedToken == nil ||
		*entry.DecodedToken != AnswerPrefix {
		return &ReadoutError{Message: fmt.Sprintf(
			"the last prompt token is %s (id %s), not %s (id %d); the distribution was not "+
				"read at the answer boundary", decoded, key,
			pythonQuote(AnswerPrefix), prefixID)}
	}
	return nil
}

// boundaryEntry is the entry at the answer boundary, preferring the terminator's
// own id.
//
// `prompt_logprobs: 0` returns one token per position, so a well-formed response
// has one entry here. A position carrying several is an ambiguous boundary, and
// refusing an ambiguous boundary is this check's whole job. The Python client
// takes the dict's first key, which is insertion order there and unordered here,
// so this looks for the terminator by id and otherwise reports the lowest id:
// the same answer twice, rather than a different one each run.
//
// The key is returned as the server sent it, so a refusal can print it. A key
// that is not a token id cannot be the boundary, and it is reported as itself
// rather than as an id it does not have.
func boundaryEntry(entries map[string]*chatPromptEntry, prefixID int) (string, *chatPromptEntry) {
	wanted := strconv.Itoa(prefixID)
	best := ""
	var chosen *chatPromptEntry
	found := false
	for key, entry := range entries {
		if key == wanted {
			return key, entry
		}
		id, err := strconv.Atoi(key)
		if err != nil {
			// A key that is not a token id is still what the server sent, so it is
			// reported when nothing else can be.
			if !found {
				best, chosen, found = key, entry, true
			}
			continue
		}
		if !found {
			best, chosen, found = key, entry, true
			continue
		}
		if previous, err := strconv.Atoi(best); err != nil || id < previous {
			best, chosen = key, entry
		}
	}
	return best, chosen
}

// systemRender reports whether the server rendered the system turn as the text
// scaffold does.
//
// A chat server that hands the template content parts instead of strings can
// render the system turn differently: Gemma 4's template writes a space after a
// system message that arrives as parts. The answer boundary still checks out, so
// this compares the server's prompt tokens with the local render through the
// token that follows the system line.
//
// It returns [ChatSystemString] when they agree, [ChatSystemDiffers] when they do
// not, and [ChatSystemUnverified] when the response does not carry enough prompt
// tokens to tell.
func (r *Readout) systemRender(response chatResponse, state any, question Question) (string, error) {
	tok, err := r.ensureTokenizer()
	if err != nil {
		return "", err
	}
	return r.systemRenderFor(tok, response, state, question)
}

// systemRenderFor is [Readout.systemRender] against one tokenizer, so a read
// that already holds one compares the server's prompt with the same tokenizer it
// resolved the answer boundary with.
func (r *Readout) systemRenderFor(tok Tokenizer, response chatResponse, state any,
	question Question) (string, error) {
	systemPrompt := r.systemLine()
	local, err := r.Render(state, question, RenderOptions{})
	if err != nil {
		return "", err
	}
	at := strings.Index(local, systemPrompt)
	entries := response.PromptLogprobs
	if at < 0 || entries == nil {
		return ChatSystemUnverified, nil
	}
	localIDs := tok.Encode(local)
	throughSystem := tok.Encode(local[:at+len(systemPrompt)])
	want := localIDs[:min(len(throughSystem)+1, len(localIDs))]
	if len(want) < 2 || len(want)-1 > len(throughSystem) ||
		!slices.Equal(want[:len(want)-1], throughSystem) || len(entries) < len(want) {
		return ChatSystemUnverified, nil
	}
	// The first prompt position carries no logprob, so it is not compared.
	served := make([]string, 0, len(want)-1)
	for _, entry := range entries[1:len(want)] {
		if len(entry) != 1 {
			// An empty entry is what the Python client refuses. Several tokens at
			// one position is a server reporting alternatives, which
			// `prompt_logprobs: 0` does not, and the Python client's answer there
			// depends on its dict's insertion order. Neither says enough to
			// compare, so neither is compared.
			return ChatSystemUnverified, nil
		}
		for key := range entry {
			served = append(served, key)
		}
	}
	for index, id := range want[1:] {
		if served[index] != strconv.Itoa(id) {
			return ChatSystemDiffers, nil
		}
	}
	return ChatSystemString, nil
}

// ProbeChatRender asks the chat endpoint, without an image, how it renders the
// system turn.
//
// It returns [ChatSystemString], [ChatSystemDiffers], [ChatSystemUnverified], or
// `unavailable: ...` when the request could not be made. The image readout goes
// through the chat endpoint, so this is what an image question will be rendered
// with.
//
// The Python client converts only a ReadoutError into `unavailable` and lets a
// QuestionError propagate; this returns the same prefix for both, because a
// caller with no error channel cannot do anything else with it. `Health` asks a
// Noul, so the QuestionError case does not arise there.
func (r *Readout) ProbeChatRender(ctx context.Context, state any, question Question) string {
	payload, _, err := Payload(state, question, MaxOptions)
	if err != nil {
		return "unavailable: " + err.Error()
	}
	user, err := PythonJSON(payload)
	if err != nil {
		return "unavailable: shisade: the state cannot be rendered as the user turn: " + err.Error()
	}
	zero := 0
	body := chatRequest{
		Model: r.model,
		Messages: []Object{
			{{Key: "role", Value: "system"}, {Key: "content", Value: r.systemLine()}},
			{{Key: "role", Value: "user"}, {Key: "content", Value: user}},
		},
		MaxTokens:          1,
		Temperature:        0,
		PromptLogprobs:     &zero,
		ChatTemplateKwargs: chatTemplateKwargs{EnableThinking: false},
	}
	var response chatResponse
	if err := r.chatCompletions(ctx, body, &response); err != nil {
		return "unavailable: " + err.Error()
	}
	verdict, err := r.systemRender(response, state, question)
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return verdict
}

// chatCompletions posts one request to the chat endpoint.
func (r *Readout) chatCompletions(ctx context.Context, body chatRequest, into *chatResponse) error {
	client, err := r.httpClient()
	if err != nil {
		return err
	}
	return postJSON(ctx, client, r.baseURL+"/v1/chat/completions", r.headers, body, into)
}

// ListModels is the model ids the endpoint serves.
func (r *Readout) ListModels(ctx context.Context) ([]string, error) {
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	client, err := r.httpClient()
	if err != nil {
		return nil, err
	}
	if err := getJSON(ctx, client, r.baseURL+"/v1/models", r.headers, &response); err != nil {
		return nil, err
	}
	ids := make([]string, len(response.Data))
	for i, entry := range response.Data {
		ids[i] = entry.ID
	}
	return ids, nil
}

func (r *Readout) completions(ctx context.Context, body completionRequest, into *completionResponse) error {
	client, err := r.httpClient()
	if err != nil {
		return err
	}
	return postJSON(ctx, client, r.baseURL+"/v1/completions", r.headers, body, into)
}

// httpClient refuses the zero value. A `Readout{}` has no client, and a nil
// *http.Client panics inside net/http instead of returning an error; every other
// unusable readout reports why.
func (r *Readout) httpClient() (*http.Client, error) {
	if r == nil || r.client == nil {
		return nil, &ReadoutError{Message: "the readout has no HTTP client; build it with NewReadout"}
	}
	return r.client, nil
}

func (r *Readout) usesCandidateIDs() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.candidateIDs
}

func (r *Readout) disableCandidateIDs() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.candidateIDs = false
}
