package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// openAIRequest is the body POSTed to /v1/images/generations for gpt-image-*
// models. Fields are omitted when empty so the API gets to apply its own
// defaults (e.g. quality=auto, size=auto).
type openAIRequest struct {
	Model        string `json:"model"`
	Prompt       string `json:"prompt"`
	N            int    `json:"n,omitempty"`
	Size         string `json:"size,omitempty"`
	Quality      string `json:"quality,omitempty"`
	OutputFormat string `json:"output_format,omitempty"`
}

type openAIResponse struct {
	Data  []openAIImageData `json:"data"`
	Error *openAIError      `json:"error,omitempty"`
}

type openAIImageData struct {
	B64JSON       string `json:"b64_json"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type openAIError struct {
	Message string `json:"message"`
}

func generateOpenAI(apiKey, model, prompt, size, quality, format string, count int, inputs []string, mask string, stream bool, stderr io.Writer) ([]generatedImage, error) {
	// Suppress unused warnings until openai_stream.go wires these in.
	_ = stream
	_ = stderr

	if size == "" {
		size = defaultOpenAISize
	}
	if quality == "" {
		quality = defaultOpenAIQuality
	}
	if format == "" {
		format = defaultOpenAIFormat
	}
	format = strings.ToLower(format)
	switch format {
	case "png", "jpeg", "webp":
	default:
		return nil, fmt.Errorf("invalid OpenAI format %q (expected png, jpeg, or webp)", format)
	}

	label := "openai"
	call := func() ([]byte, error) {
		return openAICall(apiKey, model, prompt, size, quality, format, count)
	}
	if len(inputs) > 0 || mask != "" {
		label = "openai-edit"
		call = func() ([]byte, error) {
			return openAIEditCall(apiKey, model, prompt, size, quality, format, count, inputs, mask)
		}
	}

	body, err := withRetry(label, call)
	if err != nil {
		return nil, err
	}

	var resp openAIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("api error: %s", resp.Error.Message)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("api returned no images")
	}

	ext := format
	if ext == "jpeg" {
		ext = "jpg"
	}

	out := make([]generatedImage, 0, len(resp.Data))
	for _, d := range resp.Data {
		if d.B64JSON == "" {
			return nil, fmt.Errorf("api returned image without b64_json data")
		}
		raw, err := decodeB64(d.B64JSON)
		if err != nil {
			return nil, fmt.Errorf("decode image data: %w", err)
		}
		out = append(out, generatedImage{
			data:          raw,
			ext:           ext,
			revisedPrompt: d.RevisedPrompt,
		})
	}
	return out, nil
}

// openAIEditCall posts to /v1/images/edits as multipart/form-data with one or
// more reference images (image[]) and an optional alpha-channel mask. Plain
// generation parameters ride along as form fields.
func openAIEditCall(apiKey, model, prompt, size, quality, format string, count int, inputs []string, mask string) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	addField := func(name, value string) error {
		if value == "" {
			return nil
		}
		return mw.WriteField(name, value)
	}
	for _, kv := range []struct{ k, v string }{
		{"model", model},
		{"prompt", prompt},
		{"size", size},
		{"quality", quality},
		{"output_format", format},
		{"n", fmt.Sprintf("%d", count)},
	} {
		if err := addField(kv.k, kv.v); err != nil {
			return nil, fmt.Errorf("write field %s: %w", kv.k, err)
		}
	}

	for _, path := range inputs {
		if err := attachFile(mw, "image[]", path); err != nil {
			return nil, err
		}
	}
	if mask != "" {
		if err := attachFile(mw, "mask", mask); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	httpReq, err := http.NewRequest("POST", openAIEditsAPIURL, &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 300 * time.Second}
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

// attachFile streams a file into a multipart writer using the file's basename.
// The OpenAI API derives the MIME type from the filename extension, so the
// name passed here matters.
func attachFile(mw *multipart.Writer, field, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	fw, err := mw.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return fmt.Errorf("create form file for %s: %w", path, err)
	}
	if _, err := io.Copy(fw, f); err != nil {
		return fmt.Errorf("copy %s into form: %w", path, err)
	}
	return nil
}

func openAICall(apiKey, model, prompt, size, quality, format string, count int) ([]byte, error) {
	req := openAIRequest{
		Model:        model,
		Prompt:       prompt,
		N:            count,
		Size:         size,
		Quality:      quality,
		OutputFormat: format,
	}
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", openAIAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 180 * time.Second}
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
