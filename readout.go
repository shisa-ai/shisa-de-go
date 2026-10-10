package shisade

import (
	"math"
	"sort"
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
// which is the codebook's own order -- so the result is reproducible. A Go map
// iterates in a randomized order, and summing floats in a different order
// changes the last bits; the Python client sums in codebook order, so this
// order also matches its arithmetic for code keys. The returned map carries no
// order, so a caller that needs the option order keeps its own list of codes.
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
