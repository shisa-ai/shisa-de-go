package main

// The command line's parity gate: `testdata/cli_parity.json` is the Python
// command line's own output, recorded by `scripts/gen_cli_parity.py`, and this
// replays every invocation through `run` against a stub endpoint and compares
// stdout, stderr, the exit status and the requests in order.
//
// Every other contract in this port has a recording like this one. The command
// line did not, which `docs/PORT.md` said out loud: its comparisons "are not
// fixtures and are not re-run at test time". That is the gap this closes.
//
// The fixture's token ids are the ones `cliTokenizerFor` hands out, so the
// recorded request bodies are replayable byte for byte rather than only
// structurally. `scripts/gen_cli_parity.py`'s `CLIStubTokenizer` is the other
// half of that pair; the two have to move together.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cliParityCase is one recorded invocation.
type cliParityCase struct {
	Name string   `json:"name"`
	Argv []string `json:"argv"`
	// Status is the exit status Python's `main` returned, or 1 when the command
	// let an exception escape.
	Status int `json:"status"`
	// Ended is `returned`, `systemexit` or `uncaught`. Only `uncaught` changes
	// what is comparable: see the stderr field.
	Ended string `json:"ended"`
	// Stdout is the recorded text, with the stub endpoint written as
	// `{{base_url}}` because a real one carries an ephemeral port.
	Stdout string `json:"stdout"`
	// Stderr is absent for the one case Python ends with a traceback, where the
	// text is the interpreter's and not the command line's.
	Stderr   *string            `json:"stderr"`
	Requests []cliParityRequest `json:"requests"`
}

// cliParityRequest is one request the recorded run made, in order.
type cliParityRequest struct {
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Body   map[string]any `json:"body"`
}

type cliParityFixture struct {
	Cases []cliParityCase `json:"cases"`
}

type cliParityMeta struct {
	PythonRepo  string   `json:"python_repo"`
	StubBaseURL string   `json:"stub_base_url"`
	StubModels  []string `json:"stub_models"`
	Cases       int      `json:"cases"`
	Requests    int      `json:"requests"`
	SHA256      string   `json:"sha256"`
}

// loadCLIParity reads the fixture and its provenance, and checks that the two
// agree before anything is replayed.
//
// The digest is checked, so a fixture edited by hand fails here rather than
// replaying text no Python command line ever printed. Every count is checked for
// the same reason: a regeneration that silently dropped the image cases would
// otherwise satisfy the case list it left behind.
func loadCLIParity(t *testing.T) (cliParityFixture, cliParityMeta) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "cli_parity.json"))
	if err != nil {
		t.Fatalf("reading the command-line fixture: %v", err)
	}
	var fixture cliParityFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("cli_parity.json is not JSON: %v", err)
	}
	var meta cliParityMeta
	readFixture(t, "cli_parity.meta.json", &meta)
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != meta.SHA256 {
		t.Fatalf("the command-line fixture hashes to %s, the provenance records %s",
			got, meta.SHA256)
	}
	if meta.Cases != len(fixture.Cases) {
		t.Fatalf("the provenance records %d cases and the fixture holds %d",
			meta.Cases, len(fixture.Cases))
	}
	requests := 0
	for _, one := range fixture.Cases {
		requests += len(one.Requests)
	}
	if meta.Requests != requests {
		t.Fatalf("the provenance records %d requests and the fixture holds %d",
			meta.Requests, requests)
	}
	if meta.StubBaseURL == "" {
		t.Fatal("the provenance records no placeholder endpoint, so the replay cannot " +
			"substitute its own")
	}
	if len(meta.StubModels) == 0 {
		t.Fatal("the provenance records no served model ids, so the replay cannot list any")
	}
	return fixture, meta
}

// readFixture unmarshals one file from the repository's testdata directory.
func readFixture(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s is not JSON: %v", name, err)
	}
}

// TestCLIParity replays the Python command line, invocation for invocation.
func TestCLIParity(t *testing.T) {
	fixture, meta := loadCLIParity(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			server := newCLIServer(t, meta.StubModels...)
			installCLIModel(t, server)

			// The recording names a placeholder endpoint; this one names the
			// stub. `doctor` prints whichever it was given, so both the
			// invocation and the expectation move together.
			argv := make([]string, len(tc.Argv))
			for index, arg := range tc.Argv {
				argv[index] = strings.ReplaceAll(arg, meta.StubBaseURL, server.server.URL)
			}
			status, stdout, stderr := invoke(t, argv...)

			want := strings.ReplaceAll(tc.Stdout, "{{base_url}}", server.server.URL)
			// Python's optional quote-stripping feature is outside this port.
			// Regenerated recordings report its disabled setting; require that
			// exact value and omit only that documented Python-only field.
			if tc.Name == "doctor-json" || tc.Name == "doctor-probe-json" {
				const pythonOnly = "  \"strip_state_quotes\": false,\n"
				if strings.Count(want, pythonOnly) != 1 {
					t.Fatal("recording must report quote stripping disabled")
				}
				want = strings.Replace(want, pythonOnly, "", 1)
			}
			if stdout != want {
				t.Errorf("stdout is\n%s\nwant\n%s", stdout, want)
			}
			if status != tc.Status {
				t.Errorf("exit status is %d, want %d", status, tc.Status)
			}
			// Python ends one case with a traceback where this port reports the
			// error, so its stderr is the interpreter's and not the command
			// line's; `docs/PORT.md` records that difference.
			if tc.Stderr != nil && stderr != *tc.Stderr {
				t.Errorf("stderr is %q, want %q", stderr, *tc.Stderr)
			}
			if tc.Ended == "uncaught" && tc.Stderr == nil {
				if stderr == "" {
					t.Error("a case Python ends with an exception reported nothing")
				}
			}

			compareCLIParityRequests(t, tc, server)
		})
	}
}

