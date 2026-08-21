package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ebitengine/oto/v3"
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

func TestChunkText_FallsBackToSingleNewline(t *testing.T) {
	// No paragraph breaks, no sentence endings, no spaces near the boundary —
	// just a single newline that the splitter should fall through to.
	first := strings.Repeat("a", 80)
	second := strings.Repeat("b", 80)
	chunks := chunkText(first+"\n"+second, 100)
	if len(chunks) != 2 || chunks[0] != first || chunks[1] != second {
		t.Fatalf("expected split on single newline, got %#v", chunks)
	}
}

func TestChunkText_HardCutFallsBackToMaxSizeWhenAllBytesAreContinuations(t *testing.T) {
	// Synthetic pathological input: a long run with no break points and a hard
	// cut position that lands inside a multibyte char. The rewind loop walks
	// back, hits zero, then resets to maxSize so progress is made. The chunks
	// won't be valid UTF-8 (the input isn't either) but the function must
	// still terminate and produce chunks bounded by maxSize.
	text := strings.Repeat("\x80", 300) // every byte is a UTF-8 continuation byte
	chunks := chunkText(text, 50)
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	total := 0
	for i, c := range chunks {
		if len(c) > 50 {
			t.Fatalf("chunk %d exceeds maxSize: %d bytes", i, len(c))
		}
		total += len(c)
	}
	if total == 0 {
		t.Fatal("expected nonzero total chunk bytes")
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

func TestStripID3v2_LeavesTruncatedTagAlone(t *testing.T) {
	// Header claims a 100-byte body but only 3 trailing bytes are present.
	// stripID3v2 must refuse to slice past the buffer and return the input
	// unchanged so callers get something safe to write to the joined stream.
	header := []byte{'I', 'D', '3', 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x64}
	in := append(header, 0x01, 0x02, 0x03)

	out := stripID3v2(in)
	if len(out) != len(in) {
		t.Fatalf("expected unchanged slice for truncated tag, got %d bytes", len(out))
	}
	for i, b := range in {
		if out[i] != b {
			t.Fatalf("byte %d mismatch: got %x, want %x", i, out[i], b)
		}
	}
}

func TestStripID3v2_LeavesShortBufferAlone(t *testing.T) {
	// Anything shorter than the 10-byte header can't possibly be a tag.
	in := []byte{'I', 'D', '3', 0x03}
	out := stripID3v2(in)
	if len(out) != len(in) {
		t.Fatalf("expected unchanged slice for too-short buffer, got %d bytes", len(out))
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

func TestSynthesizeDeepgram_SendsExpectedRequest(t *testing.T) {
	wantAudio := []byte{0xFF, 0xFB, 0x90, 0x44, 'D', 'G'}

	var captured struct {
		method, auth, query string
		body                DeepgramTTSRequest
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.auth = r.Header.Get("Authorization")
		captured.query = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&captured.body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wantAudio)
	}))
	defer server.Close()

	defer swapURL(&deepgramAPIURL, server.URL)()

	got, err := synthesizeDeepgram("dg-test", "aura-asteria-en", "hello deepgram")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytesEqual(got, wantAudio) {
		t.Fatalf("audio mismatch: got %x, want %x", got, wantAudio)
	}
	if captured.method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.method)
	}
	if captured.auth != "Token dg-test" {
		t.Errorf("auth = %q, want %q", captured.auth, "Token dg-test")
	}
	if !strings.Contains(captured.query, "model=aura-asteria-en") {
		t.Errorf("model not in query: %q", captured.query)
	}
	if !strings.Contains(captured.query, "encoding=mp3") {
		t.Errorf("encoding not in query: %q", captured.query)
	}
	if captured.body.Text != "hello deepgram" {
		t.Errorf("body text = %q, want %q", captured.body.Text, "hello deepgram")
	}
}

