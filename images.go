package shisade

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/user"
	"strings"
)

// Image references for the multimodal readout.
//
// An image cannot travel through the text readout: the client renders that
// prompt itself with the checkpoint's chat template, and only the server can
// expand an image placeholder into the image tokens the checkpoint expects. The
// image readout therefore posts to `/v1/chat/completions` and lets the server
// render the prompt. This file builds the one value that request needs, the
// `image_url` content part: an `http(s)` URL, a `data:` URL, or a base64 data
// URL made from a local PNG, JPEG or WebP file.
//
// Nothing here decodes, resizes or re-encodes an image, and no image library is
// imported. The bytes of a local file are passed to the server unchanged.
//
// The two entry points answer two different questions:
//
//   - [PrepareImage] turns whatever the caller has (a path, a URL) into a value
//     the request can carry.
//   - [ValidateImageURL] rejects anything that is not already a URL, so a bare
//     local file name is never sent to the server as the image.

// SupportedMediaTypes are the media types the readout sends for local files, by
// lowercased file extension.
var SupportedMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
}

// SupportedExtensions are the extensions named in error messages, in the order a
// caller should read them.
var SupportedExtensions = []string{".png", ".jpg", ".jpeg", ".webp"}

// HTTPPrefixes are the URL schemes an image reference may carry.
var HTTPPrefixes = []string{"http://", "https://"}

// DataPrefix is the URL scheme a data URL carries.
const DataPrefix = "data:"

// Image signatures. A file extension is a claim; these bytes are the evidence.
var (
	pngSignature  = []byte("\x89PNG\r\n\x1a\n")
	jpegSignature = []byte("\xff\xd8\xff")
	riffSignature = []byte("RIFF")
	webpSignature = []byte("WEBP")
)

// sniffImage is the media type of `data` from its leading bytes, or "" when it
// is not one of the three supported formats.
//
// A PNG named `.jpg` is sent as what it is rather than as what it is called.
func sniffImage(data []byte) string {
	switch {
	case bytes.HasPrefix(data, pngSignature):
		return "image/png"
	case bytes.HasPrefix(data, jpegSignature):
		return "image/jpeg"
	case len(data) >= 12 && bytes.HasPrefix(data, riffSignature) &&
		bytes.Equal(data[8:12], webpSignature):
		return "image/webp"
	}
	return ""
}

// ValidateImageURL returns an `http(s)` or `data:` image URL unchanged, or an
// [ImageError].
//
// [Readout.ReadImage] calls this before it builds a request. A bare local path
// is rejected rather than forwarded: the request carries the URL itself, so a
// file name that reached this point would be sent to the server as the image.
// Use [PrepareImage] to turn a local file into a data URL first.
func ValidateImageURL(value string) (string, error) {
	url := trimPythonSpace(value)
	if url == "" {
		return "", &ImageError{Message: "image_url is empty"}
	}
	if hasAnyPrefix(url, HTTPPrefixes) {
		at := strings.Index(url, "://")
		if len(url) <= at+3 {
			return "", &ImageError{Message: fmt.Sprintf("image_url has no host: %s", pythonQuote(url))}
		}
		return url, nil
	}
	if strings.HasPrefix(url, DataPrefix) {
		if !strings.HasPrefix(url, DataPrefix+"image/") {
			return "", &ImageError{Message: fmt.Sprintf(
				"image_url is a data URL that is not an image: %s", pythonQuote(pythonSlice(url, 32)))}
		}
		comma := strings.Index(url, ",")
		if comma < 0 || url[comma+1:] == "" {
			return "", &ImageError{Message: "image_url is a data URL with no payload"}
		}
		return url, nil
	}
	if at := strings.Index(url, "://"); at >= 0 {
		return "", &ImageError{Message: fmt.Sprintf(
			"image_url scheme %s is not supported; use http://, https://, or a data URL",
			pythonQuote(url[:at]))}
	}
	return "", &ImageError{Message: fmt.Sprintf(
		"image_url %s is a local path, not a URL; call prepare_image() first so the "+
			"server receives image bytes rather than a file name", pythonQuote(url))}
}

