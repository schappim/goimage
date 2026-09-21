# goimage

A self-contained command-line tool for image generation using OpenAI (GPT Image), Google Gemini 2.5 Flash Image (nano banana), or xAI Grok image APIs. Written in Go — just a single binary, no external dependencies.

## Features

- **Multiple image providers**: OpenAI, Google nano banana, xAI Grok
- **Single binary** - written in pure Go, no Python, no node, no ffmpeg
- **Reference input images** (`-i`) for image-to-image / edits on OpenAI and Google
- **Mask support** (`--mask`) for OpenAI inpainting
- **Automatic retries** - transient network/upstream blips retried up to 3 times with exponential backoff (a client-side timeout is reported immediately, not retried)
- **Configurable timeout** - `--timeout` sets a single deadline (default 5m) applied as a context deadline, so slow renders aren't cut off mid-flight
- **Actionable errors** - a timeout or connectivity failure is explained in plain language with what to try next, instead of a raw Go transport string
- **Streaming progress** - OpenAI partial-image events surface on stderr as the model renders (`--stream`, default on)
- Provider-native model defaults and overrides
- Prompt from arguments or stdin (great for piping)
- Save to PNG, JPEG, or WebP (where the provider supports it)
- Auto-named output files when `-o` is omitted
- `--open` to view the result in your default image viewer
- Cross-platform (macOS, Linux, Windows)

## Requirements

- macOS, Linux, or Windows
- API key for your chosen provider (OpenAI, Google AI Studio, or xAI)

## Installation

### Homebrew (Recommended)

```bash
brew tap schappim/goimage
brew install goimage
```

