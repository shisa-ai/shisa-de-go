package shisade

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The friendly layer: labels in, answers out.
//
// Two entry points, both on `DecisionModel`:
//
//   - `Classify(state, {"intent": [...]})` mirrors the label-set style of
//     encoder classifiers: name a head, pass the labels, get a map back.
//   - `Decide(state, {"route": NewChoice(...)})` is the full System One call,
//     with the three question types and their typed answers.
//
// Questions run concurrently over a bounded head pool. This port implements the
// DE-2 contract only, so every question is read twice, and thought about when
// that read is unsure. Every answer records its strategy and cost.
//
// The Python client also reads images and, on DE-1, overflows a wide text choice
// through balanced chunks. Neither is ported: this client's readout has no image
// path and no DE-1 read, so `Classify` and `Decide` have no image argument and a
// DE-1 checkpoint is refused outright.

// DefaultEndpoint is the hosted DE endpoint.
const DefaultEndpoint = "https://api.shisa.ai/openai"

// DefaultModel is the checkpoint a client reads when nothing names one.
const DefaultModel = "shisa-ai/shisa-de-1"

// EndpointEnv names the environment variable that overrides the endpoint.
const EndpointEnv = "SHISA_DE_ENDPOINT"

// APIKeyEnvs are the environment variables the client reads the key from, in
// order.
var APIKeyEnvs = []string{"SHISA_DE_API_KEY", "SHISA_API_KEY"}

// Answer is one typed answer, with the detail behind it.
//
// The three pointer fields (`Confidence`, `Choice`, `Noul`, `Score`, `Level`,
// `ThoughtClosed`) are the ones the Python client leaves as `None` for a type
// that does not use them; a nil pointer is that absence.
type Answer struct {
	// Type is "noul", "choice" or "score".
	Type string
	// Calibrated reports whether a fitted temperature was applied. It is not
	// whether one was requested: a record that does not cover this read leaves
	// the answer raw.
	Calibrated bool
	// Temperature is the temperature that was applied, 1.0 when none was.
	Temperature float64
	// Requests is the HTTP requests this answer cost.
	Requests int
	// MissingFromTop lists the codes the server's top-k did not carry.
	MissingFromTop []string
	// Probabilities is the distribution over the answer keys, tempered when
	// Calibrated.
	Probabilities map[string]float64
	// Confidence is the TypeSafe confidence statistic, absent only where the
	// Python client leaves it None.
	Confidence *float64
	// Strategy is the policy that produced the answer.
	Strategy string
	// ScoreSemantics says what the probabilities mean for a score answer.
	ScoreSemantics string
	// Stages is how many logical reads the strategy took.
	Stages int
	// LogicalReads is the same number, under the name the usage block uses.
	LogicalReads int
	// Finalists is the DE-1 overflow finalists, which this port never sets.
	Finalists []string
	// ThoughtTokens is the length of the thought, when the strategy thought.
	ThoughtTokens int
	// ThoughtClosed reports whether the model closed the thought itself.
	ThoughtClosed *bool
	// Choice is the chosen option key, for a choice answer.
	Choice *string
	// Noul is the probability that the answer is yes.
	Noul *float64
	// Score is the probability-weighted zero-based position.
	Score *float64
	// Level is the level a score answer sits on.
	Level *string
	// Legend maps each zero-based position to its level text.
	Legend map[string]string
}

// Value is the primary value: the option key, the probability of yes, or the
// weighted position. A type that does not use the field yields nil, which is the
// Python client's None.
func (a Answer) Value() any {
	switch a.Type {
	case "noul":
		if a.Noul == nil {
			return nil
		}
		return *a.Noul
	case "score":
		if a.Score == nil {
			return nil
		}
		return *a.Score
	default:
		if a.Choice == nil {
			return nil
		}
		return *a.Choice
	}
}

// Label is the answer as a label: the option key, or the level the answer sits
// on.
//
// `Classify` returns this, so an ordered scale reads as one of its levels rather
// than as an index or a weighted position.
func (a Answer) Label() any {
	if a.Type == "score" && a.Level != nil {
		return *a.Level
	}
	return a.Value()
}

// ToWire is the TypeSafe answer shape.
//
// The primary field carries the value, not a pointer to it, so a caller reads it
// with `wire["choice"].(string)` rather than through a dereference. That is what
// the Python client returns, and JSON cannot tell the two apart.
func (a Answer) ToWire() map[string]any {
	switch a.Type {
	case "noul":
		return map[string]any{"type": "noul", "noul": a.Value()}
	case "score":
		return map[string]any{
			"type":          "score",
			"score":         a.Value(),
			"legend":        a.Legend,
			"probabilities": a.Probabilities,
			"confidence":    numberOrNil(a.Confidence),
		}
	}
	out := map[string]any{
		"type":          "choice",
		"choice":        a.Value(),
		"probabilities": a.Probabilities,
	}
	if a.Confidence != nil {
		out["confidence"] = *a.Confidence
	}
	if a.ScoreSemantics != "option-softmax" {
		out["strategy"] = a.Strategy
		out["score_semantics"] = a.ScoreSemantics
		out["calibrated"] = false
	}
	return out
}

// ToDict is everything, including the fields TypeSafe does not return. Its keys
// are the Python client's `Answer.to_dict()`, so the two can be compared
// directly.
func (a Answer) ToDict() map[string]any {
	out := map[string]any{
		"type":          a.Type,
		"value":         a.Value(),
		"confidence":    numberOrNil(a.Confidence),
		"probabilities": a.Probabilities,
		"calibrated":    a.Calibrated,
		"temperature":   a.Temperature,
		"requests":      a.Requests,
	}
	if len(a.MissingFromTop) != 0 {
		out["missing_from_top"] = a.MissingFromTop
	}
	out["strategy"] = a.Strategy
	out["score_semantics"] = a.ScoreSemantics
	out["stages"] = a.Stages
	out["logical_reads"] = a.LogicalReads
	if a.Finalists != nil {
		out["finalists"] = a.Finalists
	}
	if a.ThoughtClosed != nil {
		out["thought_tokens"] = a.ThoughtTokens
		out["thought_closed"] = *a.ThoughtClosed
	}
	return out
}

