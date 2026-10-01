package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSixtyDBResponses(t *testing.T) {
	audio := []byte{0xff, 0xfb, 0x90, 0x44}
	b64 := base64.StdEncoding.EncodeToString(audio)
	inner := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"result":{"audioContent":%q}}`, b64)))
	for _, tc := range []struct {
		name, contentType, body string
		status                  int
		permanent               bool
		wantError               string
		want                    []byte
	}{
		{name: "ID3 binary", contentType: "audio/mpeg", body: string(append([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 0}, audio...)), want: append([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 0}, audio...)},
		{name: "truncated ID3", contentType: "audio/mpeg", body: "ID3", permanent: true, wantError: "expected MP3"},
		{name: "binary", contentType: "audio/mpeg", body: string(audio), want: audio},
		{name: "octet stream", contentType: "application/octet-stream", body: string(audio), want: audio},
		{name: "JSON", body: fmt.Sprintf(`{"success":true,"audio_base64":%q,"encoding":"mp3"}`, b64), want: audio},
		{name: "nested JSON", body: fmt.Sprintf(`{"success":true,"backendResponse":{"success":true,"audio_base64":%q}}`, b64), want: audio},
		{name: "NDJSON", body: fmt.Sprintf("{\"type\":\"meta\"}\n{\"result\":{\"audioContent\":%q}}\n{\"result\":{\"audioContent\":%q}}", b64, b64), want: append(append([]byte{}, audio...), audio...)},
		{name: "double encoded", body: fmt.Sprintf(`{"result":{"audioContent":%q}}`, inner), want: audio},
		{name: "no audio", body: `{"success":true}`, permanent: true, wantError: "expected MP3"},
		{name: "malformed", body: `{oops`, permanent: true, wantError: "invalid 60db JSON"},
		{name: "invalid base64", body: `{"audio_base64":"!"}`, permanent: true, wantError: "base64"},
		{name: "application error", body: `{"success":false,"message":"voice missing"}`, permanent: true, wantError: "voice missing"},
		{name: "late stream error", body: fmt.Sprintf("{\"audioContent\":%q}\n{\"type\":\"error\",\"message\":\"failed\"}", b64), permanent: true, wantError: "failed"},
		{name: "nested application error", body: `{"backendResponse":{"success":false,"message":"failed"}}`, permanent: true, wantError: "failed"},
		{name: "WAV", contentType: "audio/wav", body: "RIFFxxxxWAVE", permanent: true, wantError: "expected MP3"},
		{name: "PCM", body: fmt.Sprintf(`{"audio_base64":%q,"encoding":"LINEAR16"}`, b64), permanent: true, wantError: "MP3 is required"},
		{name: "raw PCM", contentType: "application/octet-stream", body: "\x00\x00\x00\x00", permanent: true, wantError: "expected MP3"},
		{name: "unauthorized", status: 401, body: "unauthorized", permanent: true, wantError: "401"},
		{name: "rate limit", status: 429, body: "slow down", wantError: "429"},
		{name: "server error", status: 503, body: "unavailable", wantError: "503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			defer swapURL(&sixtyDBAPIURL, srv.URL)()
			got, err := synthesizeSixtyDB("test-key", defaultSixtyDBModel, "workspace-voice", "hello", 1)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || isPermanent(err) != tc.permanent {
					t.Fatalf("error=%v permanent=%v", err, isPermanent(err))
				}
				return
			}
			if err != nil || !bytes.Equal(got, tc.want) {
				t.Fatalf("audio=%x error=%v", got, err)
			}
		})
	}
}

func TestRunSixtyDB(t *testing.T) {
	audio := []byte{0xff, 0xfb, 0x90, 0x44}
	var body map[string]any
	var auth string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("invalid request headers")
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	}))
	defer srv.Close()
	defer swapURL(&sixtyDBAPIURL, srv.URL)()
	for _, tc := range []struct {
		name         string
		args         []string
		config       string
		token, model string
		speed        float64
	}{
		{name: "env defaults", args: []string{"--no-config", "-p", "60db", "-v", "workspace-voice"}, token: "env-key", model: defaultSixtyDBModel, speed: 1},
		{name: "flag overrides", args: []string{"--no-config", "-p", "60db", "-v", "workspace-voice", "--token", "flag-key", "-m", "another-model", "-x", "2"}, token: "flag-key", model: "another-model", speed: 2},
		{name: "config", config: `{"provider":"60DB","providers":{"60db":{"voice":"workspace-voice","model":"configured-model","speed":0.5}}}`, token: "env-key", model: "configured-model", speed: 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{}, tc.args...)
			if tc.config != "" {
				args = append(args, "--config", writeConfigFile(t, tc.config))
			}
			out := filepath.Join(t.TempDir(), "speech.mp3")
			args = append(args, "-o", out, "hello")
			code, stderr := runCLI(t, args, "", envMap(map[string]string{"SIXTYDB_API_KEY": "env-key"}))
			if code != 0 {
				t.Fatalf("exit=%d: %s", code, stderr)
			}
			got, err := os.ReadFile(out)
			if err != nil || !bytes.Equal(got, audio) {
				t.Fatalf("saved audio=%x error=%v", got, err)
			}
			if auth != "Bearer "+tc.token || body["text"] != "hello" || body["voice_id"] != "workspace-voice" || body["model_id"] != tc.model || body["speed"] != tc.speed || body["output_format"] != "mp3" || len(body) != 5 {
				t.Fatalf("request=%v auth=%q", body, auth)
			}
		})
	}
	before := calls
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-p", "60db"}, "workspace voice_id"},
		{[]string{"-p", "60db", "-v", " "}, "workspace voice_id"},
		{[]string{"-p", "60db", "-v", "id", "-x", "NaN"}, "0.5 and 2.0"},
		{[]string{"-p", "60db", "-v", "id", "-x", "0.49"}, "0.5 and 2.0"},
		{[]string{"-p", "60db", "-v", "id", "-x", "2.01"}, "0.5 and 2.0"},
		{[]string{"-p", "60db", "-v", "id", "--all"}, "only supported for OpenAI"},
		{[]string{"-p", "60db", "--sfx"}, "only supported by the elevenlabs"},
	} {
		args := append([]string{"--no-config"}, tc.args...)
		args = append(args, "hello")
		code, stderr := runCLI(t, args, "", envMap(map[string]string{"SIXTYDB_API_KEY": "env-key"}))
		if code != 1 || !strings.Contains(stderr, tc.want) {
			t.Errorf("args=%v exit=%d: %s", args, code, stderr)
		}
	}
	if calls != before {
		t.Fatal("invalid arguments reached API")
	}
	code, stderr := runCLI(t, []string{"--no-config", "-p", "60db", "-v", "id", "hello"}, "", emptyEnv)
	if code != 1 || !strings.Contains(stderr, "SIXTYDB_API_KEY") {
		t.Fatalf("missing key: exit=%d %s", code, stderr)
	}
}

func TestSixtyDBRetriesAndChunks(t *testing.T) {
	old := initialBackoff
	initialBackoff = 0
	defer func() { initialBackoff = old }()
	calls := 0
	audio := []byte{0xff, 0xfb, 0x90, 0x44}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	}))
	defer srv.Close()
	defer swapURL(&sixtyDBAPIURL, srv.URL)()
	got, err := synthesizeChunked(strings.Repeat("a", maxChunkSize+1), func(text string) ([]byte, error) { return synthesizeSixtyDB("key", defaultSixtyDBModel, "id", text, 1) })
	if err != nil || calls != 3 || !bytes.Equal(got, append(append([]byte{}, audio...), audio...)) {
		t.Fatalf("calls=%d audio=%x error=%v", calls, got, err)
	}
}
