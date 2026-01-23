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

	defaultSpeed    = 1.0
	defaultProvider = "openai"
)

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

	flag.StringVar(&provider, "provider", defaultProvider, "TTS provider (openai, elevenlabs)")
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
		fmt.Fprintf(os.Stderr, "gospeak - Text-to-speech using OpenAI or ElevenLabs TTS API\n\n")
		fmt.Fprintf(os.Stderr, "Usage: gospeak [options] [text]\n")
		fmt.Fprintf(os.Stderr, "       echo 'text' | gospeak [options]\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		fmt.Fprintf(os.Stderr, "  -p, --provider    TTS provider: openai, elevenlabs (default: openai)\n")
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

		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  gospeak \"Hello, world!\"\n")
		fmt.Fprintf(os.Stderr, "  gospeak -p elevenlabs -v rachel \"Hello from ElevenLabs\"\n")
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
	if provider != "openai" && provider != "elevenlabs" {
		fmt.Fprintf(os.Stderr, "Error: Invalid provider '%s'. Use 'openai' or 'elevenlabs'\n", provider)
		os.Exit(1)
	}

	// Set defaults based on provider
	if voice == "" {
		if provider == "openai" {
			voice = defaultOpenAIVoice
		} else {
			voice = defaultElevenLabsVoice
		}
	}
	if model == "" {
		if provider == "openai" {
			model = defaultOpenAIModel
		} else {
			model = defaultElevenLabsModel
		}
	}

	// Get API key
	apiKey := token
	if apiKey == "" {
		if provider == "openai" {
			apiKey = os.Getenv("OPENAI_API_KEY")
		} else {
			apiKey = os.Getenv("ELEVENLABS_API_KEY")
		}
	}
	if apiKey == "" {
		envVar := "OPENAI_API_KEY"
		if provider == "elevenlabs" {
			envVar = "ELEVENLABS_API_KEY"
		}
		fmt.Fprintf(os.Stderr, "Error: %s environment variable not set and --token not provided\n", envVar)
		os.Exit(1)
	}

	// Validate speed based on provider
	if provider == "openai" {
		if speed < 0.25 || speed > 4.0 {
			fmt.Fprintln(os.Stderr, "Error: Speed must be between 0.25 and 4.0 for OpenAI")
			os.Exit(1)
		}
	} else {
		if speed < 0.7 || speed > 1.2 {
			fmt.Fprintln(os.Stderr, "Error: Speed must be between 0.7 and 1.2 for ElevenLabs")
			os.Exit(1)
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
			audioData, err := synthesizeOpenAI(apiKey, model, v, v, speed)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error synthesizing voice announcement: %v\n", err)
				continue
			}
			if err := playAudio(audioData); err != nil {
				fmt.Fprintf(os.Stderr, "Error playing audio: %v\n", err)
				continue
			}
			time.Sleep(500 * time.Millisecond)

			audioData, err = synthesizeOpenAI(apiKey, model, v, text, speed)
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

	if provider == "openai" {
		if !isValidOpenAIVoice(voice) {
			fmt.Fprintf(os.Stderr, "Error: Invalid OpenAI voice '%s'. Valid voices: %s\n", voice, strings.Join(openAIVoices, ", "))
			os.Exit(1)
		}
		audioData, err = synthesizeOpenAI(apiKey, model, voice, text, speed)
	} else {
		voiceID := resolveElevenLabsVoice(voice)
		audioData, err = synthesizeElevenLabs(apiKey, model, voiceID, text, speed, stability, similarityBoost)
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

	client := &http.Client{Timeout: 60 * time.Second}
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

	client := &http.Client{Timeout: 60 * time.Second}
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

	return nil
}
