package shisade

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// The identity tests.
//
// One answer can cost several requests, and each response says which model
// answered it. These tests are about the rule that the responses of one answer
// have to name one checkpoint, and that a checkpoint named only by a fallback or
// only by a thought still reaches the answer.
//
// Everything here is a stub endpoint: the ids are the fixtures' own spellings,
// not a measurement of any server.

// withServedModel puts a model id into a completion body, which is what a server
// behind a gateway holding an alias answers with. An empty id leaves the body as
// it is, which is the response of a server that reports nothing.
func withServedModel(body, model string) string {
	if model == "" {
		return body
	}
	return `{"model":` + strconv.Quote(model) + `,` + strings.TrimPrefix(body, "{")
}

// codeReadWithModel is one code read's response for the first count codes, with
// the model id the server reports.
func codeReadWithModel(t *testing.T, count int, model string) string {
	t.Helper()
	return withServedModel(codeReadBody(flatLogprobs(t, count)), model)
}

// TestServedIdentity pins which reported ids one answer may combine: identical
// spellings, spellings of one released checkpoint, and a response that reports
// nothing. Anything else is two checkpoints.
func TestServedIdentity(t *testing.T) {
	cases := []struct {
		name     string
		first    string
		second   string
		ok       bool
		recorded string
	}{
		{"neither reports an id", "", "", true, ""},
		{"one reports an id", "shisa-ai/shisa-de-2.1", "", true, "shisa-ai/shisa-de-2.1"},
		{"the same id twice", "shisa-ai/shisa-de-2.1", "shisa-ai/shisa-de-2.1", true,
			"shisa-ai/shisa-de-2.1"},
		{"a quantized repo of the same release", "shisa-ai/shisa-de-2.1",
			"shisa-ai/shisa-de-2.1-FP8-dynamic", true, "shisa-ai/shisa-de-2.1"},
		{"a local directory of the same release", "shisa-ai/shisa-de-2.1",
			"/models/shisa-de-2.1", true, "shisa-ai/shisa-de-2.1"},
		{"another case of the same release", "shisa-ai/shisa-de-2.1",
			"SHISA-AI/Shisa-De-2.1", true, "shisa-ai/shisa-de-2.1"},
		{"two releases", "shisa-ai/shisa-de-2", "shisa-ai/shisa-de-2.1", false,
			"shisa-ai/shisa-de-2"},
		{"two training arms", "de2-v4-lr5e5-e3-s7", "de2-v5-lr5e5-e3-s7", false,
			"de2-v4-lr5e5-e3-s7"},
		{"a release and an arm", "shisa-ai/shisa-de-2.1", "de2-v4-lr5e5-e3-s7", false,
			"shisa-ai/shisa-de-2.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var identity servedIdentity
			if err := identity.note(tc.first, "the first read"); err != nil {
				t.Fatalf("the first id was refused: %v", err)
			}
			err := identity.note(tc.second, "the second read")
			if tc.ok {
				if err != nil {
					t.Fatalf("note(%q) after %q: %v", tc.second, tc.first, err)
				}
			} else {
				if err == nil {
					t.Fatalf("note(%q) after %q was accepted", tc.second, tc.first)
				}
				var readoutError *ReadoutError
				if !errors.As(err, &readoutError) {
					t.Fatalf("the refusal is %T, want a ReadoutError", err)
				}
				message := err.Error()
				if !strings.Contains(message, tc.second) || !strings.Contains(message, tc.first) {
					t.Errorf("the refusal names neither id: %s", message)
				}
				if !strings.Contains(message, "the second read") {
					t.Errorf("the refusal does not name the read that answered: %s", message)
				}
			}
			if identity.model != tc.recorded {
				t.Errorf("recorded %q, want %q: the first id is the one kept",
					identity.model, tc.recorded)
			}
		})
	}
}