// compareCLIParityRequests matches the requests one recorded run made against
// the ones this command line made: the same count, in the same order, to the
// same paths, with the same bodies.
//
// The bodies are the point. A recorded `ask --image` body is the image content
// part, the request fields and the rendered prompt, so a divergence in the
// message order or a flag that stopped reaching the request shows up here rather
// than only in the output.
func compareCLIParityRequests(t *testing.T, tc cliParityCase, server *cliServer) {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) != len(tc.Requests) {
		t.Fatalf("the command made %d requests, the recording made %d",
			len(server.requests), len(tc.Requests))
	}
	for index, want := range tc.Requests {
		if server.methods[index] != want.Method {
			t.Errorf("request %d used %s, the recording used %s",
				index+1, server.methods[index], want.Method)
		}
		if server.paths[index] != want.Path {
			t.Errorf("request %d went to %s, the recording went to %s",
				index+1, server.paths[index], want.Path)
		}
		if want.Body == nil {
			continue
		}
		// Round-tripped through JSON so the two sides are compared as the same
		// value: the recording's numbers are JSON numbers and the stub decodes
		// its own body into `any`.
		got, err := json.Marshal(server.requests[index])
		if err != nil {
			t.Fatalf("encoding request %d: %v", index+1, err)
		}
		wanted, err := json.Marshal(want.Body)
		if err != nil {
			t.Fatalf("encoding recorded request %d: %v", index+1, err)
		}
		if string(got) != string(wanted) {
			t.Errorf("request %d body is\n%s\nwant\n%s", index+1, got, wanted)
		}
	}
}

// TestCLIParityCoversTheImageSurface guards the fixture's reason for existing.
//
// A regeneration that quietly dropped the image cases would still satisfy every
// count in the provenance as long as the totals agreed, so the invocations this
// gate is here for are named rather than counted.
func TestCLIParityCoversTheImageSurface(t *testing.T) {
	fixture, _ := loadCLIParity(t)
	byName := map[string]cliParityCase{}
	for _, tc := range fixture.Cases {
		byName[tc.Name] = tc
	}
	for _, want := range []struct {
		name string
		// argv contains every one of these, and the case is refused if it does
		// not.
		argv []string
		// stdout contains every one of these.
		stdout []string
	}{
		{"doctor-probe", []string{"--probe"}, []string{"chat system     string"}},
		{"ask-image", []string{"--image"}, nil},
		{"ask-image-top-logprobs", []string{"--image", "--image-top-logprobs", "30"}, nil},
		{"ask-image-explicit-state", []string{"--image", "--state"}, nil},
		{"ask-image-empty", []string{"--image"}, nil},
	} {
		tc, present := byName[want.name]
		if !present {
			t.Errorf("the fixture has no %q case", want.name)
			continue
		}
		for _, arg := range want.argv {
			if !containsString(tc.Argv, arg) {
				t.Errorf("%s does not pass %s: %v", want.name, arg, tc.Argv)
			}
		}
		for _, line := range want.stdout {
			if !strings.Contains(tc.Stdout, line) {
				t.Errorf("%s does not print %q:\n%s", want.name, line, tc.Stdout)
			}
		}
	}
	// `--image-top-logprobs` has to reach the request, not only the argument
	// list: the flag is what the case exists to check.
	top := byName["ask-image-top-logprobs"]
	if len(top.Requests) != 1 || top.Requests[0].Body["top_logprobs"] != float64(30) {
		t.Errorf("ask-image-top-logprobs did not ask for 30: %v", top.Requests)
	}
	// The default is the other half: an image question that does not name the
	// flag asks for the documented default.
	def := byName["ask-image"]
	if len(def.Requests) != 1 || def.Requests[0].Body["top_logprobs"] != float64(20) {
		t.Errorf("ask-image did not ask for the default 20: %v", def.Requests)
	}
}

// containsString is the membership test the argument check needs.
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