// Decision is a map of head to value, with the typed answers and provenance
// beside it.
//
// `decision.Values["route"]` is the quick path; `decision.Answers["route"]` is
// the same answer with its distribution, confidence, and request accounting.
// Python's Decision is a dict subclass, so `result["route"]` works there; Go has
// no such thing, and the values live in one field.
type Decision struct {
	// Values is each head's primary value.
	Values map[string]any
	// Answers is each head's full answer.
	Answers map[string]Answer
	// Raw is the per-head detail, including the option order.
	Raw map[string]map[string]any
	// Usage is the request and token accounting for the whole call.
	Usage map[string]any
	// Meta is the provenance: model, family, readout version, policy and
	// calibration.
	Meta map[string]any
}

// Choices is the choice answers only.
func (d *Decision) Choices() map[string]Answer { return d.byType("choice") }

// Nouls is the yes/no answers only.
func (d *Decision) Nouls() map[string]Answer { return d.byType("noul") }

// Scores is the score answers only.
func (d *Decision) Scores() map[string]Answer { return d.byType("score") }

func (d *Decision) byType(kind string) map[string]Answer {
	selected := map[string]Answer{}
	for head, answer := range d.Answers {
		if answer.Type == kind {
			selected[head] = answer
		}
	}
	return selected
}

// ToWire is the TypeSafe response body, for callers that expect that shape.
func (d *Decision) ToWire() map[string]any {
	answers := make(map[string]any, len(d.Answers))
	for head, answer := range d.Answers {
		answers[head] = answer.ToWire()
	}
	model, _ := d.Meta["model"]
	return map[string]any{
		"model":   model,
		"answers": answers,
		"usage": map[string]any{
			"input_tokens":  d.usageInt("input_tokens"),
			"output_tokens": d.usageInt("output_tokens"),
		},
	}
}

func (d *Decision) usageInt(key string) any {
	if value, ok := d.Usage[key]; ok {
		return value
	}
	return 0
}

// HeadSpec is one head's label set, in the shorthand Classify accepts.
//
// The Python client takes a list of labels, a mapping of label to description,
// a `{"levels": [...]}` mapping, a `{"labels": [...], "multi_label": True}`
// mapping, or a question object, and tells them apart by shape. Go's maps do not
// preserve insertion order, and the order of a label set decides which answer
// code each label is read as, so a described label set is a slice here rather
// than a map: `Options` is the presentation order, and it is part of the
// question.
type HeadSpec struct {
	// Options is the label set in presentation order. A nil Description means
	// the label has none, and the label is shown instead.
	Options []Option
	// Levels is an ordered scale. It is mutually exclusive with Options.
	Levels []any
	// Instructions overrides the default question for the head.
	Instructions string
	// MultiLabel asks one yes/no question per option instead of one choice.
	MultiLabel bool
	// Threshold is the multi-label cutoff. Nil means 0.5.
	Threshold *float64
	// Question is a fully built question and overrides everything else.
	Question Question
}

// CallOptions are the settings Classify and Decide share.
type CallOptions struct {
	// Probability requires one logical read per question, so it stops a DE-2
	// answer at the repeated read instead of thinking. It does not change
	// calibration or the return shape.
	Probability bool
	// Calibrated requests calibration. Nil means "on", which is what the Python
	// client does for text.
	Calibrated *bool
	// Debug keeps the rendered prompts in Raw.
	Debug bool
	// Overrides replace the model's defaults for this call only.
	Overrides OptionOverrides
}

// ClassifyOptions are the settings Classify adds to CallOptions.
type ClassifyOptions struct {
	CallOptions
	// IncludeConfidence returns a label and its confidence per head instead of
	// the bare label.
	IncludeConfidence bool
	// IncludeProbabilities returns the distribution as well.
	IncludeProbabilities bool
}

// multiLabelHead is a label set where several labels may apply: one yes/no
// question per label.
type multiLabelHead struct {
	template     string
	labels       []string
	instructions string
}

func (m *multiLabelHead) Kind() string         { return "multi_label" }
func (m *multiLabelHead) Instructions() string { return m.instructions }
func (m *multiLabelHead) Options() []Option    { return nil }
func (m *multiLabelHead) Validate(int) error {
	return &QuestionError{Message: "a multi-label head has no single option list"}
}

func (m *multiLabelHead) noulFor(label string) (Noul, error) {
	if strings.Contains(m.template, "{label}") {
		instructions, err := formatLabel(m.template, label)
		if err != nil {
			return Noul{}, err
		}
		return NewNoul(instructions), nil
	}
	return NewNoul(m.template + " " + label), nil
}

// formatLabel is Python's `str.format(label=...)` for the shapes a multi-label
// template uses: `{label}` is replaced, `{{` and `}}` are literal braces, and a
// replacement field naming anything else is refused.
//
// The template is the prompt, and the prompt is the answer, so this follows
// `str.format` rather than doing a plain replacement: `"Is {label} a {{thing}}?"`
// renders as `Is billing a {thing}?` in both clients. Format specs (`{label!r}`,
// `{label:>10}`) are not supported and are refused rather than passed through.
func formatLabel(template, label string) (string, error) {
	var out strings.Builder
	for index := 0; index < len(template); {
		switch template[index] {
		case '{':
			if index+1 < len(template) && template[index+1] == '{' {
				out.WriteByte('{')
				index += 2
				continue
			}
			end := strings.IndexByte(template[index:], '}')
			if end < 0 {
				return "", &QuestionError{Message: fmt.Sprintf(
					"the multi-label template %q has an unclosed brace", template)}
			}
			field := template[index+1 : index+end]
			if field != "label" {
				return "", &QuestionError{Message: fmt.Sprintf(
					"the multi-label template %q names %q; the only field it may name is "+
						"{label}, which is the label being asked about", template, field)}
			}
			out.WriteString(label)
			index += end + 1
		case '}':
			if index+1 < len(template) && template[index+1] == '}' {
				out.WriteByte('}')
				index += 2
				continue
			}
			return "", &QuestionError{Message: fmt.Sprintf(
				"the multi-label template %q has a lone closing brace", template)}
		default:
			out.WriteByte(template[index])
			index++
		}
	}
	return out.String(), nil
}