// TestCodeReadKeepsIdentityAcrossBlocks pins the multi-block read: 129 options
// are sent as two requests, and both responses name the checkpoint the answer
// came from.
func TestCodeReadKeepsIdentityAcrossBlocks(t *testing.T) {
	readout, server := newPolicyReadout(t, 129,
		codeReadWithModel(t, 129, "shisa-ai/shisa-de-2.1"),
		codeReadWithModel(t, 129, "shisa-ai/shisa-de-2.1-FP8-dynamic"))
	slots := testSlots(t, readout, 129)
	read, err := readout.ReadCodes(context.Background(), AnswerPrefix, slots, false)
	if err != nil {
		t.Fatalf("ReadCodes: %v", err)
	}
	if server.at != 2 {
		t.Fatalf("the read spent %d requests, want 2: it is not the two-block case", server.at)
	}
	if read.Requests != 2 {
		t.Errorf("the read reports %d requests, want 2", read.Requests)
	}
	if read.ServedModel != "shisa-ai/shisa-de-2.1" {
		t.Errorf("ServedModel = %q, want the first response's spelling", read.ServedModel)
	}
}

// TestCodeReadRefusesBlocksFromTwoCheckpoints is the block conflict: the second
// block was answered by another checkpoint, so the two halves of the
// distribution do not describe one model.
func TestCodeReadRefusesBlocksFromTwoCheckpoints(t *testing.T) {
	readout, _ := newPolicyReadout(t, 129,
		codeReadWithModel(t, 129, "shisa-ai/shisa-de-2"),
		codeReadWithModel(t, 129, "shisa-ai/shisa-de-2.1"))
	slots := testSlots(t, readout, 129)
	_, err := readout.ReadCodes(context.Background(), AnswerPrefix, slots, false)
	if err == nil {
		t.Fatal("a two-block read answered by two checkpoints was accepted")
	}
	var readoutError *ReadoutError
	if !errors.As(err, &readoutError) {
		t.Fatalf("the refusal is %T, want a ReadoutError", err)
	}
	if !strings.Contains(err.Error(), "shisa-ai/shisa-de-2.1") ||
		!strings.Contains(err.Error(), "shisa-ai/shisa-de-2") {
		t.Errorf("the refusal names neither checkpoint: %v", err)
	}
}

// TestCodeReadTakesTheFallbacksIdentity pins a read whose top-k reported no model
// and whose fallback did: the fallback answered a code of the same distribution,
// so its id is the read's.
func TestCodeReadTakesTheFallbacksIdentity(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2,
		// The first code only, so the second is read by the fallback.
		`{"choices":[{"text":"A","logprobs":{"top_logprobs":[{"token_id:1000":-0.1}]}}],`+
			`"usage":{"prompt_tokens":{{prompt_tokens}}}}`,
		withServedModel(`{"choices":[{"text":"","prompt_logprobs":[null,`+
			`{"1001":{"logprob":-0.7,"rank":3}}]}],"usage":{"prompt_tokens":{{prompt_tokens}}}}`,
			"shisa-ai/shisa-de-2.1"))
	slots := testSlots(t, readout, 2)
	read, err := readout.ReadCodes(context.Background(), AnswerPrefix, slots, false)
	if err != nil {
		t.Fatalf("ReadCodes: %v", err)
	}
	if len(read.MissingFromTop) != 1 {
		t.Fatalf("missing from top = %v, want the one fallback code", read.MissingFromTop)
	}
	if read.ServedModel != "shisa-ai/shisa-de-2.1" {
		t.Errorf("ServedModel = %q, want the fallback's id", read.ServedModel)
	}
}

// TestCodeReadRefusesAFallbackFromAnotherCheckpoint is the fallback conflict: the
// code was scored by a checkpoint that did not answer the read.
func TestCodeReadRefusesAFallbackFromAnotherCheckpoint(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2,
		withServedModel(`{"choices":[{"text":"A","logprobs":{"top_logprobs":[`+
			`{"token_id:1000":-0.1}]}}],"usage":{"prompt_tokens":{{prompt_tokens}}}}`,
			"shisa-ai/shisa-de-2"),
		withServedModel(`{"choices":[{"text":"","prompt_logprobs":[null,`+
			`{"1001":{"logprob":-0.7,"rank":3}}]}],"usage":{"prompt_tokens":{{prompt_tokens}}}}`,
			"shisa-ai/shisa-de-2.1"))
	slots := testSlots(t, readout, 2)
	_, err := readout.ReadCodes(context.Background(), AnswerPrefix, slots, false)
	if err == nil {
		t.Fatal("a read whose fallback named another checkpoint was accepted")
	}
	if !strings.Contains(err.Error(), "code fallback") {
		t.Errorf("the refusal does not say which request answered: %v", err)
	}
}

