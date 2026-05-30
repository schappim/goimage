package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const (
	defaultProvider = "openai"

	// OpenAI defaults. gpt-image-2 is the current recommended default per
	// OpenAI's prompting guide. Note: this model requires API Organization
	// Verification on the calling key; fall back to gpt-image-1 (or one of
	// the *-1.5 / *-1-mini variants) via -m if verification isn't enabled.
	defaultOpenAIModel   = "gpt-image-2"
	defaultOpenAISize    = "1024x1024"
	defaultOpenAIQuality = "auto"
	defaultOpenAIFormat  = "png"

	// Google nano banana defaults.
	defaultGoogleModel  = "gemini-2.5-flash-image"
	defaultGoogleAspect = "1:1"

	// xAI Grok defaults.
	defaultGrokModel = "grok-2-image"

	defaultCount = 1

	// OpenAI partial-image streaming. Two intermediate snapshots strikes a
	// balance between perceived progress and extra base64 bytes on the wire.
	defaultOpenAIPartialImages = 2

	// Number of attempts per generation before giving up.
	maxRetries = 3
)

// Initial backoff between retries (doubled each attempt). Declared as a var
// rather than a const so tests can swap it in for fast-running retry tests.
var initialBackoff = 1 * time.Second

// httpTimeout is the single source of truth for how long any provider request
// may run. Applied as a context deadline (not http.Client.Timeout) so it
// covers the whole call — dial, the model's server-side render time, and the
// response read — while a slow render is still allowed to finish. Image models
// legitimately take minutes, so the default is generous; the --timeout flag
// overrides it. Declared as a var so the flag and tests can swap it.
var httpTimeout = 300 * time.Second

// Provider endpoint URLs. Declared as vars so tests can repoint them at an
// httptest server without touching the calling code.
var (
	openAIAPIURL      = "https://api.openai.com/v1/images/generations"
	openAIEditsAPIURL = "https://api.openai.com/v1/images/edits"
	googleAPIURL      = "https://generativelanguage.googleapis.com/v1beta/models"
	grokAPIURL        = "https://api.x.ai/v1/images/generations"
)

// generatedImage is the common payload returned by every provider.
type generatedImage struct {
	data          []byte
	ext           string // "png", "jpeg", "webp"
	revisedPrompt string
}

// decodeB64 accepts either a bare base64 payload or a data-URI string
// ("data:image/png;base64,..."). Providers normalise to bare base64 in their
// docs but some return data URIs in practice, so handle both.
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "data:") {
		if i := strings.Index(s, ","); i >= 0 {
			s = s[i+1:]
		}
	}
	return base64.StdEncoding.DecodeString(s)
}

// isTimeout reports whether err is a client-side deadline/timeout — i.e. we
// gave up before the server responded — as opposed to an API rejection or a
// transient network blip. Typed inspection only, never string matching:
// context.DeadlineExceeded covers our context.WithTimeout deadline, and the
// net.Error.Timeout() interface (which *url.Error unwraps to) is the backstop.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

// explainError turns a low-level failure into actionable, human-readable
// guidance whenever we can recognise it by type (never by string matching),
// falling back to the raw error otherwise. It exists so the single
// user-facing error line isn't a cryptic Go transport string like "context
// deadline exceeded (Client.Timeout exceeded while awaiting headers)" with no
// path forward — the user should be told what happened and what to try.
func explainError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case isTimeout(err):
		return fmt.Sprintf(
			"the request timed out after %v before the provider responded.\n"+
				"The image model was most likely still rendering — this is a client-side\n"+
				"deadline, not an API rejection, and re-running the same command unchanged\n"+
				"will hit the same deadline. Try one of:\n"+
				"  - give it longer:      --timeout 10m\n"+
				"  - make it cheaper:     --quality low   (or medium)\n"+
				"  - watch live progress: --stream        (on by default for a single image)\n"+
				"  - simplify the prompt, or lower --count\n"+
				"(underlying error: %v)", httpTimeout, err)
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Sprintf(
			"could not resolve the provider's hostname (%s).\n"+
				"Check your internet connection, DNS, or any proxy/VPN.\n"+
				"(underlying error: %v)", dnsErr.Name, err)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return fmt.Sprintf(
			"could not reach the provider (network error while trying to %s).\n"+
				"Check your internet connection, firewall, or proxy/VPN settings.\n"+
				"(underlying error: %v)", opErr.Op, err)
	}
	return err.Error()
}

// apiError formats a non-2xx provider response into a concise error. Some edge
// layers (Cloudflare on a 5xx, for example) return a full HTML page instead of
// JSON; echoing that verbatim buries the signal under a screenful of markup, so
// collapse HTML and over-long bodies into a short summary. Detection is
// structural only (doctype/tag, length) — not semantic string matching.
func apiError(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	switch low := strings.ToLower(msg); {
	case msg == "":
		msg = "(empty response body)"
	case strings.HasPrefix(low, "<!doctype"), strings.HasPrefix(low, "<html"), strings.Contains(low, "<html"):
		msg = "provider returned an HTML error page instead of JSON — usually a transient upstream/proxy error, try again in a moment"
	case len(msg) > 300:
		msg = msg[:300] + "… (truncated)"
	}
	return fmt.Errorf("API error (%d): %s", status, msg)
}

// withRetry retries fn up to maxRetries times with exponential backoff.
// The label is used for log messages so the user can see which attempt is
// retrying. A client-side timeout is the exception: re-issuing the identical
// request with the same deadline can only fail the same way, so we surface it
// immediately rather than burning another full timeout (and misleading the
// user with "retrying in 1s" lines) on a doomed attempt.
func withRetry(label string, fn func() ([]byte, error)) ([]byte, error) {
	var lastErr error
	backoff := initialBackoff
	for attempt := 1; attempt <= maxRetries; attempt++ {
		data, err := fn()
		if err == nil {
			return data, nil
		}
		lastErr = err
		if isTimeout(err) {
			return nil, fmt.Errorf("%s timed out after %v: %w", label, httpTimeout, err)
		}
		if attempt < maxRetries {
			fmt.Fprintf(os.Stderr, "%s: attempt %d/%d failed: %v (retrying in %v)\n",
				label, attempt, maxRetries, err, backoff)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("%s failed after %d attempts: %w", label, maxRetries, lastErr)
}
