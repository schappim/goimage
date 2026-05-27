package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	defaultProvider = "openai"

	// OpenAI defaults — gpt-image-1 over gpt-image-2 because the newer model
	// requires Organization Verification on the API key, while gpt-image-1
	// works on a fresh key out of the box.
	defaultOpenAIModel   = "gpt-image-1"
	defaultOpenAISize    = "1024x1024"
	defaultOpenAIQuality = "auto"
	defaultOpenAIFormat  = "png"

	// Google nano banana defaults.
	defaultGoogleModel  = "gemini-2.5-flash-image"
	defaultGoogleAspect = "1:1"

	// xAI Grok defaults.
	defaultGrokModel = "grok-2-image"

	defaultCount = 1

	// Number of attempts per generation before giving up.
	maxRetries = 3
)

// Initial backoff between retries (doubled each attempt). Declared as a var
// rather than a const so tests can swap it in for fast-running retry tests.
var initialBackoff = 1 * time.Second

// Provider endpoint URLs. Declared as vars so tests can repoint them at an
// httptest server without touching the calling code.
var (
	openAIAPIURL = "https://api.openai.com/v1/images/generations"
	googleAPIURL = "https://generativelanguage.googleapis.com/v1beta/models"
	grokAPIURL   = "https://api.x.ai/v1/images/generations"
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

// withRetry retries fn up to maxRetries times with exponential backoff.
// The label is used for log messages so the user can see which attempt is retrying.
func withRetry(label string, fn func() ([]byte, error)) ([]byte, error) {
	var lastErr error
	backoff := initialBackoff
	for attempt := 1; attempt <= maxRetries; attempt++ {
		data, err := fn()
		if err == nil {
			return data, nil
		}
		lastErr = err
		if attempt < maxRetries {
			fmt.Fprintf(os.Stderr, "%s: attempt %d/%d failed: %v (retrying in %v)\n",
				label, attempt, maxRetries, err, backoff)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("%s failed after %d attempts: %w", label, maxRetries, lastErr)
}
