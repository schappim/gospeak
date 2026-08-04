# gospeak

A self-contained command-line tool for text-to-speech using OpenAI, ElevenLabs, or Deepgram TTS APIs — plus AI sound effect generation via ElevenLabs. Written in Go with no external dependencies like ffmpeg - just a single binary.

## Features

- **Multiple TTS providers**: OpenAI, ElevenLabs, and Deepgram
- **Sound effects** - generate audio from a text description with `--sfx` (ElevenLabs)
- **No ffmpeg required** - uses native Go audio libraries
- **Auto-chunking for long text** - splits on paragraph/sentence boundaries and joins the audio so multi-paragraph input never hits a per-request timeout
- **Automatic retries** - up to 3 attempts per chunk with exponential backoff
- Multiple voice options for each provider
- Standard and HD quality models
- Adjustable speech speed
- Read from arguments or stdin (perfect for piping)
- Save to MP3 or play directly
- Cross-platform audio playback

## Requirements

- macOS, Linux, or Windows
- API key for your chosen provider (OpenAI, ElevenLabs, or Deepgram)

## Installation

### Homebrew (Recommended)

```bash
brew tap schappim/gospeak
brew install gospeak
```

See the [homebrew-gospeak](https://github.com/schappim/homebrew-gospeak) tap for more details.

### Download Binary

Download the latest release from the [releases page](https://github.com/schappim/gospeak/releases).

### Build from Source

```bash
# Clone the repository
git clone https://github.com/schappim/gospeak.git
cd gospeak

# Build
go build -o gospeak .

# Optional: Install to PATH
sudo cp gospeak /usr/local/bin/
```

### Configuration

Set your API key(s) as environment variables:

```bash
# For OpenAI (default provider)
export OPENAI_API_KEY="your-openai-api-key"

# For ElevenLabs
export ELEVENLABS_API_KEY="your-elevenlabs-api-key"

# For Deepgram
export DEEPGRAM_API_KEY="your-deepgram-api-key"
```

Or pass the key directly with the `--token` flag.

## Usage

### Basic Usage (OpenAI)

```bash
# Speak text directly (uses OpenAI by default)
gospeak "Hello, world!"

# Pipe text from another command
echo "Hello from the command line" | gospeak

# Use with other tools
cat article.txt | gospeak
```

### Using ElevenLabs

```bash
# Switch to ElevenLabs provider
gospeak -p elevenlabs "Hello from ElevenLabs"

# Use a specific ElevenLabs voice
gospeak -p elevenlabs -v josh "Hello with Josh's voice"

# Use a custom voice_id directly
gospeak -p elevenlabs -v "your-custom-voice-id" "Hello"
```

### Choose a Voice

**OpenAI voices:** `alloy` (default), `echo`, `fable`, `onyx`, `nova`, `shimmer`

```bash
gospeak -v nova "Hello with the nova voice"
gospeak -v echo "This is the echo voice"
```

**ElevenLabs voices:** `rachel` (default), `domi`, `bella`, `antoni`, `elli`, `josh`, `arnold`, `adam`, `sam`, `george`, `charlie`, `emily`, `lily`, `michael`

```bash
gospeak -p elevenlabs -v rachel "Hello with Rachel's voice"
gospeak -p elevenlabs -v josh "Hello with Josh's voice"
```

You can also use any ElevenLabs voice_id directly:

```bash
gospeak -p elevenlabs -v "21m00Tcm4TlvDq8ikWAM" "Using voice ID directly"
```

### Using Deepgram

```bash
# Switch to Deepgram provider
gospeak -p deepgram "Hello from Deepgram"

# Use a specific Deepgram voice
gospeak -p deepgram -v asteria "Hello with Asteria"
gospeak -p deepgram -v luna "Hello with Luna"

# Use Aura 2 voices
gospeak -p deepgram -v thalia "Hello with Thalia (Aura 2)"
gospeak -p deepgram -v apollo "Hello with Apollo (Aura 2)"

# Use a full model name directly
gospeak -p deepgram -v "aura-asteria-en" "Using model name directly"
```

**Deepgram voices:** `asteria` (default), `luna`, `stella`, `athena`, `hera`, `orion`, `arcas`, `perseus`, `angus`, `orpheus`, `helios`, `zeus`

**Deepgram Aura 2 voices:** `thalia`, `andromeda`, `helena`, `jason`, `apollo`, `ares`

### Sound Effects (ElevenLabs)

Generate a sound effect from a plain-English description instead of speaking
text. `--sfx` selects the ElevenLabs provider automatically, so you only need
`ELEVENLABS_API_KEY` set.

```bash
# Describe the sound you want
gospeak --sfx "distant thunder rolling across a valley"

# Pin the length (0.5-30 seconds)
gospeak --sfx -d 8 "a heavy wooden door creaking open"

# Make it loop seamlessly - handy for ambience beds
gospeak --sfx --loop -d 10 -o rain.mp3 "steady rain on a tin roof"

# Follow the prompt more literally (0.0-1.0, default 0.3)
gospeak --sfx --influence 0.9 "single glass marble dropped on tile"

# Prompts can be piped in too
echo "hydraulic press crushing metal" | gospeak --sfx -o press.mp3
```

Omit `--duration` (or pass `0`) and the model picks a length that suits the
prompt. Lower `--influence` values give the model more creative latitude; higher
values track the prompt more closely and produce less variation between runs.
`--loop` is a v2-model feature and is only sent when you ask for it.

Sound effect prompts are sent to the API whole — unlike narration, they are
never split into chunks — and are retried up to 3 times on transient failures.

Voice-related flags (`--voice`, `--speed`, `--stability`, `--similarity`) have
no meaning for sound effects; passing them prints a warning and they are
ignored. `--sfx` cannot be combined with `--all`.

### Hear All Voices (OpenAI)

Demo all OpenAI voices with the same text:

```bash
gospeak --all "The quick brown fox jumps over the lazy dog"
```

### Save to File

```bash
# Save to MP3
gospeak -o output.mp3 "Save this to a file"

# Save and play
gospeak -o output.mp3 -s "Save and speak at the same time"
```

### Adjust Speed

**OpenAI:** Speed ranges from 0.25 (slow) to 4.0 (fast)

```bash
gospeak -x 0.5 "Speaking slowly"
gospeak -x 2.0 "Speaking faster"
```

**ElevenLabs:** Speed ranges from 0.7 to 1.2

```bash
gospeak -p elevenlabs -x 0.8 "Speaking a bit slower"
gospeak -p elevenlabs -x 1.2 "Speaking faster"
```

### ElevenLabs Voice Settings

Fine-tune ElevenLabs voice output:

```bash
# Adjust stability (0.0-1.0, default: 0.5)
gospeak -p elevenlabs --stability 0.8 "More stable voice"

# Adjust similarity boost (0.0-1.0, default: 0.75)
gospeak -p elevenlabs --similarity 0.9 "Higher similarity to original voice"
```

### Use Different Models

**OpenAI:**

```bash
# HD model (default)
gospeak -m tts-1-hd "High definition audio"

# Standard model (faster, lower quality)
gospeak -m tts-1 "Standard quality"
```

**ElevenLabs:**

```bash
# Multilingual v2 (default)
gospeak -p elevenlabs -m eleven_multilingual_v2 "Multilingual model"

# Turbo v2.5 (faster)
gospeak -p elevenlabs -m eleven_turbo_v2_5 "Turbo model"
```

## Options

| Option | Short | Description | Default |
|--------|-------|-------------|---------|
| `--provider` | `-p` | TTS provider (`openai`, `elevenlabs`) | `openai` |
| `--voice` | `-v` | Voice to use | Provider-specific |
| `--model` | `-m` | Model to use | Provider-specific |
| `--output` | `-o` | Save audio to file | - |
| `--speed` | `-x` | Speech speed | `1.0` |
| `--speak` | `-s` | Play audio even when saving to file | `false` |
| `--token` | - | API key | From env var |
| `--all` | - | Speak with all voices (OpenAI only) | `false` |
| `--stability` | - | Voice stability (ElevenLabs only) | `0.5` |
| `--similarity` | - | Similarity boost (ElevenLabs only) | `0.75` |
| `--sfx` | - | Generate a sound effect instead of speech (ElevenLabs) | `false` |
| `--duration` | `-d` | Sound effect length in seconds, 0.5-30, or 0 for auto (`--sfx` only) | model decides |
| `--influence` | - | Prompt adherence, 0.0-1.0 (`--sfx` only) | `0.3` |
| `--loop` | - | Make the sound effect loop seamlessly (`--sfx` only) | `false` |
| `--help` | `-h` | Show help message | - |

## Provider Comparison

| Feature | OpenAI | ElevenLabs | Deepgram |
|---------|--------|------------|----------|
| Env var | `OPENAI_API_KEY` | `ELEVENLABS_API_KEY` | `DEEPGRAM_API_KEY` |
| Default voice | `alloy` | `rachel` | `asteria` |
| Default model | `tts-1-hd` | `eleven_multilingual_v2` | `aura-asteria-en` |
| Speed range | 0.25 - 4.0 | 0.7 - 1.2 | Not supported |
| Voice count | 6 built-in | 14 presets + custom | 18 presets + custom |
| Custom voices | No | Yes (via voice_id) | Yes (via model name) |
| Sound effects | No | Yes (`--sfx`) | No |

## Scripting Examples

### Read a file aloud

```bash
cat README.md | gospeak
```

### Speak command output

```bash
date | gospeak -v nova
```

### Generate audio files for multiple texts

```bash
for voice in alloy echo fable onyx nova shimmer; do
  gospeak -v $voice -o "${voice}_sample.mp3" "This is the ${voice} voice"
done
```

### Use with LLM output

```bash
# Pipe output from an LLM CLI tool
llm "Tell me a joke" | gospeak -v nova

# Use ElevenLabs for more natural speech
llm "Tell me a story" | gospeak -p elevenlabs -v josh

# Use Deepgram
llm "Tell me a fact" | gospeak -p deepgram -v asteria
```

### Compare providers

```bash
# Same text with all providers
gospeak "Hello world"
gospeak -p elevenlabs "Hello world"
gospeak -p deepgram "Hello world"
```

## Long Text and Reliability

Input longer than ~1500 characters is automatically split on paragraph,
sentence, or word boundaries, synthesized chunk-by-chunk, and joined back into
a single MP3 stream. Each chunk is retried up to 3 times with exponential
backoff (1s, 2s, 4s) before the command gives up, so transient timeouts and
upstream blips no longer require a manual rerun.

Retries apply to failures that might succeed on a second attempt — network
errors, `429` rate limits, and `5xx` responses. A request the provider has
already rejected outright (`400`, `401`, `403`, `404`, `422`) fails immediately
rather than re-sending the same doomed request twice more, so a stale API key or
a typo'd model name reports in well under a second instead of after 3 seconds of
backoff and three metered API calls.

Progress is reported on stderr:

```
Text is 4023 chars — splitting into 3 chunks
Synthesizing chunk 1/3 (1492 chars)...
Synthesizing chunk 2/3 (1487 chars)...
Synthesizing chunk 3/3 (1044 chars)...
```

## Error Handling

When an error occurs, the tool outputs a message to stderr:

```
Error: OPENAI_API_KEY environment variable not set and --token not provided
Error: ELEVENLABS_API_KEY environment variable not set and --token not provided
Error: DEEPGRAM_API_KEY environment variable not set and --token not provided
Error: Invalid provider 'invalid'. Use 'openai', 'elevenlabs', or 'deepgram'
Error: Speed must be between 0.25 and 4.0 for OpenAI
Error: Speed must be between 0.7 and 1.2 for ElevenLabs
Warning: Speed adjustment is not supported for Deepgram, ignoring
Error: --sfx is only supported by the elevenlabs provider (got 'openai')
Error: Duration must be between 0.5 and 30 seconds
Error: Influence must be between 0.0 and 1.0
Error: --all cannot be combined with --sfx
```

## Help

```bash
gospeak --help
```

## License

MIT License

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