// headWork is one question to ask, and the head name it answers under.
type headWork struct {
	head     string
	question Question
}

// QuestionFromHead turns a head's label spec into a question.
//
// `Classify` calls it for every head; it is exported so a caller can build the
// same question for `Decide`, which takes typed questions rather than label
// sets. A multi-label spec becomes one noul per label when the question is read,
// and `Decide` returns the per-label answers under `head:label`.
func QuestionFromHead(head string, spec HeadSpec) (Question, error) {
	return questionFromHead(head, spec)
}

func questionFromHead(head string, spec HeadSpec) (Question, error) {
	if spec.Question != nil {
		return spec.Question, nil
	}
	instructions := spec.Instructions
	if instructions == "" {
		instructions = defaultQuestion(head)
	}
	if spec.Levels != nil {
		return NewScore(instructions, spec.Levels), nil
	}
	if len(spec.Options) == 0 {
		return nil, &QuestionError{Message: fmt.Sprintf(
			"head %q: expected options or levels", head)}
	}
	if spec.MultiLabel {
		// Python's `{"labels": {k: v}, "multi_label": True}` reaches its mapping
		// branch, which reads a single choice over the described labels and
		// ignores `multi_label`. There is no way to spell that intent here without
		// silently dropping the descriptions, so it is refused.
		labels := make([]string, 0, len(spec.Options))
		seen := make(map[string]bool, len(spec.Options))
		for _, option := range spec.Options {
			if !isNilDescription(option.Description) {
				return nil, &QuestionError{Message: fmt.Sprintf(
					"head %q: a multi-label head asks one yes/no question per label and has "+
						"nowhere to show %q's description; drop the descriptions, or set "+
						"MultiLabel to false to read one choice over them", head, option.Key)}
			}
			// A repeated label reads the same noul twice under one key, and the
			// second answer overwrites the first. Python pops the key once and
			// then raises a KeyError on the second; this port refuses it before
			// anything is sent.
			if seen[option.Key] {
				return nil, &QuestionError{Message: fmt.Sprintf(
					"head %q: the multi-label head repeats the label %q; each label is one "+
						"yes/no question and the answers would overwrite each other",
					head, option.Key)}
			}
			seen[option.Key] = true
			labels = append(labels, option.Key)
		}
		template := spec.Instructions
		if template == "" {
			template = multiLabelQuestion(head)
		}
		return &multiLabelHead{template: template, labels: labels, instructions: template}, nil
	}
	return NewChoice(instructions, spec.Options), nil
}

// multiLabelThreshold is the cutoff a multi-label head uses.
func multiLabelThreshold(spec HeadSpec) float64 {
	if spec.Threshold == nil {
		return 0.5
	}
	return *spec.Threshold
}

func defaultQuestion(head string) string {
	return "What is the " + strings.ReplaceAll(head, "_", " ") + "?"
}

func multiLabelQuestion(head string) string {
	return "Is {label} one of the " + strings.ReplaceAll(head, "_", " ") + "?"
}

// DecisionModelConfig is everything NewDecisionModel takes.
//
// The Python client takes these as keyword arguments. Go has no keyword
// arguments, so the two settings that are "unset or a value" (`APIKey` and
// `Calibration`) are pointers, and every policy setting lives in `Overrides`.
type DecisionModelConfig struct {
	// BaseURL is the endpoint. Empty means SHISA_DE_ENDPOINT, then the hosted
	// default.
	BaseURL string
	// Model is the served checkpoint id. Empty means DefaultModel.
	Model string
	// APIKey is the bearer token. Nil means read it from APIKeyEnvs.
	APIKey *string
	// Tokenizer is the tokenizer source. Empty means the model id.
	Tokenizer string
	// TokenizerRevision pins the tokenizer snapshot.
	TokenizerRevision string
	// LocalFilesOnly refuses to fetch a tokenizer that is not already cached.
	LocalFilesOnly bool
	// Timeout bounds one HTTP request. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxWorkers bounds the concurrent head pool. Zero means 8.
	MaxWorkers int
	// MaxLogprobs is the top-k the server is asked for. Zero means
	// DefaultMaxLogprobs.
	MaxLogprobs int
	// Calibration overrides the shipped record. A record passed here is the
	// caller's assertion and is applied as given; Health still reports how far
	// it matches.
	Calibration *Calibration
	// Family declares the contract. Empty means resolve it from the model id,
	// then the tokenizer.
	Family Family
	// Overrides replace the family's default read.
	Overrides OptionOverrides
	// Readout replaces the built one. A caller that supplies a readout also
	// supplies its tokenizer.
	Readout *Readout
	// HTTPClient is a test seam: the transport the readout uses. Nil builds one
	// from Timeout.
	HTTPClient *http.Client
}

// DecisionModel is a client for a served DE-2 decision model.
//
// The model is served, never loaded: this type holds an HTTP client and a
// tokenizer, and the weights stay on the GPU server.
//
// `Family` declares which contract the served checkpoint is read through. Left
// unset it is taken from the model id, then the tokenizer source, and otherwise
// assumed to be DE-2. This port implements DE-2 only, so a checkpoint that
// resolves to DE-1 is an error rather than a fallback.
//
// `Overrides` selects the read: `Policy` (repeat-think, repeat or direct),
// `Reads`, `Reasoning`, `ReasoningProb`/`ThinkGate`, `ReasoningLen`/
// `ThinkBudget`. The same read can be set a part at a time, here as the model's
// default or on one Classify/Decide call.
type DecisionModel struct {
	// BaseURL is the endpoint that was resolved.
	BaseURL string
	// Model is the served checkpoint id.
	Model string
	// Family is the contract the checkpoint is read through.
	Family Family
	// FamilySource says whether the family was declared, detected or assumed.
	FamilySource FamilySource
	// FamilyExplicit is false when the family was assumed.
	FamilyExplicit bool
	// ReadoutVersion is the readout contract in use.
	ReadoutVersion string
	// Options is the model's default read.
	Options ReadOptions
	// Calibration is the record this client holds.
	Calibration Calibration
	// CalibrationLevel is how far that record applies to the served model.
	CalibrationLevel ApplicabilityLevel
	// CalibrationReasons explains a level that is not "checkpoint".
	CalibrationReasons []string
	// CalibrationApplied reports whether the record is actually tempering
	// answers.
	CalibrationApplied bool
	// Warnings are what the Python client would have sent to `warnings.warn`.
	// Go has no warning channel, so they are recorded here.
	Warnings []string
	// Readout is the raw-read client underneath.
	Readout *Readout

	maxWorkers int
}

