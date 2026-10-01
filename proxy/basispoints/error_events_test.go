package basispoints

import (
	"io"
	"strings"
	"testing"
	"time"
)

func newStreamBridge() *Bridge {
	return &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
}

const createdEvent = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\",\"output\":[]}}\n\n"

// Codex ignores an `error` event, sees a stream that never finished and
// retries it; the bridge must end such a stream with response.failed.
func TestStreamTurnsErrorEventIntoOneResponseFailed(t *testing.T) {
	errorEvent := "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"server_error\",\"message\":\"secret detail\"}}\n\n"
	failed := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r1\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"secret detail\"}}}\n\n"
	cases := map[string]string{
		"followed by response.failed": createdEvent + errorEvent + failed,
		"then the stream ends":        createdEvent + errorEvent,
	}
	for name, stream := range cases {
		out := readStream(t, newStreamBridge(), io.NopCloser(strings.NewReader(stream)))
		if strings.Contains(out, "event: error") || strings.Count(out, `"type":"response.failed"`) != 1 {
			t.Fatalf("%s: want exactly one response.failed and no error event: %s", name, out)
		}
		if !strings.Contains(out, `"id":"r1"`) || !strings.Contains(out, "basispoints_upstream_error") || strings.Contains(out, "secret detail") {
			t.Fatalf("%s: response.failed lost the response or leaked provider text: %s", name, out)
		}
	}
}

func TestStreamEndsResponseWhenUpstreamStaysOpenAfterErrorEvent(t *testing.T) {
	previous := errorEventGrace
	errorEventGrace = 20 * time.Millisecond
	t.Cleanup(func() { errorEventGrace = previous })
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	go func() {
		_, _ = io.WriteString(writer, createdEvent+"event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"server_error\"}}\n\n")
	}()
	done := make(chan string, 1)
	go func() { done <- readStream(t, newStreamBridge(), reader) }()
	select {
	case out := <-done:
		if !strings.Contains(out, `"type":"response.failed"`) {
			t.Fatalf("held error was not sent: %s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream hung after an error event with the upstream still open")
	}
}

func TestStreamRelaysCodesCodexActsOnWithGatewayMessages(t *testing.T) {
	cases := []struct {
		name, stream, code string
	}{
		{"error event", createdEvent + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"context_length_exceeded\",\"message\":\"Your input of 921375 tokens about project-x exceeds\"}}\n\n", "context_length_exceeded"},
		{"response.failed", createdEvent + "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"invalid_prompt\",\"message\":\"flagged: echo of project-x\"}}}\n\n", "invalid_prompt"},
	}
	for _, tc := range cases {
		bridge := newStreamBridge()
		out := readStream(t, bridge, io.NopCloser(strings.NewReader(tc.stream)))
		message, _ := ClientError(tc.code)
		if !strings.Contains(out, `"code":"`+tc.code+`"`) || !strings.Contains(out, message) || strings.Contains(out, "project-x") {
			t.Fatalf("%s: code not relayed with the gateway message: %s", tc.name, out)
		}
		if got := bridge.UpstreamFailure().ClientCode; got != tc.code {
			t.Fatalf("%s: ClientCode = %q", tc.name, got)
		}
	}
	// Account-level codes stay generic so the client retries, possibly on
	// another account, instead of giving up the turn.
	for _, code := range []string{"insufficient_quota", "usage_not_included", "server_is_overloaded"} {
		out := readStream(t, newStreamBridge(), io.NopCloser(strings.NewReader(createdEvent+
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\""+code+"\"}}}\n\n")))
		if strings.Contains(out, code) || !strings.Contains(out, "basispoints_upstream_error") {
			t.Fatalf("%s was relayed: %s", code, out)
		}
	}
}

func TestSlowDownIsWaitedOutLikeARateLimit(t *testing.T) {
	bridge := newStreamBridge()
	out := readStream(t, bridge, io.NopCloser(strings.NewReader(createdEvent+
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"slow_down\",\"message\":\"Please try again in 2s.\"}}\n\n")))
	if !strings.Contains(out, `"code":"rate_limit_exceeded"`) {
		t.Fatalf("slow_down was not reported as a rate limit: %s", out)
	}
	if got := bridge.UpstreamFailure(); got.RateLimit == "" || got.RetryAfter != 2*time.Second {
		t.Fatalf("UpstreamFailure = %+v", got)
	}
}
