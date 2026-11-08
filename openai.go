package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
)

// quoteEscaper mirrors the (unexported) escaper mime/multipart uses for
// Content-Disposition field/file names, so our hand-built parts stay valid.
var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

// openAIRequest is the body POSTed to /v1/images/generations for gpt-image-*
// models. Fields are omitted when empty so the API gets to apply its own
// defaults (e.g. quality=auto, size=auto).
type openAIRequest struct {
	Model           string      `json:"model"`
	Prompt          string      `json:"prompt"`
	N               int         `json:"n,omitempty"`
	Size            string      `json:"size,omitempty"`
	Quality         string      `json:"quality,omitempty"`
	OutputFormat    string      `json:"output_format,omitempty"`
	Complexity      string      `json:"complexity,omitempty"`
	FrameCount      int         `json:"frame_count,omitempty"`
	FrameDurationMS int         `json:"frame_duration_ms,omitempty"`
	SVG             *svgOptions `json:"svg,omitempty"`
}

// svgOptions selects the model (and its reasoning effort) that authors SVG
// output from the image model's reference render.
type svgOptions struct {
	Model     string        `json:"model"`
	Reasoning *svgReasoning `json:"reasoning,omitempty"`
}

type svgReasoning struct {
	Effort string `json:"effort"`
}

// openAIOptions carries the GPT Image 2.6 request fields. Zero values are
// omitted from the request so the API applies its own defaults (complexity
// high, 9 GIF frames at 200 ms each).
type openAIOptions struct {
	complexity      string // low, medium, high
	markup          string // transparent PNG annotation layer for the first input image
	frameCount      int    // GIF only: 2–16
	frameDurationMS int    // GIF only: 20 ms or more
	svgModel        string // SVG only: model that authors the SVG; empty omits the svg object
	svgEffort       string // SVG only: author reasoning effort; empty uses the model default
}

// svgRequest returns the svg request object, or nil when it should be omitted
// (non-SVG output, or an account without SVG model selection).
func (o openAIOptions) svgRequest(format string) *svgOptions {
	if format != "svg" || o.svgModel == "" {
		return nil
	}
	svg := &svgOptions{Model: o.svgModel}
	if o.svgEffort != "" {
		svg.Reasoning = &svgReasoning{Effort: o.svgEffort}
	}
	return svg
}

// formFields renders the options as multipart form fields for the edits endpoint.
func (o openAIOptions) formFields() []struct{ k, v string } {
	var out []struct{ k, v string }
	if o.complexity != "" {
		out = append(out, struct{ k, v string }{"complexity", o.complexity})
	}
	if o.frameCount > 0 {
		out = append(out, struct{ k, v string }{"frame_count", fmt.Sprint(o.frameCount)})
	}
	if o.frameDurationMS > 0 {
		out = append(out, struct{ k, v string }{"frame_duration_ms", fmt.Sprint(o.frameDurationMS)})
	}
	return out
}