func TestSynthesizeDeepgram_ReturnsErrorOnNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"err":"upstream"}`)
	}))
	defer server.Close()
	defer swapURL(&deepgramAPIURL, server.URL)()

	_, err := synthesizeDeepgram("dg", "aura-asteria-en", "hi")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "upstream") {
		t.Fatalf("expected status and body in error, got %v", err)
	}
}

func TestSynthesizeOpenAI_FailsOnUnreachable(t *testing.T) {
	defer swapURL(&openAIAPIURL, unreachableURL(t))()
	_, err := synthesizeOpenAI("sk", "tts-1", "alloy", "hi", 1.0)
	assertNetworkError(t, err)
}

func TestSynthesizeElevenLabs_FailsOnUnreachable(t *testing.T) {
	defer swapURL(&elevenLabsAPIURL, unreachableURL(t))()
	_, err := synthesizeElevenLabs("xi", "eleven_turbo_v2", "voice", "hi", 1.0, 0.5, 0.75)
	assertNetworkError(t, err)
}

func TestSynthesizeDeepgram_FailsOnUnreachable(t *testing.T) {
	defer swapURL(&deepgramAPIURL, unreachableURL(t))()
	_, err := synthesizeDeepgram("dg", "aura-asteria-en", "hi")
	assertNetworkError(t, err)
}

func TestSynthesizeOpenAI_FailsOnMalformedURL(t *testing.T) {
	defer swapURL(&openAIAPIURL, "://no-scheme")()
	_, err := synthesizeOpenAI("sk", "tts-1", "alloy", "hi", 1.0)
	assertRequestBuildError(t, err)
}

func TestSynthesizeElevenLabs_FailsOnMalformedURL(t *testing.T) {
	defer swapURL(&elevenLabsAPIURL, "://no-scheme")()
	_, err := synthesizeElevenLabs("xi", "eleven_turbo_v2", "voice", "hi", 1.0, 0.5, 0.75)
	assertRequestBuildError(t, err)
}

func TestSynthesizeDeepgram_FailsOnMalformedURL(t *testing.T) {
	defer swapURL(&deepgramAPIURL, "://no-scheme")()
	_, err := synthesizeDeepgram("dg", "aura-asteria-en", "hi")
	assertRequestBuildError(t, err)
}

func assertRequestBuildError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error from malformed URL")
	}
	if !strings.Contains(err.Error(), "failed to create request") {
		t.Fatalf("expected wrapped http.NewRequest error, got %v", err)
	}
}

// unreachableURL returns a URL that's guaranteed to refuse connections by
// starting a server, taking its URL, then closing it.
func unreachableURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

func assertNetworkError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error from unreachable server")
	}
	if !strings.Contains(err.Error(), "failed to make request") {
		t.Fatalf("expected wrapped client.Do error, got %v", err)
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

func TestDecodeMP3_RejectsGarbage(t *testing.T) {
	_, err := decodeMP3([]byte("not an mp3 stream"))
	if err == nil {
		t.Fatal("expected error decoding garbage as MP3")
	}
	if !strings.Contains(err.Error(), "failed to decode MP3") {
		t.Fatalf("expected wrapped decode error, got %v", err)
	}
}

func TestDecodeMP3_AcceptsValidStream(t *testing.T) {
	mp3 := silentMP3OrSkip(t)
	dec, err := decodeMP3(mp3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.SampleRate() <= 0 {
		t.Fatalf("expected positive sample rate, got %d", dec.SampleRate())
	}
}

func TestPlayAudio_ReturnsDecodeErrorBeforeTouchingAudioDevice(t *testing.T) {
	// Garbage input must fail at decode, so this test doesn't depend on a
	// working system audio device — useful for CI runners with no sink.
	err := playAudio([]byte{0x00, 0x01, 0x02, 0x03})
	if err == nil {
		t.Fatal("expected error from garbage MP3")
	}
	if !strings.Contains(err.Error(), "failed to decode MP3") {
		t.Fatalf("expected decode-stage error, got %v", err)
	}
}

// TestMain re-routes execution to main() when GOSPEAK_TEST_BINARY is set, so
// subprocess tests below can exercise the real entry point with coverage.
func TestMain(m *testing.M) {
	if os.Getenv("GOSPEAK_TEST_BINARY") == "1" {
		main()
		return
	}
	// Point the config lookup at an empty directory so a real ~/.gospeak.json on
	// the machine running the suite cannot change what any of these tests see.
	// Tests that want a config file install their own userHomeDir or pass
	// --config.
	emptyHome, err := os.MkdirTemp("", "gospeak-no-config")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create config-free home: %v\n", err)
		os.Exit(1)
	}
	userHomeDir = func() (string, error) { return emptyHome, nil }
	code := m.Run()
	_ = os.RemoveAll(emptyHome)
	os.Exit(code)
}

func TestMainEntryPoint_NoArgsExitsOne(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "GOSPEAK_TEST_BINARY=1", "OPENAI_API_KEY=",
		"HOME="+t.TempDir(), "GOSPEAK_CONFIG=")
	cmd.Stdin = strings.NewReader("") // simulate no piped input
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exit error, got %v", err)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("exit code = %d, want 1", exitErr.ExitCode())
	}
	// Either of these is acceptable: it could complain about the missing key
	// (when OPENAI_API_KEY isn't set in the env) or about empty text.
	if !strings.Contains(stderr.String(), "OPENAI_API_KEY") &&
		!strings.Contains(stderr.String(), "No text provided") {
		t.Fatalf("expected an actionable error, got %q", stderr.String())
	}
}

func TestMainEntryPoint_HelpExitsZero(t *testing.T) {
	cmd := exec.Command(os.Args[0], "--help")
	cmd.Env = append(os.Environ(), "GOSPEAK_TEST_BINARY=1",
		"HOME="+t.TempDir(), "GOSPEAK_CONFIG=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("expected exit 0 for --help, got %v: %s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gospeak - Text-to-speech") {
		t.Fatal("expected usage banner in stderr")
	}
}

func TestMainEntryPoint_PipedStdinReachesRun(t *testing.T) {
	// Piping text in with no API key set should hit the missing-key branch
	// through the real main() — exercises the os.Stdin.Stat non-character
	// device branch that resolves stdin to the pipe.
	cmd := exec.Command(os.Args[0], "-p", "openai")
	cmd.Env = append(os.Environ(), "GOSPEAK_TEST_BINARY=1", "OPENAI_API_KEY=",
		"HOME="+t.TempDir(), "GOSPEAK_CONFIG=")
	cmd.Stdin = strings.NewReader("from a real pipe")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exit error, got %v", err)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("exit code = %d, want 1", exitErr.ExitCode())
	}
	if !strings.Contains(stderr.String(), "OPENAI_API_KEY") {
		t.Fatalf("expected missing-key error, got %q", stderr.String())
	}
}

// emptyEnv simulates no environment variables being set.
func emptyEnv(string) string { return "" }

// envMap returns a getenv function that looks up keys in the given map.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func runCLI(t *testing.T, args []string, stdin string, getenv func(string) string) (int, string) {
	t.Helper()
	var stderr bytes.Buffer
	code := run(args, strings.NewReader(stdin), &stderr, getenv)
	return code, stderr.String()
}

func TestRun_HelpReturnsZeroAndPrintsUsage(t *testing.T) {
	code, stderr := runCLI(t, []string{"--help"}, "", emptyEnv)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr, "gospeak - Text-to-speech") {
		t.Errorf("expected usage banner in stderr, got %q", stderr)
	}
}

func TestRun_InvalidFlagReturnsTwo(t *testing.T) {
	code, _ := runCLI(t, []string{"--nope"}, "", emptyEnv)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (flag parse error)", code)
	}
}

func TestRun_InvalidProviderReturnsOne(t *testing.T) {
	code, stderr := runCLI(t, []string{"-p", "weird", "hello"}, "", emptyEnv)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "Invalid provider 'weird'") {
		t.Errorf("expected provider error in stderr, got %q", stderr)
	}
}

func TestRun_MissingAPIKeyPerProvider(t *testing.T) {
	cases := []struct {
		provider, env string
	}{
		{"openai", "OPENAI_API_KEY"},
		{"elevenlabs", "ELEVENLABS_API_KEY"},
		{"deepgram", "DEEPGRAM_API_KEY"},
	}
	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			code, stderr := runCLI(t, []string{"-p", c.provider, "hello"}, "", emptyEnv)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if !strings.Contains(stderr, c.env) {
				t.Errorf("expected %s in error message, got %q", c.env, stderr)
			}
		})
	}
}

func TestRun_SpeedOutOfRangePerProvider(t *testing.T) {
	cases := []struct {
		name, provider string
		speed          string
		wantErr        string
	}{
		{"openai too slow", "openai", "0.1", "0.25 and 4.0"},
		{"openai too fast", "openai", "5.0", "0.25 and 4.0"},
		{"elevenlabs too slow", "elevenlabs", "0.5", "0.7 and 1.2"},
		{"elevenlabs too fast", "elevenlabs", "1.5", "0.7 and 1.2"},
	}
	env := envMap(map[string]string{
		"OPENAI_API_KEY":     "k",
		"ELEVENLABS_API_KEY": "k",
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stderr := runCLI(t, []string{"-p", c.provider, "-x", c.speed, "hello"}, "", env)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if !strings.Contains(stderr, c.wantErr) {
				t.Errorf("expected %q in stderr, got %q", c.wantErr, stderr)
			}
		})
	}
}

func TestRun_DeepgramSpeedWarnsButContinues(t *testing.T) {
	// Deepgram doesn't support speed adjustment but doesn't exit; the warning
	// should be on stderr while the rest of the pipeline proceeds. Pair this
	// with -o and a stub server so we exercise the warning path without
	// actually hitting a real API.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB, 0x90, 0x44})
	}))
	defer server.Close()
	defer swapURL(&deepgramAPIURL, server.URL)()

	dir := t.TempDir()
	out := dir + "/x.mp3"
	code, stderr := runCLI(t, []string{
		"-p", "deepgram", "-x", "1.5", "-o", out, "hi",
	}, "", envMap(map[string]string{"DEEPGRAM_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "Speed adjustment is not supported") {
		t.Errorf("expected speed warning in stderr, got %q", stderr)
	}
}

func TestRun_EmptyTextReturnsError(t *testing.T) {
	code, stderr := runCLI(t, []string{"-p", "deepgram"}, "", envMap(map[string]string{
		"DEEPGRAM_API_KEY": "k",
	}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "No text provided") {
		t.Errorf("expected empty-text error, got %q", stderr)
	}
}

func TestRun_InvalidOpenAIVoiceReturnsError(t *testing.T) {
	code, stderr := runCLI(t, []string{"-v", "not-a-voice", "hello"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "Invalid OpenAI voice 'not-a-voice'") {
		t.Errorf("expected voice error, got %q", stderr)
	}
}

// stubAudio captures calls to playAudioFn so tests can verify playback was
// invoked without driving the real audio device. Returns a restore func.
func stubAudio() (*[][]byte, func()) {
	var captured [][]byte
	original := playAudioFn
	playAudioFn = func(b []byte) error {
		captured = append(captured, append([]byte(nil), b...))
		return nil
	}
	return &captured, func() { playAudioFn = original }
}

// stubAudioReturning installs a playAudioFn that returns the supplied error.
func stubAudioReturning(err error) func() {
	original := playAudioFn
	playAudioFn = func([]byte) error { return err }
	return func() { playAudioFn = original }
}

func TestRun_OpenAIHappyPathWritesFile(t *testing.T) {
	wantAudio := []byte{0xFF, 0xFB, 'O', 'A'}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(wantAudio)
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()

	out := t.TempDir() + "/out.mp3"
	code, stderr := runCLI(t, []string{"-o", out, "hello"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("output file not written: %v", err)
	}
	if !bytesEqual(got, wantAudio) {
		t.Fatalf("file contents = %x, want %x", got, wantAudio)
	}
	if !strings.Contains(stderr, "Saved to "+out) {
		t.Errorf("expected save confirmation in stderr, got %q", stderr)
	}
}

func TestRun_ElevenLabsHappyPathReadsStdin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB, 'E', 'L'})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsAPIURL, server.URL)()

	out := t.TempDir() + "/out.mp3"
	code, stderr := runCLI(t, []string{"-p", "elevenlabs", "-o", out},
		"  piped text  ",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("expected output file, got %v", err)
	}
}

func TestRun_PlaysAudioWhenNoOutputFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB, 'P', '1'})
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()

	captured, restore := stubAudio()
	defer restore()

	code, stderr := runCLI(t, []string{"hello"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 playback call, got %d", len(*captured))
	}
	if !bytesEqual((*captured)[0], []byte{0xFF, 0xFB, 'P', '1'}) {
		t.Errorf("playback received wrong bytes: %x", (*captured)[0])
	}
}

func TestRun_SpeakFlagPlaysEvenWhenSavingFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB, 'S', 'P'})
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()

	captured, restore := stubAudio()
	defer restore()

	out := t.TempDir() + "/out.mp3"
	code, _ := runCLI(t, []string{"-o", out, "-s", "hi"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("expected file written, got %v", err)
	}
	if len(*captured) != 1 {
		t.Errorf("expected playback even with -o when -s is set, got %d calls", len(*captured))
	}
}

func TestRun_PlaybackFailureReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB, 'X', 'X'})
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()

	defer stubAudioReturning(errors.New("no sink"))()

	code, stderr := runCLI(t, []string{"hi"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "Error playing audio") || !strings.Contains(stderr, "no sink") {
		t.Errorf("expected playback error in stderr, got %q", stderr)
	}
}

func TestRun_SynthFailureReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()
	defer swapBackoff(time.Millisecond)()

	out := t.TempDir() + "/out.mp3"
	code, stderr := runCLI(t, []string{"-o", out, "hi"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "Error synthesizing speech") {
		t.Errorf("expected synth error in stderr, got %q", stderr)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("expected no file written when synth fails")
	}
}

func TestRun_FileWriteFailureReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB, 'W', 'R'})
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()

	// A path under a non-existent directory cannot be written.
	out := t.TempDir() + "/does-not-exist/out.mp3"
	code, stderr := runCLI(t, []string{"-o", out, "hi"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "Error saving file") {
		t.Errorf("expected save error in stderr, got %q", stderr)
	}
}

func TestRun_AllFlagIteratesOpenAIVoices(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body OpenAITTSRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.Voice)
		mu.Unlock()
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()
	defer stubAudioReturning(nil)()
	originalSleep := allVoiceSleep
	allVoiceSleep = func(time.Duration) {}
	defer func() { allVoiceSleep = originalSleep }()

	code, _ := runCLI(t, []string{"--all", "hello"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// --all calls each voice twice (announcement + text), so we expect every
	// canonical voice to appear at least once.
	got := map[string]bool{}
	for _, v := range seen {
		got[v] = true
	}
	for _, v := range openAIVoices {
		if !got[v] {
			t.Errorf("expected --all to synthesize for voice %q", v)
		}
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestRun_StdinReadFailureReturnsError(t *testing.T) {
	var stderr bytes.Buffer
	code := run(nil, failingReader{err: errors.New("pipe broken")},
		&stderr, envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Error reading stdin") ||
		!strings.Contains(stderr.String(), "pipe broken") {
		t.Fatalf("expected wrapped stdin error, got %q", stderr.String())
	}
}

func TestRun_AllFlagContinuesAfterAnnouncementSynthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()
	defer swapBackoff(time.Millisecond)()
	defer stubAudioReturning(nil)()
	originalSleep := allVoiceSleep
	allVoiceSleep = func(time.Duration) {}
	defer func() { allVoiceSleep = originalSleep }()

	code, stderr := runCLI(t, []string{"--all", "hello"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (--all should soldier on)", code)
	}
	// Every voice should have produced a synthesis error line.
	count := strings.Count(stderr, "Error synthesizing voice announcement")
	if count != len(openAIVoices) {
		t.Fatalf("expected %d synth errors, got %d in %q", len(openAIVoices), count, stderr)
	}
}

func TestRun_AllFlagContinuesAfterAnnouncementPlaybackError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()
	defer stubAudioReturning(errors.New("no sink"))()
	originalSleep := allVoiceSleep
	allVoiceSleep = func(time.Duration) {}
	defer func() { allVoiceSleep = originalSleep }()

	code, stderr := runCLI(t, []string{"--all", "hello"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr, "no sink") {
		t.Fatalf("expected playback error in stderr, got %q", stderr)
	}
}

func TestRun_AllFlagContinuesAfterTextStageErrors(t *testing.T) {
	// Succeeds on announcement (Input matches a voice name), fails on text.
	voices := map[string]bool{}
	for _, v := range openAIVoices {
		voices[v] = true
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body OpenAITTSRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if voices[body.Input] {
			_, _ = w.Write([]byte{0xFF, 0xFB})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()
	defer swapBackoff(time.Millisecond)()

	// Announcement playback succeeds, text playback would too but the text
	// stage fails at synth before playback is called.
	defer stubAudioReturning(nil)()
	originalSleep := allVoiceSleep
	allVoiceSleep = func(time.Duration) {}
	defer func() { allVoiceSleep = originalSleep }()

	code, stderr := runCLI(t, []string{"--all", "hello"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	count := strings.Count(stderr, "Error synthesizing:")
	if count != len(openAIVoices) {
		t.Fatalf("expected %d text-synth errors, got %d in %q", len(openAIVoices), count, stderr)
	}
}

func TestRun_AllFlagLogsButDoesNotExitOnTextPlaybackError(t *testing.T) {
	// Announcement playback succeeds, text playback fails — the loop should
	// log the error but fall through to the next voice without continue/exit.
	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()

	original := playAudioFn
	// Odd calls (announcement) succeed; even calls (text) fail.
	playAudioFn = func([]byte) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls%2 == 0 {
			return errors.New("text playback boom")
		}
		return nil
	}
	defer func() { playAudioFn = original }()

	originalSleep := allVoiceSleep
	allVoiceSleep = func(time.Duration) {}
	defer func() { allVoiceSleep = originalSleep }()

	code, stderr := runCLI(t, []string{"--all", "hi"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
	}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr, "text playback boom") {
		t.Errorf("expected playback error to be logged, got %q", stderr)
	}
}

func TestRun_AllFlagRejectsNonOpenAIProvider(t *testing.T) {
	code, stderr := runCLI(t, []string{"--all", "-p", "deepgram", "hello"}, "", envMap(map[string]string{
		"DEEPGRAM_API_KEY": "k",
	}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "--all flag is only supported for OpenAI") {
		t.Errorf("expected --all guard message, got %q", stderr)
	}
}

func TestPlayDecoded_WrapsAudioContextError(t *testing.T) {
	mp3 := silentMP3OrSkip(t)
	dec, err := decodeMP3(mp3)
	if err != nil {
		t.Fatalf("unexpected decode error: %v", err)
	}

	original := newAudioContext
	newAudioContext = func(*oto.NewContextOptions) (*oto.Context, chan struct{}, error) {
		return nil, nil, errors.New("no audio sink")
	}
	defer func() { newAudioContext = original }()

	err = playDecoded(dec)
	if err == nil {
		t.Fatal("expected error from injected audio context failure")
	}
	if !strings.Contains(err.Error(), "failed to create audio context") {
		t.Fatalf("expected wrapped context error, got %v", err)
	}
	if !strings.Contains(err.Error(), "no audio sink") {
		t.Fatalf("expected underlying cause in error, got %v", err)
	}
}

func TestPlayAudio_PlaysSilentMP3EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping audio playback test in -short mode")
	}
	mp3 := silentMP3OrSkip(t)

	err := playAudio(mp3)
	if err != nil {
		// On a headless CI host there's no audio sink. oto reports this as
		// "failed to create audio context"; treat it as a skip rather than a
		// hard failure so the suite is still useful in that environment.
		if strings.Contains(err.Error(), "failed to create audio context") {
			t.Skipf("no audio device available: %v", err)
		}
		t.Fatalf("playAudio returned unexpected error: %v", err)
	}
}

// silentMP3OrSkip generates a short silent MP3 with ffmpeg so tests that need
// a valid MP3 byte stream can run without bundling a binary fixture or hitting
// a TTS provider. Skips the test if ffmpeg isn't available.
func silentMP3OrSkip(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skipf("ffmpeg not in PATH: %v", err)
	}
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anullsrc=channel_layout=mono:sample_rate=22050",
		"-t", "0.1", "-f", "mp3", "-y", "pipe:1")
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg failed to generate silent fixture: %v: %s", err, errBuf.String())
	}
	if out.Len() == 0 {
		t.Skip("ffmpeg produced empty fixture")
	}
	return out.Bytes()
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

// --- Sound effects (ElevenLabs sound-generation) ---

func TestSynthesizeSoundEffect_SendsExpectedRequest(t *testing.T) {
	wantAudio := []byte{0xFF, 0xFB, 'S', 'F', 'X'}

	var captured struct {
		method, path, key, contentType, query string
		raw                                   map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.query = r.URL.RawQuery
		captured.key = r.Header.Get("xi-api-key")
		captured.contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&captured.raw)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wantAudio)
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	dur := 7.5
	got, err := synthesizeSoundEffect("xi-test", "eleven_text_to_sound_v2", "thunder rolling", &dur, 0.8, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytesEqual(got, wantAudio) {
		t.Fatalf("audio mismatch: got %x, want %x", got, wantAudio)
	}
	if captured.method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.method)
	}
	if captured.key != "xi-test" {
		t.Errorf("xi-api-key = %q, want xi-test", captured.key)
	}
	if captured.contentType != "application/json" {
		t.Errorf("Content-Type = %q", captured.contentType)
	}
	if !strings.Contains(captured.query, "output_format=mp3_44100_128") {
		t.Errorf("expected mp3 output_format query, got %q", captured.query)
	}
	if captured.raw["text"] != "thunder rolling" {
		t.Errorf("text = %v, want thunder rolling", captured.raw["text"])
	}
	if captured.raw["model_id"] != "eleven_text_to_sound_v2" {
		t.Errorf("model_id = %v", captured.raw["model_id"])
	}
	if captured.raw["duration_seconds"] != 7.5 {
		t.Errorf("duration_seconds = %v, want 7.5", captured.raw["duration_seconds"])
	}
	if captured.raw["prompt_influence"] != 0.8 {
		t.Errorf("prompt_influence = %v, want 0.8", captured.raw["prompt_influence"])
	}
	if captured.raw["loop"] != true {
		t.Errorf("loop = %v, want true", captured.raw["loop"])
	}
}

// A nil duration must be omitted from the payload entirely: sending an explicit
// null or zero would be rejected, and omission is what makes the model infer
// the length from the prompt.
func TestSynthesizeSoundEffect_OmitsUnsetDuration(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	if _, err := synthesizeSoundEffect("xi", "m", "a door creaking", nil, 0.3, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := raw["duration_seconds"]; present {
		t.Errorf("duration_seconds should be omitted when unset, got %v", raw["duration_seconds"])
	}
}

// prompt_influence of 0 is meaningful (maximum variability), so it must survive
// serialisation rather than being dropped as a zero value.
func TestSynthesizeSoundEffect_SendsZeroInfluence(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	if _, err := synthesizeSoundEffect("xi", "m", "silence", nil, 0, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, present := raw["prompt_influence"]
	if !present {
		t.Fatal("prompt_influence must always be sent, even at 0")
	}
	if got != float64(0) {
		t.Errorf("prompt_influence = %v, want 0", got)
	}
}

func TestSynthesizeSoundEffect_ReturnsErrorOnNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"detail":"prompt too long"}`)
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	_, err := synthesizeSoundEffect("xi", "m", "boom", nil, 0.3, false)
	if err == nil {
		t.Fatal("expected error on 422")
	}
	if !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "prompt too long") {
		t.Fatalf("expected status and body in error, got %v", err)
	}
}

