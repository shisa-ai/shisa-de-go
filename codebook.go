// Package shisade is a Go client for served Shisa DE decision models: typed
// questions in, typed answers out.
//
// It sends a question to an OpenAI-compatible server, reads the distribution
// over the option codes at the answer position, and returns the answer with its
// probabilities and provenance. It never loads weights.
//
// This port implements the DE-2 contract, de2-codebook-v1, specified in
// docs/READOUT-DE2.md. DE-1 is not implemented: a model that resolves to DE-1
// is an error, not an assumption, because reading a checkpoint through the
// other family's contract returns answers nobody has measured.
package shisade

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Letters are the answer codes for the first 26 options, and the first 26
// entries of the DE-2 codebook.
const Letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// MaxOptions is the letter-slot limit: one answer code per option, so at most
// this many options in a DE-1 prompt and in a DE-2 noul, score or image
// question.
const MaxOptions = 26

// MaxCodes is the length of the DE-2 codebook, and the most options one DE-2
// text choice can carry.
const MaxCodes = 256

// codebookFile is the shipped audit the codebook came from. The codes are the
// contract; the rest is provenance.
type codebookFile struct {
	Codes             []string       `json:"codes"`
	Tokenizer         string         `json:"tokenizer"`
	TokenizerSnapshot string         `json:"tokenizer_snapshot"`
	Source            string         `json:"source"`
	Note              string         `json:"note"`
	TokenIDs          map[string]int `json:"token_ids"`
}

//go:embed data/codebook-de2.json
var codebookJSON []byte

var (
	codebookOnce sync.Once
	codebookData codebookFile
	codebookErr  error
)

func loadCodebook() {
	codebookOnce.Do(func() {
		if err := json.Unmarshal(codebookJSON, &codebookData); err != nil {
			codebookErr = &ReadoutError{Message: "data/codebook-de2.json is not readable", Err: err}
			return
		}
		codes := codebookData.Codes
		if len(codes) != MaxCodes {
			codebookErr = &ReadoutError{Message: fmt.Sprintf(
				"data/codebook-de2.json holds %d codes, not the pinned %d", len(codes), MaxCodes)}
			return
		}
		if strings.Join(codes[:MaxOptions], "") != Letters {
			codebookErr = &ReadoutError{Message: fmt.Sprintf(
				"data/codebook-de2.json does not start with %s", Letters)}
			return
		}
		seen := make(map[string]struct{}, len(codes))
		for _, code := range codes {
			if _, dup := seen[code]; dup {
				codebookErr = &ReadoutError{Message: fmt.Sprintf(
					"data/codebook-de2.json repeats the code %q", code)}
				return
			}
			seen[code] = struct{}{}
		}
	})
}

// Codebook returns the pinned DE-2 answer codes, in order: A to Z, then the
// uppercase pairs that pass the single-token answer-boundary audit.
//
// The returned slice is shared and must not be modified.
func Codebook() ([]string, error) {
	loadCodebook()
	if codebookErr != nil {
		return nil, codebookErr
	}
	return codebookData.Codes, nil
}

// CodesFor returns the answer codes for a question with count options, in
// option order: option i is shown code i.
//
// Up to 26 options these are the letters, so a small question renders the same
// under either contract. Above 26 they are the first count entries of the
// codebook, and above MaxCodes the question cannot be asked at all.
//
// The returned slice is shared and must not be modified.
func CodesFor(count int) ([]string, error) {
	if count < 0 {
		return nil, &QuestionError{Message: fmt.Sprintf("a question cannot have %d options", count)}
	}
	if count == 0 {
		return []string{}, nil
	}
	if count <= MaxOptions {
		return strings.Split(Letters[:count], ""), nil
	}
	if count > MaxCodes {
		return nil, &QuestionError{Message: fmt.Sprintf(
			"%d answers exceeds the %d codes the readout has", count, MaxCodes)}
	}
	codes, err := Codebook()
	if err != nil {
		return nil, err
	}
	return codes[:count], nil
}
