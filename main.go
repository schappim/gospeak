package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ebitengine/oto/v3"
	"github.com/hajimehoshi/go-mp3"
)

const (
	// OpenAI defaults
	defaultOpenAIVoice = "alloy"
	defaultOpenAIModel = "tts-1-hd"
	openAIAPIURL       = "https://api.openai.com/v1/audio/speech"

	// ElevenLabs defaults
	defaultElevenLabsVoice = "rachel"
	defaultElevenLabsModel = "eleven_multilingual_v2"
	elevenLabsAPIURL       = "https://api.elevenlabs.io/v1/text-to-speech"

	// Deepgram defaults
	defaultDeepgramVoice = "aura-asteria-en"
	deepgramAPIURL       = "https://api.deepgram.com/v1/speak"

	defaultSpeed    = 1.0
	defaultProvider = "openai"

	// Maximum characters per TTS request before splitting into chunks.
	// Sized to stay well under each provider's hard limit and to keep
	// per-request latency comfortably under the HTTP client timeout.
	maxChunkSize = 1500

	// Number of attempts per chunk before giving up.
	maxRetries = 3
)

// Initial backoff between retries (doubled each attempt). Declared as a var
// rather than a const so tests can swap it in for fast-running retry tests.
var initialBackoff = 1 * time.Second

var openAIVoices = []string{"alloy", "echo", "fable", "onyx", "nova", "shimmer"}

// ElevenLabs voice presets (name -> voice_id)
var elevenLabsVoices = map[string]string{
	"rachel":  "21m00Tcm4TlvDq8ikWAM",
	"domi":    "AZnzlk1XvdvUeBnXmlld",
	"bella":   "EXAVITQu4vr4xnSDxMaL",
	"antoni":  "ErXwobaYiN019PkySvjV",
	"elli":    "MF3mGyEYCl7XYWbV9V6O",
	"josh":    "TxGEqnHWrfWFTfGW9XjX",
	"arnold":  "VR6AewLTigWG4xSOukaG",
	"adam":    "pNInz6obpgDQGcFmaJgB",
	"sam":     "yoZ06aMxZJJ28mfd3POQ",
	"george":  "JBFqnCBsd6RMkjVDRZzb",
	"charlie": "IKne3meq5aSn9XLyUdCD",
	"emily":   "LcfcDJNUP1GQjkzn1xUU",
	"lily":    "pFZP5JQG7iQjIQuC4Bku",
	"michael": "flq6f7yk4E4fJM5XTYuZ",
}

// Deepgram voice presets (short name -> full model name)
var deepgramVoices = map[string]string{
	// Aura voices (English)
	"asteria": "aura-asteria-en",
	"luna":    "aura-luna-en",
	"stella":  "aura-stella-en",
	"athena":  "aura-athena-en",
	"hera":    "aura-hera-en",
	"orion":   "aura-orion-en",
	"arcas":   "aura-arcas-en",
	"perseus": "aura-perseus-en",
	"angus":   "aura-angus-en",
	"orpheus": "aura-orpheus-en",
	"helios":  "aura-helios-en",
	"zeus":    "aura-zeus-en",
	// Aura 2 voices (English)
	"thalia":   "aura-2-thalia-en",
	"andromeda": "aura-2-andromeda-en",
	"helena":   "aura-2-helena-en",
	"jason":    "aura-2-jason-en",
	"apollo":   "aura-2-apollo-en",
	"ares":     "aura-2-ares-en",
}

// Deepgram TTS request
type DeepgramTTSRequest struct {
	Text string `json:"text"`
}

// OpenAI TTS request
type OpenAITTSRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed"`
}

// ElevenLabs TTS request
type ElevenLabsTTSRequest struct {
	Text          string                    `json:"text"`
	ModelID       string                    `json:"model_id"`
	VoiceSettings *ElevenLabsVoiceSettings `json:"voice_settings,omitempty"`
}

type ElevenLabsVoiceSettings struct {
	Stability       float64 `json:"stability"`
	SimilarityBoost float64 `json:"similarity_boost"`
	Style           float64 `json:"style,omitempty"`
	Speed           float64 `json:"speed,omitempty"`
}