// NewDecisionModel builds a client.
func NewDecisionModel(config DecisionModelConfig) (*DecisionModel, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = os.Getenv(EndpointEnv)
	}
	if baseURL == "" {
		baseURL = DefaultEndpoint
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		baseURL = baseURL[:len(baseURL)-3]
	}
	model := config.Model
	if model == "" {
		model = DefaultModel
	}

	readout := config.Readout
	tokenizerSource := config.Tokenizer
	if tokenizerSource == "" && readout != nil {
		tokenizerSource = readout.tokenizerSource
	}
	family, source, err := ResolveFamily(model, tokenizerSource, string(config.Family))
	if err != nil {
		return nil, err
	}
	// This port implements DE-2 only. Resolving to DE-1 is refused here rather
	// than read through the wrong contract.
	if family != FamilyDE2 {
		return nil, &UnsupportedFamilyError{Family: family, Model: model, Source: source}
	}

	model_ := &DecisionModel{
		BaseURL:        baseURL,
		Model:          model,
		Family:         family,
		FamilySource:   source,
		FamilyExplicit: source != SourceAssumed,
		ReadoutVersion: ReadoutVersionFor(family),
	}
	if !model_.FamilyExplicit {
		model_.Warnings = append(model_.Warnings, fmt.Sprintf(
			"%s names neither DE-1 nor DE-2, so it is read as %s; pass Family to say which contract it was trained for",
			pythonQuote(model), pythonQuote(string(family))))
	}

	options, err := ResolveOptions(family, nil, config.Overrides)
	if err != nil {
		return nil, err
	}
	model_.Options = options

	supplied := config.Calibration != nil
	if supplied {
		model_.Calibration = *config.Calibration
	} else {
		record, err := CalibrationFor(family)
		if err != nil {
			return nil, err
		}
		model_.Calibration = record
	}
	level, reasons := model_.Calibration.Applicability(model, model_.ReadoutVersion, ApplicabilityOptions{
		Family: family, Tokenizer: tokenizerSource,
	})
	model_.CalibrationLevel = level
	model_.CalibrationReasons = reasons
	model_.CalibrationApplied = model_.Calibration.Fitted() && (supplied || level == ApplicabilityCheckpoint)
	if model_.Calibration.Fitted() && !model_.CalibrationApplied {
		model_.Warnings = append(model_.Warnings, fmt.Sprintf(
			"the bundled %s calibration is not applied to %s: %s. Answers are raw probabilities; pass Calibration to apply a record you vouch for",
			string(family), pythonQuote(model), strings.Join(reasons, "; ")))
	}

	maxWorkers := config.MaxWorkers
	if maxWorkers == 0 {
		maxWorkers = 8
	}
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	model_.maxWorkers = maxWorkers

	maxLogprobs := config.MaxLogprobs
	if maxLogprobs == 0 {
		maxLogprobs = DefaultMaxLogprobs
	}
	if maxLogprobs < 1 {
		return nil, &ReadoutError{Message: "max_logprobs must be a positive integer"}
	}

	if readout != nil {
		model_.Readout = readout
		return model_, nil
	}
	apiKey := ""
	if config.APIKey != nil {
		apiKey = *config.APIKey
	} else if fromEnv := APIKeyFromEnv(); fromEnv != nil {
		apiKey = *fromEnv
	}
	built, err := NewReadout(ReadoutConfig{
		BaseURL:           baseURL,
		Model:             model,
		Tokenizer:         config.Tokenizer,
		TokenizerRevision: config.TokenizerRevision,
		LocalFilesOnly:    config.LocalFilesOnly,
		Family:            string(config.Family),
		Timeout:           config.Timeout,
		MaxLogprobs:       maxLogprobs,
		APIKey:            apiKey,
		HTTPClient:        config.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	model_.Readout = built
	return model_, nil
}

// APIKeyFromEnv reads the key from the environment, in APIKeyEnvs order.
func APIKeyFromEnv() *string {
	for _, name := range APIKeyEnvs {
		// `TrimPythonSpace`, not `strings.TrimSpace`: Python's `_api_key_from_env`
		// strips with `str.strip()`, and this value becomes the bearer token.
		value := TrimPythonSpace(os.Getenv(name))
		if value != "" {
			return &value
		}
	}
	return nil
}

// FromEndpoint points at a specific endpoint, such as a local vLLM on port 8021.
func FromEndpoint(baseURL, model string, config DecisionModelConfig) (*DecisionModel, error) {
	config.BaseURL = baseURL
	if model != "" {
		config.Model = model
	}
	return NewDecisionModel(config)
}

// FromPretrained picks a checkpoint by name and talks to it where it is served.
//
// No weights are downloaded. The endpoint comes from `BaseURL`, then
// `SHISA_DE_ENDPOINT`, then the hosted default, and the key comes from
// `SHISA_DE_API_KEY` or `SHISA_API_KEY`.
func FromPretrained(model string, config DecisionModelConfig) (*DecisionModel, error) {
	if model != "" {
		config.Model = model
	}
	return NewDecisionModel(config)
}

// Close releases the readout's connections.
func (m *DecisionModel) Close() { m.Readout.Close() }

// readout is the read client, or an error naming the zero value. A
// DecisionModel built by NewDecisionModel always has one; the zero value does
// not, and a nil *Readout would panic inside net/http instead of reporting.
func (m *DecisionModel) readout() (*Readout, error) {
	if m.Readout == nil {
		return nil, &ReadoutError{Message: "the model has no readout; build it with NewDecisionModel"}
	}
	return m.Readout, nil
}

// Policy is the read the model uses by default.
func (m *DecisionModel) Policy() string { return m.Options.Policy }

// Reads is how many times the user turn is written by default.
func (m *DecisionModel) Reads() string { return m.Options.Reads() }

// Reasoning reports whether the default read thinks when unsure.
func (m *DecisionModel) Reasoning() bool { return m.Options.Reasoning() }

// ThinkGate is the default gate.
func (m *DecisionModel) ThinkGate() float64 { return m.Options.ThinkGate }

// ThinkBudget is the default thought budget.
func (m *DecisionModel) ThinkBudget() int { return m.Options.ThinkBudget }

// Health checks the endpoint, the served model id, the answer boundary, and the
// calibration.
//
// A passing boundary check says the answer codes were read from the right
// position. It says nothing about whether the temperatures applied to them
// belong to this checkpoint, which is a separate failure and a silent one, so
// the calibration's provenance is checked here too and a mismatch fails the
// report.
//
// `probe` also spends requests: one question through the family's own read,
// which fails the report if the server cannot serve it.
func (m *DecisionModel) Health(ctx context.Context, probe bool) map[string]any {
	report := map[string]any{
		"base_url":                    m.BaseURL,
		"model":                       m.Model,
		"model_family":                string(m.Family),
		"family_source":               string(m.FamilySource),
		"family_explicit":             m.FamilyExplicit,
		"readout_version":             m.ReadoutVersion,
		"policy":                      m.Policy(),
		"reads":                       m.Reads(),
		"reasoning":                   m.Reasoning(),
		"compound":                    m.Options.Compound(),
		"calibration":                 m.Calibration.ID(),
		"calibration_model":           m.Calibration.Model,
		"calibration_readout_version": m.Calibration.ReadoutVersion,
		"calibration_serving_shape":   m.Calibration.ServingShape,
		"calibration_level":           string(m.CalibrationLevel),
		"calibration_applied":         m.CalibrationApplied,
		"ok":                          false,
	}
	if !m.FamilyExplicit {
		report["family_note"] = fmt.Sprintf(
			"%s carries no DE-1 or DE-2 slug; %s was assumed, not detected",
			pythonQuote(m.Model), pythonQuote(string(m.Family)))
	}
	matched := m.CalibrationLevel != ApplicabilityMismatch
	report["calibration_match"] = matched
	if !matched {
		report["calibration_mismatch"] = m.CalibrationReasons
	} else if len(m.CalibrationReasons) != 0 {
		report["calibration_note"] = m.CalibrationReasons
	}
	readout, readoutErr := m.readout()
	if readoutErr != nil {
		report["models_status"] = "error: " + readoutErr.Error()
		report["boundary_check"] = "failed: " + readoutErr.Error()
		return report
	}
	models, err := readout.ListModels(ctx)
	if err != nil {
		report["models_status"] = "error: " + err.Error()
	} else {
		report["models_status"] = 200
		report["served_models"] = models
		report["model_listed"] = containsString(models, m.Model)
	}
	question := NewNoul("Is this a health check?")
	// Every code the contract can use, not only the two a noul needs.
	slots, err := readout.CodeSlots(MaxCodes)
	if err != nil {
		report["boundary_check"] = "failed: " + err.Error()
	} else {
		repeat := 2
		if m.Policy() == "direct" {
			repeat = 1
		}
		prompt, err := readout.Render(map[string]any{"health": "check"}, question,
			RenderOptions{Repeat: repeat})
		if err != nil {
			report["boundary_check"] = "failed: " + err.Error()
		} else if tok, err := readout.EnsureTokenizer(); err != nil {
			report["boundary_check"] = "failed: " + err.Error()
		} else if err := readout.CheckCodeBoundary(tok.Encode(prompt), slots); err != nil {
			report["boundary_check"] = "failed: " + err.Error()
		} else {
			report["boundary_check"] = "passed"
			// An `Object`, not a map: Python's `{"count": ..., **{letter: id}}`
			// has an order, and `doctor --json` prints it. A map would come out
			// sorted, which is the codebook's order only while the first two
			// codes happen to sort that way.
			slotReport := make(Object, 0, 3)
			slotReport = append(slotReport, Member{Key: "count", Value: len(slots)})
			for _, slot := range slots[:2] {
				slotReport = append(slotReport, Member{Key: slot.Code, Value: slot.TokenID})
			}
			report["slots"] = slotReport
			listed, _ := report["model_listed"].(bool)
			report["ok"] = listed && matched
		}
	}
	if probe {
		decision, err := m.Decide(ctx, map[string]any{"health": "check"},
			map[string]Question{"probe": question}, CallOptions{Calibrated: boolPointer(false)})
		if err != nil {
			report["read_probe"] = "failed: " + err.Error()
			report["ok"] = false
		} else {
			answer := decision.Answers["probe"]
			report["read_probe"] = fmt.Sprintf("passed (%s, %d requests)", answer.Strategy, answer.Requests)
		}
	}
	return report
}

// Classify classifies a state against named label sets.
//
// `heads` maps a head name to its label set: options in presentation order,
// `Levels` for an ordered scale, `MultiLabel` for several labels at once, or a
// question object for full control.
//
// Calibration is on by default and is applied only where a fitted record covers
// the served checkpoint. `Probability` requires one logical read per question,
// so it skips the thinking read.
//
// Returns a Decision: `Values["intent"]` is the chosen label, and
// `Answers["intent"]` carries the distribution behind it.
func (m *DecisionModel) Classify(ctx context.Context, state any, heads map[string]HeadSpec,
	options ClassifyOptions) (*Decision, error) {
	questions := make(map[string]Question, len(heads))
	for head, spec := range heads {
		question, err := questionFromHead(head, spec)
		if err != nil {
			return nil, err
		}
		questions[head] = question
	}
	settings, err := ResolveOptions(m.Family, &m.Options, options.Overrides)
	if err != nil {
		return nil, err
	}
	decision, err := m.run(ctx, state, questions, options.CallOptions, settings,
		options.Overrides.Reasoning != nil && *options.Overrides.Reasoning)
	if err != nil {
		return nil, err
	}
	for head, question := range questions {
		if multi, ok := question.(*multiLabelHead); ok {
			threshold := multiLabelThreshold(heads[head])
			entries := make([]map[string]any, 0, len(multi.labels))
			for _, label := range multi.labels {
				key := head + ":" + label
				answer := decision.Answers[key]
				delete(decision.Answers, key)
				entry := map[string]any{
					"label":         label,
					"confidence":    numberOrNil(answer.Confidence),
					"probabilities": answer.Probabilities,
				}
				if answer.Noul != nil && *answer.Noul >= threshold {
					entries = append(entries, entry)
				}
				decision.Raw[key] = map[string]any{
					"noul": numberOrNil(answer.Noul), "requests": answer.Requests}
			}
			switch {
			case options.IncludeProbabilities:
				decision.Values[head] = entries
			case options.IncludeConfidence:
				labels := make([]map[string]any, 0, len(entries))
				for _, entry := range entries {
					labels = append(labels, map[string]any{
						"label": entry["label"], "confidence": entry["confidence"]})
				}
				decision.Values[head] = labels
			default:
				labels := make([]string, 0, len(entries))
				for _, entry := range entries {
					labels = append(labels, entry["label"].(string))
				}
				decision.Values[head] = labels
			}
			continue
		}
		answer := decision.Answers[head]
		switch {
		case options.IncludeProbabilities:
			value := map[string]any{
				"label":         answer.Label(),
				"confidence":    numberOrNil(answer.Confidence),
				"probabilities": answer.Probabilities,
			}
			if answer.ScoreSemantics != "option-softmax" {
				value["strategy"] = answer.Strategy
				value["score_semantics"] = answer.ScoreSemantics
				value["calibrated"] = false
			}
			decision.Values[head] = value
		case options.IncludeConfidence:
			value := map[string]any{"label": answer.Label(), "confidence": numberOrNil(answer.Confidence)}
			if answer.ScoreSemantics != "option-softmax" {
				value["strategy"] = answer.Strategy
				value["score_semantics"] = answer.ScoreSemantics
				value["calibrated"] = false
			}
			decision.Values[head] = value
		default:
			decision.Values[head] = answer.Label()
		}
	}
	return decision, nil
}

// Decide asks typed questions and gets typed answers.
//
// `questions` maps a question id to a Noul, Choice or Score. The Values map
// returns each answer's primary value; Answers returns the full answer. For a
// Score, `Values` is the weighted position and `Answers[id].Label()` is the
// level it landed on, which is what `Classify` returns instead.
//
// The Python client's `decide` also accepts the label-set shorthand `classify`
// takes; this port keeps the two apart, and `QuestionFromHead` builds the
// question a shorthand describes.
//
// Calibration is on by default. `Probability` requires one logical read per
// question, so it skips the thinking read, and leaves calibration and the return
// shape unchanged.
func (m *DecisionModel) Decide(ctx context.Context, state any, questions map[string]Question,
	options CallOptions) (*Decision, error) {
	settings, err := ResolveOptions(m.Family, &m.Options, options.Overrides)
	if err != nil {
		return nil, err
	}
	decision, err := m.run(ctx, state, questions, options, settings,
		options.Overrides.Reasoning != nil && *options.Overrides.Reasoning)
	if err != nil {
		return nil, err
	}
	for head, answer := range decision.Answers {
		decision.Values[head] = answer.Value()
	}
	return decision, nil
}

// SystemOne is Decide under the name the Python client also exposes.
func (m *DecisionModel) SystemOne(ctx context.Context, state any, questions map[string]Question,
	options CallOptions) (*Decision, error) {
	return m.Decide(ctx, state, questions, options)
}

func (m *DecisionModel) run(ctx context.Context, state any, questions map[string]Question,
	options CallOptions, settings ReadOptions, askedReasoning bool) (*Decision, error) {
	if options.Probability && askedReasoning {
		return nil, &QuestionError{Message: "probability=true takes one logical read per question; " +
			"it cannot be combined with reasoning=true"}
	}
	if _, err := m.readout(); err != nil {
		return nil, err
	}
	started := time.Now()
	requestedCalibration := options.Calibrated
	calibrated := true
	if requestedCalibration != nil {
		calibrated = *requestedCalibration
	}
	decision := &Decision{
		Values:  map[string]any{},
		Answers: map[string]Answer{},
		Raw:     map[string]map[string]any{},
	}

	heads := make([]string, 0, len(questions))
	for head := range questions {
		heads = append(heads, head)
	}
	sort.Strings(heads)
	work := make([]headWork, 0, len(questions))
	for _, head := range heads {
		question := questions[head]
		if multi, ok := question.(*multiLabelHead); ok {
			for _, label := range multi.labels {
				noul, err := multi.noulFor(label)
				if err != nil {
					return nil, err
				}
				work = append(work, headWork{head: head + ":" + label, question: noul})
			}
			continue
		}
		work = append(work, headWork{head: head, question: question})
	}

	// A probability call takes one logical read per question, so it stops at the
	// repeated read: an answer read after a thought is a second.
	policy := settings.Policy
	if options.Probability && policy == "repeat-think" {
		policy = "repeat"
	}

	// Validate every head before issuing any requests.
	for _, item := range work {
		limit := MaxOptions
		if item.question.Kind() == "choice" {
			limit = MaxCodes
		}
		if err := item.question.Validate(limit); err != nil {
			return nil, err
		}
	}

	results := make([]headResult, len(work))
	runOne := func(index int, item headWork) headResult {
		read, err := ReadPolicy(ctx, m.Readout, state, item.question, ReadOptions{
			Family:      m.Family,
			Policy:      policy,
			ThinkGate:   settings.ThinkGate,
			ThinkBudget: settings.ThinkBudget,
			Overflow:    settings.Overflow,
		}, options.Debug)
		if err != nil {
			return headResult{head: item.head, err: err}
		}
		return headResult{head: item.head, question: item.question, read: read}
	}
	if m.maxWorkers > 1 && len(work) > 1 {
		limit := m.maxWorkers
		if len(work) < limit {
			limit = len(work)
		}
		semaphore := make(chan struct{}, limit)
		var group sync.WaitGroup
		for index, item := range work {
			group.Add(1)
			go func(index int, item headWork) {
				defer group.Done()
				semaphore <- struct{}{}
				defer func() { <-semaphore }()
				results[index] = runOne(index, item)
			}(index, item)
		}
		group.Wait()
	} else {
		for index, item := range work {
			results[index] = runOne(index, item)
		}
	}
	for _, result := range results {
		if result.err != nil {
			return nil, result.err
		}
	}

	inputTokens := 0
	requests := 0
	logicalReads := 0
	thoughtTokens := 0
	for _, result := range results {
		read := result.read
		// Each read is tempered by its own fit. A record without per-read
		// temperatures was fitted on a read before any thought, so an answer read
		// after one stays raw under it.
		covered := len(m.Calibration.Reads) != 0 || read.Thought == nil
		answer, err := answerFromRead(result.question, read.Read(), result.question.Options(),
			m.Calibration, calibrated && m.CalibrationApplied && covered, read.Components[len(read.Components)-1].Stage)
		if err != nil {
			return nil, err
		}
		answer.Strategy = read.Strategy
		answer.Stages = read.LogicalReads()
		answer.LogicalReads = read.LogicalReads()
		answer.Requests = read.Requests()
		if read.Thought != nil {
			answer.ThoughtTokens = read.ThoughtTokens()
			closed := read.Thought.Closed
			answer.ThoughtClosed = &closed
		}
		decision.Answers[result.head] = answer
		raw := read.Raw(options.Debug)
		raw["options"] = optionKeys(result.question.Options())
		decision.Raw[result.head] = raw
		inputTokens += read.PromptTokens()
		requests += read.Requests()
		logicalReads += read.LogicalReads()
		thoughtTokens += read.ThoughtTokens()
	}
	decision.Usage = map[string]any{
		"input_tokens":   inputTokens,
		"output_tokens":  logicalReads + thoughtTokens,
		"logical_reads":  logicalReads,
		"thought_tokens": thoughtTokens,
		"requests":       requests,
		"wall_ms":        round2(float64(time.Since(started).Nanoseconds()) / 1e6),
	}
	// What the answers carry, not what was asked for: a requested calibration
	// that no fitted record covers leaves every answer raw.
	flags := map[bool]bool{}
	for _, answer := range decision.Answers {
		flags[answer.Calibrated] = true
	}
	var calibratedMeta any
	switch {
	case len(flags) == 1:
		for value := range flags {
			calibratedMeta = value
		}
	case len(decision.Answers) == 0:
		calibratedMeta = calibrated
	default:
		calibratedMeta = nil
	}
	calibrationByHead := map[string]bool{}
	strategyByHead := map[string]string{}
	for head, answer := range decision.Answers {
		calibrationByHead[head] = answer.Calibrated
		strategyByHead[head] = answer.Strategy
	}
	decision.Meta = map[string]any{
		"model":               m.Model,
		"base_url":            m.BaseURL,
		"family":              string(m.Family),
		"readout_version":     m.ReadoutVersion,
		"policy":              policy,
		"probability":         options.Probability,
		"input_type":          "text",
		"calibration":         m.Calibration.ID(),
		"calibrated":          calibratedMeta,
		"calibration_by_head": calibrationByHead,
		"strategy_by_head":    strategyByHead,
	}
	decision.Meta["think_gate"] = settings.ThinkGate
	decision.Meta["think_budget"] = settings.ThinkBudget
	decision.Meta["think_option_cap"] = ThinkOptionCap
	return decision, nil
}

type headResult struct {
	head     string
	question Question
	read     PolicyRead
	err      error
}

// answerFromRead turns one code read into a typed answer, tempering if asked.
func answerFromRead(question Question, read CodeRead, options []Option, calibration Calibration,
	calibrated bool, readName string) (Answer, error) {
	temperature := 1.0
	if calibrated {
		temperature = calibration.TemperatureFor(question.Kind(), readName)
	}
	applied := calibrated && temperature != 1.0
	letters, err := CodesFor(len(options))
	if err != nil {
		return Answer{}, err
	}
	raw := make(map[string]float64, len(letters))
	for _, letter := range letters {
		value, ok := read.Probabilities[letter]
		if !ok {
			return Answer{}, &ReadoutError{Message: fmt.Sprintf(
				"the read returned no probability for code %q", letter)}
		}
		raw[letter] = value
	}

	common := func() (Answer, error) {
		return Answer{
			Calibrated:     applied,
			Temperature:    temperature,
			Requests:       read.Requests,
			MissingFromTop: append([]string{}, read.MissingFromTop...),
			Strategy:       StrategyDirect,
			ScoreSemantics: "option-softmax",
			Stages:         1,
			LogicalReads:   1,
		}, nil
	}

	if question.Kind() == "noul" {
		yes := raw["A"]
		if applied {
			yes = TemperBinary(raw["A"], temperature)
		}
		probabilities := map[string]float64{"yes": yes, "no": 1.0 - yes}
		answer, _ := common()
		confidence := Confidence(probabilities)
		answer.Type = "noul"
		answer.Noul = &yes
		answer.Probabilities = probabilities
		answer.Confidence = &confidence
		return answer, nil
	}

	probabilities := raw
	if applied {
		probabilities = TemperDistribution(raw, temperature)
	}
	best := letters[0]
	for _, letter := range letters {
		if probabilities[letter] > probabilities[best] {
			best = letter
		}
	}
	index := indexOf(letters, best)

	if question.Kind() == "score" {
		weights := make([]float64, len(letters))
		for i, letter := range letters {
			weights[i] = probabilities[letter]
		}
		score := 0.0
		for position, weight := range weights {
			score += float64(position) * weight
		}
		level := pythonStr(options[index].Description)
		legend := make(map[string]string, len(options))
		distributions := make(map[string]float64, len(letters))
		for position, letter := range letters {
			legend[strconv.Itoa(position)] = pythonStr(options[position].Description)
			distributions[strconv.Itoa(position)] = probabilities[letter]
		}
		choice := options[index].Key
		confidence := Confidence(probabilities)
		answer, _ := common()
		answer.Type = "score"
		answer.Score = &score
		answer.Level = &level
		answer.Choice = &choice
		answer.Legend = legend
		answer.Probabilities = distributions
		answer.Confidence = &confidence
		return answer, nil
	}

	keyed := make(map[string]float64, len(letters))
	for _, letter := range letters {
		keyed[options[indexOf(letters, letter)].Key] = probabilities[letter]
	}
	choice := options[index].Key
	confidence := Confidence(probabilities)
	answer, _ := common()
	answer.Type = "choice"
	answer.Choice = &choice
	answer.Probabilities = keyed
	answer.Confidence = &confidence
	return answer, nil
}

func optionKeys(options []Option) []string {
	keys := make([]string, 0, len(options))
	for _, option := range options {
		keys = append(keys, option.Key)
	}
	return keys
}

func indexOf(values []string, wanted string) int {
	for index, value := range values {
		if value == wanted {
			return index
		}
	}
	return -1
}

func boolPointer(value bool) *bool { return &value }

// numberOrNil is the value behind a pointer, or nil when there is none.
//
// The Python client returns a number or None where this port holds a *float64,
// and a map[string]any holding a pointer prints as an address and type-asserts
// to *float64 rather than float64. Every caller-visible map goes through this.
func numberOrNil(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// stringOrNil is the string behind a pointer, or nil when there is none.
//
// The same rule as numberOrNil, for the fields the Python client types as
// `str | None`: a caller-visible map holds the string, never a pointer to it.
func stringOrNil(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// PythonRepr is Python's `repr()`.
//
// It is exported for the command line, which prints the same `sampled` text and
// the same repeated-turn separator the Python command line prints, both with
// `!r`. The container forms sort a Go map's keys, which is the one place this
// spelling is not Python's, and `str()` of a container is `repr` of its members
// in both languages.
func PythonRepr(value any) string { return pythonRepr(value, true) }

// PythonStr is Python's `str()`.
//
// It is exported for the command line, which prints a question's option
// descriptions with `str` where it prints the sampled text with `repr`.
func PythonStr(value any) string { return pythonRepr(value, false) }

// pythonStr is Python's `str()`.
//
// A score's level and its legend are rendered with it, and both are part of what
// the caller reads, so it has to be Python's spelling: Python writes `inf` where
// this package's JSON writer writes `Infinity`, and a container is spelled with
// `repr` for its elements. `Answer.Level` and `Answer.Legend` carry the result.
func pythonStr(value any) string { return pythonRepr(value, false) }

// pythonRepr is `str()` at the top level and `repr()` for anything nested, which
// is what `str()` applies to a container's contents.
func pythonRepr(value any, nested bool) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case string:
		if nested {
			return pythonQuote(typed)
		}
		return typed
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case float64:
		return pythonReprFloat(typed)
	case float32:
		return pythonReprFloat(float64(typed))
	case []any:
		parts := make([]string, len(typed))
		for index, item := range typed {
			parts[index] = pythonRepr(item, true)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(typed))
		for index, item := range typed {
			parts[index] = pythonQuote(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		// Python keeps insertion order and a Go map has none, so the keys are
		// sorted. That is the one place this spelling is not Python's.
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, pythonQuote(key)+": "+pythonRepr(typed[key], true))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case fmt.Stringer:
		return typed.String()
	}
	// A type with no Python spelling: fall back to the JSON writer, which is
	// right for the scalars it covers and deterministic for the rest.
	text, err := PythonJSON(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return text
}

// pythonReprFloat is Python's `repr()` for a float. `json.dumps` writes `NaN`
// and `Infinity`; `repr` writes `nan` and `inf`.
func pythonReprFloat(value float64) string {
	switch {
	case math.IsNaN(value):
		return "nan"
	case math.IsInf(value, 1):
		return "inf"
	case math.IsInf(value, -1):
		return "-inf"
	}
	return pythonFloatString(value)
}

// pythonQuote is Python's `repr()` for a string.
//
// The quote is single, unless the string holds a single quote and no double
// quote, in which case it is double. Inside it, a backslash and the quote are
// escaped, and so is everything Python's `str.isprintable()` calls unprintable:
// the control characters, the format characters, the private-use and unassigned
// code points, and every space separator but U+0020. The predicate is the
// interpreter's own, from `data/python-printable.json`, because Go's
// `strconv.IsPrint` is the same rule against a different Unicode version and
// disagrees on the code points assigned since.
func pythonQuote(text string) string {
	quote := byte('\'')
	if strings.ContainsRune(text, '\'') && !strings.ContainsRune(text, '"') {
		quote = '"'
	}
	var out strings.Builder
	out.WriteByte(quote)
	for _, r := range text {
		switch {
		case r == rune(quote) || r == '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case r == '\n':
			out.WriteString("\\n")
		case r == '\r':
			out.WriteString("\\r")
		case r == '\t':
			out.WriteString("\\t")
		case !isPythonPrintable(r):
			switch {
			case r < 0x100:
				fmt.Fprintf(&out, "\\x%02x", r)
			case r < 0x10000:
				fmt.Fprintf(&out, "\\u%04x", r)
			default:
				fmt.Fprintf(&out, "\\U%08x", r)
			}
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte(quote)
	return out.String()
}
