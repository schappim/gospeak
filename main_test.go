package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
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
