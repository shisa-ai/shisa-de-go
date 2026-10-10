package tokenizer

import (
	"errors"
	"fmt"
	"io"
	"net/http"
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
)

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
	if path, ok := findInHubCache(repo, revision); ok {
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

// Fetch downloads tokenizer.json for a repository into the cache and returns
// the local path. A file already in the cache is returned as is.
func Fetch(repo, revision string) (string, error) {
	if repo == "" {
		return "", errors.New("tokenizer: no repository given")
	}
	if revision == "" {
		revision = "main"
	}
	dest := filepath.Join(cacheDir(), filepath.FromSlash(repo), revision, "tokenizer.json")
	if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("tokenizer: creating the cache directory: %w", err)
	}
	url := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/tokenizer.json", repo, revision)
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("tokenizer: building the request for %s: %w", url, err)
	}
	request.Header.Set("Accept", "application/json")
	if token := hubToken(); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("tokenizer: fetching %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 400))
		return "", fmt.Errorf("tokenizer: %s returned HTTP %d: %s",
			url, response.StatusCode, strings.TrimSpace(string(detail)))
	}
	// Write beside the destination and rename, so a concurrent reader never
	// sees a half-written tokenizer.
	temp, err := os.CreateTemp(filepath.Dir(dest), "tokenizer-*.json")
	if err != nil {
		return "", fmt.Errorf("tokenizer: creating a temporary file: %w", err)
	}
	defer os.Remove(temp.Name())
	if _, err := io.Copy(temp, response.Body); err != nil {
		temp.Close()
		return "", fmt.Errorf("tokenizer: writing %s: %w", dest, err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("tokenizer: writing %s: %w", dest, err)
	}
	if err := os.Rename(temp.Name(), dest); err != nil {
		return "", fmt.Errorf("tokenizer: installing %s: %w", dest, err)
	}
	return dest, nil
}

// cacheDir is where this package keeps downloaded tokenizers.
func cacheDir() string {
	if dir := os.Getenv(CacheEnv); dir != "" {
		return dir
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "shisa-de", "tokenizer")
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

// findInHubCache looks for the tokenizer in a Hugging Face hub cache, so a
// tokenizer the Python client already downloaded is reused.
func findInHubCache(repo, revision string) (string, bool) {
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
		// A revision that names a snapshot directly.
		if isSnapshot(revision) {
			path := filepath.Join(base, "snapshots", revision, "tokenizer.json")
			if exists(path) {
				return path, true
			}
		}
		// Otherwise the ref is resolved through the refs directory.
		if target, err := os.ReadFile(filepath.Join(base, "refs", filepath.FromSlash(revision))); err == nil {
			path := filepath.Join(base, "snapshots", strings.TrimSpace(string(target)), "tokenizer.json")
			if exists(path) {
				return path, true
			}
		}
		// Otherwise any snapshot that has one.
		matches, _ := filepath.Glob(filepath.Join(base, "snapshots", "*", "tokenizer.json"))
		if len(matches) > 0 {
			return matches[0], true
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
