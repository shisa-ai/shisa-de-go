package tokenizer

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Environment variables this package reads.
const (
	// CacheEnv overrides where downloaded tokenizers are kept.
	CacheEnv = "SHISA_DE_TOKENIZER_CACHE"
	// TokenEnv is the Hugging Face access token, for a gated repository.
	TokenEnv = "HF_TOKEN"
	// AltTokenEnv is the older name for the same token.
	AltTokenEnv = "HUGGING_FACE_HUB_TOKEN"
	// HubCacheEnv overrides the Hugging Face hub cache this package looks in
	// before downloading, so a tokenizer the Python client already fetched is
	// reused rather than fetched twice.
	HubCacheEnv = "HF_HUB_CACHE"
	// TokenizerJSONEnv names a tokenizer.json to load directly. It is read by
	// LoadPinned, which the tests use.
	TokenizerJSONEnv = "SHISA_DE_TOKENIZER_JSON"
	// RequireEnv turns LoadPinned's "not found" into an error instead of a
	// skip, so CI can require that the parity suite actually ran.
	RequireEnv = "SHISA_DE_TOKENIZER_REQUIRE"
	// DownloadEnv lets LoadPinned fetch the pinned tokenizer when it is not on
	// this machine.
	DownloadEnv = "SHISA_DE_TOKENIZER_DOWNLOAD"
)

// ErrNoPinnedTokenizer reports that no tokenizer.json for the pinned revision
// is on this machine. Callers that can tolerate that, such as the parity test,
// skip on it.
var ErrNoPinnedTokenizer = errors.New("tokenizer: no pinned tokenizer.json found")

// The identity of the checkpoint this port targets. VerifyPinned checks a
// loaded tokenizer against these, so a fallback to a different revision of the
// same model fails loudly instead of producing confusing id mismatches.
const (
	PinnedRepo      = "shisa-ai/shisa-de-2"
	PinnedSnapshot  = "8f21734b27b3462471c4662d5638e52195b005a3"
	PinnedVocabSize = 262144
	PinnedCloseID   = 101
)

// VerifyPinned reports whether a loaded tokenizer is the pinned one. It is a
// cheap identity check, not a full comparison: the vocab size and the id of the
// closing channel token are enough to catch a different revision.
func VerifyPinned(t *Tokenizer) error {
	if t.Len() != PinnedVocabSize {
		return fmt.Errorf("tokenizer: %d vocab entries, want %d: not the pinned tokenizer",
			t.Len(), PinnedVocabSize)
	}
	if id, ok := t.TokenID("<channel|>"); !ok || id != PinnedCloseID {
		return fmt.Errorf("tokenizer: <channel|> at id %d (present=%v), want %d: not the pinned tokenizer",
			id, ok, PinnedCloseID)
	}
	return nil
}

// LoadPinned finds a pinned tokenizer.json without fetching it and reports
// which path it used, so a caller can check its identity.
//
// It looks at TokenizerJSONEnv first, then the testdata directories, then the
// Hugging Face hub cache for the named revision. A path named by
// TokenizerJSONEnv that cannot be read is an error rather than a fall-through:
// a typo must not become a different tokenizer.
func LoadPinned(repo, revision string) (*Tokenizer, string, error) {
	if path := strings.TrimSpace(os.Getenv(TokenizerJSONEnv)); path != "" {
		if !exists(path) {
			return nil, path, fmt.Errorf("tokenizer: %s=%s is not a readable file", TokenizerJSONEnv, path)
		}
		tokenizer, err := FromFile(path)
		return tokenizer, path, err
	}
	candidates := []string{
		filepath.Join("testdata", "tokenizer.json"),
		filepath.Join("..", "testdata", "tokenizer.json"),
		filepath.Join("tokenizer", "testdata", "tokenizer.json"),
	}
	if path, ok := Lookup(repo, revision); ok {
		candidates = append(candidates, path)
	}
	for _, path := range candidates {
		if !exists(path) {
			continue
		}
		tokenizer, err := FromFile(path)
		return tokenizer, path, err
	}
	return nil, "", ErrNoPinnedTokenizer
}

// Load resolves a tokenizer source and loads it.
//
// A source that names an existing file is read directly. Anything else is
// treated as a Hugging Face repository id and fetched through the hub cache.
func Load(source string) (*Tokenizer, error) {
	path, err := Resolve(source)
	if err != nil {
		return nil, err
	}
	return FromFile(path)
}

// Resolve returns a local path to the tokenizer.json for a source, downloading
// it from the Hugging Face hub if it is not already on disk.
func Resolve(source string) (string, error) {
	if source == "" {
		return "", errors.New("tokenizer: no tokenizer source given")
	}
	if info, err := os.Stat(source); err == nil && !info.IsDir() {
		return source, nil
	}
	repo, revision := splitRevision(source)
	if err := checkSource(repo, revision); err != nil {
		return "", err
	}
	if path, ok := Lookup(repo, revision); ok {
		return path, nil
	}
	return Fetch(repo, revision)
}

// splitRevision splits "owner/name@rev" into its parts. The revision defaults
// to "main".
func splitRevision(source string) (repo, revision string) {
	repo, revision, found := strings.Cut(source, "@")
	if !found || revision == "" {
		revision = "main"
	}
	return repo, revision
}

// maxTokenizerBytes bounds a download. The pinned tokenizer.json is about
// 32 MB; anything past this is not a tokenizer, and refusing it stops an error
// page or a runaway response from filling the disk.
const maxTokenizerBytes = 256 << 20

