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
