package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunOpenAIReferenceImages(t *testing.T) {
	for _, tc := range []struct {
		name, model, quality string
		stream               bool
		count                int
	}{
		{"default streaming", "", "xhigh", true, 1},
		{"default JSON", "", "max", false, 1},
		{"multiple outputs", "", "high", true, 2},
		{"Flare override", "gpt-image-2.5-flare", "max", true, 1},
		{"snapshot override", "gpt-image-2.5-sunburst-2026-09-08", "high", false, 1},
		{"older model", "gpt-image-2", "high", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := []string{writeTempPNG(t, "product"), writeTempPNG(t, "style.jpg"), writeTempPNG(t, "third.png")}
			wantModel := tc.model
			if wantModel == "" {
				wantModel = "gpt-image-2.5-sunburst"
			}
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if r.Method != "POST" || r.URL.Path != "/edits" || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				defer r.MultipartForm.RemoveAll()
				for field, want := range map[string]string{"model": wantModel, "prompt": "combine these references", "quality": tc.quality, "size": "1536x864", "output_format": "png", "n": fmt.Sprint(tc.count)} {
					if got := r.FormValue(field); got != want {
						t.Errorf("%s = %q, want %q", field, got, want)
					}
				}
				files := r.MultipartForm.File["image[]"]
				if len(files) != len(refs) {
					t.Errorf("got %d references, want %d", len(files), len(refs))
					w.WriteHeader(400)
					return
				}
				for i, file := range files {
					if file.Filename != filepath.Base(refs[i]) || file.Header.Get("Content-Type") != "image/png" {
						t.Errorf("reference %d: wrong filename or content type: %+v", i, file)
					}
					f, err := file.Open()
					if err != nil {
						t.Error(err)
						return
					}
					data, err := io.ReadAll(f)
					f.Close()
					if err != nil || !bytes.Equal(data, tinyPNG) {
						t.Errorf("reference %d upload was changed or truncated", i)
					}
				}
				if tc.stream && tc.count == 1 {
					if r.FormValue("stream") != "true" || r.FormValue("partial_images") != "2" {
						t.Error("missing streaming fields")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					writeSSE(t, w, "image_edit.completed", map[string]any{"type": "image_edit.completed", "b64_json": base64.StdEncoding.EncodeToString(tinyPNG)})
				} else {
					if r.FormValue("stream") != "" {
						t.Error("unexpected streaming request")
					}
					resp := openAIResponse{}
					for i := 0; i < tc.count; i++ {
						resp.Data = append(resp.Data, openAIImageData{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG)})
					}
					json.NewEncoder(w).Encode(resp)
				}
			}))
			defer srv.Close()
			swapURL(&openAIEditsAPIURL, srv.URL+"/edits")(t)
			swapURL(&openAIAPIURL, srv.URL+"/generations")(t)
			output := filepath.Join(t.TempDir(), "result.png")
			args := []string{"--reference", refs[0], "--input", refs[1], "-i", refs[2], "-o", output, "-q", tc.quality, "-s", "1536x864", "-n", fmt.Sprint(tc.count), "--stream=" + fmt.Sprint(tc.stream)}
			if tc.model != "" {
				args = append(args, "--model", tc.model)
			}
			args = append(args, "combine these references")
			var stdout, stderr bytes.Buffer
			if code := run(args, strings.NewReader(""), &stdout, &stderr, envMap(map[string]string{"OPENAI_API_KEY": "test-key"})); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			paths := strings.Fields(stdout.String())
			if hits != 1 || len(paths) != tc.count {
				t.Fatalf("requests=%d outputs=%v", hits, paths)
			}
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, tinyPNG) {
					t.Errorf("saved image %q: data=%x err=%v", path, data, err)
				}
			}
		})
	}
}

func TestOpenAIRejectsInvalidReferencesBeforeRequest(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.png")
	textFile := filepath.Join(dir, "not-an-image.png")
	for path, data := range map[string][]byte{empty: nil, textFile: []byte("not an image")} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid reference reached API")
		w.WriteHeader(400)
	}))
	defer srv.Close()
	swapURL(&openAIEditsAPIURL, srv.URL)(t)
	for _, stream := range []bool{false, true} {
		for _, path := range []string{empty, textFile, dir, filepath.Join(dir, "missing.png")} {
			_, err := generateOpenAI("key", defaultOpenAIModel, "edit", "", "", "", 1, []string{path}, "", stream, io.Discard)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("path=%q stream=%v: expected file error, got %v", path, stream, err)
			}
		}
	}
}

func TestRunOpenAIDefaultGenerationModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != "gpt-image-2.5-sunburst" {
			t.Errorf("default generation model = %q", req.Model)
		}
		json.NewEncoder(w).Encode(openAIResponse{Data: []openAIImageData{{B64JSON: base64.StdEncoding.EncodeToString(tinyPNG)}}})
	}))
	defer srv.Close()
	swapURL(&openAIAPIURL, srv.URL)(t)
	var stderr bytes.Buffer
	code := run([]string{"--stream=false", "-o", filepath.Join(t.TempDir(), "out.png"), "a cat"}, strings.NewReader(""), io.Discard, &stderr, envMap(map[string]string{"OPENAI_API_KEY": "test-key"}))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}
