package shisade

import (
	"context"
	"fmt"
	"math"
	"time"
)

// The DE-2 inference policy: read the question twice, think when unsure.
//
// DE-2 answers from a sequence of reads over the same weights rather than from
// one. `docs/READOUT-DE2.md` is the contract; this file sequences the reads
// `readout.go` provides.
//
//  1. Render the scaffold with the user turn written twice and read the codes.
//  2. If the top probability of that read is below `ThinkGate` and the question
//     has at most `ThinkOptionCap` options, render the question once more with
//     thinking on, generate a thought of at most `ThinkBudget` tokens, close it,
//     and read the codes after it. Otherwise the first read is the answer.
//
// `repeat` stops after step 1 and `direct` reads the scaffold once. The gate
// reads the raw distribution, before any calibration, as the measured policy
// did.
//
// `ReadOptions` holds the settings one question is read under, and
// `ResolveOptions` layers them: the family's defaults, then the model's, then
// one call's. The option names (`reads`, `reasoning`, `compound`) select among
// the reads the two contracts define; a combination neither defines is an error
// rather than an unmeasured read.

// Policies are the three DE-2 read sequences, from cheapest to dearest.
var Policies = []string{"direct", "repeat", "repeat-think"}

// FamilyPolicies is the policies each family's contract defines. DE-1 was
// measured on one read.
var FamilyPolicies = map[Family][]string{
	FamilyDE1: {"direct"},
	FamilyDE2: {"direct", "repeat", "repeat-think"},
}

// DefaultPolicy is the policy a family reads with when nothing selects one.
var DefaultPolicy = map[Family]string{
	FamilyDE1: "direct",
	FamilyDE2: "repeat-think",
}

const (
	// ThinkGate is the adopted gate: think when the repeated read's top
	// probability is below this.
	ThinkGate = 0.7
	// ThinkOptionCap means questions with more options than this never think.
	ThinkOptionCap = 26
	// ThinkBudget is the most tokens one thought may run to.
	ThinkBudget = 1024
)

// Answer.strategy for each way a policy can end.
const (
	StrategyDirect      = "direct"
	StrategyRepeat      = "repeat2"
	StrategyRepeatThink = "repeat2-think"
)

// ReadCounts are the two ways the user turn can be written.
var ReadCounts = []string{"single", "double"}

// OverflowStrategies are the two ways a DE-1 text choice above the letter limit
// can be handled. This port implements neither; the names are kept so a DE-1
// setting is refused by name rather than by an unknown-policy error.
var OverflowStrategies = []string{"finalist-top1", "error"}

// policyParts is each policy as (reads, reasoning). A single read that reasons
// is not one of them.
var policyParts = map[string]struct {
	reads     string
	reasoning bool
}{
	"direct":       {"single", false},
	"repeat":       {"double", false},
	"repeat-think": {"double", true},
}

var partsPolicy = map[string]string{
	"single,false": "direct",
	"double,false": "repeat",
	"double,true":  "repeat-think",
}

// ReadOptions is the settings one question is read under.
type ReadOptions struct {
	Family      Family
	Policy      string
	ThinkGate   float64
	ThinkBudget int
	Overflow    string
}

// Reads is `single` or `double`: how many times the user turn is written.
func (o ReadOptions) Reads() string { return policyParts[o.Policy].reads }

// Reasoning reports whether an unsure read is followed by a thought and a second
// read.
func (o ReadOptions) Reasoning() bool { return policyParts[o.Policy].reasoning }

// ReasoningProb is the gate, under the name the friendly API uses.
func (o ReadOptions) ReasoningProb() float64 { return o.ThinkGate }

// ReasoningLen is the budget, under the name the friendly API uses.
func (o ReadOptions) ReasoningLen() int { return o.ThinkBudget }

// Compound reports whether a DE-1 text choice above 26 options is read in two
// rounds.
func (o ReadOptions) Compound() bool {
	return o.Family == FamilyDE1 && o.Overflow == OverflowStrategies[0]
}

// OptionOverrides is one level of settings layered over a base. A nil field is
// inherited; a non-nil field sets that setting.
//
// The Python client takes these as keyword arguments, where the absence of an
// argument is what inherits. Go has no such absence for a bool or a number, so
// each is a pointer and nil is the Python `None`.
type OptionOverrides struct {
	// Policy names a whole read.
	Policy *string
	// Reads and Reasoning change one part of the inherited read.
	Reads     *string
	Reasoning *bool
	// ReasoningProb and ThinkGate are the same setting under two names.
	ReasoningProb *float64
	ThinkGate     *float64
	// ReasoningLen and ThinkBudget are the same setting under two names.
	ReasoningLen *int
	ThinkBudget  *int
	// Compound and Overflow are the same setting under two names.
	Compound *bool
	Overflow *string
}

