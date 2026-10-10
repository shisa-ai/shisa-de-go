package shisade

import (
	"fmt"
	"reflect"
	"strings"
	"unicode"
)

// Option is one answer option: the key the caller gets back, and what the model
// is shown for it.
//
// A Choice holds the descriptions it was given and renders them through
// RenderOption when Options is called, so a label with no description is shown
// as its key rather than as an empty string.
type Option struct {
	Key         string
	Description any
}

// Question is one typed question.
//
// The three kinds are Noul, Choice and Score. A Question knows the answer keys
// in presentation order, which is what assigns the answer codes: option i is
// shown code i, so the order is part of the question, not a detail of it.
type Question interface {
	// Instructions is the criterion the model applies.
	Instructions() string
	// Kind is "noul", "choice" or "score".
	Kind() string
	// Options are the answer keys and rendered descriptions, in presentation
	// order. The first is shown as A.
	Options() []Option
	// Validate rejects a question the readout cannot render, with at most
	// optionLimit options. A non-positive optionLimit means the letter limit,
	// the same default Render and Payload use.
	Validate(optionLimit int) error
}

// isPythonSpace reports whether a rune is whitespace to Python's `str.strip()`.
//
// Python strips a character whose Unicode general category is Zs or whose
// bidirectional class is WS, B or S. Go's unicode.IsSpace is that set minus the
// U+001C-U+001F separators, which Python's class B includes, so a description of
// "\u001c" is blank to Python and not to Go. The two sets agree everywhere else.
func isPythonSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1C && r <= 0x1F)
}

// trimPythonSpace is Python's `str.strip()`.
func trimPythonSpace(text string) string {
	return strings.TrimFunc(text, isPythonSpace)
}

// TrimPythonSpace is Python's `str.strip()`.
//
// It differs from `strings.TrimSpace` on U+001C-U+001F, which Python's
// `string.whitespace` includes and Go's `unicode.IsSpace` does not. A caller
// parsing text the Python client would parse has to use this one to agree with
// it on a string that ends in one of those four characters.
func TrimPythonSpace(text string) string {
	return trimPythonSpace(text)
}

// isNilDescription reports whether a description is absent.
//
// Python has one None; Go has a typed nil as well. A caller who passes a nil
// pointer means the same thing as None, and it must not reach the prompt as
// "null", which leaves the model a description it cannot use. A nil map or
// slice is not absent: Python's {} and [] are real descriptions, and PythonJSON
// writes them.
func isNilDescription(description any) bool {
	if description == nil {
		return true
	}
	reflected := reflect.ValueOf(description)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

// RenderOption is what the model is shown for one option.
//
// Some label sets carry the label in the key and no description. Showing an
// empty description leaves the model with indistinguishable options, so the key
// is shown instead. This follows the renderer fix recorded in the research
// notes, where discarding keys cost 0.129 to 0.948 accuracy on `dbpedia14`.
func RenderOption(key string, description any) any {
	if isNilDescription(description) {
		return key
	}
	if text, ok := description.(string); ok && trimPythonSpace(text) == "" {
		return key
	}
	return description
}

func validateQuestion(kind, instructions string, keys []string, optionLimit int) error {
	if trimPythonSpace(instructions) == "" {
		return &QuestionError{Message: "instructions must be a non-empty question"}
	}
	if optionLimit <= 0 {
		// The same default Payload and Render use: a zero limit is the letter
		// limit, not a limit of zero.
		optionLimit = MaxOptions
	}
	if len(keys) < 2 {
		return &QuestionError{Message: fmt.Sprintf(
			"%s question needs at least two answers, got %d", kind, len(keys))}
	}
	if len(keys) > optionLimit {
		return &QuestionError{Message: fmt.Sprintf(
			"%s question offers %d answers; the readout binds one code per answer and this "+
				"contract supports at most %d per prompt", kind, len(keys), optionLimit)}
	}
	// Python's criteria are a dict, so a repeated key cannot survive into a read.
	// This port takes a slice, because a Go map has no order and the order is what
	// assigns the answer codes; a repeated key would score two codes into one
	// entry and the returned distribution would no longer sum to one.
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			return &QuestionError{Message: fmt.Sprintf(
				"%s question repeats the answer key %q; a repeated key would read two codes "+
					"into one option, so the distribution would not sum to one", kind, key)}
		}
		seen[key] = true
	}
	return nil
}

// Noul is a yes/no question. The answer is the probability that it is yes.
type Noul struct {
	instructions string
}

// NewNoul builds a yes/no question.
func NewNoul(instructions string) Noul {
	return Noul{instructions: instructions}
}

func (Noul) Kind() string           { return "noul" }
func (n Noul) Instructions() string { return n.instructions }
func (Noul) Options() []Option {
	return []Option{{Key: "yes", Description: "Yes"}, {Key: "no", Description: "No"}}
}
func (n Noul) Validate(limit int) error {
	return validateQuestion(n.Kind(), n.instructions, []string{"yes", "no"}, limit)
}

// Choice is one option out of a set. The answer is the chosen key plus the
// distribution over the keys.
type Choice struct {
	instructions string
	criteria     []Option
}

// NewChoice builds a choice. The criteria's order is the presentation order, and
// therefore decides which code each option is shown as: the first is A. A Go map
// cannot carry that order, which is why this takes a slice.
func NewChoice(instructions string, criteria []Option) Choice {
	return Choice{instructions: instructions, criteria: criteria}
}

func (Choice) Kind() string           { return "choice" }
func (c Choice) Instructions() string { return c.instructions }

func (c Choice) Options() []Option {
	rendered := make([]Option, len(c.criteria))
	for i, option := range c.criteria {
		rendered[i] = Option{Key: option.Key, Description: RenderOption(option.Key, option.Description)}
	}
	return rendered
}

func (c Choice) Validate(limit int) error {
	return validateQuestion(c.Kind(), c.instructions, optionKeys(c.criteria), limit)
}

// Score is a position along ordered levels. The answer is a probability-weighted,
// zero-based position plus the distribution over the positions.
type Score struct {
	instructions string
	levels       []any
}

// NewScore builds an ordered scale.
func NewScore(instructions string, levels []any) Score {
	return Score{instructions: instructions, levels: levels}
}

func (Score) Kind() string           { return "score" }
func (s Score) Instructions() string { return s.instructions }

// Options returns the levels keyed by their zero-based position.
//
// A level is shown exactly as it was given: unlike a Choice, a blank level is
// not replaced by its key, because the key is the position and the model is
// meant to read the level's text.
func (s Score) Options() []Option {
	options := make([]Option, len(s.levels))
	for i, level := range s.levels {
		options[i] = Option{Key: fmt.Sprint(i), Description: level}
	}
	return options
}

func (s Score) Validate(limit int) error {
	keys := make([]string, len(s.levels))
	for i := range s.levels {
		keys[i] = fmt.Sprint(i)
	}
	return validateQuestion(s.Kind(), s.instructions, keys, limit)
}
