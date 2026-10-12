package shisade

// The README's Go snippets are not compiled by `go test`, so nothing stops them
// naming a function that has been renamed or a field that no longer exists. This
// file closes that gap: every `shisade.X` a README block mentions has to be an
// exported name of this package.
//
// It is deliberately not a compiler. Compiling the blocks would need a synthetic
// package, a module and a build cache, and the failure it would catch beyond this
// one is a wrong argument type in prose nobody runs. A renamed identifier is the
// failure that actually happens, and this catches it in a millisecond.

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// readmeDocs are the files whose Go blocks are checked. The readout pages are
// excluded: they are copied verbatim from the Python repository and specify a
// wire protocol, not a Go API.
var readmeDocs = []string{"README.md", "docs/PORT.md"}

// goBlocks returns every ```go fenced block in a file.
func goBlocks(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var blocks []string
	lines := strings.Split(string(raw), "\n")
	inBlock, isGo := false, false
	var current []string
	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			if !inBlock {
				inBlock, isGo = true, strings.TrimSpace(line[3:]) == "go"
				current = nil
			} else {
				if isGo {
					blocks = append(blocks, strings.Join(current, "\n"))
				}
				inBlock = false
			}
			continue
		}
		if inBlock {
			current = append(current, line)
		}
	}
	if inBlock {
		t.Errorf("%s has an unterminated code fence", path)
	}
	return blocks
}

// exportedNames are this package's exported top-level identifiers, and the
// exported methods and fields of its exported types.
func exportedNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			switch typed := decl.(type) {
			case *ast.FuncDecl:
				if typed.Recv == nil {
					if ast.IsExported(typed.Name.Name) {
						names[typed.Name.Name] = true
					}
					continue
				}
				// A method: record it under both the bare name and the
				// receiver-qualified one.
				if ast.IsExported(typed.Name.Name) {
					names[typed.Name.Name] = true
					names[receiverName(typed)+"."+typed.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						if ast.IsExported(spec.Name.Name) {
							names[spec.Name.Name] = true
						}
						if structType, ok := spec.Type.(*ast.StructType); ok {
							for _, field := range structType.Fields.List {
								for _, fieldName := range field.Names {
									if ast.IsExported(fieldName.Name) {
										names[spec.Name.Name+"."+fieldName.Name] = true
									}
								}
							}
						}
					case *ast.ValueSpec:
						for _, valueName := range spec.Names {
							if ast.IsExported(valueName.Name) {
								names[valueName.Name] = true
							}
						}
					}
				}
			}
		}
	}
	return names
}

// receiverName is a method's receiver type, with any pointer stripped.
func receiverName(decl *ast.FuncDecl) string {
	switch typed := decl.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if ident, ok := typed.X.(*ast.Ident); ok {
			return ident.Name
		}
	case *ast.Ident:
		return typed.Name
	}
	return ""
}

// shisadeReference matches `shisade.Something` and `shisade.Something.Field`.
var shisadeReference = regexp.MustCompile(`\bshisade\.([A-Z][A-Za-z0-9_]*(?:\.[A-Z][A-Za-z0-9_]*)?)`)

// proseFieldReference matches a backticked `Type.Field` in the docs' prose.
//
// The Go-block check cannot see these, and a field named in prose is exactly
// where a rename goes unnoticed: `Answer.SystemRender` was documented in two
// files while `Answer` had no such field. A reference is only checked when the
// head is an exported type of this package, so `json.dumps`, `pathlib.Path` and
// the Python client's own classes are left alone.
var proseFieldReference = regexp.MustCompile("`([A-Z][A-Za-z0-9_]*)\\.([A-Z][A-Za-z0-9_]*)`")