// ResolveOptions layers one level of settings over base, or over the family's
// defaults when base is nil.
//
// Anything left nil is inherited. `Policy` names a whole read; `Reads` and
// `Reasoning` change one part of the inherited one. `Reasoning` true reads twice
// first and `Reads` "single" does not reason, because the contract's only
// thinking read follows a repeated one. ReasoningProb/ThinkGate,
// ReasoningLen/ThinkBudget and Compound/Overflow are the same settings under two
// names.
func ResolveOptions(family Family, base *ReadOptions, over OptionOverrides) (ReadOptions, error) {
	if base == nil {
		defaultPolicy, ok := DefaultPolicy[family]
		if !ok {
			return ReadOptions{}, &ReadoutError{Message: fmt.Sprintf(
				"unknown family %q; expected one of %s", string(family), familyNames())}
		}
		resolved := ReadOptions{Family: family, Policy: defaultPolicy, ThinkGate: ThinkGate, ThinkBudget: ThinkBudget}
		resolved.Overflow = OverflowStrategies[0]
		base = &resolved
	}
	if over.Policy != nil && !containsString(Policies, *over.Policy) {
		return ReadOptions{}, &ReadoutError{Message: fmt.Sprintf(
			"unknown policy %q; expected one of %v", *over.Policy, Policies)}
	}
	if over.Reads != nil && !containsString(ReadCounts, *over.Reads) {
		return ReadOptions{}, &ReadoutError{Message: fmt.Sprintf(
			"reads must be 'single' or 'double', got %q", *over.Reads)}
	}
	if over.Reads != nil && *over.Reads == "single" && over.Reasoning != nil && *over.Reasoning {
		return ReadOptions{}, &ReadoutError{Message: "reasoning=true follows a repeated read; " +
			"it cannot be combined with reads='single'"}
	}

	// `policy` names a whole read, so it replaces the inherited one rather than
	// layering over it; `reads` and `reasoning` then change one part of that.
	selected := base.Policy
	if over.Policy != nil {
		selected = *over.Policy
	}
	parts := policyParts[selected]
	count, thinks := parts.reads, parts.reasoning
	if over.Reasoning != nil {
		thinks = *over.Reasoning
		if thinks {
			count = "double"
		}
	}
	if over.Reads != nil {
		count = *over.Reads
		thinks = thinks && count == "double"
	}
	resolved := partsPolicy[count+","+boolText(thinks)]
	if over.Policy != nil && resolved != *over.Policy {
		given := make([]string, 0, 2)
		if over.Reads != nil {
			given = append(given, fmt.Sprintf("reads=%q", *over.Reads))
		}
		if over.Reasoning != nil {
			given = append(given, fmt.Sprintf("reasoning=%v", *over.Reasoning))
		}
		return ReadOptions{}, &ReadoutError{Message: fmt.Sprintf(
			"policy=%q disagrees with %s; pass one or the other", *over.Policy, joinComma(given))}
	}
	if !containsString(FamilyPolicies[family], resolved) {
		return ReadOptions{}, &ReadoutError{Message: fmt.Sprintf(
			"policy %q is not defined for %s; expected one of %v",
			resolved, string(family), FamilyPolicies[family])}
	}

	gate, err := oneOf("think_gate", over.ThinkGate, "reasoning_prob", over.ReasoningProb)
	if err != nil {
		return ReadOptions{}, err
	}
	if gate == nil {
		value := base.ThinkGate
		gate = &value
	} else if math.IsNaN(*gate) || *gate < 0.0 || *gate > 1.0 {
		return ReadOptions{}, &ReadoutError{Message: "reasoning_prob (think_gate) must be a probability"}
	}
	budget, err := oneOf("think_budget", over.ThinkBudget, "reasoning_len", over.ReasoningLen)
	if err != nil {
		return ReadOptions{}, err
	}
	if budget == nil {
		value := base.ThinkBudget
		budget = &value
	} else if *budget < 1 {
		return ReadOptions{}, &ReadoutError{Message: "reasoning_len (think_budget) must be a positive integer"}
	}

	if over.Overflow != nil && !containsString(OverflowStrategies, *over.Overflow) {
		return ReadOptions{}, &ReadoutError{Message: fmt.Sprintf(
			"overflow must be %q or %q", OverflowStrategies[0], OverflowStrategies[1])}
	}
	if over.Compound != nil && *over.Compound && family != FamilyDE1 {
		return ReadOptions{}, &ReadoutError{Message: fmt.Sprintf(
			"compound=true is not defined for %s: it reads a text choice of up to %d options in one prompt",
			string(family), MaxCodes)}
	}
	var compoundValue *string
	if over.Compound != nil {
		value := OverflowStrategies[1]
		if *over.Compound {
			value = OverflowStrategies[0]
		}
		compoundValue = &value
	}
	wide, err := oneOf("overflow", over.Overflow, "compound", compoundValue)
	if err != nil {
		return ReadOptions{}, err
	}
	overflow := base.Overflow
	if wide != nil {
		overflow = *wide
	}
	return ReadOptions{
		Family:      family,
		Policy:      resolved,
		ThinkGate:   *gate,
		ThinkBudget: *budget,
		Overflow:    overflow,
	}, nil
}

