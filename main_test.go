package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tinyPNG is a 1×1 transparent PNG used to satisfy provider response decoders
// in tests without shipping a real image asset.
var tinyPNG = mustDecode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR4nGP4DwQACfsD/THTGykAAAAASUVORK5CYII=")

func mustDecode(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// ---------- helpers ----------

func TestDecodeB64_PlainAndDataURI(t *testing.T) {
	raw, err := decodeB64(base64.StdEncoding.EncodeToString([]byte("hello")))
	if err != nil || string(raw) != "hello" {
		t.Fatalf("plain b64 round-trip failed: got %q err=%v", raw, err)
	}
	prefixed := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("world"))
	raw, err = decodeB64(prefixed)
	if err != nil || string(raw) != "world" {
		t.Fatalf("data-uri b64 round-trip failed: got %q err=%v", raw, err)
	}
}

func TestDestPath_AutoNamesWhenNoOutput(t *testing.T) {
	got := destPath("", "openai", 0, 1, "png")
	if !strings.HasPrefix(got, "goimage-openai-") || !strings.HasSuffix(got, ".png") {
		t.Fatalf("auto name unexpected: %q", got)
	}
}

func TestDestPath_UsesOutputVerbatimForSingleImage(t *testing.T) {
	if got := destPath("foo.png", "openai", 0, 1, "png"); got != "foo.png" {
		t.Fatalf("want foo.png, got %q", got)
	}
}

func TestDestPath_AppendsIndexForMultiImage(t *testing.T) {
	if got := destPath("foo.png", "openai", 1, 3, "png"); got != "foo-2.png" {
		t.Fatalf("want foo-2.png, got %q", got)
	}
	if got := destPath("noext", "openai", 0, 2, "png"); got != "noext-1" {
		t.Fatalf("want noext-1, got %q", got)
	}
}

func TestExtFromMIME(t *testing.T) {
	cases := map[string]string{
		"image/png":     "png",
		"image/jpeg":    "jpg",
		"image/JPG":     "jpg",
		"image/webp":    "webp",
		"image/gif":     "gif",
		"":              "png",
		"weird/foo-bar": "png",
	}
	for in, want := range cases {
		if got := extFromMIME(in); got != want {
			t.Fatalf("extFromMIME(%q): want %q, got %q", in, want, got)
		}
	}
}

func TestLookupAPIKey_FallbacksAndPrimary(t *testing.T) {
	env := envMap(map[string]string{
		"OPENAI_API_KEY": "sk-openai",
		"GOOGLE_API_KEY": "gk-google",
		"GROK_API_KEY":   "xk-grok",
	})
	if got := lookupAPIKey("openai", env); got != "sk-openai" {
		t.Fatalf("openai key: want sk-openai, got %q", got)
	}
	if got := lookupAPIKey("google", env); got != "gk-google" {
		t.Fatalf("google fallback key: want gk-google, got %q", got)
	}
	if got := lookupAPIKey("grok", env); got != "xk-grok" {
		t.Fatalf("grok fallback key: want xk-grok, got %q", got)
	}

	envPrimary := envMap(map[string]string{
		"GEMINI_API_KEY": "primary-gemini",
		"GOOGLE_API_KEY": "fallback-google",
		"XAI_API_KEY":    "primary-xai",
		"GROK_API_KEY":   "fallback-grok",
	})
	if got := lookupAPIKey("google", envPrimary); got != "primary-gemini" {
		t.Fatalf("GEMINI_API_KEY should win over GOOGLE_API_KEY, got %q", got)
	}
	if got := lookupAPIKey("grok", envPrimary); got != "primary-xai" {
		t.Fatalf("XAI_API_KEY should win over GROK_API_KEY, got %q", got)
	}
	if got := lookupAPIKey("unknown", env); got != "" {
		t.Fatalf("unknown provider should return empty, got %q", got)
	}
}

func TestPrimaryEnvVar(t *testing.T) {
	cases := map[string]string{
		"openai":  "OPENAI_API_KEY",
		"google":  "GEMINI_API_KEY",
		"grok":    "XAI_API_KEY",
		"unknown": "",
	}
	for p, want := range cases {
		if got := primaryEnvVar(p); got != want {
			t.Fatalf("primaryEnvVar(%q): want %q, got %q", p, want, got)
		}
	}
}

// ---------- retry ----------