func main() {
	var (
		provider        string
		voice           string
		model           string
		output          string
		speed           float64
		speak           bool
		token           string
		help            bool
		allFlag         bool
		stability       float64
		similarityBoost float64
	)

	flag.StringVar(&provider, "provider", defaultProvider, "TTS provider (openai, elevenlabs, deepgram)")
	flag.StringVar(&provider, "p", defaultProvider, "TTS provider (shorthand)")
	flag.StringVar(&voice, "voice", "", "Voice to use (see --help for options)")
	flag.StringVar(&voice, "v", "", "Voice to use (shorthand)")
	flag.StringVar(&model, "model", "", "Model to use")
	flag.StringVar(&model, "m", "", "Model to use (shorthand)")
	flag.StringVar(&output, "output", "", "Save audio to this file")
	flag.StringVar(&output, "o", "", "Save audio to this file (shorthand)")
	flag.Float64Var(&speed, "speed", defaultSpeed, "Speed of the voice")
	flag.Float64Var(&speed, "x", defaultSpeed, "Speed of the voice (shorthand)")
	flag.BoolVar(&speak, "speak", false, "Speak the text even when saving to a file")
	flag.BoolVar(&speak, "s", false, "Speak the text (shorthand)")
	flag.StringVar(&token, "token", "", "API key for the provider")
	flag.BoolVar(&help, "help", false, "Show help")
	flag.BoolVar(&help, "h", false, "Show help (shorthand)")
	flag.BoolVar(&allFlag, "all", false, "Use all voices (OpenAI only)")
	flag.Float64Var(&stability, "stability", 0.5, "Voice stability (ElevenLabs only, 0.0-1.0)")
	flag.Float64Var(&similarityBoost, "similarity", 0.75, "Similarity boost (ElevenLabs only, 0.0-1.0)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "gospeak - Text-to-speech using OpenAI, ElevenLabs, or Deepgram TTS API\n\n")
		fmt.Fprintf(os.Stderr, "Usage: gospeak [options] [text]\n")
		fmt.Fprintf(os.Stderr, "       echo 'text' | gospeak [options]\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		fmt.Fprintf(os.Stderr, "  -p, --provider    TTS provider: openai, elevenlabs, deepgram (default: openai)\n")
		fmt.Fprintf(os.Stderr, "  -v, --voice       Voice to use (see below for options)\n")
		fmt.Fprintf(os.Stderr, "  -m, --model       Model to use\n")
		fmt.Fprintf(os.Stderr, "  -o, --output      Save audio to this file\n")
		fmt.Fprintf(os.Stderr, "  -x, --speed       Speed of the voice (default: 1.0)\n")
		fmt.Fprintf(os.Stderr, "  -s, --speak       Speak the text even when saving to a file\n")
		fmt.Fprintf(os.Stderr, "      --token       API key (or set env var)\n")
		fmt.Fprintf(os.Stderr, "      --all         Speak with all voices (OpenAI only)\n")
		fmt.Fprintf(os.Stderr, "      --stability   Voice stability, 0.0-1.0 (ElevenLabs only)\n")
		fmt.Fprintf(os.Stderr, "      --similarity  Similarity boost, 0.0-1.0 (ElevenLabs only)\n")
		fmt.Fprintf(os.Stderr, "  -h, --help        Show this help message\n\n")

		fmt.Fprintf(os.Stderr, "OpenAI:\n")
		fmt.Fprintf(os.Stderr, "  Env var: OPENAI_API_KEY\n")
		fmt.Fprintf(os.Stderr, "  Voices:  alloy, echo, fable, onyx, nova, shimmer\n")
		fmt.Fprintf(os.Stderr, "  Models:  tts-1, tts-1-hd (default: tts-1-hd)\n")
		fmt.Fprintf(os.Stderr, "  Speed:   0.25 to 4.0\n\n")

		fmt.Fprintf(os.Stderr, "ElevenLabs:\n")
		fmt.Fprintf(os.Stderr, "  Env var: ELEVENLABS_API_KEY\n")
		fmt.Fprintf(os.Stderr, "  Voices:  rachel, domi, bella, antoni, elli, josh, arnold,\n")
		fmt.Fprintf(os.Stderr, "           adam, sam, george, charlie, emily, lily, michael\n")
		fmt.Fprintf(os.Stderr, "           (or use a voice_id directly)\n")
		fmt.Fprintf(os.Stderr, "  Models:  eleven_multilingual_v2 (default), eleven_turbo_v2_5,\n")
		fmt.Fprintf(os.Stderr, "           eleven_turbo_v2, eleven_monolingual_v1\n")
		fmt.Fprintf(os.Stderr, "  Speed:   0.7 to 1.2\n\n")

		fmt.Fprintf(os.Stderr, "Deepgram:\n")
		fmt.Fprintf(os.Stderr, "  Env var: DEEPGRAM_API_KEY\n")
		fmt.Fprintf(os.Stderr, "  Voices:  asteria (default), luna, stella, athena, hera, orion,\n")
		fmt.Fprintf(os.Stderr, "           arcas, perseus, angus, orpheus, helios, zeus\n")
		fmt.Fprintf(os.Stderr, "           Aura 2: thalia, andromeda, helena, jason, apollo, ares\n")
		fmt.Fprintf(os.Stderr, "           (or use a model name directly like aura-asteria-en)\n")
		fmt.Fprintf(os.Stderr, "  Note:    Speed adjustment not supported\n\n")

		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  gospeak \"Hello, world!\"\n")
		fmt.Fprintf(os.Stderr, "  gospeak -p elevenlabs -v rachel \"Hello from ElevenLabs\"\n")
		fmt.Fprintf(os.Stderr, "  gospeak -p deepgram -v asteria \"Hello from Deepgram\"\n")
		fmt.Fprintf(os.Stderr, "  echo \"Hello\" | gospeak -v nova\n")
		fmt.Fprintf(os.Stderr, "  gospeak -o output.mp3 \"Save this to a file\"\n")
	}

	flag.Parse()

	if help {
		flag.Usage()
		os.Exit(0)
	}

	// Normalize provider
	provider = strings.ToLower(provider)
	if provider != "openai" && provider != "elevenlabs" && provider != "deepgram" {
		fmt.Fprintf(os.Stderr, "Error: Invalid provider '%s'. Use 'openai', 'elevenlabs', or 'deepgram'\n", provider)
		os.Exit(1)
	}

	// Set defaults based on provider
	if voice == "" {
		switch provider {
		case "openai":
			voice = defaultOpenAIVoice
		case "elevenlabs":
			voice = defaultElevenLabsVoice
		case "deepgram":
			voice = defaultDeepgramVoice
		}
	}
	if model == "" {
		switch provider {
		case "openai":
			model = defaultOpenAIModel
		case "elevenlabs":
			model = defaultElevenLabsModel
		case "deepgram":
			// Deepgram uses voice as model, no separate model
			model = ""
		}
	}

	// Get API key
	apiKey := token
	if apiKey == "" {
		switch provider {
		case "openai":
			apiKey = os.Getenv("OPENAI_API_KEY")
		case "elevenlabs":
			apiKey = os.Getenv("ELEVENLABS_API_KEY")
		case "deepgram":
			apiKey = os.Getenv("DEEPGRAM_API_KEY")
		}
	}
	if apiKey == "" {
		envVars := map[string]string{
			"openai":     "OPENAI_API_KEY",
			"elevenlabs": "ELEVENLABS_API_KEY",
			"deepgram":   "DEEPGRAM_API_KEY",
		}
		fmt.Fprintf(os.Stderr, "Error: %s environment variable not set and --token not provided\n", envVars[provider])
		os.Exit(1)
	}

	// Validate speed based on provider
	switch provider {
	case "openai":
		if speed < 0.25 || speed > 4.0 {
			fmt.Fprintln(os.Stderr, "Error: Speed must be between 0.25 and 4.0 for OpenAI")
			os.Exit(1)
		}
	case "elevenlabs":
		if speed < 0.7 || speed > 1.2 {
			fmt.Fprintln(os.Stderr, "Error: Speed must be between 0.7 and 1.2 for ElevenLabs")
			os.Exit(1)
		}
	case "deepgram":
		if speed != defaultSpeed {
			fmt.Fprintln(os.Stderr, "Warning: Speed adjustment is not supported for Deepgram, ignoring")
		}
	}

	// Get text input
	var text string
	if flag.NArg() > 0 {
		text = strings.Join(flag.Args(), " ")
	} else {
		// Read from stdin
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error reading stdin: %v\n", err)
				os.Exit(1)
			}
			text = strings.TrimSpace(string(data))
		}
	}

	if text == "" {
		fmt.Fprintln(os.Stderr, "Error: No text provided")
		flag.Usage()
		os.Exit(1)
	}

	// Handle --all flag (OpenAI only)
	if allFlag {
		if provider != "openai" {
			fmt.Fprintln(os.Stderr, "Error: --all flag is only supported for OpenAI provider")
			os.Exit(1)
		}
		for _, v := range openAIVoices {
			fmt.Fprintf(os.Stderr, "Speaking with voice: %s\n", v)
			audioData, err := synthesizeChunked(v, func(chunk string) ([]byte, error) {
				return synthesizeOpenAI(apiKey, model, v, chunk, speed)
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error synthesizing voice announcement: %v\n", err)
				continue
			}
			if err := playAudio(audioData); err != nil {
				fmt.Fprintf(os.Stderr, "Error playing audio: %v\n", err)
				continue
			}
			time.Sleep(500 * time.Millisecond)

			audioData, err = synthesizeChunked(text, func(chunk string) ([]byte, error) {
				return synthesizeOpenAI(apiKey, model, v, chunk, speed)
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error synthesizing: %v\n", err)
				continue
			}
			if err := playAudio(audioData); err != nil {
				fmt.Fprintf(os.Stderr, "Error playing audio: %v\n", err)
			}
			time.Sleep(1 * time.Second)
		}
		return
	}

	// Synthesize speech
	var audioData []byte
	var err error

	switch provider {
	case "openai":
		if !isValidOpenAIVoice(voice) {
			fmt.Fprintf(os.Stderr, "Error: Invalid OpenAI voice '%s'. Valid voices: %s\n", voice, strings.Join(openAIVoices, ", "))
			os.Exit(1)
		}
		audioData, err = synthesizeChunked(text, func(chunk string) ([]byte, error) {
			return synthesizeOpenAI(apiKey, model, voice, chunk, speed)
		})
	case "elevenlabs":
		voiceID := resolveElevenLabsVoice(voice)
		audioData, err = synthesizeChunked(text, func(chunk string) ([]byte, error) {
			return synthesizeElevenLabs(apiKey, model, voiceID, chunk, speed, stability, similarityBoost)
		})
	case "deepgram":
		voiceModel := resolveDeepgramVoice(voice)
		audioData, err = synthesizeChunked(text, func(chunk string) ([]byte, error) {
			return synthesizeDeepgram(apiKey, voiceModel, chunk)
		})
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error synthesizing speech: %v\n", err)
		os.Exit(1)
	}

	// Save to file if requested
	if output != "" {
		if err := os.WriteFile(output, audioData, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving file: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Saved to %s\n", output)
	}

	// Play audio if no output file or if --speak flag is set
	if output == "" || speak {
		if err := playAudio(audioData); err != nil {
			fmt.Fprintf(os.Stderr, "Error playing audio: %v\n", err)
			os.Exit(1)
		}
	}
}

func isValidOpenAIVoice(voice string) bool {
	for _, v := range openAIVoices {
		if v == voice {
			return true
		}
	}
	return false
}

func resolveElevenLabsVoice(voice string) string {
	// Check if it's a preset name
	if id, ok := elevenLabsVoices[strings.ToLower(voice)]; ok {
		return id
	}
	// Otherwise assume it's a voice_id
	return voice
}

func resolveDeepgramVoice(voice string) string {
	// Check if it's a preset name
	if model, ok := deepgramVoices[strings.ToLower(voice)]; ok {
		return model
	}
	// Otherwise assume it's a full model name (e.g., aura-asteria-en)
	return voice
}

// stripID3v2 removes a leading ID3v2 tag from an MP3 byte slice if present.
// Used to clean each chunk after the first so concatenated audio has at most
// one tag block at the very start.
func stripID3v2(data []byte) []byte {
	if len(data) < 10 || data[0] != 'I' || data[1] != 'D' || data[2] != '3' {
		return data
	}
	// The size field is a syncsafe integer: four bytes, each contributing 7 bits.
	size := int(data[6]&0x7F)<<21 | int(data[7]&0x7F)<<14 | int(data[8]&0x7F)<<7 | int(data[9]&0x7F)
	headerLen := 10 + size
	if headerLen > len(data) {
		return data
	}
	return data[headerLen:]
}

// chunkText splits text into chunks no larger than maxSize bytes, preferring
// to break on paragraph, sentence, line, then word boundaries. Falls back to a
// UTF-8-safe hard cut when no natural boundary is found in the latter half of
// the window.
func chunkText(text string, maxSize int) []string {
	text = strings.TrimSpace(text)
	if len(text) <= maxSize {
		if text == "" {
			return nil
		}
		return []string{text}
	}

	var chunks []string
	remaining := text
	minSplit := maxSize / 2

	for len(remaining) > maxSize {
		window := remaining[:maxSize]
		splitAt := -1

		// 1. Paragraph break.
		if idx := strings.LastIndex(window, "\n\n"); idx >= minSplit {
			splitAt = idx + 2
		}
		// 2. Sentence end followed by whitespace.
		if splitAt == -1 {
			for _, sep := range []string{". ", "! ", "? ", ".\n", "!\n", "?\n"} {
				if idx := strings.LastIndex(window, sep); idx >= minSplit && idx+len(sep) > splitAt {
					splitAt = idx + len(sep)
				}
			}
		}
		// 3. Single newline.
		if splitAt == -1 {
			if idx := strings.LastIndex(window, "\n"); idx >= minSplit {
				splitAt = idx + 1
			}
		}
		// 4. Word boundary.
		if splitAt == -1 {
			if idx := strings.LastIndex(window, " "); idx >= minSplit {
				splitAt = idx + 1
			}
		}
		// 5. Hard cut, rewound to a UTF-8 codepoint boundary.
		if splitAt == -1 {
			splitAt = maxSize
			for splitAt > 0 && remaining[splitAt]&0xC0 == 0x80 {
				splitAt--
			}
			if splitAt == 0 {
				splitAt = maxSize
			}
		}

		chunk := strings.TrimSpace(remaining[:splitAt])
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		remaining = strings.TrimSpace(remaining[splitAt:])
	}

	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

// synthesizeChunked chunks long text, calls synth for each piece with retries,
// and concatenates the resulting MP3 byte streams.
func synthesizeChunked(text string, synth func(string) ([]byte, error)) ([]byte, error) {
	chunks := chunkText(text, maxChunkSize)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("no text to synthesize")
	}
	if len(chunks) == 1 {
		return withRetry("synthesize", func() ([]byte, error) {
			return synth(chunks[0])
		})
	}

	fmt.Fprintf(os.Stderr, "Text is %d chars — splitting into %d chunks\n", len(text), len(chunks))
	var combined []byte
	for i, chunk := range chunks {
		fmt.Fprintf(os.Stderr, "Synthesizing chunk %d/%d (%d chars)...\n", i+1, len(chunks), len(chunk))
		label := fmt.Sprintf("chunk %d/%d", i+1, len(chunks))
		data, err := withRetry(label, func() ([]byte, error) {
			return synth(chunk)
		})
		if err != nil {
			return nil, err
		}
		if i > 0 {
			data = stripID3v2(data)
		}
		combined = append(combined, data...)
	}
	return combined, nil
}

// withRetry retries fn up to maxRetries times with exponential backoff.
// The label is used for log messages so the user can see which chunk is retrying.
func withRetry(label string, fn func() ([]byte, error)) ([]byte, error) {
	var lastErr error
	backoff := initialBackoff
	for attempt := 1; attempt <= maxRetries; attempt++ {
		data, err := fn()
		if err == nil {
			return data, nil
		}
		lastErr = err
		if attempt < maxRetries {
			fmt.Fprintf(os.Stderr, "%s: attempt %d/%d failed: %v (retrying in %v)\n",
				label, attempt, maxRetries, err, backoff)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("%s failed after %d attempts: %w", label, maxRetries, lastErr)
}

func synthesizeOpenAI(apiKey, model, voice, text string, speed float64) ([]byte, error) {
	reqBody := OpenAITTSRequest{
		Model:          model,
		Input:          text,
		Voice:          voice,
		ResponseFormat: "mp3",
		Speed:          speed,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", openAIAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}

func synthesizeElevenLabs(apiKey, model, voiceID, text string, speed, stability, similarityBoost float64) ([]byte, error) {
	reqBody := ElevenLabsTTSRequest{
		Text:    text,
		ModelID: model,
		VoiceSettings: &ElevenLabsVoiceSettings{
			Stability:       stability,
			SimilarityBoost: similarityBoost,
			Speed:           speed,
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/%s?output_format=mp3_44100_128", elevenLabsAPIURL, voiceID)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}

func synthesizeDeepgram(apiKey, voiceModel, text string) ([]byte, error) {
	reqBody := DeepgramTTSRequest{
		Text: text,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s?model=%s&encoding=mp3", deepgramAPIURL, voiceModel)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Token "+apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}

func playAudio(audioData []byte) error {
	// Decode MP3
	decoder, err := mp3.NewDecoder(bytes.NewReader(audioData))
	if err != nil {
		return fmt.Errorf("failed to decode MP3: %w", err)
	}

	// Create oto context
	op := &oto.NewContextOptions{
		SampleRate:   decoder.SampleRate(),
		ChannelCount: 2,
		Format:       oto.FormatSignedInt16LE,
	}

	otoCtx, readyChan, err := oto.NewContext(op)
	if err != nil {
		return fmt.Errorf("failed to create audio context: %w", err)
	}
	<-readyChan

	// Create player and play
	player := otoCtx.NewPlayer(decoder)
	defer player.Close()

	player.Play()

	// Wait for playback to finish
	for player.IsPlaying() {
		time.Sleep(10 * time.Millisecond)
	}

	// Allow audio buffer to fully drain
	time.Sleep(1 * time.Second)

	return nil
}
