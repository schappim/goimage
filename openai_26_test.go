package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GPT Image 2.6 request fields: complexity, markup-guided edits, SVG output
// and animated GIF output.

var tinyGIF = []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")

func jsonImageServer(t *testing.T, payload []byte, check func(r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check(r)
		json.NewEncoder(w).Encode(openAIResponse{Data: []openAIImageData{{B64JSON: base64.StdEncoding.EncodeToString(payload)}}})
	}))
}

func TestRunOpenAIComplexityGeneration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		var got map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				writeSSE(t, w, "image_generation.completed", map[string]any{"type": "image_generation.completed", "b64_json": base64.StdEncoding.EncodeToString(tinyPNG)})
				return
			}
			json.NewEncoder(w).Encode(openAIResponse{Data: []openAIImageData{{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG)}}})
		}))
		swapURL(&openAIAPIURL, srv.URL)(t)
		var stderr bytes.Buffer
		args := []string{"--complexity", "Low", "--stream=" + map[bool]string{true: "true", false: "false"}[stream], "-o", filepath.Join(t.TempDir(), "x.png"), "a sign"}
		if code := run(args, strings.NewReader(""), io.Discard, &stderr, envMap(map[string]string{"OPENAI_API_KEY": "k"})); code != 0 {
			t.Fatalf("stream=%v exit %d: %s", stream, code, stderr.String())
		}
		srv.Close()
		if got["complexity"] != "low" || got["model"] != "gpt-image-2.6-sunburst" {
			t.Errorf("stream=%v: request = %v", stream, got)
		}
		if _, ok := got["frame_count"]; ok {
			t.Errorf("stream=%v: frame_count sent for non-GIF request", stream)
		}
	}
}

func TestRunOpenAIComplexityOmittedByDefault(t *testing.T) {
	var raw map[string]any
	srv := jsonImageServer(t, tinyPNG, func(r *http.Request) { json.NewDecoder(r.Body).Decode(&raw) })
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)
	if code := run([]string{"--stream=false", "-o", filepath.Join(t.TempDir(), "x.png"), "x"}, strings.NewReader(""), io.Discard, io.Discard, envMap(map[string]string{"OPENAI_API_KEY": "k"})); code != 0 {
		t.Fatal("run failed")
	}
	for _, k := range []string{"complexity", "frame_count", "frame_duration_ms"} {
		if _, ok := raw[k]; ok {
			t.Errorf("%s sent without being requested", k)
		}
	}
}

func TestRunOpenAISVG(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)
	var req openAIRequest
	srv := jsonImageServer(t, svg, func(r *http.Request) {
		if r.Header.Get("Accept") == "text/event-stream" {
			t.Error("SVG must not stream")
		}
		json.NewDecoder(r.Body).Decode(&req)
	})
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)
	dir := t.TempDir()
	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	// --stream defaults to true; SVG must fall back to a plain JSON request.
	if code := run([]string{"--format", "SVG", "a bakery logo"}, strings.NewReader(""), &stdout, &stderr, envMap(map[string]string{"OPENAI_API_KEY": "k"})); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if req.OutputFormat != "svg" || req.N != 1 || req.SVG == nil || req.SVG.Model != defaultOpenAISVGModel || req.SVG.Reasoning != nil {
		t.Errorf("request = %+v svg = %+v", req, req.SVG)
	}
	path := strings.TrimSpace(stdout.String())
	if !strings.HasSuffix(path, ".svg") {
		t.Errorf("saved as %q, want .svg", path)
	}
	if data, _ := os.ReadFile(path); !bytes.Equal(data, svg) {
		t.Errorf("saved %q", data)
	}
}

func TestRunOpenAIGIF(t *testing.T) {
	var req openAIRequest
	srv := jsonImageServer(t, tinyGIF, func(r *http.Request) { json.NewDecoder(r.Body).Decode(&req) })
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--format", "gif", "--frames", "12", "--frame-duration", "150ms", "a waving cat"}, strings.NewReader(""), &stdout, &stderr, envMap(map[string]string{"OPENAI_API_KEY": "k"})); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if req.OutputFormat != "gif" || req.FrameCount != 12 || req.FrameDurationMS != 150 || req.Size != "auto" {
		t.Errorf("request = %+v", req)
	}
	if path := strings.TrimSpace(stdout.String()); !strings.HasSuffix(path, ".gif") {
		t.Errorf("saved as %q, want .gif", path)
	}
}