// TestReadmeProseNamesRealFields is the rot check for a field named in prose.
func TestReadmeProseNamesRealFields(t *testing.T) {
	names := exportedNames(t)
	checked := 0
	for _, path := range readmeDocs {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, match := range proseFieldReference.FindAllStringSubmatch(string(raw), -1) {
			typeName, field := match[1], match[2]
			if !names[typeName] {
				// Not a type of this package: the Python client's, or a package
				// from the standard library.
				continue
			}
			checked++
			if !names[typeName+"."+field] && !names[field] {
				t.Errorf("%s names %s.%s, and %s has no such exported field or method",
					path, typeName, field, typeName)
			}
		}
	}
	if checked < 10 {
		t.Fatalf("only %d prose field references were checked, so this is not reading the docs",
			checked)
	}
	t.Logf("checked %d prose field references in %v", checked, readmeDocs)
}

// TestReadmeGoBlocksNameRealIdentifiers is the rot check.
func TestReadmeGoBlocksNameRealIdentifiers(t *testing.T) {
	names := exportedNames(t)
	if len(names) < 100 {
		t.Fatalf("only %d exported names were found, so this check is not reading the package", len(names))
	}

	blocks := 0
	for _, path := range readmeDocs {
		for index, block := range goBlocks(t, path) {
			blocks++
			for _, match := range shisadeReference.FindAllStringSubmatch(block, -1) {
				reference := match[1]
				if names[reference] {
					continue
				}
				// `shisade.SomeType.Field` is not how the field is recorded when
				// the type is only ever reached through a value, so a miss on the
				// qualified form is only a failure if the bare name is missing too.
				if head, _, found := strings.Cut(reference, "."); found && names[head] {
					continue
				}
				t.Errorf("%s block %d mentions shisade.%s, which is not an exported name of this package",
					path, index+1, reference)
			}
		}
	}
	if blocks == 0 {
		t.Fatalf("no Go blocks were found in %v, so this check is not reading the docs", readmeDocs)
	}
	t.Logf("checked %d Go blocks in %v", blocks, readmeDocs)
}

// TestReadmeGoBlocksAreParsable catches a block that was edited into something
// that is not Go at all, which the identifier check would otherwise skip past.
//
// A block is a fragment, not a file, so it is tried both ways: as a file body
// (which is what an import or a declaration needs) and as a statement list
// inside a function (which is what a usage snippet is).
func TestReadmeGoBlocksAreParsable(t *testing.T) {
	fileSet := token.NewFileSet()
	parse := func(source string) error {
		_, err := parser.ParseFile(fileSet, "fragment.go", source, 0)
		return err
	}
	for _, path := range readmeDocs {
		for index, block := range goBlocks(t, path) {
			if parse("package fragment\n\n"+block+"\n") == nil {
				continue
			}
			if err := parse("package fragment\n\nfunc fragment() {\n" + block + "\n}\n"); err != nil {
				t.Errorf("%s block %d is not parsable Go: %v", path, index+1, err)
			}
		}
	}
}

// TestGoBlocksAreFound guards the finder itself: a fenced-block scanner that
// silently matches nothing would make both checks above pass on a README with no
// Go in it.
func TestGoBlocksAreFound(t *testing.T) {
	blocks := goBlocks(t, "README.md")
	if len(blocks) < 10 {
		t.Fatalf("README.md yielded %d Go blocks, want at least 10", len(blocks))
	}
	// One block is the import line; every other one is a usage snippet and has
	// to name something from the package, or the finder is matching prose.
	usage := 0
	for _, block := range blocks {
		if strings.Contains(block, "shisade.") {
			usage++
		}
	}
	if usage < len(blocks)-1 {
		t.Errorf("%d of %d Go blocks name shisade., which is fewer than expected",
			usage, len(blocks))
	}
}

