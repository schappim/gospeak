package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChunkText_ShortInputReturnsSingleChunk(t *testing.T) {
	chunks := chunkText("Hello, world!", 100)
	if len(chunks) != 1 || chunks[0] != "Hello, world!" {
		t.Fatalf("expected single unchanged chunk, got %#v", chunks)
	}
}

func TestChunkText_EmptyInputReturnsNothing(t *testing.T) {
	if got := chunkText("   ", 100); got != nil {
		t.Fatalf("expected nil for whitespace-only input, got %#v", got)
	}
}

func TestChunkText_PrefersParagraphBoundary(t *testing.T) {
	first := strings.Repeat("a", 80)
	second := strings.Repeat("b", 80)
	text := first + "\n\n" + second
	chunks := chunkText(text, 100)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %#v", len(chunks), chunks)
	}
	if chunks[0] != first || chunks[1] != second {
		t.Fatalf("paragraphs were not preserved cleanly: %#v", chunks)
	}
}

func TestChunkText_PrefersSentenceBoundary(t *testing.T) {
	first := strings.Repeat("a", 60) + "."
	second := strings.Repeat("b", 60) + "."
	chunks := chunkText(first+" "+second, 80)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %#v", len(chunks), chunks)
	}
	if chunks[0] != first {
		t.Fatalf("first chunk should end at sentence boundary, got %q", chunks[0])
	}
}

func TestChunkText_FallsBackToWordBoundary(t *testing.T) {
	long := strings.Repeat("word ", 40) // 200 chars, no sentence breaks
	chunks := chunkText(strings.TrimSpace(long), 80)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d: %#v", len(chunks), chunks)
	}
	for _, c := range chunks {
		if len(c) > 80 {
			t.Fatalf("chunk exceeds max size: len=%d", len(c))
		}
		if strings.Contains(c, "wor ") || strings.HasSuffix(c, "wor") {
			t.Fatalf("word was split mid-token: %q", c)
		}
	}
}

func TestChunkText_HardCutRespectsUTF8(t *testing.T) {
	// Each "é" is 2 bytes; together with the surrounding ASCII the string
	// is forced past maxSize with no natural break points.
	text := strings.Repeat("é", 200)
	chunks := chunkText(text, 50)
	for i, c := range chunks {
		if !isValidUTF8(c) {
			t.Fatalf("chunk %d is not valid UTF-8: %q", i, c)
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' && len(s) > 0 {
			return false
		}
	}
	return true
}

func TestStripID3v2_NoTagIsUnchanged(t *testing.T) {
	in := []byte{0xFF, 0xFB, 0x90, 0x44}
	if got := stripID3v2(in); &got[0] != &in[0] || len(got) != len(in) {
		t.Fatalf("expected unchanged slice when no ID3 tag is present")
	}
}

func TestStripID3v2_RemovesLeadingTag(t *testing.T) {
	// ID3v2 header: "ID3" + version 03 00 + flags 00 + syncsafe size of 5.
	header := []byte{'I', 'D', '3', 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05}
	tagBody := []byte{1, 2, 3, 4, 5}
	frames := []byte{0xFF, 0xFB, 0x90, 0x44}
	in := append(append(header, tagBody...), frames...)

	out := stripID3v2(in)
	if len(out) != len(frames) {
		t.Fatalf("expected tag stripped, got %d bytes (want %d)", len(out), len(frames))
	}
	for i, b := range frames {
		if out[i] != b {
			t.Fatalf("byte %d mismatch: got %x, want %x", i, out[i], b)
		}
	}
}

func TestIsValidOpenAIVoice(t *testing.T) {
	for _, v := range openAIVoices {
		if !isValidOpenAIVoice(v) {
			t.Errorf("expected %q to be valid", v)
		}
	}
	for _, v := range []string{"", "rachel", "asteria", "ALLOY"} {
		if isValidOpenAIVoice(v) {
			t.Errorf("expected %q to be invalid", v)
		}
	}
}