// Fetch downloads tokenizer.json for a repository into the cache and returns
// the local path. A usable file already in the cache is returned as is; a cache
// entry that does not parse is treated as absent and replaced, so one bad
// download cannot poison the cache permanently.
func Fetch(repo, revision string) (string, error) {
	if revision == "" {
		revision = "main"
	}
	if err := checkSource(repo, revision); err != nil {
		return "", err
	}
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, filepath.FromSlash(repo), revision, "tokenizer.json")
	if exists(dest) {
		if _, err := FromFile(dest); err == nil {
			return dest, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("tokenizer: creating the cache directory: %w", err)
	}
	endpoint := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/tokenizer.json",
		escapePath(repo), escapePath(revision))
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("tokenizer: building the request for %s: %w", endpoint, err)
	}
	request.Header.Set("Accept", "application/json")
	if token := hubToken(); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("tokenizer: fetching %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 400))
		return "", fmt.Errorf("tokenizer: %s returned HTTP %d: %s",
			endpoint, response.StatusCode, strings.TrimSpace(string(detail)))
	}
	// Write beside the destination and rename, so a concurrent reader never
	// sees a half-written tokenizer.
	temp, err := os.CreateTemp(filepath.Dir(dest), "tokenizer-*.json")
	if err != nil {
		return "", fmt.Errorf("tokenizer: creating a temporary file: %w", err)
	}
	defer os.Remove(temp.Name())
	written, err := io.Copy(temp, io.LimitReader(response.Body, maxTokenizerBytes+1))
	if err != nil {
		temp.Close()
		return "", fmt.Errorf("tokenizer: writing %s: %w", dest, err)
	}
	if written > maxTokenizerBytes {
		temp.Close()
		return "", fmt.Errorf("tokenizer: %s returned more than %d bytes", endpoint, maxTokenizerBytes)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("tokenizer: writing %s: %w", dest, err)
	}
	// A body that is not a tokenizer is never installed, whatever its status.
	if _, err := FromFile(temp.Name()); err != nil {
		return "", fmt.Errorf("tokenizer: %s did not return a tokenizer: %w", endpoint, err)
	}
	if err := os.Rename(temp.Name(), dest); err != nil {
		return "", fmt.Errorf("tokenizer: installing %s: %w", dest, err)
	}
	return dest, nil
}

// checkSource rejects a repository or revision that would escape the cache
// directory or change the request's meaning. A revision may contain slashes,
// because branch names do, but never an empty or ".." component.
func checkSource(repo, revision string) error {
	if repo == "" {
		return errors.New("tokenizer: no repository given")
	}
	if revision == "" {
		return errors.New("tokenizer: no revision given")
	}
	for _, source := range []struct{ what, value string }{{"repository", repo}, {"revision", revision}} {
		value := source.value
		if strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\?#\x00") {
			return &ErrUnsupported{What: fmt.Sprintf("%s %q", source.what, value)}
		}
		for _, part := range strings.Split(value, "/") {
			if part == "" || part == "." || part == ".." {
				return &ErrUnsupported{What: fmt.Sprintf("%s %q", source.what, value)}
			}
		}
	}
	return nil
}

// escapePath escapes each path segment of a repository or revision while
// keeping the slashes that separate them.
func escapePath(value string) string {
	parts := strings.Split(value, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// cacheDir is where this package keeps downloaded tokenizers. It does not fall
// back to a shared temporary directory: a predictable path under /tmp is one
// another user can create first.
func cacheDir() (string, error) {
	if dir := os.Getenv(CacheEnv); dir != "" {
		return dir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("tokenizer: no cache directory; set %s: %w", CacheEnv, err)
	}
	return filepath.Join(base, "shisa-de", "tokenizer"), nil
}

// hubToken is the Hugging Face access token, from either of its names.
func hubToken() string {
	for _, name := range []string{TokenEnv, AltTokenEnv} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// Lookup finds the tokenizer in a Hugging Face hub cache without downloading
// anything, so a tokenizer the Python client already fetched is reused.
//
// It resolves only the revision asked for: a snapshot hash is looked up as that
// snapshot, and anything else through the refs directory. It never substitutes
// a different snapshot, because the caller cannot see which revision it got and
// a silent substitution would defeat an explicit pin.
//
// It reports false when no cache holds one. Use Resolve to download when it
// does not.
func Lookup(repo, revision string) (string, bool) {
	if checkSource(repo, revision) != nil {
		return "", false
	}
	roots := []string{}
	if dir := os.Getenv(HubCacheEnv); dir != "" {
		roots = append(roots, dir)
	}
	if home := os.Getenv("HF_HOME"); home != "" {
		roots = append(roots, filepath.Join(home, "hub"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".cache", "huggingface", "hub"))
	}
	folder := "models--" + strings.ReplaceAll(repo, "/", "--")
	for _, root := range roots {
		base := filepath.Join(root, folder)
		if isSnapshot(revision) {
			path := filepath.Join(base, "snapshots", revision, "tokenizer.json")
			if exists(path) {
				return path, true
			}
			continue
		}
		if target, err := os.ReadFile(filepath.Join(base, "refs", filepath.FromSlash(revision))); err == nil {
			// The ref's content is another path component. Validate it the same
			// way as a caller-supplied revision rather than joining it blindly.
			resolved := strings.TrimSpace(string(target))
			if !isSnapshot(resolved) {
				continue
			}
			path := filepath.Join(base, "snapshots", resolved, "tokenizer.json")
			if exists(path) {
				return path, true
			}
		}
	}
	return "", false
}

// isSnapshot reports whether a revision looks like a commit hash rather than a
// branch name.
func isSnapshot(revision string) bool {
	if len(revision) < 7 || len(revision) > 64 {
		return false
	}
	for _, r := range revision {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}
