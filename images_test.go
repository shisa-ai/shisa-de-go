package shisade

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// imagesFixture is testdata/images_parity.json: the files to write and the
// references to judge, both recorded from the Python client.
type imagesFixture struct {
	Files map[string]string `json:"files"`
	// SuffixDivergence records the two shapes the 3.14 and 3.12 `Path.suffix`
	// rules disagree about, one input each, with both rules' answers. It is what
	// turns a regeneration under another interpreter into a named failure rather
	// than two unexplained rows in a diff.
	SuffixDivergence imagesSuffixDivergence `json:"suffix_divergence"`
	Cases            []imagesCase           `json:"cases"`
}

// imagesSuffixDivergence is the recorded version-dependence of `Path.suffix`.
type imagesSuffixDivergence struct {
	Note           string `json:"note"`
	CurrentVersion string `json:"current_version"`
	// Shapes names the shapes the rules differ on. The fixture exercises one
	// input per shape, so this is what makes a fixture that pinned only one of
	// them detectable.
	Shapes []string             `json:"shapes"`
	Cases  []imagesSuffixChange `json:"cases"`
}

// imagesSuffixChange is one input the two rules disagree about.
type imagesSuffixChange struct {
	// Name is the final path component, without a directory.
	Name string `json:"name"`
	// Current is `pathlib.Path(name).suffix` on the interpreter that ran the
	// recorder. This port implements that rule.
	Current string `json:"current"`
	// Older is the 3.12 rule transcribed, which the recorder did not run.
	Older string `json:"older"`
	// Shape is which of the two shapes this input is an instance of. It is a
	// shape and not a name: `..a`, `...a` and `..png` all diverge the same way.
	Shape string `json:"shape"`
}

// suffixShapes are the two shapes the 3.14 and 3.12 `Path.suffix` rules differ
// on. The recorder checks exhaustively that there is no third; this is the list
// the fixture has to cover one input from.
var suffixShapes = []string{"leading-dots", "trailing-dot"}

type imagesCase struct {
	Func  string  `json:"func"`
	Input string  `json:"input"`
	Value *string `json:"result"`
	Error *string `json:"error"`
}

// imagesMeta is testdata/images_parity.meta.json: what produced the fixture.
type imagesMeta struct {
	PythonRepo    string `json:"python_repo"`
	PythonVersion string `json:"python_version"`
	Cases         int    `json:"cases"`
	Files         int    `json:"files"`
	SHA256        string `json:"sha256"`
}

func loadImagesFixture(t *testing.T) imagesFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "images_parity.json"))
	if err != nil {
		t.Fatalf("reading the images fixture: %v", err)
	}
	var fixture imagesFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parsing the images fixture: %v", err)
	}
	if len(fixture.Files) == 0 {
		t.Fatal("the images fixture holds no files")
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("the images fixture holds no cases")
	}
	return fixture
}

// writeImagesTree writes the fixture's files under a fresh temp directory and
// returns its root. The two runs each build their own tree, so the fixture
// carries the bytes rather than a path.
func writeImagesTree(t *testing.T, fixture imagesFixture) string {
	t.Helper()
	root := t.TempDir()
	for relative, encoded := range fixture.Files {
		payload, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("the fixture's %s is not base64: %v", relative, err)
		}
		target := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("creating the directory for %s: %v", relative, err)
		}
		if err := os.WriteFile(target, payload, 0o644); err != nil {
			t.Fatalf("writing %s: %v", relative, err)
		}
	}
	return root
}

