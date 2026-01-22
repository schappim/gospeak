# gospeak

A self-contained command-line tool for text-to-speech using OpenAI's TTS API. Written in Go with no external dependencies like ffmpeg - just a single binary.

## Features

- Text-to-speech using OpenAI's TTS API
- **No ffmpeg required** - uses native Go audio libraries
- Six voice options: alloy, echo, fable, onyx, nova, shimmer
- Standard and HD quality models
- Adjustable speech speed (0.25x to 4.0x)
- Read from arguments or stdin (perfect for piping)
- Save to MP3 or play directly
- Cross-platform audio playback

## Requirements

- macOS, Linux, or Windows
- OpenAI API key

## Installation

### Homebrew (Recommended)

```bash
brew tap schappim/gospeak
brew install gospeak
```

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

### First Run

Set your OpenAI API key:

```bash
export OPENAI_API_KEY="your-api-key-here"
```

Or pass it directly with the `--token` flag.

## Usage

### Basic Usage

```bash
# Speak text directly
gospeak "Hello, world!"

# Pipe text from another command
echo "Hello from the command line" | gospeak

# Use with other tools
cat article.txt | gospeak
```

### Choose a Voice

Available voices: `alloy` (default), `echo`, `fable`, `onyx`, `nova`, `shimmer`

```bash
gospeak -v nova "Hello with the nova voice"
gospeak -v echo "This is the echo voice"
```

### Hear All Voices

Demo all voices with the same text (announces each voice first):

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

Speed ranges from 0.25 (slow) to 4.0 (fast):

```bash
gospeak -x 0.5 "Speaking slowly"
gospeak -x 1.5 "Speaking faster"
gospeak -x 2.0 "Speaking even faster"
```

### Use HD Model

```bash
# Default is tts-1-hd, but you can explicitly set it
gospeak -m tts-1-hd "High definition audio quality"

# Or use the faster standard model
gospeak -m tts-1 "Standard quality, lower latency"
```

## Options

| Option | Short | Description | Default |
|--------|-------|-------------|---------|
| `--voice` | `-v` | Voice to use | `alloy` |
| `--model` | `-m` | Model to use (`tts-1`, `tts-1-hd`) | `tts-1-hd` |
| `--output` | `-o` | Save audio to file | - |
| `--speed` | `-x` | Speech speed (0.25-4.0) | `1.0` |
| `--speak` | `-s` | Play audio even when saving to file | `false` |
| `--token` | - | OpenAI API key | `$OPENAI_API_KEY` |
| `--all` | - | Speak with all voices | `false` |
| `--help` | `-h` | Show help message | - |

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
```

## Error Handling

When an error occurs, the tool outputs a message to stderr:

```
Error: OPENAI_API_KEY environment variable not set and --token not provided
Error: Invalid voice 'invalid'. Valid voices: alloy, echo, fable, onyx, nova, shimmer
Error: Speed must be between 0.25 and 4.0
```

## Help

```bash
gospeak --help
```

## License

MIT License

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
