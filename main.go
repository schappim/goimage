package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// openImageFn is the function used to open a saved image in the OS viewer.
// Wired through a package var so tests can substitute a no-op without
// shelling out to `open`/`xdg-open`/`start`.
var openImageFn = openImage

func main() {
	var stdin io.Reader = strings.NewReader("")
	if stat, err := os.Stdin.Stat(); err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		stdin = os.Stdin
	}
	os.Exit(run(os.Args[1:], stdin, os.Stdout, os.Stderr, os.Getenv))
}

// run is the testable entry point. It returns an exit code instead of calling
// os.Exit so tests can drive the CLI without killing the process. Stdin,
// stdout, stderr, and the env lookup are all injected for the same reason.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	var (
		provider string
		model    string
		output   string
		size     string
		quality  string
		format   string
		aspect   string
		count    int
		openIt   bool
		token    string
		help     bool
	)

	fs := flag.NewFlagSet("goimage", flag.ContinueOnError)
	fs.SetOutput(stderr)

	fs.StringVar(&provider, "provider", defaultProvider, "Image provider (openai, google, grok)")
	fs.StringVar(&provider, "p", defaultProvider, "Image provider (shorthand)")
	fs.StringVar(&model, "model", "", "Model to use")
	fs.StringVar(&model, "m", "", "Model to use (shorthand)")
	fs.StringVar(&output, "output", "", "Save image to this path (auto-named when omitted)")
	fs.StringVar(&output, "o", "", "Save image to this path (shorthand)")
	fs.StringVar(&size, "size", "", "Image size, e.g. 1024x1024 (OpenAI / Grok)")
	fs.StringVar(&size, "s", "", "Image size (shorthand)")
	fs.StringVar(&quality, "quality", "", "Quality: low, medium, high, auto (OpenAI)")
	fs.StringVar(&quality, "q", "", "Quality (shorthand)")
	fs.StringVar(&format, "format", "", "Output format: png, jpeg, webp (OpenAI)")
	fs.StringVar(&aspect, "aspect", "", "Aspect ratio: 1:1, 16:9, 9:16, 4:3, 3:4 (Google)")
	fs.IntVar(&count, "count", defaultCount, "Number of images to generate")
	fs.IntVar(&count, "n", defaultCount, "Number of images to generate (shorthand)")
	fs.BoolVar(&openIt, "open", false, "Open saved image(s) in the default OS viewer")
	fs.StringVar(&token, "token", "", "API key for the provider")
	fs.BoolVar(&help, "help", false, "Show help")
	fs.BoolVar(&help, "h", false, "Show help (shorthand)")

	// Usage triggered by flag parser errors or by the "missing required input"
	// paths goes to stderr. Explicit --help is handled below and prints to
	// stdout so callers like `goimage --help | grep ...` (and the Homebrew
	// formula test, which only captures stdout) see the help text.
	fs.Usage = func() { printUsage(stderr) }

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if help {
		printUsage(stdout)
		return 0
	}

	provider = strings.ToLower(provider)
	switch provider {
	case "openai", "google", "grok":
	default:
		fmt.Fprintf(stderr, "Error: Invalid provider '%s'. Use 'openai', 'google', or 'grok'\n", provider)
		return 1
	}

	if model == "" {
		switch provider {
		case "openai":
			model = defaultOpenAIModel
		case "google":
			model = defaultGoogleModel
		case "grok":
			model = defaultGrokModel
		}
	}

	apiKey := token
	if apiKey == "" {
		apiKey = lookupAPIKey(provider, getenv)
	}
	if apiKey == "" {
		fmt.Fprintf(stderr, "Error: %s environment variable not set and --token not provided\n", primaryEnvVar(provider))
		return 1
	}

	if count < 1 {
		fmt.Fprintln(stderr, "Error: --count must be >= 1")
		return 1
	}

	var prompt string
	if fs.NArg() > 0 {
		prompt = strings.Join(fs.Args(), " ")
	} else {
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading stdin: %v\n", err)
			return 1
		}
		prompt = strings.TrimSpace(string(data))
	}
	if prompt == "" {
		fmt.Fprintln(stderr, "Error: No prompt provided")
		fs.Usage()
		return 1
	}

	images, err := generate(provider, apiKey, model, prompt, size, quality, format, aspect, count, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "Error generating image: %v\n", err)
		return 1
	}
	if len(images) == 0 {
		fmt.Fprintln(stderr, "Error: provider returned no images")
		return 1
	}

	for i, img := range images {
		path := destPath(output, provider, i, len(images), img.ext)
		if err := os.WriteFile(path, img.data, 0644); err != nil {
			fmt.Fprintf(stderr, "Error saving file: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, path)
		if img.revisedPrompt != "" {
			fmt.Fprintf(stderr, "Revised prompt: %s\n", img.revisedPrompt)
		}
		if openIt {
			if err := openImageFn(path); err != nil {
				fmt.Fprintf(stderr, "Warning: could not open image: %v\n", err)
			}
		}
	}
	return 0
}

// printUsage writes the CLI's help text. POSIX convention is help-on-stdout
// when the user asked explicitly (--help), help-on-stderr when triggered by a
// flag parser error or missing required input. Centralised here so both paths
// stay identical.
func printUsage(w io.Writer) {
	fmt.Fprintf(w, "goimage - Image generation via OpenAI, Google nano banana, or xAI Grok\n\n")
	fmt.Fprintf(w, "Usage: goimage [options] [prompt]\n")
	fmt.Fprintf(w, "       echo 'prompt' | goimage [options]\n\n")
	fmt.Fprintf(w, "Options:\n")
	fmt.Fprintf(w, "  -p, --provider   Provider: openai, google, grok (default: openai)\n")
	fmt.Fprintf(w, "  -m, --model      Model to use (provider-specific default)\n")
	fmt.Fprintf(w, "  -o, --output     Save image to this path (auto-named if omitted)\n")
	fmt.Fprintf(w, "  -s, --size       Image size, e.g. 1024x1024 (OpenAI / Grok)\n")
	fmt.Fprintf(w, "  -q, --quality    Quality: low, medium, high, auto (OpenAI)\n")
	fmt.Fprintf(w, "      --format     Output format: png, jpeg, webp (OpenAI)\n")
	fmt.Fprintf(w, "      --aspect     Aspect ratio (Google): 1:1, 16:9, 9:16, 4:3, 3:4\n")
	fmt.Fprintf(w, "  -n, --count      Number of images to generate (default: 1)\n")
	fmt.Fprintf(w, "      --open       Open the saved image in your default viewer\n")
	fmt.Fprintf(w, "      --token      API key (or set provider env var)\n")
	fmt.Fprintf(w, "  -h, --help       Show this help message\n\n")

	fmt.Fprintf(w, "OpenAI:\n")
	fmt.Fprintf(w, "  Env var: OPENAI_API_KEY\n")
	fmt.Fprintf(w, "  Models:  gpt-image-2 (default), gpt-image-1.5, gpt-image-1, gpt-image-1-mini\n")
	fmt.Fprintf(w, "           (gpt-image-2 requires API Organization Verification)\n")
	fmt.Fprintf(w, "  Sizes:   1024x1024, 1536x1024, 1024x1536, auto\n")
	fmt.Fprintf(w, "  Quality: low, medium, high, auto\n")
	fmt.Fprintf(w, "  Formats: png (default), jpeg, webp\n\n")

	fmt.Fprintf(w, "Google (nano banana):\n")
	fmt.Fprintf(w, "  Env var: GEMINI_API_KEY (or GOOGLE_API_KEY)\n")
	fmt.Fprintf(w, "  Models:  gemini-2.5-flash-image (default)\n")
	fmt.Fprintf(w, "  Aspect:  1:1 (default), 16:9, 9:16, 4:3, 3:4\n\n")

	fmt.Fprintf(w, "Grok (xAI):\n")
	fmt.Fprintf(w, "  Env var: XAI_API_KEY (or GROK_API_KEY)\n")
	fmt.Fprintf(w, "  Models:  grok-2-image (default)\n")
	fmt.Fprintf(w, "  Note:    size/quality/format are ignored by Grok\n\n")

	fmt.Fprintf(w, "Examples:\n")
	fmt.Fprintf(w, "  goimage \"a watercolor of a fox in autumn leaves\"\n")
	fmt.Fprintf(w, "  goimage -p google \"a cyberpunk teacup\" --aspect 16:9 --open\n")
	fmt.Fprintf(w, "  echo \"a logo for goimage\" | goimage -p grok -o logo.png\n")
	fmt.Fprintf(w, "  goimage -n 4 -o variants.png \"hand-drawn space whale\"\n")
}

// generate dispatches to the requested provider. Kept as its own seam so each
// provider's signature can vary while run() stays small.
func generate(provider, apiKey, model, prompt, size, quality, format, aspect string, count int, stderr io.Writer) ([]generatedImage, error) {
	switch provider {
	case "openai":
		return generateOpenAI(apiKey, model, prompt, size, quality, format, count)
	case "google":
		return generateGoogle(apiKey, model, prompt, aspect, count)
	case "grok":
		return generateGrok(apiKey, model, prompt, count)
	}
	return nil, fmt.Errorf("unknown provider %q", provider)
}

// lookupAPIKey resolves the API key for a provider, accepting common
// alternative env-var names (GOOGLE_API_KEY for Gemini, GROK_API_KEY for xAI).
func lookupAPIKey(provider string, getenv func(string) string) string {
	switch provider {
	case "openai":
		return getenv("OPENAI_API_KEY")
	case "google":
		if v := getenv("GEMINI_API_KEY"); v != "" {
			return v
		}
		return getenv("GOOGLE_API_KEY")
	case "grok":
		if v := getenv("XAI_API_KEY"); v != "" {
			return v
		}
		return getenv("GROK_API_KEY")
	}
	return ""
}

func primaryEnvVar(provider string) string {
	switch provider {
	case "openai":
		return "OPENAI_API_KEY"
	case "google":
		return "GEMINI_API_KEY"
	case "grok":
		return "XAI_API_KEY"
	}
	return ""
}

// destPath picks the file path to write a generated image to. When the user
// passed --output, single-image runs use that path verbatim and multi-image
// runs append a 1-based index before the extension. Otherwise an auto-named
// timestamped file lands in the current directory.
func destPath(userPath, provider string, idx, total int, ext string) string {
	if userPath != "" {
		if total <= 1 {
			return userPath
		}
		return numberedFilename(userPath, idx)
	}
	stamp := time.Now().Format("20060102-150405")
	if total <= 1 {
		return fmt.Sprintf("goimage-%s-%s.%s", provider, stamp, ext)
	}
	return fmt.Sprintf("goimage-%s-%s-%d.%s", provider, stamp, idx+1, ext)
}

func numberedFilename(base string, idx int) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return fmt.Sprintf("%s-%d%s", stem, idx+1, ext)
}

func openImage(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "linux":
		cmd = exec.Command("xdg-open", path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", path)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	return cmd.Start()
}
