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
			"confidence":    a.Confidence,
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

// ToDict is everything, including the fields TypeSafe does not return.
func (a Answer) ToDict() map[string]any {
	out := map[string]any{
		"type":          a.Type,
		"value":         a.Value(),
		"confidence":    a.Confidence,
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

func (m *multiLabelHead) noulFor(label string) Noul {
	if strings.Contains(m.template, "{label}") {
		return NewNoul(strings.ReplaceAll(m.template, "{label}", label))
	}
	return NewNoul(m.template + " " + label)
}

// headWork is one question to ask, and the head name it answers under.
type headWork struct {
	head     string
	question Question
}

// questionFromHead turns a head's label spec into a question.
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
	if spec.Options == nil {
		return nil, &QuestionError{Message: fmt.Sprintf(
			"head %q: expected options or levels", head)}
	}
	if len(spec.Options) == 0 {
		return nil, &QuestionError{Message: fmt.Sprintf(
			"head %q: expected options or levels", head)}
	}
	if spec.MultiLabel {
		labels := make([]string, 0, len(spec.Options))
		for _, option := range spec.Options {
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
			"%q names neither DE-1 nor DE-2, so it is read as %q; pass Family to say which contract it was trained for",
			model, string(family)))
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
			"the bundled %s calibration is not applied to %q: %s. Answers are raw probabilities; pass Calibration to apply a record you vouch for",
			string(family), model, strings.Join(reasons, "; ")))
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
		value := strings.TrimSpace(os.Getenv(name))
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
			"%q carries no DE-1 or DE-2 slug; %q was assumed, not detected", m.Model, string(m.Family))
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
			slotReport := map[string]any{"count": len(slots)}
			for _, slot := range slots[:2] {
				slotReport[slot.Code] = slot.TokenID
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
					"confidence":    answer.Confidence,
					"probabilities": answer.Probabilities,
				}
				if answer.Noul != nil && *answer.Noul >= threshold {
					entries = append(entries, entry)
				}
				decision.Raw[key] = map[string]any{"noul": answer.Noul, "requests": answer.Requests}
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
				"confidence":    answer.Confidence,
				"probabilities": answer.Probabilities,
			}
			if answer.ScoreSemantics != "option-softmax" {
				value["strategy"] = answer.Strategy
				value["score_semantics"] = answer.ScoreSemantics
				value["calibrated"] = false
			}
			decision.Values[head] = value
		case options.IncludeConfidence:
			value := map[string]any{"label": answer.Label(), "confidence": answer.Confidence}
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

// Decide asks typed questions and get typed answers.
//
// `questions` maps a question id to a Noul, Choice or Score. The Values map
// returns each answer's primary value; Answers returns the full answer.
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
				work = append(work, headWork{head: head + ":" + label, question: multi.noulFor(label)})
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

// pythonStr is Python's `str()` for the value kinds a level can hold.
func pythonStr(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case string:
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
	case float64:
		if typed == math.Trunc(typed) && math.Abs(typed) < 1e16 {
			return pythonFloatString(typed)
		}
		return pythonFloatString(typed)
	case float32:
		return pythonFloatString(float64(typed))
	case fmt.Stringer:
		return typed.String()
	default:
		text, err := PythonJSON(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return text
	}
}