// TestImagesParity replays the Python client's verdicts for every image
// reference, including the exact text of every rejection.
//
// The messages are the point as much as the verdicts are: a caller who mistypes
// a path reads one, and `ask --image` prints it.
func TestImagesParity(t *testing.T) {
	fixture := loadImagesFixture(t)
	root := writeImagesTree(t, fixture)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory to expand against: %v", err)
	}

	for index, tc := range fixture.Cases {
		// Both the reference and the recorded verdict can name the two
		// directories, because the Python client puts the path it looked for in
		// its message.
		substitute := func(text string) string {
			text = strings.ReplaceAll(text, "{tmp}", root)
			return strings.ReplaceAll(text, "{home}", home)
		}
		input := substitute(tc.Input)

		var got string
		var callErr error
		switch tc.Func {
		case "prepare_image":
			got, callErr = PrepareImage(input)
		case "validate_image_url":
			got, callErr = ValidateImageURL(input)
		default:
			t.Fatalf("case %d names an unknown function %q", index, tc.Func)
		}

		name := tc.Func + "(" + tc.Input + ")"
		switch {
		case tc.Error != nil:
			want := substitute(*tc.Error)
			if callErr == nil {
				t.Errorf("%s: got %q, want the error %q", name, got, want)
				continue
			}
			var imageError *ImageError
			if !errors.As(callErr, &imageError) {
				t.Errorf("%s: got a %T, want an *ImageError", name, callErr)
				continue
			}
			if imageError.Message != want {
				t.Errorf("%s: the message differs:\n got %s\nwant %s",
					name, pythonQuote(imageError.Message), pythonQuote(want))
			}
		case tc.Value != nil:
			want := substitute(*tc.Value)
			if callErr != nil {
				t.Errorf("%s: got the error %v, want %q", name, callErr, want)
				continue
			}
			if got != want {
				t.Errorf("%s: the value differs:\n got %q\nwant %q", name, got, want)
			}
		default:
			t.Errorf("%s: the fixture records neither a result nor an error", name)
		}
	}
}

// TestImagesFixtureIsNotStale checks the fixture against its own metadata, so a
// hand-edited fixture fails here rather than passing quietly.
func TestImagesFixtureIsNotStale(t *testing.T) {
	raw, meta := loadImagesFixtureMeta(t)
	var fixture imagesFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parsing the images fixture: %v", err)
	}
	if problems := imagesFixtureProblems(raw, fixture, meta); len(problems) != 0 {
		t.Errorf("the images fixture does not match its provenance: %s", strings.Join(problems, "; "))
	}
}

// TestImagesFixtureProblemsCanFail checks the provenance check against mutated
// copies. A check that cannot be shown to fail is a check nobody has made.
func TestImagesFixtureProblemsCanFail(t *testing.T) {
	raw, meta := loadImagesFixtureMeta(t)
	var fixture imagesFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parsing the images fixture: %v", err)
	}
	// The provenance and the fixture are both checked, so both are mutated. The
	// divergence block is the only thing standing between a regeneration under an
	// older interpreter and two unexplained rows, so it is mutated hardest.
	for name, mutate := range map[string]func(*imagesFixture, *imagesMeta){
		"case count":  func(_ *imagesFixture, m *imagesMeta) { m.Cases++ },
		"file count":  func(_ *imagesFixture, m *imagesMeta) { m.Files++ },
		"digest":      func(_ *imagesFixture, m *imagesMeta) { m.SHA256 = strings.Repeat("0", 64) },
		"interpreter": func(_ *imagesFixture, m *imagesMeta) { m.PythonVersion = "" },
		"divergence dropped": func(f *imagesFixture, _ *imagesMeta) {
			f.SuffixDivergence.Cases = nil
		},
		"divergence without an interpreter": func(f *imagesFixture, _ *imagesMeta) {
			f.SuffixDivergence.CurrentVersion = ""
		},
		"divergence under another interpreter": func(f *imagesFixture, _ *imagesMeta) {
			f.SuffixDivergence.CurrentVersion = "3.12.0"
		},
		"divergence that is not one": func(f *imagesFixture, _ *imagesMeta) {
			f.SuffixDivergence.Cases[0].Older = f.SuffixDivergence.Cases[0].Current
		},
		"divergence from the wrong rule": func(f *imagesFixture, _ *imagesMeta) {
			// The shape a 3.12 recording would carry: the older column becomes
			// what the port is told to implement.
			change := &f.SuffixDivergence.Cases[0]
			change.Current, change.Older = change.Older, change.Current
		},
		"one shape only": func(f *imagesFixture, _ *imagesMeta) {
			// Both remaining cases are the same shape, so the trailing-dot rule
			// is left unrecorded while every other check still passes.
			f.SuffixDivergence.Cases[1] = f.SuffixDivergence.Cases[0]
		},
		"unknown shape": func(f *imagesFixture, _ *imagesMeta) {
			f.SuffixDivergence.Cases[0].Shape = "third-shape"
		},
		"shape list disagrees": func(f *imagesFixture, _ *imagesMeta) {
			f.SuffixDivergence.Shapes = []string{"leading-dots"}
		},
		"unnamed input": func(f *imagesFixture, _ *imagesMeta) {
			f.SuffixDivergence.Cases[0].Name = ""
		},
	} {
		mutatedFixture := fixture
		mutatedFixture.SuffixDivergence.Cases = append(
			[]imagesSuffixChange(nil), fixture.SuffixDivergence.Cases...)
		mutated := meta
		// The digest is recomputed first, so a mutation is what the checks see
		// rather than a stale hash, and a mutation of the digest itself still
		// wins.
		sum := sha256.Sum256(raw)
		mutated.SHA256 = hex.EncodeToString(sum[:])
		mutate(&mutatedFixture, &mutated)
		if problems := imagesFixtureProblems(raw, mutatedFixture, mutated); len(problems) == 0 {
			t.Errorf("a mutated %s reported no problem", name)
		}
	}
}