func TestSynthesizeSoundEffect_FailsOnUnreachable(t *testing.T) {
	defer swapURL(&elevenLabsSFXAPIURL, unreachableURL(t))()
	_, err := synthesizeSoundEffect("xi", "m", "boom", nil, 0.3, false)
	assertNetworkError(t, err)
}

func TestSynthesizeSoundEffect_FailsOnMalformedURL(t *testing.T) {
	defer swapURL(&elevenLabsSFXAPIURL, "://bad")()
	_, err := synthesizeSoundEffect("xi", "m", "boom", nil, 0.3, false)
	assertRequestBuildError(t, err)
}

func TestRun_SFXHappyPathWritesFile(t *testing.T) {
	wantAudio := []byte{0xFF, 0xFB, 'S', 'X'}
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write(wantAudio)
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, stderr := runCLI(t, []string{"--sfx", "-o", out, "glass", "shattering"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("output file not written: %v", err)
	}
	if !bytesEqual(got, wantAudio) {
		t.Fatalf("file contents = %x, want %x", got, wantAudio)
	}
	if raw["text"] != "glass shattering" {
		t.Errorf("prompt = %v, want 'glass shattering'", raw["text"])
	}
	// --sfx must default to the sound model, not the speech model.
	if raw["model_id"] != defaultSFXModel {
		t.Errorf("model_id = %v, want %s", raw["model_id"], defaultSFXModel)
	}
}

// --sfx must not route through the ElevenLabs text-to-speech endpoint.
func TestRun_SFXDoesNotHitSpeechEndpoint(t *testing.T) {
	speechHits := 0
	speech := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		speechHits++
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer speech.Close()
	defer swapURL(&elevenLabsAPIURL, speech.URL)()

	sfxHits := 0
	sfxServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sfxHits++
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer sfxServer.Close()
	defer swapURL(&elevenLabsSFXAPIURL, sfxServer.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, stderr := runCLI(t, []string{"--sfx", "-o", out, "rain"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, stderr)
	}
	if speechHits != 0 {
		t.Errorf("speech endpoint hit %d times, want 0", speechHits)
	}
	if sfxHits != 1 {
		t.Errorf("sfx endpoint hit %d times, want 1", sfxHits)
	}
}

// --sfx implies the elevenlabs provider, so it must read ELEVENLABS_API_KEY
// even though the provider flag was never passed.
func TestRun_SFXUsesElevenLabsKeyWithoutProviderFlag(t *testing.T) {
	code, stderr := runCLI(t, []string{"--sfx", "thunder"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "openai-only"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "ELEVENLABS_API_KEY") {
		t.Errorf("expected ElevenLabs key error, got %q", stderr)
	}
}

func TestRun_SFXRejectsNonElevenLabsProvider(t *testing.T) {
	for _, p := range []string{"openai", "deepgram"} {
		code, stderr := runCLI(t, []string{"--sfx", "-p", p, "thunder"}, "",
			envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
		if code != 1 {
			t.Errorf("provider %s: exit code = %d, want 1", p, code)
		}
		if !strings.Contains(stderr, "only supported by the elevenlabs provider") {
			t.Errorf("provider %s: unexpected stderr %q", p, stderr)
		}
	}
}

func TestRun_SFXAcceptsExplicitElevenLabsProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, stderr := runCLI(t, []string{"--sfx", "-p", "ElevenLabs", "-o", out, "thunder"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
}

func TestRun_SFXRejectsOutOfRangeDuration(t *testing.T) {
	for _, d := range []string{"0.4", "30.1", "-3"} {
		code, stderr := runCLI(t, []string{"--sfx", "-d", d, "thunder"}, "",
			envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
		if code != 1 {
			t.Errorf("duration %s: exit code = %d, want 1", d, code)
		}
		if !strings.Contains(stderr, "Duration must be between") {
			t.Errorf("duration %s: unexpected stderr %q", d, stderr)
		}
	}
}

func TestRun_SFXRejectsOutOfRangeInfluence(t *testing.T) {
	for _, i := range []string{"-0.1", "1.1"} {
		code, stderr := runCLI(t, []string{"--sfx", "--influence", i, "thunder"}, "",
			envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
		if code != 1 {
			t.Errorf("influence %s: exit code = %d, want 1", i, code)
		}
		if !strings.Contains(stderr, "Influence must be between") {
			t.Errorf("influence %s: unexpected stderr %q", i, stderr)
		}
	}
}

// Speed has no meaning for a sound effect, so the ElevenLabs speech speed range
// must not be enforced in --sfx mode.
func TestRun_SFXIgnoresSpeedRangeButWarns(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, stderr := runCLI(t, []string{"--sfx", "-x", "3.0", "-o", out, "thunder"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "does not apply to sound effects") {
		t.Errorf("expected ignored-flag warning, got %q", stderr)
	}
}

func TestRun_SFXWarnsOnIrrelevantVoiceFlags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, stderr := runCLI(t,
		[]string{"--sfx", "-v", "josh", "--stability", "0.9", "--similarity", "0.2", "-o", out, "thunder"},
		"", envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	for _, want := range []string{"-v does not apply", "--stability does not apply", "--similarity does not apply"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("missing warning %q in stderr %q", want, stderr)
		}
	}
}

// The sound-effect-only flags are silently meaningless in speech mode, so the
// user gets told rather than left wondering why nothing changed.
func TestRun_WarnsWhenSFXFlagsUsedWithoutSFX(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()

	out := t.TempDir() + "/out.mp3"
	code, stderr := runCLI(t, []string{"-d", "5", "--loop", "--influence", "0.9", "-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	for _, want := range []string{"-d only applies with --sfx", "--loop only applies with --sfx", "--influence only applies with --sfx"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("missing warning %q in stderr %q", want, stderr)
		}
	}
}

func TestRun_SFXRejectsAllFlag(t *testing.T) {
	code, stderr := runCLI(t, []string{"--sfx", "--all", "thunder"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "--all cannot be combined with --sfx") {
		t.Errorf("unexpected stderr %q", stderr)
	}
}

func TestRun_SFXEmptyPromptReportsPromptError(t *testing.T) {
	code, stderr := runCLI(t, []string{"--sfx"}, "   ",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "No sound effect prompt provided") {
		t.Errorf("unexpected stderr %q", stderr)
	}
}

// A long prompt must reach the API intact rather than being chunked into
// several separate sound effects and concatenated.
func TestRun_SFXDoesNotChunkLongPrompts(t *testing.T) {
	longPrompt := strings.Repeat("rolling thunder over a wide valley ", 80) // >1500 chars
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body ElevenLabsSFXRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body.Text)
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, stderr := runCLI(t, []string{"--sfx", "-o", out}, longPrompt,
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(bodies))
	}
	if bodies[0] != strings.TrimSpace(longPrompt) {
		t.Errorf("prompt was altered: got %d chars, want %d", len(bodies[0]), len(strings.TrimSpace(longPrompt)))
	}
}

func TestRun_SFXRetriesTransientFailures(t *testing.T) {
	defer swapBackoff(time.Millisecond)()

	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, stderr := runCLI(t, []string{"--sfx", "-o", out, "thunder"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

func TestRun_SFXFailureReportsSoundEffectError(t *testing.T) {
	defer swapBackoff(time.Millisecond)()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"detail":"bad prompt"}`)
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	code, stderr := runCLI(t, []string{"--sfx", "thunder"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "Error generating sound effect") {
		t.Errorf("expected sound-effect-specific error, got %q", stderr)
	}
}

func TestRun_SFXPlaysAudioWhenNoOutputFile(t *testing.T) {
	wantAudio := []byte{0xFF, 0xFB, 'P', 'X'}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(wantAudio)
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	captured, restore := stubAudio()
	defer restore()

	code, stderr := runCLI(t, []string{"--sfx", "thunder"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if len(*captured) != 1 || !bytesEqual((*captured)[0], wantAudio) {
		t.Fatalf("expected the sound effect to be played, got %d plays", len(*captured))
	}
}

func TestRun_SFXHonoursTokenFlag(t *testing.T) {
	var key string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("xi-api-key")
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, stderr := runCLI(t, []string{"--sfx", "--token", "flag-key", "-o", out, "thunder"}, "", emptyEnv)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if key != "flag-key" {
		t.Errorf("xi-api-key = %q, want flag-key", key)
	}
}

func TestRun_SFXModelFlagOverridesDefault(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, stderr := runCLI(t, []string{"--sfx", "-m", "eleven_text_to_sound_v1", "-o", out, "thunder"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if raw["model_id"] != "eleven_text_to_sound_v1" {
		t.Errorf("model_id = %v, want eleven_text_to_sound_v1", raw["model_id"])
	}
}

func TestRun_HelpMentionsSoundEffects(t *testing.T) {
	code, stderr := runCLI(t, []string{"--help"}, "", emptyEnv)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, want := range []string{"--sfx", "--duration", "--influence", "--loop", "Sound effects"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("help missing %q", want)
		}
	}
}

// --- Regression tests for the adversarial review findings ---

// The tuning flags must actually reach the wire. Without this, --duration,
// --influence and --loop could all be disconnected and the suite would stay green.
func TestRun_SFXTuningFlagsReachRequestBody(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, stderr := runCLI(t,
		[]string{"--sfx", "-d", "12.5", "--influence", "0.85", "--loop", "-o", out, "rain"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if raw["duration_seconds"] != 12.5 {
		t.Errorf("duration_seconds = %v, want 12.5", raw["duration_seconds"])
	}
	if raw["prompt_influence"] != 0.85 {
		t.Errorf("prompt_influence = %v, want 0.85", raw["prompt_influence"])
	}
	if raw["loop"] != true {
		t.Errorf("loop = %v, want true", raw["loop"])
	}
}

// The long-form --duration spelling must be wired to the same variable as -d.
func TestRun_SFXLongDurationFlagReachesRequestBody(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, _ := runCLI(t, []string{"--sfx", "--duration", "3", "-o", out, "rain"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if raw["duration_seconds"] != float64(3) {
		t.Errorf("duration_seconds = %v, want 3", raw["duration_seconds"])
	}
}

// loop is a v2-only field, so it must not be pinned to false on every request.
func TestSynthesizeSoundEffect_OmitsLoopWhenFalse(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	if _, err := synthesizeSoundEffect("xi", "m", "boom", nil, 0.3, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := raw["loop"]; present {
		t.Errorf("loop should be omitted when false, got %v", raw["loop"])
	}
}

// NaN passes every naive range comparison, so it must be rejected explicitly
// rather than reaching json.Marshal and failing there.
func TestRun_SFXRejectsNaNAndInfTuningValues(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	cases := []struct{ flag, value, wantMsg string }{
		{"-d", "NaN", "Duration must be between"},
		{"-d", "+Inf", "Duration must be between"},
		{"--influence", "NaN", "Influence must be between"},
		{"--influence", "+Inf", "Influence must be between"},
	}
	for _, c := range cases {
		code, stderr := runCLI(t, []string{"--sfx", c.flag, c.value, "thunder"}, "",
			envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
		if code != 1 {
			t.Errorf("%s %s: exit code = %d, want 1", c.flag, c.value, code)
		}
		if !strings.Contains(stderr, c.wantMsg) {
			t.Errorf("%s %s: stderr = %q, want %q", c.flag, c.value, stderr, c.wantMsg)
		}
	}
	if hits != 0 {
		t.Errorf("invalid tuning values reached the API %d times, want 0", hits)
	}
}

// A permanent rejection must fail on the first response instead of burning two
// more API calls and 3 seconds of backoff.
func TestWithRetry_DoesNotRetryPermanentErrors(t *testing.T) {
	defer swapBackoff(time.Millisecond)()

	var attempts int
	_, err := withRetry("thing", func() ([]byte, error) {
		attempts++
		return nil, permanent(errors.New("API error (401): bad key"))
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (permanent errors must not be retried)", attempts)
	}
	if !strings.Contains(err.Error(), "bad key") {
		t.Errorf("underlying error lost: %v", err)
	}
	if strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("permanent failure should not claim 3 attempts: %v", err)
	}
}

func TestAPIError_ClassifiesStatusCodes(t *testing.T) {
	cases := []struct {
		status        int
		wantPermanent bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusNotFound, true},
		{http.StatusUnprocessableEntity, true},
		{http.StatusTooManyRequests, false}, // rate limited: retrying is the point
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
		{http.StatusServiceUnavailable, false},
	}
	for _, c := range cases {
		resp := &http.Response{
			StatusCode: c.status,
			Body:       io.NopCloser(strings.NewReader("boom")),
		}
		err := apiError(resp)
		if got := isPermanent(err); got != c.wantPermanent {
			t.Errorf("status %d: isPermanent = %v, want %v", c.status, got, c.wantPermanent)
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("status %d: body missing from error %v", c.status, err)
		}
	}
}

// A stale API key should cost the user one request, not three.
func TestRun_SFXDoesNotRetryClientErrors(t *testing.T) {
	defer swapBackoff(time.Millisecond)()

	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":"Invalid API key"}`)
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	code, stderr := runCLI(t, []string{"--sfx", "thunder"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "stale"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if !strings.Contains(stderr, "Invalid API key") {
		t.Errorf("expected the API message to be surfaced, got %q", stderr)
	}
}

// 429 means "slow down", which is exactly what the backoff is for.
func TestRun_SFXStillRetriesRateLimits(t *testing.T) {
	defer swapBackoff(time.Millisecond)()

	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	out := t.TempDir() + "/sfx.mp3"
	code, stderr := runCLI(t, []string{"--sfx", "-o", out, "thunder"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

// The speech providers share withRetry, so they inherit fail-fast too.
func TestRun_SpeechDoesNotRetryClientErrors(t *testing.T) {
	defer swapBackoff(time.Millisecond)()

	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad key"}`)
	}))
	defer server.Close()
	defer swapURL(&openAIAPIURL, server.URL)()

	code, _ := runCLI(t, []string{"hello"}, "", envMap(map[string]string{"OPENAI_API_KEY": "stale"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

// A whitespace-only prompt passed as an argument must be caught locally rather
// than spending an API call to be told it is empty.
func TestRun_SFXRejectsWhitespaceOnlyArgumentPrompt(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte{0xFF, 0xFB})
	}))
	defer server.Close()
	defer swapURL(&elevenLabsSFXAPIURL, server.URL)()

	code, stderr := runCLI(t, []string{"--sfx", "   "}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "No sound effect prompt provided") {
		t.Errorf("unexpected stderr %q", stderr)
	}
	if hits != 0 {
		t.Errorf("whitespace prompt reached the API %d times, want 0", hits)
	}
}

// The same trimming must not break the speech path.
func TestRun_RejectsWhitespaceOnlyArgumentText(t *testing.T) {
	code, stderr := runCLI(t, []string{"  "}, "", envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "No text provided") {
		t.Errorf("unexpected stderr %q", stderr)
	}
}

// Contradictory flags should be reported before environment-dependent checks,
// so the user hears about the real mistake first.
func TestRun_SFXAllConflictReportedBeforeMissingKey(t *testing.T) {
	code, stderr := runCLI(t, []string{"--sfx", "--all", "boom"}, "", emptyEnv)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "--all cannot be combined with --sfx") {
		t.Errorf("expected the flag conflict, got %q", stderr)
	}
	if strings.Contains(stderr, "ELEVENLABS_API_KEY") {
		t.Errorf("missing-key error masked the real problem: %q", stderr)
	}
}

func TestRun_SFXAllConflictReportedBeforeEmptyPrompt(t *testing.T) {
	code, stderr := runCLI(t, []string{"--sfx", "--all"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "--all cannot be combined with --sfx") {
		t.Errorf("expected the flag conflict, got %q", stderr)
	}
}

func TestFlagName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"d", "-d"},
		{"v", "-v"},
		{"duration", "--duration"},
		{"stability", "--stability"},
	}
	for _, c := range cases {
		if got := flagName(c.in); got != c.want {
			t.Errorf("flagName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- Config file -----------------------------------------------------------

// writeConfigFile drops a config file into a fresh temp directory and returns
// its path, for the cases that point --config or GOSPEAK_CONFIG at it.
func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), configFileName)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing config file: %v", err)
	}
	return path
}

// useHomeConfig installs a home directory holding the given config file for the
// duration of the test, so the default lookup finds it.
func useHomeConfig(t *testing.T, contents string) string {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, configFileName)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing config file: %v", err)
	}
	original := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = original })
	return path
}

// recordedRequest holds what a stub provider endpoint was sent.
type recordedRequest struct {
	body []byte
	url  string
}

// recordProvider repoints a provider endpoint at a server that records the
// request and replies with a stub MP3, so tests can assert on the settings that
// actually reached the wire.
func recordProvider(t *testing.T, target *string) *recordedRequest {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.body, _ = io.ReadAll(r.Body)
		rec.url = r.URL.String()
		_, _ = w.Write([]byte{0xFF, 0xFB, 0x00, 0x00})
	}))
	t.Cleanup(srv.Close)
	restore := swapURL(target, srv.URL)
	t.Cleanup(restore)
	return rec
}