// PolicyComponent is one read of a policy, and the stage it belongs to.
type PolicyComponent struct {
	// Stage is StrategyDirect, StrategyRepeat or "think".
	Stage string
	Read  CodeRead
}

// PolicyRead is every read one question cost under a policy; the last one is the
// answer.
type PolicyRead struct {
	Strategy   string
	Components []PolicyComponent
	Thought    *Thought
	ElapsedMS  float64
}

// Read is the read the answer came from.
func (p PolicyRead) Read() CodeRead { return p.Components[len(p.Components)-1].Read }

// Requests is every HTTP request the answer cost, counting the thought's own
// request.
func (p PolicyRead) Requests() int {
	total := 0
	for _, component := range p.Components {
		total += component.Read.Requests
	}
	if p.Thought != nil {
		total++
	}
	return total
}

// PromptTokens is every prompt token the policy sent, a two-block read counting
// its prompt twice.
func (p PolicyRead) PromptTokens() int {
	total := 0
	for _, component := range p.Components {
		total += component.Read.PromptTokens
	}
	if p.Thought != nil {
		total += p.Thought.PromptTokens
	}
	return total
}

// LogicalReads is how many times the question was read, as opposed to how many
// HTTP requests that took.
func (p PolicyRead) LogicalReads() int { return len(p.Components) }

// ThoughtTokens is the length of the thought, zero when there was none.
func (p PolicyRead) ThoughtTokens() int {
	if p.Thought == nil {
		return 0
	}
	return len(p.Thought.TokenIDs)
}

// MissingFromTop is the final read's codes the top-k did not carry.
func (p PolicyRead) MissingFromTop() []string {
	return append([]string{}, p.Read().MissingFromTop...)
}

// Raw is the policy read as the plain data the recorded fixtures carry.
func (p PolicyRead) Raw(debug bool) map[string]any {
	stages := make([]any, 0, len(p.Components))
	for _, component := range p.Components {
		item := map[string]any{
			"stage":            component.Stage,
			"logprobs":         copyFloatMap(component.Read.Logprobs),
			"probabilities":    copyFloatMap(component.Read.Probabilities),
			"requests":         component.Read.Requests,
			"prompt_tokens":    component.Read.PromptTokens,
			"missing_from_top": append([]string{}, component.Read.MissingFromTop...),
			"ranks":            copyIntMap(component.Read.Ranks),
		}
		if debug {
			item["prompt"] = component.Read.Prompt
			item["top_logprobs"] = copyOptionalFloatMap(component.Read.TopLogprobs)
		}
		stages = append(stages, item)
	}
	final := p.Read()
	out := map[string]any{
		"strategy":         p.Strategy,
		"logprobs":         copyFloatMap(final.Logprobs),
		"probabilities":    copyFloatMap(final.Probabilities),
		"requests":         p.Requests(),
		"prompt_tokens":    p.PromptTokens(),
		"logical_reads":    p.LogicalReads(),
		"missing_from_top": append([]string{}, final.MissingFromTop...),
		"ranks":            copyIntMap(final.Ranks),
		"sampled":          final.Sampled,
		"components":       stages,
	}
	if p.Thought != nil {
		out["thought_tokens"] = p.ThoughtTokens()
		out["thought_closed"] = p.Thought.Closed
		if debug {
			out["thought"] = p.Thought.Text
		}
	}
	return out
}