// PrepareImage turns an image reference into the `image_url` a request can
// carry.
//
//   - A local PNG, JPEG or WebP file becomes a base64 `data:` URL.
//   - An `http(s)` or `data:` URL passes through unchanged.
//   - Anything else returns an [ImageError] before any request is made: an empty
//     value, an unsupported extension or URL scheme, a missing file, an empty
//     file, or a file whose bytes are not a supported image.
//
// The file name is never part of the returned value, so a caller cannot leak a
// local path into a request by attaching an image.
//
// The Python client's `prepare_image` also accepts a `pathlib.Path`, which this
// signature does not need: a path is a string either way, so the type error the
// Python function raises for anything else cannot arise here.
func PrepareImage(image string) (string, error) {
	text := trimPythonSpace(image)
	if text == "" {
		return "", &ImageError{Message: "image is empty"}
	}
	if hasAnyPrefix(text, HTTPPrefixes) || strings.HasPrefix(text, DataPrefix) {
		return ValidateImageURL(text)
	}
	if at := strings.Index(text, "://"); at >= 0 {
		return "", &ImageError{Message: fmt.Sprintf(
			"image scheme %s is not supported; pass a local file, an http(s) URL, "+
				"or a data URL", pythonQuote(text[:at]))}
	}
	path, err := pythonPath(text)
	if err != nil {
		return "", err
	}
	suffix := pythonSuffix(path)
	if _, ok := SupportedMediaTypes[strings.ToLower(suffix)]; !ok {
		shown := suffix
		if shown == "" {
			shown = "(none)"
		}
		return "", &ImageError{Message: fmt.Sprintf(
			"unsupported image extension %s; supported extensions are %s",
			pythonQuote(shown), strings.Join(SupportedExtensions, ", "))}
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", &ImageError{Message: "image file not found: " + path}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", &ImageError{Message: "image file not found: " + path}
	}
	if len(data) == 0 {
		return "", &ImageError{Message: "image file is empty: " + path}
	}
	sniffed := sniffImage(data)
	if sniffed == "" {
		return "", &ImageError{Message: "image file is not a PNG, JPEG, or WebP image: " + path}
	}
	return "data:" + sniffed + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// pythonSlice is `text[:count]` for a Python string: the first `count` code
// points, not bytes. Slicing a URL by bytes could split a multi-byte rune and
// put a replacement character in an error message.
func pythonSlice(text string, count int) string {
	runes := []rune(text)
	if len(runes) <= count {
		return text
	}
	return string(runes[:count])
}

func hasAnyPrefix(text string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// pythonPath is `str(pathlib.Path(text).expanduser())` for a POSIX path.
//
// The Python client checks the extension of this value and prints it in two of
// its error messages, so the Go port has to produce the same string rather than
// a merely equivalent one. pathlib cleans a path lexically: it drops empty and
// `.` components, keeps `..`, drops a trailing separator, and collapses repeated
// separators except for the POSIX `//` root. It does not resolve `..` against
// the filesystem, and neither does this.
//
// Only the POSIX flavour is implemented. The Python client runs on Windows too,
// where `Path` is a `WindowsPath` with drive letters and backslashes; a Go
// caller on Windows gets the POSIX reading, which is a difference this port
// records rather than guesses at.
func pythonPath(text string) (string, error) {
	drive, root, rest := pythonSplitRoot(text)
	components := make([]string, 0, 8)
	for _, part := range strings.Split(rest, "/") {
		if part == "" || part == "." {
			continue
		}
		components = append(components, part)
	}
	if len(components) != 0 && strings.HasPrefix(components[0], "~") {
		home, err := pythonExpandUser(components[0])
		if err != nil {
			return "", err
		}
		components[0] = home
	}
	joined := strings.Join(components, "/")
	if drive == "" && root == "" && joined == "" {
		return ".", nil
	}
	return drive + root + joined, nil
}

// pythonSplitRoot is `posixpath.splitroot`, whose one special case is that
// exactly two leading separators are a root of their own under POSIX.
func pythonSplitRoot(path string) (string, string, string) {
	if !strings.HasPrefix(path, "/") {
		return "", "", path
	}
	if !strings.HasPrefix(path[1:], "/") || strings.HasPrefix(path[2:], "/") {
		return "", "/", path[1:]
	}
	return path[:2], "", path[2:]
}

// pythonExpandUser is `os.path.expanduser` for a leading `~` component.
//
// Python raises a bare `RuntimeError` when the home directory cannot be
// determined, which is not an error the image API promises to return; here it is
// an [ImageError] like every other unusable reference, so a caller has one type
// to handle.
func pythonExpandUser(component string) (string, error) {
	if component == "~" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", &ImageError{Message: "Could not determine home directory."}
		}
		return home, nil
	}
	account, err := user.Lookup(component[1:])
	if err != nil || account.HomeDir == "" {
		return "", &ImageError{Message: "Could not determine home directory."}
	}
	return account.HomeDir, nil
}

// pythonSuffix is `pathlib.Path(name).suffix` for the final component of an
// already-cleaned path: the last dot and everything after it, unless the name
// has no dot after its leading ones.
//
// This is Python 3.14's rule (`name.lstrip('.').rfind('.')`). Python 3.10 and
// 3.12 use `0 < i < len(name) - 1` over the whole name instead, which disagrees
// on exactly two shapes: `..png` has suffix `.png` there and none here, and `a.`
// has none there and `.` here. Neither can name a supported image, so the two
// interpreters accept and reject the same set of files and produce the same
// media type for every one of them; only those two error messages differ.
func pythonSuffix(path string) string {
	name := path
	if at := strings.LastIndexByte(path, '/'); at >= 0 {
		name = path[at+1:]
	}
	trimmed := strings.TrimLeft(name, ".")
	at := strings.LastIndexByte(trimmed, '.')
	if at < 0 {
		return ""
	}
	return trimmed[at:]
}