// validateOpenAIOptions checks the combinations the API documents as invalid,
// so the user gets a clear message instead of a round-trip and a 400.
func validateOpenAIOptions(format string, count int, inputs []string, mask string, opts openAIOptions) error {
	if opts.markup != "" {
		if len(inputs) == 0 {
			return fmt.Errorf("--markup requires an --input image to annotate")
		}
		if mask != "" {
			return fmt.Errorf("--markup cannot be combined with --mask")
		}
	}
	if format == "svg" {
		if count != 1 {
			return fmt.Errorf("SVG output supports only one image per request (use -n 1)")
		}
		if len(inputs) > 0 {
			return fmt.Errorf("SVG output is only available for generation, not edits; remove --input")
		}
	}
	if format != "svg" && opts.svgEffort != "" {
		return fmt.Errorf("--svg-effort requires --format svg")
	}
	if format == "gif" && mask != "" {
		return fmt.Errorf("GIF output does not support --mask")
	}
	if format != "gif" && (opts.frameCount != 0 || opts.frameDurationMS != 0) {
		return fmt.Errorf("--frames and --frame-duration require --format gif")
	}
	if opts.frameCount != 0 && (opts.frameCount < 2 || opts.frameCount > 16) {
		return fmt.Errorf("--frames must be between 2 and 16 (got %d)", opts.frameCount)
	}
	if opts.frameDurationMS != 0 && opts.frameDurationMS < 20 {
		return fmt.Errorf("--frame-duration must be at least 20ms (got %dms)", opts.frameDurationMS)
	}
	return nil
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

func generateOpenAI(apiKey, model, prompt, size, quality, format string, count int, inputs []string, mask string, stream bool, stderr io.Writer, opts openAIOptions) ([]generatedImage, error) {
	if mask != "" && len(inputs) == 0 {
		return nil, fmt.Errorf("--mask requires at least one --input image")
	}
	if format == "" {
		format = defaultOpenAIFormat
	}
	format = strings.ToLower(format)
	switch format {
	case "png", "jpeg", "webp", "svg", "gif":
	default:
		return nil, fmt.Errorf("invalid OpenAI format %q (expected png, jpeg, webp, svg, or gif)", format)
	}
	opts.complexity = strings.ToLower(opts.complexity)
	if err := validateOpenAIOptions(format, count, inputs, mask, opts); err != nil {
		return nil, err
	}

	// Reject unusable local files before making (or retrying) an API request.
	paths := append([]string(nil), inputs...)
	if mask != "" {
		paths = append(paths, mask)
	}
	for _, path := range paths {
		f, _, err := openReferenceImage(path)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	if opts.markup != "" {
		f, contentType, err := openReferenceImage(opts.markup)
		if err != nil {
			return nil, err
		}
		f.Close()
		// The annotation layer must keep its transparency, so only PNG works.
		if contentType != "image/png" {
			return nil, fmt.Errorf("markup %q: must be a transparent PNG, got %s", opts.markup, contentType)
		}
	}

	if size == "" {
		size = defaultOpenAISize
		// For GIFs size is per frame and every frame counts against the
		// request's pixel budget, so let the API pick a frame size that fits.
		if format == "gif" {
			size = "auto"
		}
	}
	if quality == "" {
		quality = defaultOpenAIQuality
	}

	// Streaming path: only meaningful for single-image requests. The SSE
	// schema returns one final image regardless of n, so anything > 1 falls
	// back to the JSON endpoint to preserve current behaviour. SVG and GIF
	// output don't support streaming or partial images at all.
	if stream && count == 1 && format != "svg" && format != "gif" {
		var (
			img generatedImage
			err error
		)
		if len(inputs) > 0 || mask != "" {
			img, err = openAIEditStreamCall(apiKey, model, prompt, size, quality, format, inputs, mask, opts, stderr)
		} else {
			img, err = openAIStreamCall(apiKey, model, prompt, size, quality, format, opts, stderr)
		}
		if err != nil {
			return nil, err
		}
		return []generatedImage{img}, nil
	}

	label := "openai"
	call := func() ([]byte, error) {
		return openAICall(apiKey, model, prompt, size, quality, format, count, opts)
	}
	if len(inputs) > 0 || mask != "" {
		label = "openai-edit"
		call = func() ([]byte, error) {
			return openAIEditCall(apiKey, model, prompt, size, quality, format, count, inputs, mask, opts)
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

	ext := extForFormat(format)

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

// extForFormat maps an output_format value to the file extension we save with.
func extForFormat(format string) string {
	switch format {
	case "", "png":
		return "png"
	case "jpeg":
		return "jpg"
	}
	return format
}

// openAIEditCall posts to /v1/images/edits as multipart/form-data with one or
// more reference images (image[]), an optional alpha-channel mask or markup
// layer, and the plain generation parameters as form fields.
func openAIEditCall(apiKey, model, prompt, size, quality, format string, count int, inputs []string, mask string, opts openAIOptions) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	addField := func(name, value string) error {
		if value == "" {
			return nil
		}
		return mw.WriteField(name, value)
	}
	fields := append([]struct{ k, v string }{
		{"model", model},
		{"prompt", prompt},
		{"size", size},
		{"quality", quality},
		{"output_format", format},
		{"n", fmt.Sprintf("%d", count)},
	}, opts.formFields()...)
	for _, kv := range fields {
		if err := addField(kv.k, kv.v); err != nil {
			return nil, fmt.Errorf("write field %s: %w", kv.k, err)
		}
	}

	if err := attachEditFiles(mw, inputs, mask, opts.markup); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, "POST", openAIEditsAPIURL, &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	// No http.Client.Timeout: the context deadline above governs the whole
	// call so a long-but-progressing render isn't cut off mid-flight.
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
		return nil, apiError(resp.StatusCode, body)
	}
	return body, nil
}

// attachEditFiles adds the reference images, then the optional mask or markup
// layer. Markup applies to the first image; the rest act as references.
func attachEditFiles(mw *multipart.Writer, inputs []string, mask, markup string) error {
	for _, path := range inputs {
		if err := attachFile(mw, "image[]", path); err != nil {
			return err
		}
	}
	if mask != "" {
		if err := attachFile(mw, "mask", mask); err != nil {
			return err
		}
	}
	if markup != "" {
		if err := attachFile(mw, "markup", markup); err != nil {
			return err
		}
	}
	return nil
}

// attachFile streams a file into a multipart writer as an image part. We build
// the part by hand rather than using mw.CreateFormFile because that helper
// hardcodes Content-Type: application/octet-stream, which the OpenAI edits
// endpoint rejects ("unsupported mimetype"). We set the real image mimetype
// (detected from the content) so extensionless and misnamed files also work.
func attachFile(mw *multipart.Writer, field, path string) error {
	f, contentType, err := openReferenceImage(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
		quoteEscaper.Replace(field), quoteEscaper.Replace(filepath.Base(path))))
	h.Set("Content-Type", contentType)
	fw, err := mw.CreatePart(h)
	if err != nil {
		return fmt.Errorf("create form part for %s: %w", path, err)
	}
	if _, err := io.Copy(fw, f); err != nil {
		return fmt.Errorf("copy %s into form: %w", path, err)
	}
	return nil
}

// openReferenceImage validates an upload and returns it rewound for copying.
func openReferenceImage(path string) (*os.File, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", path, err)
	}
	fail := func(err error) (*os.File, string, error) {
		f.Close()
		return nil, "", fmt.Errorf("reference image %q: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fail(fmt.Errorf("expected a non-empty regular image file"))
	}
	var header [512]byte
	n, err := f.Read(header[:])
	if err != nil && err != io.EOF {
		return fail(err)
	}
	contentType := http.DetectContentType(header[:n])
	switch contentType {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return fail(fmt.Errorf("unsupported content type %s; use PNG, JPEG, or WebP", contentType))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return f, contentType, nil
}

func openAICall(apiKey, model, prompt, size, quality, format string, count int, opts openAIOptions) ([]byte, error) {
	req := openAIRequest{
		Model:           model,
		Prompt:          prompt,
		N:               count,
		Size:            size,
		Quality:         quality,
		OutputFormat:    format,
		Complexity:      opts.complexity,
		FrameCount:      opts.frameCount,
		FrameDurationMS: opts.frameDurationMS,
		SVG:             opts.svgRequest(format),
	}
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, "POST", openAIAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	// No http.Client.Timeout: the context deadline above governs the whole
	// call so a long-but-progressing render isn't cut off mid-flight.
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
		return nil, apiError(resp.StatusCode, body)
	}
	return body, nil
}
