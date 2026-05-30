package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// imageStreamEvent is the SSE payload OpenAI emits for image generations and
// edits when stream=true. The same shape is reused across .partial_image,
// .completed, and .in_progress events; fields not present on a given event
// just stay zero-valued.
type imageStreamEvent struct {
	Type              string `json:"type"`
	B64JSON           string `json:"b64_json"`
	PartialImageIndex int    `json:"partial_image_index"`
}

// openAIStreamCall posts to /v1/images/generations with stream=true and
// returns the final image. While the SSE response is being read, progress
// updates are written to stderr so the caller sees partial frames as the
// model generates them. Returns an empty generatedImage if no final event
// arrives (the API contract guarantees one, but we surface it as an error
// rather than panicking on zero-length data).
func openAIStreamCall(apiKey, model, prompt, size, quality, format string, stderr io.Writer) (generatedImage, error) {
	payload := map[string]any{
		"model":          model,
		"prompt":         prompt,
		"n":              1,
		"stream":         true,
		"partial_images": defaultOpenAIPartialImages,
	}
	if size != "" {
		payload["size"] = size
	}
	if quality != "" {
		payload["quality"] = quality
	}
	if format != "" {
		payload["output_format"] = format
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return generatedImage{}, fmt.Errorf("marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", openAIAPIURL, bytes.NewReader(body))
	if err != nil {
		return generatedImage{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")

	return doStream(req, "openai", format, stderr)
}

// openAIEditStreamCall is the multipart equivalent of openAIStreamCall for the
// edits endpoint. Reference images and the optional mask ride along as form
// files; everything else mirrors the generations stream.
func openAIEditStreamCall(apiKey, model, prompt, size, quality, format string, inputs []string, mask string, stderr io.Writer) (generatedImage, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	fields := []struct{ k, v string }{
		{"model", model},
		{"prompt", prompt},
		{"size", size},
		{"quality", quality},
		{"output_format", format},
		{"n", "1"},
		{"stream", "true"},
		{"partial_images", fmt.Sprintf("%d", defaultOpenAIPartialImages)},
	}
	for _, kv := range fields {
		if kv.v == "" {
			continue
		}
		if err := mw.WriteField(kv.k, kv.v); err != nil {
			return generatedImage{}, fmt.Errorf("write field %s: %w", kv.k, err)
		}
	}
	for _, p := range inputs {
		if err := attachFile(mw, "image[]", p); err != nil {
			return generatedImage{}, err
		}
	}
	if mask != "" {
		if err := attachFile(mw, "mask", mask); err != nil {
			return generatedImage{}, err
		}
	}
	if err := mw.Close(); err != nil {
		return generatedImage{}, fmt.Errorf("close multipart writer: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", openAIEditsAPIURL, &buf)
	if err != nil {
		return generatedImage{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")

	return doStream(req, "openai-edit", format, stderr)
}

// doStream runs an SSE request to completion, logging partial-image events to
// stderr as they arrive and returning the final image bytes.
func doStream(req *http.Request, label, format string, stderr io.Writer) (generatedImage, error) {
	// The request already carries a context deadline (httpTimeout) set by the
	// caller, so the client itself needs no separate Timeout.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return generatedImage{}, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return generatedImage{}, apiError(resp.StatusCode, body)
	}

	ext := format
	switch ext {
	case "", "png":
		ext = "png"
	case "jpeg":
		ext = "jpg"
	}

	start := time.Now()
	var finalB64 string

	// SSE frames are blank-line separated; each frame is one or more
	// "field: value" lines. We only care about the data: lines.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		raw := data.String()
		data.Reset()
		if strings.TrimSpace(raw) == "[DONE]" {
			return nil
		}
		var ev imageStreamEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			// Non-fatal: skip unrecognised event shapes.
			return nil
		}
		elapsed := time.Since(start).Round(100 * time.Millisecond)
		switch {
		case strings.HasSuffix(ev.Type, ".partial_image"):
			fmt.Fprintf(stderr, "%s: partial %d received (%v)\n", label, ev.PartialImageIndex+1, elapsed)
		case strings.HasSuffix(ev.Type, ".completed"):
			fmt.Fprintf(stderr, "%s: final image received (%v)\n", label, elapsed)
			finalB64 = ev.B64JSON
		}
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return generatedImage{}, err
			}
			continue
		}
		// Comments / event: lines are not needed (the type is also in JSON).
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(payload)
		}
	}
	if err := scanner.Err(); err != nil {
		return generatedImage{}, fmt.Errorf("read stream: %w", err)
	}
	// Final flush for any trailing frame without a closing blank line.
	if err := flush(); err != nil {
		return generatedImage{}, err
	}

	if finalB64 == "" {
		return generatedImage{}, fmt.Errorf("stream ended without a completed event")
	}
	raw, err := decodeB64(finalB64)
	if err != nil {
		return generatedImage{}, fmt.Errorf("decode final image: %w", err)
	}
	return generatedImage{data: raw, ext: ext}, nil
}