// TestExportedNamesIsNotVacuous pins that the collector returns what it claims,
// so the identifier check cannot pass by finding nothing.
func TestExportedNamesIsNotVacuous(t *testing.T) {
	names := exportedNames(t)
	for _, want := range []string{
		"NewDecisionModel", "DecisionModel", "Classify", "Decide", "Noul", "Choice",
		"Score", "Option", "HeadSpec", "CallOptions", "OptionOverrides", "Answer",
		"Decision", "Calibration", "Softmax", "Codebook", "Render",
		"DecisionModel.Classify", "DecisionModel.Decide", "Answer.Probabilities",
	} {
		if !names[want] {
			t.Errorf("%s is missing from the collected exported names", want)
		}
	}
	// A name that does not exist must not be collected, or the check is vacuous.
	for _, unwanted := range []string{"DecisionModel.classify", "notAType", "DecisionModel.Nope"} {
		if names[unwanted] {
			t.Errorf("%s was collected as an exported name", unwanted)
		}
	}
}

// TestReadmeSectionsArePresent keeps the README's structure, which the examples
// and the Python README's sections are mapped from.
func TestReadmeSectionsArePresent(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	var headings []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, strings.TrimPrefix(line, "## "))
		}
	}
	sort.Strings(headings)
	for _, want := range []string{
		"Ask typed questions", "Classify images", "Classify with labels", "Command line",
		"Development", "Large label sets", "License", "Performance",
		"Probabilities and calibration", "Quick start", "Read settings",
		"Use a local server",
	} {
		if !containsString(headings, want) {
			t.Errorf("README.md has no %q section; it has %v", want, headings)
		}
	}
}

// portFixtureCounts are the fixture sizes `docs/PORT.md` states, with the
// metadata each one comes from and a pattern that reads it out of the prose.
//
// The paragraph and the fixture are regenerated together, and nothing was
// checking the paragraph. The readout's counts were stale -- 22 scenarios and 62
// exchanges against the metadata's 38 and 77 -- until the M7 review found them by
// hand, and regenerating the client fixture for the image cases moved its counts
// too. A number a test does not read is a number that drifts.
//
// One claim per row rather than one paragraph per row, so a paragraph whose two
// numbers sit in different sentences is covered the same way. Commas are dropped
// from both sides, because the tokenizer's prose writes 1,056 and the metadata
// stores 1056. The command line's count is spelled out ("eleven invocations")
// rather than written as a digit, and the Python-JSON figure has no meta file,
// so those two stay hand-maintained and are named here so the gap is visible.
var portFixtureCounts = []struct {
	name    string
	meta    string
	field   string
	pattern string
}{
	{"readout scenarios", "readout_parity.meta.json", "scenarios",
		`\*\*Readout\.\*\* (\d+) scenarios,`},
	{"readout exchanges", "readout_parity.meta.json", "exchanges",
		`\*\*Readout\.\*\* \d+ scenarios, (\d+) recorded exchanges`},
	{"client cases", "client_parity.meta.json", "cases",
		`\*\*Client\.\*\* (\d+) cases,`},
	{"client exchanges", "client_parity.meta.json", "exchanges",
		`\*\*Client\.\*\* \d+ cases, (\d+) recorded exchanges`},
	{"image references", "images_parity.meta.json", "cases",
		"`testdata/images_parity.json` replays (\\d+) references"},
	{"render prompts", "render_parity.meta.json", "renders",
		"compares (\\d+) rendered prompts"},
	// The same count is stated twice: once in the renderer discussion and once
	// under Verified parity. A regeneration can update one and not the other.
	{"render fixture renders", "render_parity.meta.json", "renders",
		`\*\*Render\.\*\* All (\d+) fixture renders`},
	{"tokenizer strings", "tokenizer_parity.meta.json", "strings",
		`\*\*Tokenizer\.\*\* ([\d,]+) strings,`},
	{"tokenizer ids", "tokenizer_parity.meta.json", "ids",
		`\*\*Tokenizer\.\*\* [\d,]+ strings, ([\d,]+) token ids`},
	{"tokenizer decode outputs", "tokenizer_parity.meta.json", "decode_cases",
		"replays (\\d+) `tokenizer.decode` outputs"},
}