// decodeOpenAI unpacks a recorded OpenAI request body.
func (r *recordedRequest) decodeOpenAI(t *testing.T) OpenAITTSRequest {
	t.Helper()
	var req OpenAITTSRequest
	if err := json.Unmarshal(r.body, &req); err != nil {
		t.Fatalf("decoding recorded request %q: %v", r.body, err)
	}
	return req
}

// decodeElevenLabs unpacks a recorded ElevenLabs request body.
func (r *recordedRequest) decodeElevenLabs(t *testing.T) ElevenLabsTTSRequest {
	t.Helper()
	var req ElevenLabsTTSRequest
	if err := json.Unmarshal(r.body, &req); err != nil {
		t.Fatalf("decoding recorded request %q: %v", r.body, err)
	}
	return req
}

func TestIsValidProvider(t *testing.T) {
	for _, valid := range []string{"openai", "elevenlabs", "deepgram"} {
		if !isValidProvider(valid) {
			t.Errorf("isValidProvider(%q) = false, want true", valid)
		}
	}
	for _, invalid := range []string{"", "OpenAI", "azure", "openai "} {
		if isValidProvider(invalid) {
			t.Errorf("isValidProvider(%q) = true, want false", invalid)
		}
	}
}

func TestLoadConfig_MissingDefaultFileIsNotAnError(t *testing.T) {
	original := userHomeDir
	userHomeDir = func() (string, error) { return t.TempDir(), nil }
	defer func() { userHomeDir = original }()

	cfg, err := loadConfig("", emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Provider != "" || cfg.Voice != "" || cfg.path != "" {
		t.Fatalf("expected an empty config, got %+v", cfg)
	}
}

func TestLoadConfig_NoHomeDirectoryIsNotAnError(t *testing.T) {
	original := userHomeDir
	userHomeDir = func() (string, error) { return "", errors.New("no home here") }
	defer func() { userHomeDir = original }()

	cfg, err := loadConfig("", emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Provider != "" {
		t.Fatalf("expected an empty config, got %+v", cfg)
	}
}

func TestLoadConfig_ReadsHomeDirectoryFile(t *testing.T) {
	path := useHomeConfig(t, `{"provider": "deepgram", "voice": "thalia"}`)

	cfg, err := loadConfig("", emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Provider != "deepgram" {
		t.Errorf("provider = %q, want deepgram", cfg.Provider)
	}
	if cfg.Voice != "thalia" {
		t.Errorf("voice = %q, want thalia", cfg.Voice)
	}
	if cfg.path != path {
		t.Errorf("path = %q, want %q", cfg.path, path)
	}
}

func TestLoadConfig_FlagPathBeatsEnvAndHome(t *testing.T) {
	useHomeConfig(t, `{"voice": "from-home"}`)
	envPath := writeConfigFile(t, `{"voice": "from-env"}`)
	flagPath := writeConfigFile(t, `{"voice": "from-flag"}`)

	cfg, err := loadConfig(flagPath, envMap(map[string]string{"GOSPEAK_CONFIG": envPath}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Voice != "from-flag" {
		t.Fatalf("voice = %q, want from-flag", cfg.Voice)
	}
}

func TestLoadConfig_EnvPathBeatsHome(t *testing.T) {
	useHomeConfig(t, `{"voice": "from-home"}`)
	envPath := writeConfigFile(t, `{"voice": "from-env"}`)

	cfg, err := loadConfig("", envMap(map[string]string{"GOSPEAK_CONFIG": envPath}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Voice != "from-env" {
		t.Fatalf("voice = %q, want from-env", cfg.Voice)
	}
}

func TestLoadConfig_MissingExplicitFileIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")

	t.Run("flag", func(t *testing.T) {
		if _, err := loadConfig(missing, emptyEnv); err == nil {
			t.Fatal("expected an error for a --config path that does not exist")
		} else if !strings.Contains(err.Error(), missing) {
			t.Fatalf("expected the path in the error, got %v", err)
		}
	})

	t.Run("env", func(t *testing.T) {
		_, err := loadConfig("", envMap(map[string]string{"GOSPEAK_CONFIG": missing}))
		if err == nil {
			t.Fatal("expected an error for a GOSPEAK_CONFIG path that does not exist")
		}
	})
}

func TestLoadConfig_UnreadableFileIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-0 file, so there is nothing to test")
	}
	path := writeConfigFile(t, `{"voice": "nova"}`)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer func() { _ = os.Chmod(path, 0o644) }()

	if _, err := loadConfig(path, emptyEnv); err == nil {
		t.Fatal("expected a read error for an unreadable config file")
	} else if !strings.Contains(err.Error(), "reading config") {
		t.Fatalf("expected a read-stage error, got %v", err)
	}
}

func TestLoadConfig_RejectsMalformedJSON(t *testing.T) {
	path := writeConfigFile(t, `{"provider": "openai",}`)

	_, err := loadConfig(path, emptyEnv)
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "parsing config") {
		t.Fatalf("expected a parse-stage error, got %v", err)
	}
}

func TestLoadConfig_RejectsUnknownField(t *testing.T) {
	// A typo'd key must be reported: a setting that looks applied but is not is
	// the failure mode this guards against.
	path := writeConfigFile(t, `{"provdier": "openai"}`)

	_, err := loadConfig(path, emptyEnv)
	if err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	if !strings.Contains(err.Error(), "provdier") {
		t.Fatalf("expected the offending key in the error, got %v", err)
	}
}

func TestLoadConfig_RejectsInvalidProvider(t *testing.T) {
	path := writeConfigFile(t, `{"provider": "azure"}`)

	_, err := loadConfig(path, emptyEnv)
	if err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
	if !strings.Contains(err.Error(), "azure") || !strings.Contains(err.Error(), path) {
		t.Fatalf("expected provider and path in the error, got %v", err)
	}
}

func TestLoadConfig_RejectsUnknownProviderSection(t *testing.T) {
	path := writeConfigFile(t, `{"providers": {"azure": {"voice": "nova"}}}`)

	_, err := loadConfig(path, emptyEnv)
	if err == nil {
		t.Fatal("expected an error for an unknown providers section")
	}
	if !strings.Contains(err.Error(), "azure") {
		t.Fatalf("expected the section name in the error, got %v", err)
	}
}

func TestLoadConfig_LowercasesProviderNames(t *testing.T) {
	path := writeConfigFile(t, `{"provider": "ElevenLabs", "providers": {"OpenAI": {"voice": "nova"}}}`)

	cfg, err := loadConfig(path, emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Provider != "elevenlabs" {
		t.Errorf("provider = %q, want elevenlabs", cfg.Provider)
	}
	if got := cfg.forProvider("openai").Voice; got != "nova" {
		t.Errorf("openai voice = %q, want nova", got)
	}
}

func TestLoadConfig_ReadsEveryField(t *testing.T) {
	path := writeConfigFile(t, `{
		"provider": "elevenlabs",
		"voice": "josh",
		"model": "eleven_turbo_v2_5",
		"speed": 1.1,
		"providers": {
			"openai": {"voice": "nova", "model": "tts-1", "speed": 2.0}
		}
	}`)

	cfg, err := loadConfig(path, emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "eleven_turbo_v2_5" {
		t.Errorf("model = %q, want eleven_turbo_v2_5", cfg.Model)
	}
	if cfg.Speed == nil || *cfg.Speed != 1.1 {
		t.Errorf("speed = %v, want 1.1", cfg.Speed)
	}
	openai := cfg.forProvider("openai")
	if openai.Voice != "nova" || openai.Model != "tts-1" {
		t.Errorf("openai section = %+v, want nova/tts-1", openai)
	}
	if openai.Speed == nil || *openai.Speed != 2.0 {
		t.Errorf("openai speed = %v, want 2.0", openai.Speed)
	}
}

func TestConfig_ForProviderOnEmptyConfigIsZeroValue(t *testing.T) {
	cfg := &config{}
	if got := cfg.forProvider("openai"); got != (providerConfig{}) {
		t.Fatalf("forProvider on an empty config = %+v, want zero value", got)
	}
}

func TestConfigOrigin(t *testing.T) {
	cfg := &config{path: "/home/me/.gospeak.json"}
	if got := configOrigin(cfg, true); got != " (from /home/me/.gospeak.json)" {
		t.Errorf("configOrigin = %q", got)
	}
	if got := configOrigin(cfg, false); got != "" {
		t.Errorf("configOrigin for a non-config value = %q, want empty", got)
	}
	if got := configOrigin(&config{}, true); got != "" {
		t.Errorf("configOrigin with no path = %q, want empty", got)
	}
}

func TestRun_ConfigProvidesDefaultProvider(t *testing.T) {
	useHomeConfig(t, `{"provider": "elevenlabs"}`)
	rec := recordProvider(t, &elevenLabsAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-o", out, "hello"}, "", envMap(map[string]string{
		"ELEVENLABS_API_KEY": "k",
	}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if rec.body == nil {
		t.Fatal("expected the ElevenLabs endpoint to be called")
	}
}

func TestRun_ProviderFlagBeatsConfig(t *testing.T) {
	useHomeConfig(t, `{"provider": "elevenlabs", "voice": "josh"}`)
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-p", "openai", "-v", "nova", "-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if got := rec.decodeOpenAI(t).Voice; got != "nova" {
		t.Fatalf("voice = %q, want nova", got)
	}
}

func TestRun_ConfigProvidesDefaultVoice(t *testing.T) {
	useHomeConfig(t, `{"voice": "shimmer"}`)
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if got := rec.decodeOpenAI(t).Voice; got != "shimmer" {
		t.Fatalf("voice = %q, want shimmer", got)
	}
}

func TestRun_VoiceFlagBeatsConfig(t *testing.T) {
	useHomeConfig(t, `{"voice": "shimmer"}`)
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-v", "echo", "-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if got := rec.decodeOpenAI(t).Voice; got != "echo" {
		t.Fatalf("voice = %q, want echo", got)
	}
}

func TestRun_ProviderSectionVoiceBeatsFileWideVoice(t *testing.T) {
	// The point of the per-provider section: a file-wide voice that belongs to
	// another provider must not leak into this one.
	useHomeConfig(t, `{"voice": "josh", "providers": {"openai": {"voice": "onyx"}}}`)
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if got := rec.decodeOpenAI(t).Voice; got != "onyx" {
		t.Fatalf("voice = %q, want onyx", got)
	}
}

func TestRun_ConfigVoiceResolvesElevenLabsPreset(t *testing.T) {
	useHomeConfig(t, `{"provider": "elevenlabs", "voice": "josh"}`)
	rec := recordProvider(t, &elevenLabsAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-o", out, "hello"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if want := elevenLabsVoices["josh"]; !strings.Contains(rec.url, want) {
		t.Fatalf("request URL = %q, want it to carry voice_id %q", rec.url, want)
	}
}

func TestRun_ConfigVoiceResolvesDeepgramPreset(t *testing.T) {
	useHomeConfig(t, `{"provider": "deepgram", "providers": {"deepgram": {"voice": "thalia"}}}`)
	rec := recordProvider(t, &deepgramAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-o", out, "hello"}, "",
		envMap(map[string]string{"DEEPGRAM_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if !strings.Contains(rec.url, "aura-2-thalia-en") {
		t.Fatalf("request URL = %q, want it to carry aura-2-thalia-en", rec.url)
	}
}

func TestRun_ConfigProvidesDefaultModel(t *testing.T) {
	useHomeConfig(t, `{"model": "tts-1", "providers": {"elevenlabs": {"model": "eleven_turbo_v2_5"}}}`)

	t.Run("file-wide", func(t *testing.T) {
		rec := recordProvider(t, &openAIAPIURL)
		out := filepath.Join(t.TempDir(), "out.mp3")
		code, stderr := runCLI(t, []string{"-o", out, "hello"}, "",
			envMap(map[string]string{"OPENAI_API_KEY": "k"}))
		if code != 0 {
			t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
		}
		if got := rec.decodeOpenAI(t).Model; got != "tts-1" {
			t.Fatalf("model = %q, want tts-1", got)
		}
	})

	t.Run("provider section", func(t *testing.T) {
		rec := recordProvider(t, &elevenLabsAPIURL)
		out := filepath.Join(t.TempDir(), "out.mp3")
		code, stderr := runCLI(t, []string{"-p", "elevenlabs", "-o", out, "hello"}, "",
			envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
		if code != 0 {
			t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
		}
		if got := rec.decodeElevenLabs(t).ModelID; got != "eleven_turbo_v2_5" {
			t.Fatalf("model = %q, want eleven_turbo_v2_5", got)
		}
	})

	t.Run("flag wins", func(t *testing.T) {
		rec := recordProvider(t, &openAIAPIURL)
		out := filepath.Join(t.TempDir(), "out.mp3")
		code, stderr := runCLI(t, []string{"-m", "tts-1-hd", "-o", out, "hello"}, "",
			envMap(map[string]string{"OPENAI_API_KEY": "k"}))
		if code != 0 {
			t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
		}
		if got := rec.decodeOpenAI(t).Model; got != "tts-1-hd" {
			t.Fatalf("model = %q, want tts-1-hd", got)
		}
	})
}

func TestRun_ConfigProvidesDefaultSpeed(t *testing.T) {
	useHomeConfig(t, `{"speed": 1.75}`)
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if got := rec.decodeOpenAI(t).Speed; got != 1.75 {
		t.Fatalf("speed = %v, want 1.75", got)
	}
}

func TestRun_SpeedFlagBeatsConfig(t *testing.T) {
	useHomeConfig(t, `{"speed": 1.75}`)
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-x", "0.5", "-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if got := rec.decodeOpenAI(t).Speed; got != 0.5 {
		t.Fatalf("speed = %v, want 0.5", got)
	}
}

func TestRun_ConfigSpeedOutOfRangeIsRejected(t *testing.T) {
	useHomeConfig(t, `{"provider": "elevenlabs", "speed": 3.0}`)

	code, stderr := runCLI(t, []string{"hello"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "0.7 and 1.2") {
		t.Fatalf("expected the speed-range error, got %q", stderr)
	}
}

func TestRun_DeepgramStaysQuietAboutFileWideSpeed(t *testing.T) {
	// A file-wide speed aimed at the user's usual provider is not a Deepgram
	// mistake, so warning about it on every Deepgram run would be noise.
	useHomeConfig(t, `{"speed": 1.5}`)
	recordProvider(t, &deepgramAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-p", "deepgram", "-o", out, "hello"}, "",
		envMap(map[string]string{"DEEPGRAM_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if strings.Contains(stderr, "Speed adjustment is not supported") {
		t.Fatalf("did not expect a speed warning, got %q", stderr)
	}
}

func TestRun_DeepgramWarnsAboutSpeedInItsOwnSection(t *testing.T) {
	useHomeConfig(t, `{"providers": {"deepgram": {"speed": 1.5}}}`)
	recordProvider(t, &deepgramAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-p", "deepgram", "-o", out, "hello"}, "",
		envMap(map[string]string{"DEEPGRAM_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "Speed adjustment is not supported") {
		t.Fatalf("expected a speed warning, got %q", stderr)
	}
}

func TestRun_InvalidConfigReturnsOne(t *testing.T) {
	path := useHomeConfig(t, `{"provider": "azure"}`)

	code, stderr := runCLI(t, []string{"hello"}, "", emptyEnv)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, path) {
		t.Fatalf("expected the config path in the error, got %q", stderr)
	}
}

func TestRun_InvalidConfigVoiceNamesTheFile(t *testing.T) {
	// An OpenAI voice that came out of the config file should point the user at
	// the file rather than looking like a mistyped flag.
	path := useHomeConfig(t, `{"voice": "josh"}`)

	code, stderr := runCLI(t, []string{"hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "Invalid OpenAI voice 'josh'") {
		t.Fatalf("expected the invalid-voice error, got %q", stderr)
	}
	if !strings.Contains(stderr, "(from "+path+")") {
		t.Fatalf("expected the config path in the error, got %q", stderr)
	}
}

func TestRun_InvalidFlagVoiceDoesNotBlameTheConfig(t *testing.T) {
	path := useHomeConfig(t, `{"voice": "nova"}`)

	code, stderr := runCLI(t, []string{"-v", "josh", "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if strings.Contains(stderr, path) {
		t.Fatalf("a voice typed on the command line should not blame the config, got %q", stderr)
	}
}

func TestRun_NoConfigIgnoresTheFile(t *testing.T) {
	useHomeConfig(t, `{"provider": "elevenlabs", "voice": "josh"}`)
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"--no-config", "-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k", "ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if got := rec.decodeOpenAI(t).Voice; got != defaultOpenAIVoice {
		t.Fatalf("voice = %q, want the built-in default %q", got, defaultOpenAIVoice)
	}
}

func TestRun_NoConfigSkipsAnUnreadableFile(t *testing.T) {
	// --no-config has to short-circuit the load, not just discard the result.
	useHomeConfig(t, `{"provider": "azure"}`)
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"--no-config", "-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if rec.body == nil {
		t.Fatal("expected the OpenAI endpoint to be called")
	}
}

func TestRun_NoConfigWarnsWhenConfigPathAlsoGiven(t *testing.T) {
	path := writeConfigFile(t, `{"voice": "nova"}`)
	recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"--no-config", "--config", path, "-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "--config is ignored when --no-config is set") {
		t.Fatalf("expected the contradiction warning, got %q", stderr)
	}
}

func TestRun_ConfigFlagPointsAtAnotherFile(t *testing.T) {
	useHomeConfig(t, `{"voice": "shimmer"}`)
	path := writeConfigFile(t, `{"voice": "fable"}`)
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"--config", path, "-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if got := rec.decodeOpenAI(t).Voice; got != "fable" {
		t.Fatalf("voice = %q, want fable", got)
	}
}

func TestRun_ConfigPathFromEnvironment(t *testing.T) {
	path := writeConfigFile(t, `{"voice": "onyx"}`)
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-o", out, "hello"}, "", envMap(map[string]string{
		"OPENAI_API_KEY": "k",
		"GOSPEAK_CONFIG": path,
	}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if got := rec.decodeOpenAI(t).Voice; got != "onyx" {
		t.Fatalf("voice = %q, want onyx", got)
	}
}

func TestRun_SFXIgnoresConfigSpeechSettings(t *testing.T) {
	// Voice, model and speed describe speech. --sfx already ignores the flags
	// that carry them, so the config file's versions have to be ignored too —
	// a speech model would be rejected by the sound-generation endpoint.
	useHomeConfig(t, `{"provider": "openai", "voice": "nova", "model": "tts-1-hd", "speed": 2.0}`)
	rec := recordProvider(t, &elevenLabsSFXAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"--sfx", "-o", out, "a bell"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	var req ElevenLabsSFXRequest
	if err := json.Unmarshal(rec.body, &req); err != nil {
		t.Fatalf("decoding recorded request %q: %v", rec.body, err)
	}
	if req.ModelID != defaultSFXModel {
		t.Fatalf("model = %q, want %q", req.ModelID, defaultSFXModel)
	}
}

func TestRun_SFXOverridesConfigProviderWithoutComplaining(t *testing.T) {
	// A provider from the config file is a standing default, not a per-run
	// request, so --sfx switches to ElevenLabs silently. Only an explicit -p
	// for another provider is an error.
	useHomeConfig(t, `{"provider": "openai"}`)
	recordProvider(t, &elevenLabsSFXAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"--sfx", "-o", out, "a bell"}, "",
		envMap(map[string]string{"ELEVENLABS_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if strings.Contains(stderr, "only supported by the elevenlabs provider") {
		t.Fatalf("did not expect a provider complaint, got %q", stderr)
	}
}

func TestRun_HelpDocumentsTheConfigFile(t *testing.T) {
	code, stderr := runCLI(t, []string{"--help"}, "", emptyEnv)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, want := range []string{"--config", "--no-config", configFileName, "GOSPEAK_CONFIG"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("expected %q in the help output", want)
		}
	}
}

// --- OpenAI voice roster and model selection --------------------------------

func TestOpenAIVoices_CoverTheFullRoster(t *testing.T) {
	// The roster OpenAI publishes for /v1/audio/speech. Pinned here so a voice
	// going missing from the list is a test failure rather than a support
	// question.
	want := []string{
		"alloy", "ash", "ballad", "coral", "echo", "fable", "nova",
		"onyx", "sage", "shimmer", "verse", "marin", "cedar",
	}
	if len(openAIVoices) != len(want) {
		t.Fatalf("roster has %d voices, want %d: %v", len(openAIVoices), len(want), openAIVoices)
	}
	for _, v := range want {
		if !isValidOpenAIVoice(v) {
			t.Errorf("voice %q missing from the roster", v)
		}
	}
}

func TestSpeaksEveryOpenAIVoice(t *testing.T) {
	for _, legacy := range []string{"tts-1", "tts-1-hd"} {
		if speaksEveryOpenAIVoice(legacy) {
			t.Errorf("speaksEveryOpenAIVoice(%q) = true, want false", legacy)
		}
	}
	// A model this binary has never heard of is left alone rather than
	// second-guessed, so a future release needs no code change here.
	for _, capable := range []string{"gpt-4o-mini-tts", "some-future-tts"} {
		if !speaksEveryOpenAIVoice(capable) {
			t.Errorf("speaksEveryOpenAIVoice(%q) = false, want true", capable)
		}
	}
}

func TestOpenAIModelForVoice(t *testing.T) {
	cases := []struct {
		model, voice, want string
	}{
		// The four voices tts-1 and tts-1-hd reject.
		{"tts-1-hd", "marin", openAIAllVoiceModel},
		{"tts-1-hd", "cedar", openAIAllVoiceModel},
		{"tts-1-hd", "ballad", openAIAllVoiceModel},
		{"tts-1", "verse", openAIAllVoiceModel},
		// Voices the tts-1 family handles are left on the model as asked.
		{"tts-1-hd", "alloy", "tts-1-hd"},
		{"tts-1-hd", "sage", "tts-1-hd"},
		{"tts-1", "coral", "tts-1"},
		// A model that already speaks everything is never rewritten.
		{openAIAllVoiceModel, "marin", openAIAllVoiceModel},
		{openAIAllVoiceModel, "alloy", openAIAllVoiceModel},
	}
	for _, c := range cases {
		if got := openAIModelForVoice(c.model, c.voice); got != c.want {
			t.Errorf("openAIModelForVoice(%q, %q) = %q, want %q", c.model, c.voice, got, c.want)
		}
	}
}

func TestRun_UpgradesModelForANewerVoice(t *testing.T) {
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-v", "marin", "-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	req := rec.decodeOpenAI(t)
	if req.Model != openAIAllVoiceModel {
		t.Fatalf("model = %q, want %q", req.Model, openAIAllVoiceModel)
	}
	if req.Voice != "marin" {
		t.Fatalf("voice = %q, want marin", req.Voice)
	}
}

func TestRun_ConfigModelYieldsToTheVoiceItCannotSpeak(t *testing.T) {
	// A model from the config file is a standing default, and defaults yield —
	// the same way a config provider yields to --sfx.
	useHomeConfig(t, `{"model": "tts-1-hd"}`)
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-v", "cedar", "-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if got := rec.decodeOpenAI(t).Model; got != openAIAllVoiceModel {
		t.Fatalf("model = %q, want %q", got, openAIAllVoiceModel)
	}
}

func TestRun_ExplicitModelThatCannotSpeakTheVoiceIsAnError(t *testing.T) {
	// A model typed on the command line is a real instruction, so the
	// contradiction is reported rather than quietly overridden.
	code, stderr := runCLI(t, []string{"-v", "marin", "-m", "tts-1-hd", "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "marin") || !strings.Contains(stderr, openAIAllVoiceModel) {
		t.Fatalf("expected the voice and the model it needs in the error, got %q", stderr)
	}
}

func TestRun_ExplicitCapableModelIsLeftAlone(t *testing.T) {
	rec := recordProvider(t, &openAIAPIURL)

	out := filepath.Join(t.TempDir(), "out.mp3")
	code, stderr := runCLI(t, []string{"-v", "sage", "-m", "tts-1", "-o", out, "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if got := rec.decodeOpenAI(t).Model; got != "tts-1" {
		t.Fatalf("model = %q, want tts-1", got)
	}
}

func TestRun_AllFlagSwitchesModelPerVoice(t *testing.T) {
	// --all demos the whole roster, so it switches models for the four voices
	// tts-1-hd cannot speak even though -m named it. Skipping a third of the
	// roster would defeat the point.
	var byVoice sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req OpenAITTSRequest
		if err := json.Unmarshal(body, &req); err == nil {
			byVoice.Store(req.Voice, req.Model)
		}
		_, _ = w.Write([]byte{0xFF, 0xFB, 0x00, 0x00})
	}))
	defer srv.Close()
	defer swapURL(&openAIAPIURL, srv.URL)()
	defer stubAudioReturning(nil)()

	originalSleep := allVoiceSleep
	allVoiceSleep = func(time.Duration) {}
	defer func() { allVoiceSleep = originalSleep }()

	code, stderr := runCLI(t, []string{"--all", "-m", "tts-1-hd", "hello"}, "",
		envMap(map[string]string{"OPENAI_API_KEY": "k"}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	for _, v := range openAIVoices {
		got, ok := byVoice.Load(v)
		if !ok {
			t.Errorf("voice %q was never requested", v)
			continue
		}
		want := "tts-1-hd"
		if openAIAllVoiceModelOnly[v] {
			want = openAIAllVoiceModel
		}
		if got != want {
			t.Errorf("voice %q used model %v, want %q", v, got, want)
		}
	}
}

func TestRun_HelpListsTheNewerVoices(t *testing.T) {
	code, stderr := runCLI(t, []string{"--help"}, "", emptyEnv)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, want := range []string{"marin", "cedar", "ballad", "verse", openAIAllVoiceModel} {
		if !strings.Contains(stderr, want) {
			t.Errorf("expected %q in the help output", want)
		}
	}
}

func TestRun_ArgumentMistakesReportBeforeTheMissingKey(t *testing.T) {
	// A voice the provider does not have, or a model that cannot speak it, is a
	// mistake in the arguments. Reporting the missing API key first would send
	// the user off fixing the wrong thing.
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown voice", []string{"-v", "not-a-voice", "hello"}, "Invalid OpenAI voice"},
		{"model cannot speak the voice", []string{"-v", "marin", "-m", "tts-1-hd", "hello"}, "gpt-4o-mini-tts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stderr := runCLI(t, c.args, "", emptyEnv)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if !strings.Contains(stderr, c.want) {
				t.Fatalf("expected %q in stderr, got %q", c.want, stderr)
			}
			if strings.Contains(stderr, "OPENAI_API_KEY") {
				t.Fatalf("the missing key should not upstage the real mistake, got %q", stderr)
			}
		})
	}
}
