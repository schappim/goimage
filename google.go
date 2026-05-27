package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// googleRequest is the body POSTed to
// https://generativelanguage.googleapis.com/v1beta/models/<model>:generateContent
// for Gemini 2.5 Flash Image. The model returns images by default; aspectRatio
// is the only generation knob the REST API exposes for nano banana.
type googleRequest struct {
	Contents         []googleContent         `json:"contents"`
	GenerationConfig *googleGenerationConfig `json:"generationConfig,omitempty"`
}

type googleContent struct {
	Parts []googlePart `json:"parts"`
}

type googlePart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *googleInlineData `json:"inlineData,omitempty"`
}

type googleInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type googleGenerationConfig struct {
	ResponseFormat *googleResponseFormat `json:"responseFormat,omitempty"`
}

type googleResponseFormat struct {
	Image *googleImageConfig `json:"image,omitempty"`
}

type googleImageConfig struct {
	AspectRatio string `json:"aspectRatio,omitempty"`
}

type googleResponse struct {
	Candidates []googleCandidate `json:"candidates"`
	Error      *googleError      `json:"error,omitempty"`
}

type googleCandidate struct {
	Content googleContent `json:"content"`
}

type googleError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// generateGoogle calls Gemini 2.5 Flash Image. The REST API generates one
// image per request, so multi-image runs loop the call.
func generateGoogle(apiKey, model, prompt, aspect string, count int, inputs []string) ([]generatedImage, error) {
	if len(inputs) > 0 {
		return nil, fmt.Errorf("google reference-image support not yet wired in")
	}
	out := make([]generatedImage, 0, count)
	for i := 0; i < count; i++ {
		label := "google"
		if count > 1 {
			label = fmt.Sprintf("google %d/%d", i+1, count)
		}
		body, err := withRetry(label, func() ([]byte, error) {
			return googleCall(apiKey, model, prompt, aspect)
		})
		if err != nil {
			return nil, err
		}

		var resp googleResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("api error: %s", resp.Error.Message)
		}
		if len(resp.Candidates) == 0 {
			return nil, fmt.Errorf("api returned no candidates")
		}

		imgs, err := extractGoogleImages(resp.Candidates[0])
		if err != nil {
			return nil, err
		}
		out = append(out, imgs...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("api returned no image parts")
	}
	return out, nil
}

func extractGoogleImages(cand googleCandidate) ([]generatedImage, error) {
	var imgs []generatedImage
	for _, part := range cand.Content.Parts {
		if part.InlineData == nil || part.InlineData.Data == "" {
			continue
		}
		raw, err := decodeB64(part.InlineData.Data)
		if err != nil {
			return nil, fmt.Errorf("decode inline image: %w", err)
		}
		imgs = append(imgs, generatedImage{
			data: raw,
			ext:  extFromMIME(part.InlineData.MIMEType),
		})
	}
	return imgs, nil
}

func extFromMIME(mime string) string {
	switch strings.ToLower(mime) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	}
	return "png"
}

func googleCall(apiKey, model, prompt, aspect string) ([]byte, error) {
	req := googleRequest{
		Contents: []googleContent{
			{Parts: []googlePart{{Text: prompt}}},
		},
	}
	if aspect != "" {
		req.GenerationConfig = &googleGenerationConfig{
			ResponseFormat: &googleResponseFormat{
				Image: &googleImageConfig{AspectRatio: aspect},
			},
		}
	}

	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/%s:generateContent", googleAPIURL, model)
	httpReq, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", apiKey)

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