// imagesFixtureProblems is every way the fixture can disagree with the record of
// what produced it.
func imagesFixtureProblems(raw []byte, fixture imagesFixture, meta imagesMeta) []string {
	var problems []string
	if meta.PythonVersion == "" {
		problems = append(problems, "the provenance records no interpreter")
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != meta.SHA256 {
		problems = append(problems, fmt.Sprintf("the fixture is %s, the provenance records %s",
			got, meta.SHA256))
	}
	if len(fixture.Cases) != meta.Cases {
		problems = append(problems, fmt.Sprintf("the fixture holds %d cases, the provenance records %d",
			len(fixture.Cases), meta.Cases))
	}
	if len(fixture.Files) != meta.Files {
		problems = append(problems, fmt.Sprintf("the fixture holds %d files, the provenance records %d",
			len(fixture.Files), meta.Files))
	}
	for index, tc := range fixture.Cases {
		if tc.Func != "prepare_image" && tc.Func != "validate_image_url" {
			problems = append(problems, fmt.Sprintf("case %d names an unknown function %q", index, tc.Func))
		}
		if (tc.Value == nil) == (tc.Error == nil) {
			problems = append(problems, fmt.Sprintf("case %d records neither a result nor an error", index))
		}
	}
	// The divergence block is what makes a regeneration under another
	// interpreter diagnosable, so a fixture without a usable one is a problem
	// rather than only a missing convenience.
	divergence := fixture.SuffixDivergence
	if len(divergence.Cases) == 0 {
		problems = append(problems, "the fixture records no suffix divergence")
	}
	if divergence.CurrentVersion == "" {
		problems = append(problems, "the suffix divergence records no interpreter")
	}
	if divergence.CurrentVersion != "" && divergence.CurrentVersion != meta.PythonVersion {
		problems = append(problems, fmt.Sprintf(
			"the suffix divergence was recorded under %s and the provenance names %s",
			divergence.CurrentVersion, meta.PythonVersion))
	}
	for _, change := range divergence.Cases {
		if change.Name == "" {
			problems = append(problems, "a suffix divergence names no input")
			continue
		}
		if change.Current == change.Older {
			problems = append(problems, fmt.Sprintf(
				"%q is recorded as a divergence with both rules saying %q",
				change.Name, change.Current))
		}
		// The port implements the recording interpreter's rule, so the recorded
		// column has to be that one. This is what a regeneration under 3.12
		// trips: the rows move and this says why.
		if got := pythonSuffix(change.Name); got != change.Current {
			problems = append(problems, fmt.Sprintf(
				"%q is recorded as %q under %s, and this port's rule gives %q; the "+
					"fixture was not recorded under the rule this port implements",
				change.Name, change.Current, divergence.CurrentVersion, got))
		}
	}
	// Both shapes have to be exercised. A fixture that pinned only the leading-dot
	// shape would still pass every check above while leaving the trailing-dot rule
	// unrecorded.
	covered := make(map[string]bool, len(divergence.Cases))
	for _, change := range divergence.Cases {
		covered[change.Shape] = true
	}
	for _, shape := range suffixShapes {
		if !covered[shape] {
			problems = append(problems, fmt.Sprintf(
				"no recorded input is an instance of the %q shape", shape))
		}
	}
	for _, change := range divergence.Cases {
		if change.Shape != "" && !containsString(suffixShapes, change.Shape) {
			problems = append(problems, fmt.Sprintf(
				"%q is recorded as an instance of the unknown shape %q",
				change.Name, change.Shape))
		}
	}
	// The block's own shape list has to be the one this side checks against.
	if len(divergence.Shapes) != 0 && !equalStrings(divergence.Shapes, suffixShapes) {
		problems = append(problems, fmt.Sprintf(
			"the fixture records the shapes %v and this port knows %v",
			divergence.Shapes, suffixShapes))
	}
	return problems
}

func loadImagesFixtureMeta(t *testing.T) ([]byte, imagesMeta) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "images_parity.json"))
	if err != nil {
		t.Fatalf("reading the images fixture: %v", err)
	}
	metaRaw, err := os.ReadFile(filepath.Join("testdata", "images_parity.meta.json"))
	if err != nil {
		t.Fatalf("reading the images fixture provenance: %v", err)
	}
	var meta imagesMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("parsing the images fixture provenance: %v", err)
	}
	return raw, meta
}

