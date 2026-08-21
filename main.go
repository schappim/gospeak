package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ebitengine/oto/v3"
	"github.com/hajimehoshi/go-mp3"
)

const (
	// OpenAI defaults
	defaultOpenAIVoice = "alloy"
	defaultOpenAIModel = "tts-1-hd"

	// ElevenLabs defaults
	defaultElevenLabsVoice = "rachel"
	defaultElevenLabsModel = "eleven_multilingual_v2"

	// ElevenLabs sound-effect defaults
	defaultSFXModel = "eleven_text_to_sound_v2"
	// 0.3 matches the API's own default for prompt_influence.
	defaultPromptInfluence = 0.3
	// Sound-effect duration bounds accepted by the API. A duration of 0 means
	// "let the model pick", which is the API's behaviour when the field is omitted.
	minSFXDuration = 0.5
	maxSFXDuration = 30.0

	// Deepgram defaults
	defaultDeepgramVoice = "aura-asteria-en"

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

// Provider endpoint URLs. Declared as vars so tests can repoint them at an
// httptest server without touching the calling code.
var (
	openAIAPIURL        = "https://api.openai.com/v1/audio/speech"
	elevenLabsAPIURL    = "https://api.elevenlabs.io/v1/text-to-speech"
	elevenLabsSFXAPIURL = "https://api.elevenlabs.io/v1/sound-generation"
	deepgramAPIURL      = "https://api.deepgram.com/v1/speak"
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
	"thalia":    "aura-2-thalia-en",
	"andromeda": "aura-2-andromeda-en",
	"helena":    "aura-2-helena-en",
	"jason":     "aura-2-jason-en",
	"apollo":    "aura-2-apollo-en",
	"ares":      "aura-2-ares-en",
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
	Text          string                   `json:"text"`
	ModelID       string                   `json:"model_id"`
	VoiceSettings *ElevenLabsVoiceSettings `json:"voice_settings,omitempty"`
}

// ElevenLabs sound-effect request. DurationSeconds is a pointer so that an
// unset duration is omitted entirely, which tells the API to infer the best
// length from the prompt.
type ElevenLabsSFXRequest struct {
	Text            string   `json:"text"`
	ModelID         string   `json:"model_id"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
	PromptInfluence float64  `json:"prompt_influence"`
	// Looping is a v2-model-only feature, so the field is sent only when the
	// user actually asked for it rather than pinned to false on every request.
	Loop bool `json:"loop,omitempty"`
}

type ElevenLabsVoiceSettings struct {
	Stability       float64 `json:"stability"`
	SimilarityBoost float64 `json:"similarity_boost"`
	Style           float64 `json:"style,omitempty"`
	Speed           float64 `json:"speed,omitempty"`
}

// playAudioFn is wired through a package var so tests can substitute a no-op
// implementation without driving the real audio device.
var playAudioFn = playAudio

// allVoiceSleep is the delay used between the announcement and the text and
// between consecutive voices in --all mode. Wired through a package var so
// tests can collapse the wait without sacrificing the demo cadence in prod.
var allVoiceSleep = time.Sleep

// Config file support. gospeak reads an optional JSON file of defaults so the
// settings that stay the same run after run — which provider to use, which
// voice to speak in — do not have to be retyped every time. Command-line flags
// always beat the file, and API keys deliberately have no place in it: those
// belong in environment variables rather than a plaintext file in the home
// directory.
const configFileName = ".gospeak.json"

// userHomeDir is wired through a package var so tests can point the default
// config location at a temporary directory instead of the real home directory.
var userHomeDir = os.UserHomeDir

// providerConfig holds defaults that apply to a single provider. Voices are
// provider-specific, so this is what lets one file name a preferred OpenAI
// voice and a preferred Deepgram voice at the same time.
type providerConfig struct {
	Voice string   `json:"voice,omitempty"`
	Model string   `json:"model,omitempty"`
	Speed *float64 `json:"speed,omitempty"`
}

// config mirrors the on-disk file. Speed is a pointer so that an omitted speed
// stays distinguishable from a deliberate 0, which is out of range for every
// provider and should be reported rather than quietly replaced by the default.
type config struct {
	Provider  string                    `json:"provider,omitempty"`
	Voice     string                    `json:"voice,omitempty"`
	Model     string                    `json:"model,omitempty"`
	Speed     *float64                  `json:"speed,omitempty"`
	Providers map[string]providerConfig `json:"providers,omitempty"`

	// path records where these settings were read from so an invalid value can
	// point at the file that needs editing. Unexported, so encoding/json
	// ignores it and it never becomes part of the file format.
	path string
}

// forProvider returns the overrides for one provider, or a zero value when the
// file says nothing about it.
func (c *config) forProvider(name string) providerConfig {
	return c.Providers[name]
}

// loadConfig reads the config file if there is one. A missing file at the
// default location is not an error — most people will never write one — but a
// file the user pointed at explicitly with --config or GOSPEAK_CONFIG is
// expected to exist, and a malformed file is always reported rather than
// silently ignored.
func loadConfig(flagPath string, getenv func(string) string) (*config, error) {
	path, explicit := flagPath, flagPath != ""
	if path == "" {
		if env := getenv("GOSPEAK_CONFIG"); env != "" {
			path, explicit = env, true
		}
	}
	if path == "" {
		home, err := userHomeDir()
		if err != nil {
			// Nowhere to look amounts to the same thing as nothing to read.
			return &config{}, nil
		}
		path = filepath.Join(home, configFileName)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return &config{}, nil
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	cfg := &config{path: path}
	dec := json.NewDecoder(bytes.NewReader(data))
	// Reject unknown keys rather than ignoring them: a setting that looks
	// applied but silently is not is the worst way for a config file to fail.
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if cfg.Provider != "" {
		cfg.Provider = strings.ToLower(cfg.Provider)
		if !isValidProvider(cfg.Provider) {
			return nil, fmt.Errorf("config %s: invalid provider %q (use openai, elevenlabs, or deepgram)", path, cfg.Provider)
		}
	}
	if len(cfg.Providers) > 0 {
		byProvider := make(map[string]providerConfig, len(cfg.Providers))
		for name, pc := range cfg.Providers {
			lower := strings.ToLower(name)
			if !isValidProvider(lower) {
				return nil, fmt.Errorf("config %s: unknown provider %q under \"providers\" (use openai, elevenlabs, or deepgram)", path, name)
			}
			byProvider[lower] = pc
		}
		cfg.Providers = byProvider
	}

	return cfg, nil
}

// configOrigin renders the " (from <path>)" suffix used when a rejected
// setting came out of the config file rather than off the command line, so the
// error points at the thing that needs fixing.
func configOrigin(cfg *config, fromConfig bool) string {
	if !fromConfig || cfg.path == "" {
		return ""
	}
	return fmt.Sprintf(" (from %s)", cfg.path)
}

func main() {
	var stdin io.Reader = strings.NewReader("")
	if stat, err := os.Stdin.Stat(); err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		stdin = os.Stdin
	}
	os.Exit(run(os.Args[1:], stdin, os.Stderr, os.Getenv))
}

// run is the testable entry point. It returns an exit code instead of calling
// os.Exit so tests can drive the CLI without killing the process. Stdin,
// stderr, and the env lookup are all injected for the same reason.
func run(args []string, stdin io.Reader, stderr io.Writer, getenv func(string) string) int {
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
		sfx             bool
		duration        float64
		promptInfluence float64
		loop            bool
		configFile      string
		noConfig        bool
	)

	fs := flag.NewFlagSet("gospeak", flag.ContinueOnError)
	fs.SetOutput(stderr)

	fs.StringVar(&provider, "provider", defaultProvider, "TTS provider (openai, elevenlabs, deepgram)")
	fs.StringVar(&provider, "p", defaultProvider, "TTS provider (shorthand)")
	fs.StringVar(&voice, "voice", "", "Voice to use (see --help for options)")
	fs.StringVar(&voice, "v", "", "Voice to use (shorthand)")
	fs.StringVar(&model, "model", "", "Model to use")
	fs.StringVar(&model, "m", "", "Model to use (shorthand)")
	fs.StringVar(&output, "output", "", "Save audio to this file")
	fs.StringVar(&output, "o", "", "Save audio to this file (shorthand)")
	fs.Float64Var(&speed, "speed", defaultSpeed, "Speed of the voice")
	fs.Float64Var(&speed, "x", defaultSpeed, "Speed of the voice (shorthand)")
	fs.BoolVar(&speak, "speak", false, "Speak the text even when saving to a file")
	fs.BoolVar(&speak, "s", false, "Speak the text (shorthand)")
	fs.StringVar(&token, "token", "", "API key for the provider")
	fs.BoolVar(&help, "help", false, "Show help")
	fs.BoolVar(&help, "h", false, "Show help (shorthand)")
	fs.BoolVar(&allFlag, "all", false, "Use all voices (OpenAI only)")
	fs.Float64Var(&stability, "stability", 0.5, "Voice stability (ElevenLabs only, 0.0-1.0)")
	fs.Float64Var(&similarityBoost, "similarity", 0.75, "Similarity boost (ElevenLabs only, 0.0-1.0)")
	fs.BoolVar(&sfx, "sfx", false, "Generate a sound effect from the prompt (ElevenLabs only)")
	fs.Float64Var(&duration, "duration", 0, "Sound effect length in seconds, 0.5-30 (0 = let the model decide)")
	fs.Float64Var(&duration, "d", 0, "Sound effect length in seconds (shorthand)")
	fs.Float64Var(&promptInfluence, "influence", defaultPromptInfluence, "How closely the sound effect follows the prompt, 0.0-1.0")
	fs.BoolVar(&loop, "loop", false, "Generate a seamlessly looping sound effect")
	fs.StringVar(&configFile, "config", "", "Path to a config file (default: ~/.gospeak.json)")
	fs.BoolVar(&noConfig, "no-config", false, "Ignore the config file")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "gospeak - Text-to-speech using OpenAI, ElevenLabs, or Deepgram TTS API\n\n")
		fmt.Fprintf(stderr, "Usage: gospeak [options] [text]\n")
		fmt.Fprintf(stderr, "       echo 'text' | gospeak [options]\n\n")
		fmt.Fprintf(stderr, "Options:\n")
		fmt.Fprintf(stderr, "  -p, --provider    TTS provider: openai, elevenlabs, deepgram (default: openai)\n")
		fmt.Fprintf(stderr, "  -v, --voice       Voice to use (see below for options)\n")
		fmt.Fprintf(stderr, "  -m, --model       Model to use\n")
		fmt.Fprintf(stderr, "  -o, --output      Save audio to this file\n")
		fmt.Fprintf(stderr, "  -x, --speed       Speed of the voice (default: 1.0)\n")
		fmt.Fprintf(stderr, "  -s, --speak       Speak the text even when saving to a file\n")
		fmt.Fprintf(stderr, "      --token       API key (or set env var)\n")
		fmt.Fprintf(stderr, "      --all         Speak with all voices (OpenAI only)\n")
		fmt.Fprintf(stderr, "      --stability   Voice stability, 0.0-1.0 (ElevenLabs only)\n")
		fmt.Fprintf(stderr, "      --similarity  Similarity boost, 0.0-1.0 (ElevenLabs only)\n")
		fmt.Fprintf(stderr, "      --sfx         Generate a sound effect instead of speech (ElevenLabs)\n")
		fmt.Fprintf(stderr, "  -d, --duration    Sound effect length in seconds, 0.5-30 (--sfx only,\n")
		fmt.Fprintf(stderr, "                    omit or pass 0 to let the model decide)\n")
		fmt.Fprintf(stderr, "      --influence   Prompt adherence, 0.0-1.0 (--sfx only)\n")
		fmt.Fprintf(stderr, "      --loop        Make the sound effect loop seamlessly (--sfx only)\n")
		fmt.Fprintf(stderr, "      --config      Path to a config file (default: ~/.gospeak.json)\n")
		fmt.Fprintf(stderr, "      --no-config   Ignore the config file\n")
		fmt.Fprintf(stderr, "  -h, --help        Show this help message\n\n")

		fmt.Fprintf(stderr, "OpenAI:\n")
		fmt.Fprintf(stderr, "  Env var: OPENAI_API_KEY\n")
		fmt.Fprintf(stderr, "  Voices:  alloy, echo, fable, onyx, nova, shimmer\n")
		fmt.Fprintf(stderr, "  Models:  tts-1, tts-1-hd (default: tts-1-hd)\n")
		fmt.Fprintf(stderr, "  Speed:   0.25 to 4.0\n\n")

		fmt.Fprintf(stderr, "ElevenLabs:\n")
		fmt.Fprintf(stderr, "  Env var: ELEVENLABS_API_KEY\n")
		fmt.Fprintf(stderr, "  Voices:  rachel, domi, bella, antoni, elli, josh, arnold,\n")
		fmt.Fprintf(stderr, "           adam, sam, george, charlie, emily, lily, michael\n")
		fmt.Fprintf(stderr, "           (or use a voice_id directly)\n")
		fmt.Fprintf(stderr, "  Models:  eleven_multilingual_v2 (default), eleven_turbo_v2_5,\n")
		fmt.Fprintf(stderr, "           eleven_turbo_v2, eleven_monolingual_v1\n")
		fmt.Fprintf(stderr, "  Speed:   0.7 to 1.2\n\n")

		fmt.Fprintf(stderr, "Sound effects (--sfx, ElevenLabs):\n")
		fmt.Fprintf(stderr, "  Env var:   ELEVENLABS_API_KEY\n")
		fmt.Fprintf(stderr, "  Prompt:    describe the sound, e.g. \"distant thunder rolling\"\n")
		fmt.Fprintf(stderr, "  Models:    eleven_text_to_sound_v2 (default)\n")
		fmt.Fprintf(stderr, "  Duration:  0.5 to 30 seconds (omit or pass 0 to let the model decide)\n")
		fmt.Fprintf(stderr, "  Influence: 0.0 to 1.0 (default: 0.3)\n")
		fmt.Fprintf(stderr, "  Note:      voice, speed, stability and similarity do not apply\n\n")

		fmt.Fprintf(stderr, "Deepgram:\n")
		fmt.Fprintf(stderr, "  Env var: DEEPGRAM_API_KEY\n")
		fmt.Fprintf(stderr, "  Voices:  asteria (default), luna, stella, athena, hera, orion,\n")
		fmt.Fprintf(stderr, "           arcas, perseus, angus, orpheus, helios, zeus\n")
		fmt.Fprintf(stderr, "           Aura 2: thalia, andromeda, helena, jason, apollo, ares\n")
		fmt.Fprintf(stderr, "           (or use a model name directly like aura-asteria-en)\n")
		fmt.Fprintf(stderr, "  Note:    Speed adjustment not supported\n\n")

		fmt.Fprintf(stderr, "Config file (~/.gospeak.json):\n")
		fmt.Fprintf(stderr, "  Optional JSON file holding the defaults every run starts from. Any flag\n")
		fmt.Fprintf(stderr, "  you type overrides it. API keys stay in environment variables.\n")
		fmt.Fprintf(stderr, "    {\n")
		fmt.Fprintf(stderr, "      \"provider\": \"elevenlabs\",\n")
		fmt.Fprintf(stderr, "      \"voice\": \"josh\",\n")
		fmt.Fprintf(stderr, "      \"model\": \"eleven_turbo_v2_5\",\n")
		fmt.Fprintf(stderr, "      \"speed\": 1.0,\n")
		fmt.Fprintf(stderr, "      \"providers\": {\n")
		fmt.Fprintf(stderr, "        \"openai\":   { \"voice\": \"nova\" },\n")
		fmt.Fprintf(stderr, "        \"deepgram\": { \"voice\": \"thalia\" }\n")
		fmt.Fprintf(stderr, "      }\n")
		fmt.Fprintf(stderr, "    }\n")
		fmt.Fprintf(stderr, "  A \"providers\" entry beats the file-wide setting for that provider.\n")
		fmt.Fprintf(stderr, "  Point elsewhere with --config <path> or GOSPEAK_CONFIG.\n\n")

		fmt.Fprintf(stderr, "Examples:\n")
		fmt.Fprintf(stderr, "  gospeak \"Hello, world!\"\n")
		fmt.Fprintf(stderr, "  gospeak -p elevenlabs -v rachel \"Hello from ElevenLabs\"\n")
		fmt.Fprintf(stderr, "  gospeak -p deepgram -v asteria \"Hello from Deepgram\"\n")
		fmt.Fprintf(stderr, "  echo \"Hello\" | gospeak -v nova\n")
		fmt.Fprintf(stderr, "  gospeak -o output.mp3 \"Save this to a file\"\n")
		fmt.Fprintf(stderr, "  gospeak --sfx \"glass shattering on concrete\"\n")
		fmt.Fprintf(stderr, "  gospeak --sfx -d 8 --loop -o rain.mp3 \"steady rain on a tin roof\"\n")
	}

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if help {
		fs.Usage()
		return 0
	}

	// Which flags the user actually typed. Sound-effect mode shares a flag set
	// with speech mode, so this is how we tell "user asked for a voice" apart
	// from "voice is sitting at its default".
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	wasSet := func(names ...string) bool {
		for _, n := range names {
			if setFlags[n] {
				return true
			}
		}
		return false
	}

	if noConfig && wasSet("config") {
		fmt.Fprintln(stderr, "Warning: --config is ignored when --no-config is set")
	}
	cfg := &config{}
	if !noConfig {
		loaded, err := loadConfig(configFile, getenv)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		cfg = loaded
	}

	// A provider named in the config file is a default like any other: -p still
	// wins, and --sfx still forces ElevenLabs below.
	if !wasSet("provider", "p") && cfg.Provider != "" {
		provider = cfg.Provider
	}
	provider = strings.ToLower(provider)

	if sfx {
		// Contradictory flags are worth reporting before anything that depends
		// on the environment, so the user hears about the real mistake rather
		// than a missing API key or an empty prompt.
		if allFlag {
			fmt.Fprintln(stderr, "Error: --all cannot be combined with --sfx")
			return 1
		}
		// Sound effects are an ElevenLabs capability, so --sfx selects that
		// provider. Only complain if the user explicitly asked for another one.
		if wasSet("provider", "p") && provider != "elevenlabs" {
			fmt.Fprintf(stderr, "Error: --sfx is only supported by the elevenlabs provider (got '%s')\n", provider)
			return 1
		}
		provider = "elevenlabs"

		for _, name := range []string{"v", "voice", "x", "speed", "stability", "similarity"} {
			if setFlags[name] {
				fmt.Fprintf(stderr, "Warning: %s does not apply to sound effects, ignoring\n", flagName(name))
			}
		}
	} else {
		for _, name := range []string{"d", "duration", "influence", "loop"} {
			if setFlags[name] {
				fmt.Fprintf(stderr, "Warning: %s only applies with --sfx, ignoring\n", flagName(name))
			}
		}
	}

	if !isValidProvider(provider) {
		fmt.Fprintf(stderr, "Error: Invalid provider '%s'. Use 'openai', 'elevenlabs', or 'deepgram'\n", provider)
		return 1
	}

	// Config-file defaults fill in whatever the command line left unset, with a
	// provider-specific entry beating the file-wide one. Sound effects skip all
	// of this: voice, model and speed describe speech, and --sfx already ignores
	// the flags that carry them.
	pc := cfg.forProvider(provider)
	voiceFromConfig := false
	speedFromProviderConfig := false
	if !sfx {
		if voice == "" {
			if pc.Voice != "" {
				voice, voiceFromConfig = pc.Voice, true
			} else if cfg.Voice != "" {
				voice, voiceFromConfig = cfg.Voice, true
			}
		}
		if model == "" {
			if pc.Model != "" {
				model = pc.Model
			} else if cfg.Model != "" {
				model = cfg.Model
			}
		}
		if !wasSet("speed", "x") {
			if pc.Speed != nil {
				speed, speedFromProviderConfig = *pc.Speed, true
			} else if cfg.Speed != nil {
				speed = *cfg.Speed
			}
		}
	}

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
		switch {
		case sfx:
			model = defaultSFXModel
		case provider == "openai":
			model = defaultOpenAIModel
		case provider == "elevenlabs":
			model = defaultElevenLabsModel
		case provider == "deepgram":
			model = ""
		}
	}

	apiKey := token
	if apiKey == "" {
		switch provider {
		case "openai":
			apiKey = getenv("OPENAI_API_KEY")
		case "elevenlabs":
			apiKey = getenv("ELEVENLABS_API_KEY")
		case "deepgram":
			apiKey = getenv("DEEPGRAM_API_KEY")
		}
	}
	if apiKey == "" {
		envVars := map[string]string{
			"openai":     "OPENAI_API_KEY",
			"elevenlabs": "ELEVENLABS_API_KEY",
			"deepgram":   "DEEPGRAM_API_KEY",
		}
		fmt.Fprintf(stderr, "Error: %s environment variable not set and --token not provided\n", envVars[provider])
		return 1
	}

	if sfx {
		// A duration of 0 means "unset" — the model picks a length from the prompt.
		// These bounds are written as "not inside the range" rather than "outside
		// it" so that NaN, for which every comparison is false, is rejected too.
		if duration != 0 && !(duration >= minSFXDuration && duration <= maxSFXDuration) {
			fmt.Fprintf(stderr, "Error: Duration must be between %g and %g seconds (or 0 to let the model decide)\n",
				minSFXDuration, maxSFXDuration)
			return 1
		}
		if !(promptInfluence >= 0 && promptInfluence <= 1) {
			fmt.Fprintln(stderr, "Error: Influence must be between 0.0 and 1.0")
			return 1
		}
	} else {
		switch provider {
		case "openai":
			if speed < 0.25 || speed > 4.0 {
				fmt.Fprintln(stderr, "Error: Speed must be between 0.25 and 4.0 for OpenAI")
				return 1
			}
		case "elevenlabs":
			if speed < 0.7 || speed > 1.2 {
				fmt.Fprintln(stderr, "Error: Speed must be between 0.7 and 1.2 for ElevenLabs")
				return 1
			}
		case "deepgram":
			// A speed aimed at Deepgram — typed on the command line, or set in
			// the file's own deepgram section — earns a warning. A file-wide
			// default that simply does not apply here does not: nagging about it
			// on every Deepgram run would be noise, not information.
			if speed != defaultSpeed && (wasSet("speed", "x") || speedFromProviderConfig) {
				fmt.Fprintln(stderr, "Warning: Speed adjustment is not supported for Deepgram, ignoring")
			}
		}
	}

	var text string
	if fs.NArg() > 0 {
		text = strings.TrimSpace(strings.Join(fs.Args(), " "))
	} else {
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading stdin: %v\n", err)
			return 1
		}
		text = strings.TrimSpace(string(data))
	}

	if text == "" {
		if sfx {
			fmt.Fprintln(stderr, "Error: No sound effect prompt provided")
		} else {
			fmt.Fprintln(stderr, "Error: No text provided")
		}
		fs.Usage()
		return 1
	}

	if allFlag {
		if provider != "openai" {
			fmt.Fprintln(stderr, "Error: --all flag is only supported for OpenAI provider")
			return 1
		}
		for _, v := range openAIVoices {
			fmt.Fprintf(stderr, "Speaking with voice: %s\n", v)
			audioData, err := synthesizeChunked(v, func(chunk string) ([]byte, error) {
				return synthesizeOpenAI(apiKey, model, v, chunk, speed)
			})
			if err != nil {
				fmt.Fprintf(stderr, "Error synthesizing voice announcement: %v\n", err)
				continue
			}
			if err := playAudioFn(audioData); err != nil {
				fmt.Fprintf(stderr, "Error playing audio: %v\n", err)
				continue
			}
			allVoiceSleep(500 * time.Millisecond)

			audioData, err = synthesizeChunked(text, func(chunk string) ([]byte, error) {
				return synthesizeOpenAI(apiKey, model, v, chunk, speed)
			})
			if err != nil {
				fmt.Fprintf(stderr, "Error synthesizing: %v\n", err)
				continue
			}
			if err := playAudioFn(audioData); err != nil {
				fmt.Fprintf(stderr, "Error playing audio: %v\n", err)
			}
			allVoiceSleep(1 * time.Second)
		}
		return 0
	}

	var audioData []byte
	var err error

	switch {
	case sfx:
		// A sound-effect prompt is a single instruction, not narration, so it
		// is sent whole rather than chunked and stitched back together.
		var durationSeconds *float64
		if duration != 0 {
			durationSeconds = &duration
		}
		audioData, err = withRetry("sound effect", func() ([]byte, error) {
			return synthesizeSoundEffect(apiKey, model, text, durationSeconds, promptInfluence, loop)
		})
	case provider == "openai":
		if !isValidOpenAIVoice(voice) {
			fmt.Fprintf(stderr, "Error: Invalid OpenAI voice '%s'%s. Valid voices: %s\n",
				voice, configOrigin(cfg, voiceFromConfig), strings.Join(openAIVoices, ", "))
			return 1
		}
		audioData, err = synthesizeChunked(text, func(chunk string) ([]byte, error) {
			return synthesizeOpenAI(apiKey, model, voice, chunk, speed)
		})
	case provider == "elevenlabs":
		voiceID := resolveElevenLabsVoice(voice)
		audioData, err = synthesizeChunked(text, func(chunk string) ([]byte, error) {
			return synthesizeElevenLabs(apiKey, model, voiceID, chunk, speed, stability, similarityBoost)
		})
	case provider == "deepgram":
		voiceModel := resolveDeepgramVoice(voice)
		audioData, err = synthesizeChunked(text, func(chunk string) ([]byte, error) {
			return synthesizeDeepgram(apiKey, voiceModel, chunk)
		})
	}

	if err != nil {
		if sfx {
			fmt.Fprintf(stderr, "Error generating sound effect: %v\n", err)
		} else {
			fmt.Fprintf(stderr, "Error synthesizing speech: %v\n", err)
		}
		return 1
	}

	if output != "" {
		if err := os.WriteFile(output, audioData, 0644); err != nil {
			fmt.Fprintf(stderr, "Error saving file: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "Saved to %s\n", output)
	}

	if output == "" || speak {
		if err := playAudioFn(audioData); err != nil {
			fmt.Fprintf(stderr, "Error playing audio: %v\n", err)
			return 1
		}
	}

	return 0
}

// flagName renders a flag the way the user would have typed it: one dash for
// the single-character shorthands, two for the long spellings.
func flagName(name string) string {
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

func isValidProvider(provider string) bool {
	return provider == "openai" || provider == "elevenlabs" || provider == "deepgram"
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

// permanentError marks a failure that cannot possibly succeed on a retry: a
// request we failed to build, or a 4xx rejection from the provider. Retrying
// these just burns wall-clock time and metered API calls on a guaranteed
// failure, so withRetry surfaces them immediately.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return &permanentError{err: err} }

func isPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// apiError builds the error for a non-200 response. Client-side rejections
// (4xx other than 429, which means "slow down and try again") are marked
// permanent so a typo'd model or a stale API key fails fast instead of being
// re-sent three times.
func apiError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	err := fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		return permanent(err)
	}
	return err
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
		if isPermanent(err) {
			return nil, fmt.Errorf("%s failed: %w", label, err)
		}
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
		return nil, permanent(fmt.Errorf("failed to marshal request: %w", err))
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
		return nil, apiError(resp)
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
		return nil, permanent(fmt.Errorf("failed to marshal request: %w", err))
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
		return nil, apiError(resp)
	}

	return io.ReadAll(resp.Body)
}

// synthesizeSoundEffect turns a natural-language prompt into a sound effect via
// the ElevenLabs sound-generation endpoint. A nil durationSeconds omits the
// field so the model infers a length from the prompt.
func synthesizeSoundEffect(apiKey, model, prompt string, durationSeconds *float64, promptInfluence float64, loop bool) ([]byte, error) {
	reqBody := ElevenLabsSFXRequest{
		Text:            prompt,
		ModelID:         model,
		DurationSeconds: durationSeconds,
		PromptInfluence: promptInfluence,
		Loop:            loop,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, permanent(fmt.Errorf("failed to marshal request: %w", err))
	}

	url := elevenLabsSFXAPIURL + "?output_format=mp3_44100_128"
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
		return nil, apiError(resp)
	}

	return io.ReadAll(resp.Body)
}

func synthesizeDeepgram(apiKey, voiceModel, text string) ([]byte, error) {
	reqBody := DeepgramTTSRequest{
		Text: text,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, permanent(fmt.Errorf("failed to marshal request: %w", err))
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
		return nil, apiError(resp)
	}

	return io.ReadAll(resp.Body)
}

func playAudio(audioData []byte) error {
	decoder, err := decodeMP3(audioData)
	if err != nil {
		return err
	}
	return playDecoded(decoder)
}

// decodeMP3 wraps mp3.NewDecoder so the decode step can be unit-tested without
// touching the system audio device.
func decodeMP3(audioData []byte) (*mp3.Decoder, error) {
	decoder, err := mp3.NewDecoder(bytes.NewReader(audioData))
	if err != nil {
		return nil, fmt.Errorf("failed to decode MP3: %w", err)
	}
	return decoder, nil
}

// newAudioContext is wired through a package var so tests can substitute a
// fake that simulates a host with no audio device.
var newAudioContext = oto.NewContext

// playDecoded drives the decoded PCM stream to the system audio device. Lives
// behind its own symbol so playAudio's pre-playback failure path stays testable
// even on machines without an audio device.
func playDecoded(decoder *mp3.Decoder) error {
	op := &oto.NewContextOptions{
		SampleRate:   decoder.SampleRate(),
		ChannelCount: 2,
		Format:       oto.FormatSignedInt16LE,
	}

	otoCtx, readyChan, err := newAudioContext(op)
	if err != nil {
		return fmt.Errorf("failed to create audio context: %w", err)
	}
	<-readyChan

	player := otoCtx.NewPlayer(decoder)
	defer player.Close()

	player.Play()

	for player.IsPlaying() {
		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(1 * time.Second)

	return nil
}