func TestRunOpenAIGIFEditWithMarkupAndComplexity(t *testing.T) {
	ref := writeTempPNG(t, "kitchen.png")
	markup := writeTempPNG(t, "circle.png")
	srv := jsonImageServer(t, tinyGIF, func(r *http.Request) {
		if r.Header.Get("Accept") == "text/event-stream" {
			t.Error("GIF must not stream")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		for field, want := range map[string]string{"output_format": "gif", "frame_count": "4", "complexity": "medium", "size": "auto"} {
			if got := r.FormValue(field); got != want {
				t.Errorf("%s = %q, want %q", field, got, want)
			}
		}
		if r.FormValue("frame_duration_ms") != "" {
			t.Error("frame_duration_ms sent without being requested")
		}
		if f := r.MultipartForm.File["markup"]; len(f) != 1 || f[0].Filename != "circle.png" || f[0].Header.Get("Content-Type") != "image/png" {
			t.Errorf("markup part = %+v", f)
		}
		if len(r.MultipartForm.File["image[]"]) != 1 || len(r.MultipartForm.File["mask"]) != 0 {
			t.Error("wrong image/mask parts")
		}
	})
	defer srv.Close()
	swapURL(&openAIEditsAPIURL, srv.URL)(t)
	var stderr bytes.Buffer
	args := []string{"-i", ref, "--markup", markup, "--complexity", "medium", "--format", "gif", "--frames", "4", "-o", filepath.Join(t.TempDir(), "a.gif"), "animate the circled vase"}
	if code := run(args, strings.NewReader(""), io.Discard, &stderr, envMap(map[string]string{"OPENAI_API_KEY": "k"})); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestRunOpenAIMarkupStreamingEdit(t *testing.T) {
	ref := writeTempPNG(t, "kitchen.png")
	markup := writeTempPNG(t, "circle.png")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("stream") != "true" || len(r.MultipartForm.File["markup"]) != 1 || r.FormValue("model") != "gpt-image-2.6-flare" {
			t.Errorf("stream=%q markup=%d model=%q", r.FormValue("stream"), len(r.MultipartForm.File["markup"]), r.FormValue("model"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(t, w, "image_edit.completed", map[string]any{"type": "image_edit.completed", "b64_json": base64.StdEncoding.EncodeToString(tinyPNG)})
	}))
	defer srv.Close()
	swapURL(&openAIEditsAPIURL, srv.URL)(t)
	var stderr bytes.Buffer
	args := []string{"-m", "gpt-image-2.6-flare", "-i", ref, "--markup", markup, "-o", filepath.Join(t.TempDir(), "v.png"), "recolour the vase"}
	if code := run(args, strings.NewReader(""), io.Discard, &stderr, envMap(map[string]string{"OPENAI_API_KEY": "k"})); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestOpenAI26InvalidCombinationsRejectedBeforeRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid request reached API: %s", r.URL.Path)
		w.WriteHeader(400)
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)
	swapURL(&openAIEditsAPIURL, srv.URL)(t)
	ref := writeTempPNG(t, "ref.png")
	jpeg := filepath.Join(t.TempDir(), "markup.jpg")
	os.WriteFile(jpeg, []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), 0600)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"markup without input", []string{"--markup", ref}, "--markup requires"},
		{"markup with mask", []string{"-i", ref, "--mask", ref, "--markup", ref}, "cannot be combined"},
		{"markup not PNG", []string{"-i", ref, "--markup", jpeg}, "transparent PNG"},
		{"svg multiple", []string{"--format", "svg", "-n", "2"}, "one image"},
		{"svg edit", []string{"--format", "svg", "-i", ref}, "not edits"},
		{"frames without gif", []string{"--frames", "4"}, "--format gif"},
		{"frames out of range", []string{"--format", "gif", "--frames", "17"}, "between 2 and 16"},
		{"duration too short", []string{"--format", "gif", "--frame-duration", "10ms"}, "at least 20ms"},
		{"gif with mask", []string{"--format", "gif", "-i", ref, "--mask", ref}, "does not support --mask"},
		{"svg effort without svg", []string{"--svg-effort", "low"}, "--format svg"},
		{"negative duration", []string{"--format", "gif", "--frame-duration", "-1s"}, "must be > 0"},
		{"bad format", []string{"--format", "bmp"}, "svg, or gif"},
		{"google complexity", []string{"-p", "google", "--complexity", "low"}, "OpenAI-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			args := append(append([]string{"--token", "k"}, tc.args...), "prompt")
			if code := run(args, strings.NewReader(""), io.Discard, &stderr, envMap(nil)); code == 0 || !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("exit %d, stderr %q, want it to mention %q", code, stderr.String(), tc.want)
			}
		})
	}
}

func TestRunOpenAISVGAuthorOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string // raw JSON of the svg field, "" when omitted
	}{
		{"custom model and effort", []string{"--svg-model", "gpt-6-luna", "--svg-effort", "High"}, `{"model":"gpt-6-luna","reasoning":{"effort":"high"}}`},
		{"omitted for accounts without model selection", []string{"--svg-model", ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]json.RawMessage
			srv := jsonImageServer(t, []byte("<svg/>"), func(r *http.Request) { json.NewDecoder(r.Body).Decode(&raw) })
			defer srv.Close()
			swapURL(&openAIAPIURL, srv.URL)(t)
			args := append(append([]string{"--format", "svg", "-o", filepath.Join(t.TempDir(), "a.svg")}, tc.args...), "logo")
			var stderr bytes.Buffer
			if code := run(args, strings.NewReader(""), io.Discard, &stderr, envMap(map[string]string{"OPENAI_API_KEY": "k"})); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			if got := string(raw["svg"]); got != tc.want {
				t.Errorf("svg = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestOpenAISVGObjectOnlyForSVG(t *testing.T) {
	var raw map[string]any
	srv := jsonImageServer(t, tinyPNG, func(r *http.Request) { json.NewDecoder(r.Body).Decode(&raw) })
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)
	if code := run([]string{"--stream=false", "-o", filepath.Join(t.TempDir(), "x.png"), "x"}, strings.NewReader(""), io.Discard, io.Discard, envMap(map[string]string{"OPENAI_API_KEY": "k"})); code != 0 {
		t.Fatal("run failed")
	}
	if _, ok := raw["svg"]; ok {
		t.Error("svg object sent for PNG output")
	}
}