// TestSniffImageIgnoresTheExtension pins the rule that a file is sent as what it
// is rather than as what it is called.
func TestSniffImageIgnoresTheExtension(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"png", []byte("\x89PNG\r\n\x1a\npayload"), "image/png"},
		{"jpeg", []byte("\xff\xd8\xff\xe0payload"), "image/jpeg"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPpayload"), "image/webp"},
		// A RIFF file that is not WebP is not an image this readout sends.
		{"riff-but-not-webp", []byte("RIFF\x00\x00\x00\x00WAVEpayload"), ""},
		// The WebP marker sits at offset 8, so a short RIFF header cannot be one.
		{"too-short", []byte("RIFFWEBP"), ""},
		{"text", []byte("this is not an image"), ""},
		{"empty", nil, ""},
		// A PNG signature truncated by one byte is not a PNG.
		{"truncated-png", []byte("\x89PNG\r\n\x1a"), ""},
	} {
		if got := sniffImage(tc.data); got != tc.want {
			t.Errorf("%s: sniffImage = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestPrepareImageReadsTheFileOnlyOnce is the leak check the Python client
// states as a rule: the file name must never reach the returned value, because
// the returned value is what a request carries.
func TestPrepareImageReadsTheFileOnlyOnce(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "cat.png")
	if err := os.WriteFile(target, []byte("\x89PNG\r\n\x1a\nbytes"), 0o644); err != nil {
		t.Fatalf("writing the test image: %v", err)
	}
	url, err := PrepareImage(target)
	if err != nil {
		t.Fatalf("PrepareImage: %v", err)
	}
	if strings.Contains(url, root) || strings.Contains(url, "cat.png") {
		t.Errorf("the data URL carries the local path: %s", url)
	}
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("the data URL is not a PNG data URL: %s", url)
	}
	// A directory is not a file, even when it is named like one.
	if _, err := PrepareImage(root); err == nil {
		t.Error("PrepareImage accepted a directory")
	}
}

// TestPrepareImageSuffixDivergence records the one place the Python client's
// answer depends on which interpreter runs it.
//
// `pathlib.Path.suffix` changed in 3.14, and the two rules differ on two shapes:
// a name with two or more leading dots and a dot-free body, and a name that ends
// with a dot without being all dots. Neither shape can name a supported image, so
// both interpreters accept and reject the same set of files.
//
// The fixture names one input per shape, with both rules' answers, so a
// regeneration under an older interpreter fails on the rule rather than on two
// unexplained rows of the replay. This test reads it and checks that the port
// implements the column the recording was made under.
func TestPrepareImageSuffixDivergence(t *testing.T) {
	fixture := loadImagesFixture(t)
	divergence := fixture.SuffixDivergence
	if len(divergence.Cases) == 0 {
		t.Fatal("the fixture records no suffix divergence, so nothing here is pinned")
	}
	covered := make(map[string]bool, len(divergence.Cases))
	for _, tc := range divergence.Cases {
		if tc.Current == tc.Older {
			t.Errorf("the fixture records %q as a divergence with both rules saying %q",
				tc.Name, tc.Current)
		}
		covered[tc.Shape] = true
		if got := pythonSuffix(tc.Name); got != tc.Current {
			t.Errorf("pythonSuffix(%q) = %q, and the fixture records %q as what the "+
				"recording interpreter (%s) returns. This port implements that rule; "+
				"the other shape is %q.",
				tc.Name, got, tc.Current, divergence.CurrentVersion, tc.Older)
		}
	}
	for _, shape := range suffixShapes {
		if !covered[shape] {
			t.Errorf("no recorded input is an instance of the %q shape, so the fixture "+
				"pins one shape and not the other", shape)
		}
	}

	for _, tc := range []struct {
		path string
		want string
	}{
		// 3.14: the leading dots are not part of a suffix at all.
		{"..png", ""},
		{".png", ""},
		{".bashrc", ""},
		// 3.14: a trailing dot is a suffix of one dot; 3.10 and 3.12 say none.
		{"a.", "."},
		// The two shapes are shapes and not the two names the fixture records, so
		// the other instances are pinned here. A port that special-cased `..png`
		// and `a.` would pass the fixture and fail these.
		{"..a", ""},
		{"...a", ""},
		{"x.", "."},
		{"a..", "."},
		{".a.", "."},
		{"a.b.", "."},
		// A name that is all dots has an empty body, so both rules say none.
		{"..", ""},
		{"...", ""},
		{".", ""},
		// Both agree on everything a real image carries.
		{"a.png", ".png"},
		{"a.tar.gz", ".gz"},
		{".hidden.png", ".png"},
		{"a..png", ".png"},
		{".a", ""},
		{"dir.d/a", ""},
		{"a/b.png", ".png"},
		{"a/", ""},
	} {
		if got := pythonSuffix(tc.path); got != tc.want {
			t.Errorf("pythonSuffix(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestPythonPathMatchesPathlib pins the lexical cleaning pathlib does before the
// extension is read and before the path appears in an error message.
func TestPythonPathMatchesPathlib(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{"a.png", "a.png"},
		{"a//b.png", "a/b.png"},
		{"a///b.png", "a/b.png"},
		{"a/./b.png", "a/b.png"},
		// pathlib does not resolve `..`, and neither may this.
		{"a/../b.png", "a/../b.png"},
		{"a/", "a"},
		{"a/b/", "a/b"},
		{"./a.png", "a.png"},
		{"./", "."},
		{".", "."},
		{"", "."},
		// Exactly two leading separators are a POSIX root of their own.
		{"//x/a.png", "//x/a.png"},
		// Three or more collapse to one.
		{"///x/a.png", "/x/a.png"},
		{"/a/b.png", "/a/b.png"},
	} {
		got, err := pythonPath(tc.path)
		if err != nil {
			t.Errorf("pythonPath(%q): %v", tc.path, err)
			continue
		}
		if got != tc.want {
			t.Errorf("pythonPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestPythonPathExpandsUserOnlyForARelativePath pins pathlib's guard: `~` is
// expanded only when the path has neither a drive nor a root.
//
// `Path.expanduser` is `not (self.drive or self.root) and self._tail[0][:1] ==
// '~'`, so `/~/x.png` keeps its literal component. Expanding it here would build
// a `//home/...` path, which is a distinct POSIX root, and the OS then collapses
// the slashes and finds the file the Python client reports as missing.
func TestPythonPathExpandsUserOnlyForARelativePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory to expand against")
	}
	for _, tc := range []struct {
		path string
		want string
	}{
		{"~/a.png", home + "/a.png"},
		{"~", home},
		// The cleaning happens before the guard, so a `~` that only looks rooted
		// is still expanded.
		{"./~/a.png", home + "/a.png"},
		{"/~/a.png", "/~/a.png"},
		{"//~/a.png", "//~/a.png"},
		{"///~/a.png", "/~/a.png"},
		// A `~` that is not the first component is never expanded.
		{"a/~/b.png", "a/~/b.png"},
	} {
		got, err := pythonPath(tc.path)
		if err != nil {
			t.Errorf("pythonPath(%q): %v", tc.path, err)
			continue
		}
		if got != tc.want {
			t.Errorf("pythonPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestValidateImageURLEdgeCases covers the boundaries the fixture cannot state
// as data: the host check is a length comparison, and the payload check is a
// comma search.
func TestValidateImageURLEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		url     string
		want    string
		wantErr bool
	}{
		{"https://example.com/a.png", "https://example.com/a.png", false},
		// One character of host is enough; none is not.
		{"https://a", "https://a", false},
		{"https://", "", true},
		{"http://", "", true},
		// A data URL needs the image media type and a non-empty payload.
		{"data:image/png;base64,AA", "data:image/png;base64,AA", false},
		{"data:image/png;base64,", "", true},
		{"data:image/png;base64", "", true},
		{"data:image/png,", "", true},
		{"data:text/plain,AA", "", true},
		// The scheme error names the scheme, not the whole URL.
		{"ftp://example.com/a.png", "", true},
		{"file:///tmp/a.png", "", true},
		// A bare path is the failure this function exists for.
		{"cat.png", "", true},
		{"a/b/cat.png", "", true},
	} {
		got, err := ValidateImageURL(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ValidateImageURL(%q) = %q, want an error", tc.url, got)
				continue
			}
			var imageError *ImageError
			if !errors.As(err, &imageError) {
				t.Errorf("ValidateImageURL(%q): got a %T, want an *ImageError", tc.url, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ValidateImageURL(%q): %v", tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ValidateImageURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// TestPrepareImageSchemeErrorNamesTheScheme is the message a caller sees when
// they paste a `file://` URL, and it has to name the scheme rather than the URL.
func TestPrepareImageSchemeErrorNamesTheScheme(t *testing.T) {
	_, err := PrepareImage("file:///tmp/cat.png")
	if err == nil {
		t.Fatal("PrepareImage accepted a file:// URL")
	}
	if !strings.Contains(err.Error(), "'file'") {
		t.Errorf("the message does not name the scheme: %v", err)
	}
	// A path with a colon but no `://` is a file name, not a scheme.
	if _, err := PrepareImage("a:b.png"); err == nil {
		t.Error("PrepareImage accepted a path whose extension is unsupported")
	} else if strings.Contains(err.Error(), "scheme") {
		t.Errorf("a path with a colon was read as a URL: %v", err)
	}
}
