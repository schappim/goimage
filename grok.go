package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// grokRequest is the body POSTed to api.x.ai/v1/images/generations. The
// endpoint is OpenAI-compatible but ignores size/quality/format knobs; only
// model, prompt, n and response_format actually do anything.
type grokRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

type grokResponse struct {
	Data  []grokImageData `json:"data"`
	Error *grokError      `json:"error,omitempty"`
}

type grokImageData struct {
	B64JSON       string `json:"b64_json"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type grokError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func generateGrok(apiKey, model, prompt string, count int, inputs []string) ([]generatedImage, error) {
	if len(inputs) > 0 {
		return nil, fmt.Errorf("grok does not support reference images")
	}
	body, err := withRetry("grok", func() ([]byte, error) {
		return grokCall(apiKey, model, prompt, count)
	})
	if err != nil {
		return nil, err
	}

	var resp grokResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("api error: %s", resp.Error.Message)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("api returned no images")
	}

	out := make([]generatedImage, 0, len(resp.Data))
	for _, d := range resp.Data {
		if d.B64JSON == "" {
			return nil, fmt.Errorf("api returned image without b64_json data (set response_format=b64_json)")
		}
		raw, err := decodeB64(d.B64JSON)
		if err != nil {
			return nil, fmt.Errorf("decode image data: %w", err)
		}
		out = append(out, generatedImage{
			data:          raw,
			ext:           "png", // Grok image gen returns PNG.
			revisedPrompt: d.RevisedPrompt,
		})
	}
	return out, nil
}

func grokCall(apiKey, model, prompt string, count int) ([]byte, error) {
	req := grokRequest{
		Model:          model,
		Prompt:         prompt,
		N:              count,
		ResponseFormat: "b64_json",
	}
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, "POST", grokAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	// No http.Client.Timeout: the context deadline above governs the whole call.
	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	}
	return body, nil
}