// ReadPolicy answers one text question under a DE-2 policy.
func ReadPolicy(ctx context.Context, readout *Readout, state any, question Question,
	options ReadOptions, debug bool) (PolicyRead, error) {
	if !containsString(Policies, options.Policy) {
		return PolicyRead{}, &ReadoutError{Message: fmt.Sprintf(
			"unknown policy %q; expected one of %v", options.Policy, Policies)}
	}
	started := time.Now()
	// Only a text choice goes past the letters; a noul or a score keeps the 26 cap.
	limit := MaxOptions
	if question.Kind() == "choice" {
		limit = MaxCodes
	}
	if err := question.Validate(limit); err != nil {
		return PolicyRead{}, err
	}
	slots, err := readout.CodeSlots(len(question.Options()))
	if err != nil {
		return PolicyRead{}, err
	}

	done := func(strategy string, components []PolicyComponent, thought *Thought) PolicyRead {
		return PolicyRead{
			Strategy:   strategy,
			Components: components,
			Thought:    thought,
			ElapsedMS:  round2(float64(time.Since(started).Nanoseconds()) / 1e6),
		}
	}

	if options.Policy == "direct" {
		prompt, err := readout.Render(state, question, RenderOptions{Repeat: 1, OptionLimit: limit})
		if err != nil {
			return PolicyRead{}, err
		}
		read, err := readout.ReadCodes(ctx, prompt, slots, debug)
		if err != nil {
			return PolicyRead{}, err
		}
		return done(StrategyDirect, []PolicyComponent{{Stage: StrategyDirect, Read: read}}, nil), nil
	}

	prompt, err := readout.Render(state, question, RenderOptions{Repeat: 2, OptionLimit: limit})
	if err != nil {
		return PolicyRead{}, err
	}
	first, err := readout.ReadCodes(ctx, prompt, slots, debug)
	if err != nil {
		return PolicyRead{}, err
	}
	unsure := maxProbability(first.Probabilities) < options.ThinkGate
	if options.Policy == "repeat" || !unsure || len(slots) > ThinkOptionCap {
		return done(StrategyRepeat, []PolicyComponent{{Stage: StrategyRepeat, Read: first}}, nil), nil
	}

	thinkingPrompt, err := readout.Render(state, question, RenderOptions{Thinking: true, OptionLimit: limit})
	if err != nil {
		return PolicyRead{}, err
	}
	thought, err := readout.Think(ctx, thinkingPrompt, options.ThinkBudget, debug)
	if err != nil {
		return PolicyRead{}, err
	}
	// The model's own close token may be missing when the budget cut the thought,
	// so the close is always written here and the codes are read after it.
	prefixID, err := readout.AnswerPrefixID()
	if err != nil {
		return PolicyRead{}, err
	}
	after := make([]int, 0, len(thought.PromptIDs)+len(thought.TokenIDs)+1)
	after = append(after, thought.PromptIDs...)
	after = append(after, thought.TokenIDs...)
	after = append(after, prefixID)
	second, err := readout.ReadCodesAfterThought(ctx, after, slots, debug)
	if err != nil {
		return PolicyRead{}, err
	}
	return done(StrategyRepeatThink, []PolicyComponent{
		{Stage: StrategyRepeat, Read: first},
		{Stage: "think", Read: second},
	}, &thought), nil
}

func maxProbability(probabilities map[string]float64) float64 {
	peak := math.Inf(-1)
	for _, value := range probabilities {
		peak = math.Max(peak, value)
	}
	return peak
}

func round2(value float64) float64 { return math.Round(value*100) / 100 }

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func joinComma(parts []string) string {
	out := ""
	for index, part := range parts {
		if index != 0 {
			out += ", "
		}
		out += part
	}
	return out
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// oneOf is the value of a setting that has two names; both may be given if they
// agree.
func oneOf[T comparable](oldName string, old *T, newName string, new *T) (*T, error) {
	if old != nil && new != nil && *old != *new {
		return nil, &ReadoutError{Message: fmt.Sprintf(
			"%s=%v and %s=%v set the same thing; pass one", newName, *new, oldName, *old)}
	}
	if new != nil {
		return new, nil
	}
	return old, nil
}

func copyIntMap(values map[string]int) map[string]int {
	copied := make(map[string]int, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func copyOptionalFloatMap(values map[string]*float64) map[string]*float64 {
	copied := make(map[string]*float64, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}