// TestPortDocFixtureCounts checks the sizes `docs/PORT.md` states against the
// metadata the generators wrote, so a regenerated fixture and its paragraph
// cannot disagree in silence.
func TestPortDocFixtureCounts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("docs", "PORT.md"))
	if err != nil {
		t.Fatalf("reading docs/PORT.md: %v", err)
	}
	text := string(raw)
	for _, tc := range portFixtureCounts {
		t.Run(tc.name, func(t *testing.T) {
			metaRaw, err := os.ReadFile(filepath.Join("testdata", tc.meta))
			if err != nil {
				t.Fatalf("reading %s: %v", tc.meta, err)
			}
			var meta map[string]any
			if err := json.Unmarshal(metaRaw, &meta); err != nil {
				t.Fatalf("parsing %s: %v", tc.meta, err)
			}
			match := regexp.MustCompile(tc.pattern).FindStringSubmatch(text)
			if match == nil {
				t.Fatalf("docs/PORT.md does not state the %s in the form %q any more, "+
					"so this check reads nothing", tc.name, tc.pattern)
			}
			want, ok := meta[tc.field].(float64)
			if !ok {
				t.Fatalf("%s records no numeric %q", tc.meta, tc.field)
			}
			// The prose writes the tokenizer's counts with thousands separators
			// and the metadata stores them without, so both sides drop commas.
			if got := strings.ReplaceAll(match[1], ",", ""); got != strconv.Itoa(int(want)) {
				t.Errorf("docs/PORT.md says %s is %s, and %s records %s=%d",
					tc.name, match[1], tc.meta, tc.field, int(want))
			}
		})
	}
}

// TestNoStaleImageCoverageClaims keeps the image read's live coverage stated the
// same way everywhere.
//
// The image read gained live coverage in the M7 milestone, and the sentence
// saying the Python suite's image case was "the gate's `image-color` question
// rather than a test here" survived the round-1 review in `live_test.go` after
// its twin in `docs/PORT.md` was corrected. Every phrasing that asserts the read
// has no live coverage is now false wherever it appears, so they are pinned here
// rather than found by hand twice.
//
// The scan is Go and Markdown only, because `scripts/live_parity.py` quotes the
// old failure ("that 404 was reported as 'the endpoint does not serve images'")
// as the history of a fixed bug, which is a use of the phrase rather than a
// claim about the read.
func TestNoStaleImageCoverageClaims(t *testing.T) {
	stale := []string{
		"rather than a test here",
		"rather than a test of its own",
		"no live coverage",
		"not live-covered",
		"does not serve images",
		"cannot serve images",
	}
	walked := 0
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if strings.HasPrefix(filepath.ToSlash(path), "docs/reviews") {
				return fs.SkipDir
			}
			switch entry.Name() {
			case ".git", "testdata", "vendor", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".md":
		default:
			return nil
		}
		// This file holds the phrases, so it would match itself.
		if filepath.ToSlash(path) == "readme_test.go" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		walked++
		for _, phrase := range stale {
			if bytes.Contains(raw, []byte(phrase)) {
				t.Errorf("%s says %q, and the image read is live-covered",
					filepath.ToSlash(path), phrase)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A scan that read nothing would pass while checking nothing.
	if walked < 20 {
		t.Fatalf("the scan read %d files, which is too few to be the tree", walked)
	}
}

// TestReadmeNamesTheDeliberateAbsences keeps the real absences stated rather
// than silently missing, so a reader looking for one finds out why it is not
// there.
//
// The image path used to be on this list. It is implemented now and has its own
// section, which TestReadmeSectionsArePresent requires; what is left is the DE-1
// family and the overflow machinery that belongs to it.
func TestReadmeNamesTheDeliberateAbsences(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"DE-1", "overflow"} {
		if !strings.Contains(text, want) {
			t.Errorf("README.md never mentions %q, so its absence is unexplained", want)
		}
	}
	// The image read is present, and a reader should not be told otherwise. These
	// are the phrasings the README carried while it was a gap; a future edit that
	// reinstates one of them fails here.
	for _, stale := range []string{"No images", "not implement", "no image path"} {
		if strings.Contains(text, stale) {
			t.Errorf("README.md says %q, and the image read is implemented", stale)
		}
	}
}