func TestResolveElevenLabsVoice(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"rachel", "21m00Tcm4TlvDq8ikWAM"},
		{"RACHEL", "21m00Tcm4TlvDq8ikWAM"}, // preset lookup is case-insensitive
		{"josh", "TxGEqnHWrfWFTfGW9XjX"},
		{"custom-voice-id-123", "custom-voice-id-123"}, // unknown name passes through
	}
	for _, c := range cases {
		if got := resolveElevenLabsVoice(c.in); got != c.want {
			t.Errorf("resolveElevenLabsVoice(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWithRetry_SucceedsFirstTry(t *testing.T) {
	calls := 0
	got, err := withRetry("first-try", func() ([]byte, error) {
		calls++
		return []byte("ok"), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
	if string(got) != "ok" {
		t.Fatalf("expected payload %q, got %q", "ok", string(got))
	}
}

func TestWithRetry_RecoversAfterTransientFailure(t *testing.T) {
	defer swapBackoff(time.Millisecond)()

	calls := 0
	got, err := withRetry("transient", func() ([]byte, error) {
		calls++
		if calls < 2 {
			return nil, errors.New("temporary")
		}
		return []byte("done"), nil
	})
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
	if string(got) != "done" {
		t.Fatalf("expected payload %q, got %q", "done", string(got))
	}
}

func TestWithRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	defer swapBackoff(time.Millisecond)()

	wantErr := errors.New("upstream down")
	calls := 0
	got, err := withRetry("exhausted", func() ([]byte, error) {
		calls++
		return nil, wantErr
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped %v, got %v", wantErr, err)
	}
	if calls != maxRetries {
		t.Fatalf("expected %d calls, got %d", maxRetries, calls)
	}
	if got != nil {
		t.Fatalf("expected nil payload on failure, got %q", string(got))
	}
}

func TestSynthesizeChunked_SingleChunkPassesThrough(t *testing.T) {
	calls := 0
	var lastChunk string
	out, err := synthesizeChunked("short text", func(chunk string) ([]byte, error) {
		calls++
		lastChunk = chunk
		return []byte("AUDIO"), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
	if lastChunk != "short text" {
		t.Fatalf("expected unchanged text, got %q", lastChunk)
	}
	if string(out) != "AUDIO" {
		t.Fatalf("expected payload %q, got %q", "AUDIO", string(out))
	}
}

func TestSynthesizeChunked_JoinsMultipleChunks(t *testing.T) {
	// Build text longer than maxChunkSize with paragraph breaks so it splits cleanly.
	paragraph := strings.Repeat("a", 800)
	text := paragraph + "\n\n" + paragraph + "\n\n" + paragraph
	var calls int
	out, err := synthesizeChunked(text, func(chunk string) ([]byte, error) {
		calls++
		return []byte("[" + chunk[:1] + "]"), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected multiple chunks, got %d call(s)", calls)
	}
	expected := strings.Repeat("[a]", calls)
	if string(out) != expected {
		t.Fatalf("expected joined payload %q, got %q", expected, string(out))
	}
}

func TestSynthesizeChunked_StripsID3OnLaterChunks(t *testing.T) {
	paragraph := strings.Repeat("b", 800)
	text := paragraph + "\n\n" + paragraph

	// ID3v2 header with a 5-byte body — every synth call returns one of these
	// followed by distinct frame bytes so we can see what the joiner produced.
	id3 := []byte{'I', 'D', '3', 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05, 1, 2, 3, 4, 5}
	frames := []byte{0xFF, 0xFB, 0x90, 0x44}

	out, err := synthesizeChunked(text, func(chunk string) ([]byte, error) {
		return append(append([]byte{}, id3...), frames...), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// First chunk keeps its ID3 block, every later chunk should be frames only.
	wantPrefix := append(append([]byte{}, id3...), frames...)
	if !bytesHasPrefix(out, wantPrefix) {
		t.Fatalf("first chunk should keep its ID3 tag, got prefix %x", out[:len(wantPrefix)])
	}
	rest := out[len(wantPrefix):]
	if len(rest)%len(frames) != 0 {
		t.Fatalf("trailing bytes should be whole-frame multiples, got %d bytes", len(rest))
	}
	for i := 0; i < len(rest); i += len(frames) {
		for j, b := range frames {
			if rest[i+j] != b {
				t.Fatalf("frame mismatch at offset %d: got %x, want %x", i+j, rest[i+j], b)
			}
		}
	}
}

func TestSynthesizeChunked_PropagatesUpstreamError(t *testing.T) {
	defer swapBackoff(time.Millisecond)()

	paragraph := strings.Repeat("c", 800)
	text := paragraph + "\n\n" + paragraph

	wantErr := errors.New("provider boom")
	_, err := synthesizeChunked(text, func(chunk string) ([]byte, error) {
		return nil, wantErr
	})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped %v, got %v", wantErr, err)
	}
}

func TestSynthesizeChunked_EmptyInputReturnsError(t *testing.T) {
	_, err := synthesizeChunked("   ", func(chunk string) ([]byte, error) {
		t.Fatal("synth must not be called for empty input")
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestSynthesizeOpenAI_SendsExpectedRequest(t *testing.T) {
	wantAudio := []byte{0xFF, 0xFB, 0x90, 0x44, 'O', 'P', 'E', 'N'}

	var captured struct {
		method, path, auth, contentType string
		body                            OpenAITTSRequest
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		captured.contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&captured.body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wantAudio)
	}))
	defer server.Close()

	defer swapURL(&openAIAPIURL, server.URL)()

	got, err := synthesizeOpenAI("sk-test", "tts-1-hd", "alloy", "hello", 1.25)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytesEqual(got, wantAudio) {
		t.Fatalf("audio mismatch: got %x, want %x", got, wantAudio)
	}
	if captured.method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.method)
	}
	if captured.auth != "Bearer sk-test" {
		t.Errorf("auth = %q, want %q", captured.auth, "Bearer sk-test")
	}
	if captured.contentType != "application/json" {
		t.Errorf("content-type = %q", captured.contentType)
	}
	if captured.body.Model != "tts-1-hd" || captured.body.Voice != "alloy" ||
		captured.body.Input != "hello" || captured.body.Speed != 1.25 ||
		captured.body.ResponseFormat != "mp3" {
		t.Errorf("body mismatch: %+v", captured.body)
	}
}

func TestSynthesizeOpenAI_ReturnsErrorOnNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad key"}`)
	}))
	defer server.Close()

	defer swapURL(&openAIAPIURL, server.URL)()

	_, err := synthesizeOpenAI("sk-bad", "tts-1", "alloy", "hello", 1.0)
	if err == nil {
		t.Fatal("expected error on 401 response")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("expected status and body in error, got %v", err)
	}
}

func TestSynthesizeElevenLabs_SendsExpectedRequest(t *testing.T) {
	wantAudio := []byte{0xFF, 0xFB, 0x90, 0x44, 'E', 'L'}

	var captured struct {
		method, path, key, query string
		body                     ElevenLabsTTSRequest
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.query = r.URL.RawQuery
		captured.key = r.Header.Get("xi-api-key")
		_ = json.NewDecoder(r.Body).Decode(&captured.body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wantAudio)
	}))
	defer server.Close()

	defer swapURL(&elevenLabsAPIURL, server.URL)()

	got, err := synthesizeElevenLabs("xi-test", "eleven_multilingual_v2", "voice-xyz", "hi", 1.1, 0.6, 0.8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytesEqual(got, wantAudio) {
		t.Fatalf("audio mismatch: got %x, want %x", got, wantAudio)
	}
	if captured.method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.method)
	}
	if !strings.HasSuffix(captured.path, "/voice-xyz") {
		t.Errorf("voice ID not in path: %q", captured.path)
	}
	if !strings.Contains(captured.query, "output_format=mp3_44100_128") {
		t.Errorf("expected mp3 output_format query, got %q", captured.query)
	}
	if captured.key != "xi-test" {
		t.Errorf("xi-api-key = %q", captured.key)
	}
	if captured.body.Text != "hi" || captured.body.ModelID != "eleven_multilingual_v2" {
		t.Errorf("body mismatch: %+v", captured.body)
	}
	if captured.body.VoiceSettings == nil {
		t.Fatal("voice settings missing")
	}
	if captured.body.VoiceSettings.Stability != 0.6 ||
		captured.body.VoiceSettings.SimilarityBoost != 0.8 ||
		captured.body.VoiceSettings.Speed != 1.1 {
		t.Errorf("voice settings mismatch: %+v", *captured.body.VoiceSettings)
	}
}

func TestSynthesizeElevenLabs_ReturnsErrorOnNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"quota"}`)
	}))
	defer server.Close()
	defer swapURL(&elevenLabsAPIURL, server.URL)()

	_, err := synthesizeElevenLabs("xi", "eleven_turbo_v2", "voice", "hi", 1.0, 0.5, 0.75)
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("expected status and body in error, got %v", err)
	}
}

func swapURL(target *string, url string) func() {
	original := *target
	*target = url
	return func() { *target = original }
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func bytesHasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if b[i] != p {
			return false
		}
	}
	return true
}

// swapBackoff temporarily lowers the initial retry backoff so retry tests don't
// burn real seconds. Returns a restore func meant to be deferred.
func swapBackoff(d time.Duration) func() {
	original := initialBackoff
	initialBackoff = d
	return func() { initialBackoff = original }
}

func TestResolveDeepgramVoice(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"asteria", "aura-asteria-en"},
		{"ASTERIA", "aura-asteria-en"},
		{"thalia", "aura-2-thalia-en"},
		{"aura-custom-en", "aura-custom-en"}, // full model name passes through
	}
	for _, c := range cases {
		if got := resolveDeepgramVoice(c.in); got != c.want {
			t.Errorf("resolveDeepgramVoice(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
