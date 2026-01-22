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
	defaultVoice = "alloy"
	defaultModel = "tts-1-hd"
	defaultSpeed = 1.0
	apiURL       = "https://api.openai.com/v1/audio/speech"
)

var validVoices = []string{"alloy", "echo", "fable", "onyx", "nova", "shimmer"}

type TTSRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed"`
}

func main() {
	var (
		voice   string
		model   string
		output  string
		speed   float64
		speak   bool
		token   string
		help    bool
		allFlag bool
	)

	flag.StringVar(&voice, "voice", defaultVoice, "Voice to use (alloy, echo, fable, onyx, nova, shimmer)")
	flag.StringVar(&voice, "v", defaultVoice, "Voice to use (shorthand)")
	flag.StringVar(&model, "model", defaultModel, "Model to use (tts-1 or tts-1-hd)")
	flag.StringVar(&model, "m", defaultModel, "Model to use (shorthand)")
	flag.StringVar(&output, "output", "", "Save audio to this file")
	flag.StringVar(&output, "o", "", "Save audio to this file (shorthand)")
	flag.Float64Var(&speed, "speed", defaultSpeed, "Speed of the voice (0.25 to 4.0)")
	flag.Float64Var(&speed, "x", defaultSpeed, "Speed of the voice (shorthand)")
	flag.BoolVar(&speak, "speak", false, "Speak the text even when saving to a file")
	flag.BoolVar(&speak, "s", false, "Speak the text (shorthand)")
	flag.StringVar(&token, "token", "", "OpenAI API key")
	flag.BoolVar(&help, "help", false, "Show help")
	flag.BoolVar(&help, "h", false, "Show help (shorthand)")
	flag.BoolVar(&allFlag, "all", false, "Use all voices")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "gospeak - Text-to-speech using OpenAI's TTS API\n\n")
		fmt.Fprintf(os.Stderr, "Usage: gospeak [options] [text]\n")
		fmt.Fprintf(os.Stderr, "       echo 'text' | gospeak [options]\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		fmt.Fprintf(os.Stderr, "  -v, --voice    Voice to use: alloy, echo, fable, onyx, nova, shimmer (default: alloy)\n")
		fmt.Fprintf(os.Stderr, "  -m, --model    Model to use: tts-1, tts-1-hd (default: tts-1)\n")
		fmt.Fprintf(os.Stderr, "  -o, --output   Save audio to this file\n")
		fmt.Fprintf(os.Stderr, "  -x, --speed    Speed of the voice, 0.25 to 4.0 (default: 1.0)\n")
		fmt.Fprintf(os.Stderr, "  -s, --speak    Speak the text even when saving to a file\n")
		fmt.Fprintf(os.Stderr, "      --token    OpenAI API key (or set OPENAI_API_KEY env var)\n")
		fmt.Fprintf(os.Stderr, "      --all      Speak with all voices (announces each voice first)\n")
		fmt.Fprintf(os.Stderr, "  -h, --help     Show this help message\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  gospeak \"Hello, world!\"\n")
		fmt.Fprintf(os.Stderr, "  echo \"Hello\" | gospeak -v nova\n")
		fmt.Fprintf(os.Stderr, "  gospeak -o output.mp3 \"Save this to a file\"\n")
	}

	flag.Parse()

	if help {
		flag.Usage()
		os.Exit(0)
	}

	// Get API key
	apiKey := token
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "Error: OPENAI_API_KEY environment variable not set and --token not provided")
		os.Exit(1)
	}

	// Validate speed
	if speed < 0.25 || speed > 4.0 {
		fmt.Fprintln(os.Stderr, "Error: Speed must be between 0.25 and 4.0")
		os.Exit(1)
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

	// Handle --all flag
	if allFlag {
		for _, v := range validVoices {
			fmt.Fprintf(os.Stderr, "Speaking with voice: %s\n", v)
			// First announce the voice
			audioData, err := synthesize(apiKey, model, v, v, speed)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error synthesizing voice announcement: %v\n", err)
				continue
			}
			if err := playAudio(audioData); err != nil {
				fmt.Fprintf(os.Stderr, "Error playing audio: %v\n", err)
				continue
			}
			time.Sleep(500 * time.Millisecond)

			// Then speak the actual text
			audioData, err = synthesize(apiKey, model, v, text, speed)
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

	// Validate voice
	if !isValidVoice(voice) {
		fmt.Fprintf(os.Stderr, "Error: Invalid voice '%s'. Valid voices: %s\n", voice, strings.Join(validVoices, ", "))
		os.Exit(1)
	}

	// Synthesize speech
	audioData, err := synthesize(apiKey, model, voice, text, speed)
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

func isValidVoice(voice string) bool {
	for _, v := range validVoices {
		if v == voice {
			return true
		}
	}
	return false
}

func synthesize(apiKey, model, voice, text string, speed float64) ([]byte, error) {
	reqBody := TTSRequest{
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

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
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