func TestWithRetry_RetriesThenSucceeds(t *testing.T) {
	orig := initialBackoff
	initialBackoff = time.Millisecond
	defer func() { initialBackoff = orig }()

	calls := 0
	got, err := withRetry("test", func() ([]byte, error) {
		calls++
		if calls < 2 {
			return nil, errors.New("transient")
		}
		return []byte("ok"), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("want ok, got %q", got)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

func TestWithRetry_GivesUpAfterMaxRetries(t *testing.T) {
	orig := initialBackoff
	initialBackoff = time.Millisecond
	defer func() { initialBackoff = orig }()

	calls := 0
	_, err := withRetry("test", func() ([]byte, error) {
		calls++
		return nil, errors.New("permanent")
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != maxRetries {
		t.Fatalf("want %d calls, got %d", maxRetries, calls)
	}
}

// fakeNetTimeout is a net.Error whose Timeout() is true, standing in for the
// *url.Error a real client-side deadline produces.
type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string   { return "i/o timeout" }
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return true }

func TestIsTimeout_ClassifiesByType(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"context deadline", context.DeadlineExceeded, true},
		{"wrapped context deadline", fmt.Errorf("http request: %w", context.DeadlineExceeded), true},
		{"net.Error timeout", fakeNetTimeout{}, true},
		{"wrapped net.Error timeout", fmt.Errorf("http request: %w", fakeNetTimeout{}), true},
		{"plain error", errors.New("nope"), false},
		{"api 400", errors.New("API error (400): bad prompt"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isTimeout(tc.err); got != tc.want {
			t.Errorf("%s: isTimeout = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A client-side timeout must NOT be retried: re-running the identical request
// with the same deadline can only fail the same way and wastes minutes.
func TestWithRetry_DoesNotRetryTimeout(t *testing.T) {
	orig := initialBackoff
	initialBackoff = time.Millisecond
	defer func() { initialBackoff = orig }()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"context deadline", fmt.Errorf("http request: %w", context.DeadlineExceeded)},
		{"net timeout", fmt.Errorf("http request: %w", fakeNetTimeout{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := withRetry("test", func() ([]byte, error) {
				calls++
				return nil, tc.err
			})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if calls != 1 {
				t.Fatalf("timeout must not be retried: want 1 call, got %d", calls)
			}
			if !isTimeout(err) {
				t.Fatalf("returned error should still unwrap to a timeout, got %v", err)
			}
		})
	}
}

func TestExplainError(t *testing.T) {
	// A timeout becomes multi-line guidance naming the knobs the user can turn.
	timeoutMsg := explainError(fmt.Errorf("http request: %w", context.DeadlineExceeded))
	for _, want := range []string{"timed out", "--timeout", "--quality", "--stream", "--count"} {
		if !strings.Contains(timeoutMsg, want) {
			t.Errorf("timeout guidance is missing %q in:\n%s", want, timeoutMsg)
		}
	}

	// A DNS failure points at connectivity, not the model.
	dnsMsg := explainError(&net.DNSError{Name: "api.openai.com", Err: "no such host"})
	if !strings.Contains(dnsMsg, "resolve") || !strings.Contains(dnsMsg, "api.openai.com") {
		t.Errorf("DNS guidance unexpected:\n%s", dnsMsg)
	}

	// Anything we don't recognise passes through unchanged (no false advice).
	plain := errors.New("api error: content policy violation")
	if got := explainError(plain); got != plain.Error() {
		t.Errorf("unrecognised error should pass through verbatim, got %q", got)
	}
	if got := explainError(nil); got != "" {
		t.Errorf("nil error should explain to empty string, got %q", got)
	}
}

// ---------- OpenAI provider ----------

func TestGenerateOpenAI_HappyPath(t *testing.T) {
	var gotReq openAIRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header: want Bearer test-key, got %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		resp := openAIResponse{Data: []openAIImageData{
			{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG), RevisedPrompt: "a refined prompt"},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	swapURL(&openAIAPIURL, srv.URL)(t)

	imgs, err := generateOpenAI("test-key", "gpt-image-1", "a cat", "1024x1024", "high", "png", 1, nil, "", false, io.Discard)
	if err != nil {
		t.Fatalf("generateOpenAI: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	if !bytes.Equal(imgs[0].data, tinyPNG) {
		t.Fatalf("image bytes mismatch")
	}
	if imgs[0].ext != "png" {
		t.Fatalf("ext: want png, got %q", imgs[0].ext)
	}
	if imgs[0].revisedPrompt != "a refined prompt" {
		t.Fatalf("revised prompt: got %q", imgs[0].revisedPrompt)
	}
	if gotReq.Model != "gpt-image-1" || gotReq.Prompt != "a cat" || gotReq.OutputFormat != "png" {
		t.Fatalf("unexpected request body: %#v", gotReq)
	}
}

func TestGenerateOpenAI_JpegMapsToJpgExt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openAIResponse{Data: []openAIImageData{
			{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG)},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)

	imgs, err := generateOpenAI("k", "gpt-image-1", "x", "", "", "jpeg", 1, nil, "", false, io.Discard)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if imgs[0].ext != "jpg" {
		t.Fatalf("want jpg ext, got %q", imgs[0].ext)
	}
}

func TestGenerateOpenAI_InvalidFormatRejected(t *testing.T) {
	_, err := generateOpenAI("k", "gpt-image-1", "x", "", "", "bmp", 1, nil, "", false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "invalid OpenAI format") {
		t.Fatalf("expected format validation error, got %v", err)
	}
}

func TestGenerateOpenAI_APIErrorSurfaces(t *testing.T) {
	orig := initialBackoff
	initialBackoff = time.Millisecond
	defer func() { initialBackoff = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)

	_, err := generateOpenAI("k", "gpt-image-1", "x", "", "", "png", 1, nil, "", false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "API error (401)") {
		t.Fatalf("expected 401 surface, got %v", err)
	}
}

// ---------- Google provider ----------

func TestGenerateGoogle_HappyPathWithAspectRatio(t *testing.T) {
	var gotReq googleRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "g-key" {
			t.Errorf("api key header: got %q", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/gemini-2.5-flash-image:generateContent") {
			t.Errorf("path: %q does not target generateContent", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		resp := googleResponse{Candidates: []googleCandidate{{
			Content: googleContent{Parts: []googlePart{{
				InlineData: &googleInlineData{
					MIMEType: "image/png",
					Data:     base64.StdEncoding.EncodeToString(tinyPNG),
				},
			}}},
		}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&googleAPIURL, srv.URL)(t)

	imgs, err := generateGoogle("g-key", "gemini-2.5-flash-image", "a fox", "16:9", 1, nil)
	if err != nil {
		t.Fatalf("generateGoogle: %v", err)
	}
	if len(imgs) != 1 || imgs[0].ext != "png" || !bytes.Equal(imgs[0].data, tinyPNG) {
		t.Fatalf("unexpected images: %#v", imgs)
	}
	if gotReq.GenerationConfig == nil ||
		gotReq.GenerationConfig.ResponseFormat == nil ||
		gotReq.GenerationConfig.ResponseFormat.Image == nil ||
		gotReq.GenerationConfig.ResponseFormat.Image.AspectRatio != "16:9" {
		t.Fatalf("aspect ratio not threaded through: %#v", gotReq)
	}
}

func TestGenerateGoogle_OmitsGenerationConfigWhenNoAspect(t *testing.T) {
	var gotReq googleRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		resp := googleResponse{Candidates: []googleCandidate{{
			Content: googleContent{Parts: []googlePart{{
				InlineData: &googleInlineData{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString(tinyPNG)},
			}}},
		}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&googleAPIURL, srv.URL)(t)

	_, err := generateGoogle("k", "gemini-2.5-flash-image", "x", "", 1, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotReq.GenerationConfig != nil {
		t.Fatalf("generationConfig should be omitted when aspect is blank, got %#v", gotReq.GenerationConfig)
	}
}

func TestGenerateGoogle_LoopsForCount(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		resp := googleResponse{Candidates: []googleCandidate{{
			Content: googleContent{Parts: []googlePart{{
				InlineData: &googleInlineData{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString(tinyPNG)},
			}}},
		}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&googleAPIURL, srv.URL)(t)

	imgs, err := generateGoogle("k", "gemini-2.5-flash-image", "x", "", 3, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(imgs) != 3 {
		t.Fatalf("want 3 images, got %d", len(imgs))
	}
	if calls != 3 {
		t.Fatalf("want 3 HTTP calls, got %d", calls)
	}
}

func TestGenerateGoogle_NoInlineDataIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := googleResponse{Candidates: []googleCandidate{{
			Content: googleContent{Parts: []googlePart{{Text: "sorry no image"}}},
		}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&googleAPIURL, srv.URL)(t)

	_, err := generateGoogle("k", "gemini-2.5-flash-image", "x", "", 1, nil)
	if err == nil || !strings.Contains(err.Error(), "no image parts") {
		t.Fatalf("expected 'no image parts' error, got %v", err)
	}
}

// ---------- Grok provider ----------

func TestGenerateGrok_HappyPath(t *testing.T) {
	var gotReq grokRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer xai" {
			t.Errorf("auth header: %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		resp := grokResponse{Data: []grokImageData{
			{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG), RevisedPrompt: "refined"},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&grokAPIURL, srv.URL)(t)

	imgs, err := generateGrok("xai", "grok-2-image", "logo", 1, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(imgs) != 1 || imgs[0].ext != "png" {
		t.Fatalf("bad image: %#v", imgs)
	}
	if gotReq.ResponseFormat != "b64_json" {
		t.Fatalf("want response_format=b64_json, got %q", gotReq.ResponseFormat)
	}
}

func TestGenerateGrok_EmptyB64IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := grokResponse{Data: []grokImageData{{URL: "https://example.com/x.png"}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&grokAPIURL, srv.URL)(t)

	_, err := generateGrok("k", "grok-2-image", "x", 1, nil)
	if err == nil || !strings.Contains(err.Error(), "without b64_json") {
		t.Fatalf("expected b64_json missing error, got %v", err)
	}
}

// ---------- run() integration ----------

func TestRun_HelpFlagPrintsToStdout(t *testing.T) {
	// Explicit --help should print to stdout (POSIX convention) so that
	// `goimage --help | grep ...` and Homebrew's `shell_output` work.
	var stdout, stderr bytes.Buffer
	code := run([]string{"--help"}, strings.NewReader(""), &stdout, &stderr, envMap(nil))
	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Usage: goimage") {
		t.Fatalf("help text not on stdout: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("explicit --help should not write to stderr, got: %q", stderr.String())
	}
}

func TestRun_UsageOnErrorGoesToStderr(t *testing.T) {
	// Missing-prompt path should still print usage to stderr so it doesn't
	// pollute downstream pipes when a script accidentally invokes goimage
	// with no arg.
	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader(""), &stdout, &stderr,
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage: goimage") {
		t.Fatalf("usage missing from stderr on error path: %s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("error path should not write to stdout, got: %q", stdout.String())
	}
}

func TestRun_NoPromptIsError(t *testing.T) {
	var stderr bytes.Buffer
	code := run(nil, strings.NewReader(""), io.Discard, &stderr,
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "No prompt provided") {
		t.Fatalf("missing prompt error not surfaced: %s", stderr.String())
	}
}

func TestRun_InvalidProviderIsError(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"-p", "midjourney", "hi"}, strings.NewReader(""), io.Discard, &stderr, envMap(nil))
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Invalid provider") {
		t.Fatalf("invalid-provider error not surfaced: %s", stderr.String())
	}
}

func TestRun_MissingAPIKeyNamesEnvVar(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"-p", "google", "hi"}, strings.NewReader(""), io.Discard, &stderr, envMap(nil))
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "GEMINI_API_KEY") {
		t.Fatalf("error should name GEMINI_API_KEY: %s", stderr.String())
	}
}

func TestRun_ReadsPromptFromStdin(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openAIResponse{Data: []openAIImageData{
			{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG)},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--stream=false"}, strings.NewReader("a sunset"), &stdout, &stderr,
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr=%s)", code, stderr.String())
	}
	path := strings.TrimSpace(stdout.String())
	if path == "" {
		t.Fatalf("expected file path on stdout")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written image: %v", err)
	}
	if !bytes.Equal(data, tinyPNG) {
		t.Fatalf("file bytes mismatch")
	}
}

func TestRun_WritesToExplicitOutputPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.png")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openAIResponse{Data: []openAIImageData{
			{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG)},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--stream=false", "-o", target, "a cat"}, strings.NewReader(""), &stdout, &stderr,
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr=%s)", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != target {
		t.Fatalf("want %q on stdout, got %q", target, stdout.String())
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("output file not written: %v", err)
	}
}

func TestRun_NegativeCountIsError(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"-n", "0", "hi"}, strings.NewReader(""), io.Discard, &stderr,
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "must be >= 1") {
		t.Fatalf("count validation message missing: %s", stderr.String())
	}
}

func TestRun_OpenFlagCallsOpenImageFn(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	_ = os.Chdir(dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openAIResponse{Data: []openAIImageData{
			{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG)},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)

	var opened []string
	origOpen := openImageFn
	openImageFn = func(p string) error {
		opened = append(opened, p)
		return nil
	}
	defer func() { openImageFn = origOpen }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--stream=false", "--open", "hi"}, strings.NewReader(""), &stdout, &stderr,
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr=%s)", code, stderr.String())
	}
	if len(opened) != 1 || opened[0] != strings.TrimSpace(stdout.String()) {
		t.Fatalf("openImageFn not called with written path: opened=%v stdout=%q", opened, stdout.String())
	}
}

// ---------- reference image support ----------

func writeTempPNG(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, tinyPNG, 0644); err != nil {
		t.Fatalf("write temp png: %v", err)
	}
	return path
}

func TestGenerateOpenAI_EditWithInputsAndMask(t *testing.T) {
	in1 := writeTempPNG(t, "ref1.png")
	in2 := writeTempPNG(t, "ref2.png")
	maskPath := writeTempPNG(t, "mask.png")

	var (
		gotPrompt    string
		gotModel     string
		imageCount   int
		gotMaskBytes int
		gotImageCT   string
		gotMaskCT    string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("want POST, got %s", r.Method)
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("want multipart, got %q", r.Header.Get("Content-Type"))
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		gotPrompt = r.FormValue("prompt")
		gotModel = r.FormValue("model")
		imageCount = len(r.MultipartForm.File["image[]"])
		if hs := r.MultipartForm.File["image[]"]; len(hs) > 0 {
			gotImageCT = hs[0].Header.Get("Content-Type")
		}
		if hs, ok := r.MultipartForm.File["mask"]; ok && len(hs) > 0 {
			gotMaskCT = hs[0].Header.Get("Content-Type")
			f, _ := hs[0].Open()
			defer f.Close()
			b, _ := io.ReadAll(f)
			gotMaskBytes = len(b)
		}
		resp := openAIResponse{Data: []openAIImageData{
			{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG)},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&openAIEditsAPIURL, srv.URL)(t)

	imgs, err := generateOpenAI("k", "gpt-image-2", "swap the sky", "1024x1024", "high", "png", 1,
		[]string{in1, in2}, maskPath, false, io.Discard)
	if err != nil {
		t.Fatalf("generateOpenAI (edit): %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	if gotPrompt != "swap the sky" {
		t.Fatalf("prompt not threaded through: %q", gotPrompt)
	}
	if gotModel != "gpt-image-2" {
		t.Fatalf("model not threaded through: %q", gotModel)
	}
	if imageCount != 2 {
		t.Fatalf("want 2 image[] uploads, got %d", imageCount)
	}
	if gotMaskBytes == 0 {
		t.Fatalf("mask was not attached")
	}
	// The OpenAI edits endpoint rejects application/octet-stream, so every
	// uploaded part must declare a real image mimetype (derived from the .png
	// extension here), not the multipart default.
	if gotImageCT != "image/png" {
		t.Fatalf("image[] Content-Type: want image/png, got %q", gotImageCT)
	}
	if gotMaskCT != "image/png" {
		t.Fatalf("mask Content-Type: want image/png, got %q", gotMaskCT)
	}
}

func TestGenerateOpenAI_EditMissingInputFails(t *testing.T) {
	_, err := generateOpenAI("k", "gpt-image-2", "x", "", "", "png", 1,
		[]string{"/nonexistent/ref.png"}, "", false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "open /nonexistent/ref.png") {
		t.Fatalf("expected missing-file error, got %v", err)
	}
}

func TestGenerateGoogle_AttachesInlineDataForReferences(t *testing.T) {
	ref := writeTempPNG(t, "fox.png")

	var gotReq googleRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		resp := googleResponse{Candidates: []googleCandidate{{
			Content: googleContent{Parts: []googlePart{{
				InlineData: &googleInlineData{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString(tinyPNG)},
			}}},
		}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&googleAPIURL, srv.URL)(t)

	_, err := generateGoogle("k", "gemini-2.5-flash-image", "make it autumn", "", 1, []string{ref})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	parts := gotReq.Contents[0].Parts
	if len(parts) != 2 {
		t.Fatalf("want 2 parts (inlineData + text), got %d: %#v", len(parts), parts)
	}
	if parts[0].InlineData == nil || parts[0].InlineData.MIMEType != "image/png" {
		t.Fatalf("first part should be inlineData PNG, got %#v", parts[0])
	}
	if parts[1].Text != "make it autumn" {
		t.Fatalf("second part should carry the text prompt, got %#v", parts[1])
	}
	// The inlineData base64 should round-trip to the tiny PNG bytes.
	raw, err := base64.StdEncoding.DecodeString(parts[0].InlineData.Data)
	if err != nil || !bytes.Equal(raw, tinyPNG) {
		t.Fatalf("inline data did not round-trip: err=%v", err)
	}
}

func TestGenerateGoogle_ReferenceReadErrorBubbles(t *testing.T) {
	_, err := generateGoogle("k", "gemini-2.5-flash-image", "x", "", 1, []string{"/nonexistent/r.png"})
	if err == nil || !strings.Contains(err.Error(), "read reference image") {
		t.Fatalf("expected file-read error, got %v", err)
	}
}

func TestGenerateGrok_RefusesReferenceImages(t *testing.T) {
	_, err := generateGrok("k", "grok-2-image", "x", 1, []string{"any.png"})
	if err == nil || !strings.Contains(err.Error(), "does not support reference images") {
		t.Fatalf("expected grok rejection, got %v", err)
	}
}

func TestRun_MaskWithoutInputIsError(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"--mask", "m.png", "hi"}, strings.NewReader(""), io.Discard, &stderr,
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "--mask requires") {
		t.Fatalf("mask validation message missing: %s", stderr.String())
	}
}

func TestMimeFromExt(t *testing.T) {
	cases := map[string]string{
		"a.png":  "image/png",
		"a.JPG":  "image/jpeg",
		"a.jpeg": "image/jpeg",
		"a.webp": "image/webp",
		"a.gif":  "image/gif",
		"a.bmp":  "image/png",
		"no-ext": "image/png",
		"x.tIfF": "image/png",
	}
	for in, want := range cases {
		if got := mimeFromExt(in); got != want {
			t.Errorf("mimeFromExt(%q): want %q, got %q", in, want, got)
		}
	}
}

// swapURL swaps a *string package var for the duration of a test and
// registers a cleanup that restores it. Letting providers keep their URLs as
// package vars makes httptest substitution trivial.
func swapURL(target *string, replacement string) func(*testing.T) {
	return func(t *testing.T) {
		orig := *target
		*target = replacement
		t.Cleanup(func() { *target = orig })
	}
}

// ---------- OpenAI streaming (SSE) ----------

// writeSSE writes a single Server-Sent Event frame: an event: line, a data:
// line carrying compact JSON, and the blank-line terminator. Flushing after
// each frame so the client side sees them as discrete events.
func writeSSE(t *testing.T, w http.ResponseWriter, event string, payload any) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal sse payload: %v", err)
	}
	if _, err := io.WriteString(w, "event: "+event+"\ndata: "+string(b)+"\n\n"); err != nil {
		t.Fatalf("write sse: %v", err)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestGenerateOpenAI_StreamingHappyPath(t *testing.T) {
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept header: want text/event-stream, got %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(t, w, "image_generation.partial_image", map[string]any{
			"type":                "image_generation.partial_image",
			"b64_json":            base64.StdEncoding.EncodeToString(tinyPNG),
			"partial_image_index": 0,
		})
		writeSSE(t, w, "image_generation.partial_image", map[string]any{
			"type":                "image_generation.partial_image",
			"b64_json":            base64.StdEncoding.EncodeToString(tinyPNG),
			"partial_image_index": 1,
		})
		writeSSE(t, w, "image_generation.completed", map[string]any{
			"type":     "image_generation.completed",
			"b64_json": base64.StdEncoding.EncodeToString(tinyPNG),
		})
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)

	var stderr bytes.Buffer
	imgs, err := generateOpenAI("k", "gpt-image-2", "a cat", "1024x1024", "high", "png", 1, nil, "", true, &stderr)
	if err != nil {
		t.Fatalf("generateOpenAI streaming: %v", err)
	}
	if len(imgs) != 1 || !bytes.Equal(imgs[0].data, tinyPNG) || imgs[0].ext != "png" {
		t.Fatalf("unexpected image result: %#v", imgs)
	}
	if gotReq["stream"] != true {
		t.Fatalf("expected stream=true in request, got %v", gotReq)
	}
	if gotReq["partial_images"] == nil {
		t.Fatalf("expected partial_images set, got %v", gotReq)
	}

	log := stderr.String()
	if !strings.Contains(log, "openai: partial 1 received") {
		t.Fatalf("missing partial 1 progress: %q", log)
	}
	if !strings.Contains(log, "openai: partial 2 received") {
		t.Fatalf("missing partial 2 progress: %q", log)
	}
	if !strings.Contains(log, "openai: final image received") {
		t.Fatalf("missing completion progress: %q", log)
	}
}

func TestGenerateOpenAI_StreamFallsBackToJSONForCountGreaterOne(t *testing.T) {
	var (
		streamHits int
		jsonHits   int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The stream path sets Accept: text/event-stream; the JSON path
		// does not. Distinguishing on the header is simpler than reading
		// the body twice.
		if r.Header.Get("Accept") == "text/event-stream" {
			streamHits++
			t.Errorf("stream path unexpectedly taken for count>1")
			return
		}
		jsonHits++
		resp := openAIResponse{Data: []openAIImageData{
			{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG)},
			{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG)},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)

	imgs, err := generateOpenAI("k", "gpt-image-2", "x", "", "", "png", 2, nil, "", true, io.Discard)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(imgs) != 2 {
		t.Fatalf("want 2 images, got %d", len(imgs))
	}
	if streamHits != 0 || jsonHits != 1 {
		t.Fatalf("path hit counts off: stream=%d json=%d", streamHits, jsonHits)
	}
}

func TestGenerateOpenAI_StreamMissingCompletedIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Only a partial — no completed event.
		writeSSE(t, w, "image_generation.partial_image", map[string]any{
			"type":                "image_generation.partial_image",
			"b64_json":            base64.StdEncoding.EncodeToString(tinyPNG),
			"partial_image_index": 0,
		})
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)

	_, err := generateOpenAI("k", "gpt-image-2", "x", "", "", "png", 1, nil, "", true, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "without a completed event") {
		t.Fatalf("expected missing-completed error, got %v", err)
	}
}

