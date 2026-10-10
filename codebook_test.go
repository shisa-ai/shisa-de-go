package shisade

import (
	"errors"
	"fmt"
	"testing"
)

// TestCodebookMatchesTheAudit checks the shipped file against the structure
// docs/READOUT-DE2.md section 1 describes: A to Z, then the uppercase pairs in
// lexical order that pass the single-token answer-boundary audit, where GZ is
// the one that fails.
func TestCodebookMatchesTheAudit(t *testing.T) {
	codes, err := Codebook()
	if err != nil {
		t.Fatalf("Codebook: %v", err)
	}
	if len(codes) != MaxCodes {
		t.Fatalf("len(codes) = %d, want %d", len(codes), MaxCodes)
	}

	seen := map[string]bool{}
	for i, code := range codes {
		if seen[code] {
			t.Errorf("codes[%d] = %q repeats", i, code)
		}
		seen[code] = true
	}

	want := make([]string, 0, MaxCodes)
	for i := 0; i < MaxOptions; i++ {
		want = append(want, string(Letters[i]))
	}
	for first := 'A'; first <= 'Z'; first++ {
		for second := 'A'; second <= 'Z'; second++ {
			pair := fmt.Sprintf("%c%c", first, second)
			if pair == "GZ" {
				continue
			}
			if len(want) == MaxCodes {
				break
			}
			want = append(want, pair)
		}
		if len(want) == MaxCodes {
			break
		}
	}
	if len(want) != MaxCodes {
		t.Fatalf("the described audit yields %d codes, want %d", len(want), MaxCodes)
	}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("codes[%d] = %q, want %q (last code is %q)", i, codes[i], want[i], codes[MaxCodes-1])
		}
	}
	if last := codes[MaxCodes-1]; last != "IW" {
		t.Errorf("the last code is %q, want %q", last, "IW")
	}
}

func TestCodesFor(t *testing.T) {
	cases := []struct {
		count int
		first string
		last  string
	}{
		{0, "", ""},
		{1, "A", "A"},
		{2, "A", "B"},
		{MaxOptions, "A", "Z"},
		{MaxOptions + 1, "A", "AA"},
		{MaxCodes, "A", "IW"},
	}
	for _, tc := range cases {
		got, err := CodesFor(tc.count)
		if err != nil {
			t.Errorf("CodesFor(%d): %v", tc.count, err)
			continue
		}
		if len(got) != tc.count {
			t.Errorf("CodesFor(%d) returned %d codes", tc.count, len(got))
			continue
		}
		if tc.count == 0 {
			continue
		}
		if got[0] != tc.first {
			t.Errorf("CodesFor(%d) starts on %q, want %q", tc.count, got[0], tc.first)
		}
		if got[len(got)-1] != tc.last {
			t.Errorf("CodesFor(%d) ends on %q, want %q", tc.count, got[len(got)-1], tc.last)
		}
	}
}

func TestCodesForIsAPrefixOfTheCodebook(t *testing.T) {
	codes, err := Codebook()
	if err != nil {
		t.Fatalf("Codebook: %v", err)
	}
	for count := 2; count <= MaxCodes; count++ {
		got, err := CodesFor(count)
		if err != nil {
			t.Fatalf("CodesFor(%d): %v", count, err)
		}
		for i := range got {
			if got[i] != codes[i] {
				t.Fatalf("CodesFor(%d)[%d] = %q, but the codebook has %q", count, i, got[i], codes[i])
			}
		}
	}
}

func TestCodesForRejectsOutOfRange(t *testing.T) {
	for _, count := range []int{-1, MaxCodes + 1, 677} {
		_, err := CodesFor(count)
		var questionErr *QuestionError
		if !errors.As(err, &questionErr) {
			t.Errorf("CodesFor(%d) returned %v, want a *QuestionError", count, err)
		}
	}
}