// TestThinkRecordsTheServedModel pins that a thought carries the id its own
// response reported, which is the only way a policy can compare it with the reads
// on either side.
func TestThinkRecordsTheServedModel(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2,
		withServedModel(thinkBody(500, 501), "shisa-ai/shisa-de-2.1"))
	thought, err := readout.Think(context.Background(), AnswerPrefix, 16, false)
	if err != nil {
		t.Fatalf("Think: %v", err)
	}
	if thought.ServedModel != "shisa-ai/shisa-de-2.1" {
		t.Errorf("Thought.ServedModel = %q, want the response's id", thought.ServedModel)
	}
}

// TestReadPolicyTakesAThoughtsIdentity pins the propagation: neither code read
// named a checkpoint and the thought did, so the answer carries the thought's id.
func TestReadPolicyTakesAThoughtsIdentity(t *testing.T) {
	readout, server := newPolicyReadout(t, 2,
		codeReadWithModel(t, 2, ""),
		withServedModel(thinkBody(500, 501), "shisa-ai/shisa-de-2.1"),
		codeReadWithModel(t, 2, ""))
	read, err := ReadPolicy(context.Background(), readout,
		map[string]any{"evidence": "x"}, NewNoul("Is this urgent?"), thinkOptions(), false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if server.at != 3 {
		t.Fatalf("the policy spent %d requests, want 3", server.at)
	}
	if read.Strategy != StrategyRepeatThink {
		t.Fatalf("strategy = %q, want %q", read.Strategy, StrategyRepeatThink)
	}
	if read.Read().ServedModel != "shisa-ai/shisa-de-2.1" {
		t.Errorf("the answer's ServedModel = %q, want the thought's id",
			read.Read().ServedModel)
	}
	if read.Thought == nil || read.Thought.ServedModel != "shisa-ai/shisa-de-2.1" {
		t.Errorf("the thought does not carry its own id: %+v", read.Thought)
	}
}

// TestReadPolicyCarriesAFirstReadsIdentity pins the same propagation from the
// repeated read: a final read that reports nothing is still that checkpoint's
// answer.
func TestReadPolicyCarriesAFirstReadsIdentity(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2,
		codeReadWithModel(t, 2, "shisa-ai/shisa-de-2.1"),
		thinkBody(500, 501),
		codeReadWithModel(t, 2, ""))
	read, err := ReadPolicy(context.Background(), readout,
		map[string]any{"evidence": "x"}, NewNoul("Is this urgent?"), thinkOptions(), false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if read.Read().ServedModel != "shisa-ai/shisa-de-2.1" {
		t.Errorf("the answer's ServedModel = %q, want the repeated read's id",
			read.Read().ServedModel)
	}
}

// TestReadPolicyAcceptsEquivalentSpellings pins that one checkpoint under two
// spellings is one answer, and that the final read keeps its own spelling.
func TestReadPolicyAcceptsEquivalentSpellings(t *testing.T) {
	readout, _ := newPolicyReadout(t, 2,
		codeReadWithModel(t, 2, "shisa-ai/shisa-de-2.1"),
		withServedModel(thinkBody(500, 501), "shisa-ai/shisa-de-2.1-FP8-dynamic"),
		codeReadWithModel(t, 2, "/models/shisa-de-2.1"))
	read, err := ReadPolicy(context.Background(), readout,
		map[string]any{"evidence": "x"}, NewNoul("Is this urgent?"), thinkOptions(), false)
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if read.Read().ServedModel != "/models/shisa-de-2.1" {
		t.Errorf("the answer's ServedModel = %q, want the final read's own spelling",
			read.Read().ServedModel)
	}
}

// TestReadPolicyRefusesAThoughtFromAnotherCheckpoint pins that a thought
// generated by another checkpoint stops the read before the read after it is
// spent: continuing it would score one model's thought with another's codes.
func TestReadPolicyRefusesAThoughtFromAnotherCheckpoint(t *testing.T) {
	readout, server := newPolicyReadout(t, 2,
		codeReadWithModel(t, 2, "shisa-ai/shisa-de-2"),
		withServedModel(thinkBody(500, 501), "shisa-ai/shisa-de-2.1"))
	_, err := ReadPolicy(context.Background(), readout,
		map[string]any{"evidence": "x"}, NewNoul("Is this urgent?"), thinkOptions(), false)
	if err == nil {
		t.Fatal("a thought from another checkpoint was accepted")
	}
	var readoutError *ReadoutError
	if !errors.As(err, &readoutError) {
		t.Fatalf("the refusal is %T, want a ReadoutError", err)
	}
	if !strings.Contains(err.Error(), "thinking read") {
		t.Errorf("the refusal does not name the thinking read: %v", err)
	}
	if server.at != 2 {
		t.Errorf("the refused policy spent %d requests, want 2: the final read "+
			"should not have been sent", server.at)
	}
}

// TestReadPolicyRefusesAFinalReadFromAnotherCheckpoint is the other conflict: the
// thought and the repeated read agree and the read after the thought does not.
func TestReadPolicyRefusesAFinalReadFromAnotherCheckpoint(t *testing.T) {
	readout, server := newPolicyReadout(t, 2,
		codeReadWithModel(t, 2, "shisa-ai/shisa-de-2"),
		withServedModel(thinkBody(500, 501), "shisa-ai/shisa-de-2"),
		codeReadWithModel(t, 2, "shisa-ai/shisa-de-2.1"))
	_, err := ReadPolicy(context.Background(), readout,
		map[string]any{"evidence": "x"}, NewNoul("Is this urgent?"), thinkOptions(), false)
	if err == nil {
		t.Fatal("an answer read from two checkpoints was accepted")
	}
	if !strings.Contains(err.Error(), "read after the thought") {
		t.Errorf("the refusal does not name the read that answered: %v", err)
	}
	if server.at != 3 {
		t.Errorf("the policy spent %d requests, want 3", server.at)
	}
}

// TestReadoutKeepsNoIdentityBetweenCalls pins that the identity is per call: two
// reads against two checkpoints are two answers, not one conflict. A cached id
// would fail the second read for a difference that is not in it.
func TestReadoutKeepsNoIdentityBetweenCalls(t *testing.T) {
	readout, server := newPolicyReadout(t, 2,
		codeReadWithModel(t, 2, "shisa-ai/shisa-de-2"),
		codeReadWithModel(t, 2, "shisa-ai/shisa-de-2.1"))
	options := ReadOptions{Family: FamilyDE2, Policy: "direct"}
	for index, want := range []string{"shisa-ai/shisa-de-2", "shisa-ai/shisa-de-2.1"} {
		read, err := ReadPolicy(context.Background(), readout,
			map[string]any{"evidence": "x"}, NewNoul("Is this urgent?"), options, false)
		if err != nil {
			t.Fatalf("read %d: ReadPolicy: %v", index+1, err)
		}
		if read.Read().ServedModel != want {
			t.Errorf("read %d reports %q, want %q", index+1, read.Read().ServedModel, want)
		}
	}
	if server.at != 2 {
		t.Errorf("the two reads spent %d requests, want 2", server.at)
	}
}

// thinkOptions is the unsure policy the identity tests read under.
func thinkOptions() ReadOptions {
	return ReadOptions{Family: FamilyDE2, Policy: "repeat-think",
		ThinkGate: ThinkGate, ThinkBudget: ThinkBudget}
}