func TestGenerateOpenAI_StreamAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)

	_, err := generateOpenAI("k", "gpt-image-2", "x", "", "", "png", 1, nil, "", true, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "API error (401)") {
		t.Fatalf("expected 401 surface from stream, got %v", err)
	}
}

func TestGenerateOpenAI_StreamEditEndpointWithInput(t *testing.T) {
	ref := writeTempPNG(t, "ref.png")

	var (
		gotInputs int
		gotStream string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("want multipart, got %q", r.Header.Get("Content-Type"))
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		gotInputs = len(r.MultipartForm.File["image[]"])
		gotStream = r.FormValue("stream")

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(t, w, "image_edit.partial_image", map[string]any{
			"type":                "image_edit.partial_image",
			"b64_json":            base64.StdEncoding.EncodeToString(tinyPNG),
			"partial_image_index": 0,
		})
		writeSSE(t, w, "image_edit.completed", map[string]any{
			"type":     "image_edit.completed",
			"b64_json": base64.StdEncoding.EncodeToString(tinyPNG),
		})
	}))
	defer srv.Close()
	swapURL(&openAIEditsAPIURL, srv.URL)(t)

	var stderr bytes.Buffer
	imgs, err := generateOpenAI("k", "gpt-image-2", "edit it", "1024x1024", "high", "png", 1,
		[]string{ref}, "", true, &stderr)
	if err != nil {
		t.Fatalf("edit stream: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	if gotInputs != 1 {
		t.Fatalf("want 1 image[] upload, got %d", gotInputs)
	}
	if gotStream != "true" {
		t.Fatalf("want stream=true form field, got %q", gotStream)
	}
	if !strings.Contains(stderr.String(), "openai-edit: partial 1 received") {
		t.Fatalf("missing edit-stream progress: %q", stderr.String())
	}
}

func TestGenerateOpenAI_StreamDisabledTakesJSONPath(t *testing.T) {
	hit := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit++
		if r.Header.Get("Accept") == "text/event-stream" {
			t.Errorf("stream path taken when --stream=false")
		}
		resp := openAIResponse{Data: []openAIImageData{
			{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG)},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)

	imgs, err := generateOpenAI("k", "gpt-image-2", "x", "", "", "png", 1, nil, "", false, io.Discard)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(imgs) != 1 || hit != 1 {
		t.Fatalf("unexpected: imgs=%d hit=%d", len(imgs), hit)
	}
}