See the [homebrew-goimage](https://github.com/schappim/homebrew-goimage) tap for more details.

### Download Binary

Download the latest release from the [releases page](https://github.com/schappim/goimage/releases).

### Build from Source

```bash
git clone https://github.com/schappim/goimage.git
cd goimage
go build -o goimage .

# Optional: install to PATH
sudo cp goimage /usr/local/bin/
```

### Configuration

Set your API key(s) as environment variables:

```bash
# OpenAI (default provider)
export OPENAI_API_KEY="your-openai-api-key"

# Google Gemini (nano banana)
export GEMINI_API_KEY="your-google-ai-studio-key"   # or GOOGLE_API_KEY

# xAI Grok
export XAI_API_KEY="your-xai-key"                   # or GROK_API_KEY
```

Or pass the key directly with the `--token` flag.

## Usage

### Basic Usage (OpenAI)

```bash
# Generate from a prompt (uses OpenAI by default)
goimage "a watercolor of a fox curled up in autumn leaves"

# Pipe a prompt
echo "a hand-drawn map of a fantasy island" | goimage

# Save to a specific path
goimage -o fox.png "a watercolor of a fox in autumn leaves"

# Open the result in your default viewer after saving
goimage --open "a logo for a coffee shop called Pixel Brew"
```

### Using Google Gemini (nano banana)

```bash
# Switch provider
goimage -p google "a cyberpunk teacup on a marble counter"

# Set aspect ratio
goimage -p google --aspect 16:9 "a wide cinematic shot of a misty pine forest"
goimage -p google --aspect 9:16 "an iPhone-shaped portrait of a desert sunset"
```

**Supported aspect ratios:** `1:1` (default), `16:9`, `9:16`, `4:3`, `3:4`

### Using Grok (xAI)

```bash
# Switch provider
goimage -p grok "a Saturday-morning cartoon hedgehog as a barista"

# Multiple variations from one prompt
goimage -p grok -n 4 -o variants.png "logo for a Go CLI called goimage"
# → variants-1.png, variants-2.png, variants-3.png, variants-4.png
```

### Choose a Model

**OpenAI models:** `gpt-image-2.5-sunburst` (default), `gpt-image-2.5-flare`, `gpt-image-2`. Other model IDs and dated snapshots can be selected with `--model`.

Sunburst is OpenAI's current recommendation for precise image editing; Flare is the faster option for everyday generation. GPT Image models may require API Organization Verification. See the [official OpenAI image generation guide](https://developers.openai.com/api/docs/guides/image-generation) for availability and capabilities.

```bash
goimage "a photorealistic kitchen interior at golden hour"     # Sunburst by default
goimage -m gpt-image-2.5-flare "quick thumbnail of a smiling robot"
goimage -m gpt-image-2 "a watercolor landscape"
```

**Google models:** `gemini-2.5-flash-image` (default)

**Grok models:** `grok-2-image` (default)

### Size and Quality (OpenAI)

```bash
# Square (default) / landscape / portrait
goimage -s 1024x1024 "a square poster of a vinyl record"
goimage -s 1536x1024 "a landscape banner of mountains at dawn"
goimage -s 1024x1536 "a portrait of an astronaut on Mars"

# Quality
goimage -q low "rough draft - book cover concept"
goimage -q high "final book cover with detailed typography"
goimage -q xhigh "a detailed illustrated map"
goimage -q max "a polished product campaign image"
```

GPT Image 2.5 supports `low`, `medium`, `high`, `xhigh`, `max`, and `auto`; earlier models support up to `high`. Custom sizes such as `1536x864` are also supported by GPT Image 2 and 2.5: edges must be multiples of 16, no larger than 3840, with aspect ratio between 1:3 and 3:1 and 655,360–8,294,400 total pixels. Resolutions above `2560x1440` are experimental.

### Output Format (OpenAI)

```bash
goimage --format png  "default PNG output"
goimage --format jpeg "smaller JPEG"
goimage --format webp "modern WebP"
```

### Multiple Images at Once

```bash
goimage -n 3 "a cute pet rock with googly eyes"
# → goimage-openai-<timestamp>-1.png
# → goimage-openai-<timestamp>-2.png
# → goimage-openai-<timestamp>-3.png

goimage -n 3 -o cat.png "a cat in a hat"
# → cat-1.png cat-2.png cat-3.png
```

> Google generates one image per request, so `-n` makes N separate API calls.

### Reference Images (image-to-image / edits)

Pass `--reference`, `--input`, or `-i` one or more times to use local images as references. The aliases can be mixed and preserve input order. OpenAI uses its `/v1/images/edits` endpoint with Sunburst by default; Google attaches each image as an `inlineData` part alongside the prompt. Grok does not support reference images and will error. Place all options before the prompt.

OpenAI references must contain PNG, JPEG, or WebP data. The content type is detected from the file, so an extension is optional. Missing, empty, and unsupported files are rejected before contacting OpenAI. Reference edits support streaming progress by default, `--stream=false`, and multiple outputs with `-n`.

```bash
# Re-render an image with a new style
goimage -i original.png "make it watercolor"

# Use a product photo and a style reference
goimage --reference product.png --reference style.jpg -o poster.png \
        "create a poster for this product using the style of the second image"

# Multi-image composition (OpenAI gift basket pattern)
goimage -i lotion.png -i bath-bomb.png -i incense.png \
        "a photorealistic gift basket containing all of these items"

# Inpainting with a mask (OpenAI only — mask alpha channel defines edit area)
goimage -i lounge.png --mask pool-mask.png "put a flamingo in the pool"

# Google nano banana conversational edit
goimage -p google -i fox.png "make the season autumn, add falling leaves"
```

**Mask requirements (OpenAI):** PNG with an alpha channel, same dimensions as the first input image.

### Open in Default Viewer

```bash
goimage --open "a desk setup with mechanical keyboard and ferns"
```

`--open` shells out to `open` on macOS, `xdg-open` on Linux, and `start` on Windows.

## Options

| Option        | Short | Description                                       | Default     |
|---------------|-------|---------------------------------------------------|-------------|
| `--provider`  | `-p`  | Provider (`openai`, `google`, `grok`)             | `openai`    |
| `--model`     | `-m`  | Model to use                                      | Provider-specific |
| `--output`    | `-o`  | Save to this path                                 | Auto-named  |
| `--size`      | `-s`  | Image size (OpenAI)                               | `1024x1024` |
| `--quality`   | `-q`  | `low` / `medium` / `high` / `xhigh` / `max` / `auto` (OpenAI) | `auto` |
| `--format`    |       | `png` / `jpeg` / `webp` (OpenAI)                  | `png`       |
| `--aspect`    |       | Aspect ratio (Google)                             | `1:1`       |
| `--count`     | `-n`  | Number of images                                  | `1`         |
| `--input`     | `-i`  | Reference image path (repeatable)                 | -           |
| `--reference` |       | Alias for `--input` (repeatable)                  | -           |
| `--mask`      |       | Mask image (alpha) for OpenAI inpainting          | -           |
| `--stream`    |       | Stream OpenAI partial-image events to stderr      | `true`      |
| `--timeout`   |       | Max wait for the provider, e.g. `300s`, `10m`     | `5m`        |
| `--open`      |       | Open the saved image                              | `false`     |
| `--token`     |       | API key                                           | From env    |
| `--help`      | `-h`  | Show help                                         | -           |

## Provider Comparison

| Feature           | OpenAI                 | Google (nano banana)             | Grok                |
|-------------------|------------------------|----------------------------------|---------------------|
| Env var           | `OPENAI_API_KEY`       | `GEMINI_API_KEY` / `GOOGLE_API_KEY` | `XAI_API_KEY` / `GROK_API_KEY` |
| Default model     | `gpt-image-2.5-sunburst` | `gemini-2.5-flash-image`         | `grok-2-image`      |
| Size control      | Yes (size flag)        | Aspect ratio only                | Provider-fixed      |
| Quality control   | Yes (`low/medium/high/xhigh/max/auto`) | No                    | No                  |
| Output formats    | PNG, JPEG, WebP        | PNG                              | PNG                 |
| Multi-image (`n`) | Native (single request)| Looped (one per request)         | Native              |
| Reference (`-i`)  | Yes (`/v1/images/edits`)| Yes (inlineData parts)          | Not supported       |
| Mask (`--mask`)   | Yes (alpha channel)    | No                               | No                  |

## Scripting Examples

### Generate from a file of prompts

```bash
while IFS= read -r line; do
  goimage -o "prompt-$RANDOM.png" "$line"
done < prompts.txt
```

### Pipe from an LLM

```bash
llm "describe a cover for a sci-fi novel about whales in space" \
  | goimage -p google --aspect 9:16 --open
```

### Compare providers side-by-side

```bash
prompt="a single matte black coffee cup on white"
goimage -p openai -o openai.png "$prompt"
goimage -p google -o google.png "$prompt"
goimage -p grok   -o grok.png   "$prompt"
```

### Build a contact sheet of variants

```bash
goimage -n 9 -o variant.png "isometric pixel art of a fountain"
# variant-1.png .. variant-9.png
```

## Streaming Progress (OpenAI)

When the OpenAI provider is in use and `-n 1`, `goimage` opens an SSE
connection (`stream=true`, `partial_images=2`) and surfaces progress on
stderr as the model renders:

```
$ goimage "a watercolor of a fox in autumn leaves"
openai: partial 1 received (3.2s)
openai: partial 2 received (8.4s)
openai: final image received (11.9s)
goimage-openai-20260528-104812.png
```

stdout still only carries the final file path, so pipelines keep working
unchanged. Pass `--stream=false` to fall back to the non-streaming JSON
endpoint (e.g. when scripting and you don't want progress lines).
Streaming is automatically disabled for multi-image runs (`-n 2+`)
because the SSE API returns a single final image.

Google and Grok don't expose partial-image streaming, so `--stream` is a
no-op for those providers.

## Reliability

Transient failures — network blips and 5xx upstream errors — are retried up to 3 times with exponential backoff (1s, 2s, 4s), so they don't require a manual rerun.

A **client-side timeout** is treated differently. Every request runs under a single deadline (`--timeout`, default 5m) applied as a context deadline that covers the whole call, including the model's render time. If that deadline fires the model was almost certainly still rendering, and re-issuing the identical request would only fail the same way — so goimage reports it immediately (no wasted retries) with guidance: raise `--timeout`, lower `--quality`, use `--stream` to watch progress, or simplify the prompt. Image models can legitimately take minutes, especially at high quality, so prefer a generous `--timeout` over expecting a fixed ceiling.

Failures are reported on stderr; the final file path is written to stdout so you can pipe it into other tools.

## Error Handling

When something goes wrong, an error is written to stderr and the process exits with a non-zero status:

```
Error: OPENAI_API_KEY environment variable not set and --token not provided
Error: GEMINI_API_KEY environment variable not set and --token not provided
Error: XAI_API_KEY environment variable not set and --token not provided
Error: Invalid provider 'midjourney'. Use 'openai', 'google', or 'grok'
Error: --count must be >= 1
Error: invalid OpenAI format "bmp" (expected png, jpeg, or webp)
Error: --timeout must be > 0 (e.g. 300s, 10m)
```

A client-side timeout is reported with guidance rather than a raw transport error:

```
Error generating image: the request timed out after 5m0s before the provider responded.
The image model was most likely still rendering — this is a client-side
deadline, not an API rejection, and re-running the same command unchanged
will hit the same deadline. Try one of:
  - give it longer:      --timeout 10m
  - make it cheaper:     --quality low   (or medium)
  - watch live progress: --stream        (on by default for a single image)
  - simplify the prompt, or lower --count
```

## Help

```bash
goimage --help
```

## License

MIT License

## Contributing

Contributions welcome. Open an issue or PR on the [GitHub repo](https://github.com/schappim/goimage).
