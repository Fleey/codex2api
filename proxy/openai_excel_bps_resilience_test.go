package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/basispoints"
	"github.com/gin-gonic/gin"
)

// stubExcelBPSClock replaces the BPS clock and sleep with a fake clock that
// sleeping advances, and records the waits.
func stubExcelBPSClock(t *testing.T) *[]time.Duration {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	previousNow, previousSleep := excelBPSNow, excelBPSSleep
	t.Cleanup(func() { excelBPSNow, excelBPSSleep = previousNow, previousSleep })
	var waits []time.Duration
	excelBPSNow = func() time.Time { return now }
	excelBPSSleep = func(_ context.Context, wait time.Duration) error {
		waits = append(waits, wait)
		now = now.Add(wait)
		return nil
	}
	return &waits
}

// excelBPSReplies serves the given upstream replies in order, repeating the
// last, and records the Responses bodies sent.
func excelBPSReplies(t *testing.T, replies ...*http.Response) *[]string {
	t.Helper()
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	var bodies []string
	excelBPSDo = func(req *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(body))
		reply := replies[min(len(bodies), len(replies))-1]
		copied := *reply
		copied.Body = io.NopCloser(strings.NewReader(reply.Header.Get("X-Test-Body")))
		return &copied, nil
	}
	return &bodies
}

func excelBPSReply(status int, body string, header ...string) *http.Response {
	h := make(http.Header)
	h.Set("X-Test-Body", body)
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	return &http.Response{StatusCode: status, Header: h}
}

const (
	excelBPSOpening = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
		"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp-1\",\"status\":\"in_progress\",\"output\":[]}}\n\n"
	excelBPSAnswered = excelBPSOpening + "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[]}}\n\n"
)

func runExcelBPSStream(t *testing.T, raw string) (excelBPSResult, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
	result, err := forwardExcelBPS(ctx, ctx, testExcelBPSAccount(), []byte(raw), "account:91/"+t.Name(), "thread:1", "", false, true, false)
	if err != nil {
		t.Fatalf("forwardExcelBPS: %v", err)
	}
	return result, recorder.Body.String()
}

// checkExcelBPSSequence fails unless the stream's sequence numbers rise strictly.
func checkExcelBPSSequence(t *testing.T, body string) {
	t.Helper()
	last := -1
	for _, match := range regexp.MustCompile(`"sequence_number":(\d+)`).FindAllStringSubmatch(body, -1) {
		n, _ := strconv.Atoi(match[1])
		if n <= last {
			t.Fatalf("sequence_number %d follows %d: %s", n, last, body)
		}
		last = n
	}
}

func TestForwardExcelBPSKeepsWaitingStreamAlive(t *testing.T) {
	t.Setenv("LOG_DISABLED", "true")
	limitedStream := excelBPSOpening + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"tokens\",\"code\":\"rate_limit_exceeded\",\"headers\":{\"retry-after-ms\":\"25000\"},\"message\":\"org-synthetic is over its TPM\"}}\n\n"
	for _, tc := range []struct {
		name    string
		limited *http.Response
		created string
	}{
		// Nothing opened the response yet, so the gateway opens it itself.
		{"HTTP 429", excelBPSReply(http.StatusTooManyRequests, `{"error":{"code":"rate_limit_exceeded"}}`, "Retry-After", "25"), `"model":"gpt-5.5"`},
		// The held opening of the limited attempt is released instead.
		{"stream error", excelBPSReply(http.StatusOK, limitedStream), `"id":"resp-1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := stubExcelBPSClock(t)
			bodies := excelBPSReplies(t, tc.limited, excelBPSReply(http.StatusOK, excelBPSAnswered))
			result, body := runExcelBPSStream(t, `{"model":"gpt-5.5","input":"hello","stream":true}`)
			if len(*bodies) != 2 || result.Terminal != "response.completed" || result.RateLimitWaited != 25*time.Second {
				t.Fatalf("sends=%d terminal=%q waited=%s body=%s", len(*bodies), result.Terminal, result.RateLimitWaited, body)
			}
			if fmt.Sprint(*waits) != "[10s 10s 5s]" {
				t.Fatalf("waits = %v, want keepalives every 10s", *waits)
			}
			if strings.Count(body, `"type":"response.created"`) != 1 || strings.Count(body, `"type":"response.in_progress"`) < 2 {
				t.Fatalf("client should see one response.created and keepalives: %s", body)
			}
			var created string
			for _, line := range strings.Split(body, "\n") {
				if strings.Contains(line, `"type":"response.created"`) {
					created = line
				}
			}
			if !strings.Contains(created, tc.created) {
				t.Fatalf("opening frame = %s", created)
			}
			if !strings.Contains(body, "hello") || strings.Contains(body, "rate_limit") || strings.Contains(body, "org-synthetic") {
				t.Fatalf("answer after the wait = %s", body)
			}
			checkExcelBPSSequence(t, body)
		})
	}
}

func TestHandleExcelBPSGivesUpAfterWaiting(t *testing.T) {
	t.Setenv("LOG_DISABLED", "true")
	t.Setenv(excelBPSRateLimitWaitEnv, "15")
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			waits := stubExcelBPSClock(t)
			bodies := excelBPSReplies(t, excelBPSReply(http.StatusTooManyRequests, `{"error":{"code":"rate_limit_exceeded","message":"org-synthetic used 40000000"}}`))
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			raw := fmt.Sprintf(`{"model":"gpt-5.5","input":"hello","stream":%t}`, stream)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
			var h *Handler
			h.handleExcelBPS(ctx, testExcelBPSAccount(), []byte(raw), "account:91/"+t.Name(), "thread:1", "", false, stream, false, "/v1/responses", "gpt-5.5", "gpt-5.5", "", "", auth.SessionAffinityGuard{}, time.Now())
			body := recorder.Body.String()
			if strings.Contains(body, "org-synthetic") || !strings.Contains(body, "waited 15s") || len(*bodies) != 5 {
				t.Fatalf("sends=%d body=%s", len(*bodies), body)
			}
			if stream {
				// The 8s step is split by the keepalive that opens the stream at 10s.
				if fmt.Sprint(*waits) != "[1s 2s 4s 3s 5s]" || strings.Count(body, `"type":"response.created"`) != 1 ||
					!strings.Contains(body, `"type":"response.failed"`) || !strings.Contains(body, `"code":"invalid_prompt"`) {
					t.Fatalf("waits=%v body=%s", *waits, body)
				}
				checkExcelBPSSequence(t, body)
				return
			}
			if fmt.Sprint(*waits) != "[1s 2s 4s 8s]" || recorder.Code != http.StatusTooManyRequests || !strings.Contains(body, `"code":"rate_limit_exceeded"`) {
				t.Fatalf("waits=%v status=%d body=%s", *waits, recorder.Code, body)
			}
		})
	}
}

func TestExcelBPSRateLimitBudgetReadsEnvironment(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", 5 * time.Minute},
		{"0", 0},
		{"90", 90 * time.Second},
		{"1.5", 1500 * time.Millisecond},
		{"99999", 30 * time.Minute},
		{"-3", 5 * time.Minute},
		{"soon", 5 * time.Minute},
		{"NaN", 5 * time.Minute},
	} {
		t.Setenv(excelBPSRateLimitWaitEnv, tc.env)
		if got := excelBPSRateLimitBudget(); got != tc.want {
			t.Fatalf("%q: budget = %s, want %s", tc.env, got, tc.want)
		}
	}
}

const excelBPSForeignReasoning = `{"model":"gpt-5.5","stream":true,"input":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAAA-foreign-ciphertext"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

func TestExcelBPSRetriesWithoutRejectedEncryptedReasoning(t *testing.T) {
	t.Setenv("LOG_DISABLED", "true")
	refused := excelBPSReply(http.StatusBadRequest, `{"error":{"code":"invalid_encrypted_content","message":"The encrypted content for item rs_1 could not be verified."}}`)
	bodies := excelBPSReplies(t, refused, excelBPSReply(http.StatusOK, excelBPSAnswered))
	scope := "account:91/" + t.Name()
	send := func(scope string) {
		t.Helper()
		upstream, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), []byte(excelBPSForeignReasoning), scope, "thread:1", "", false)
		if err != nil {
			t.Fatalf("ExecuteExcelBPSRequest: %v", err)
		}
		upstream.Response.Body.Close()
	}
	send(scope)
	if len(*bodies) != 2 || !strings.Contains((*bodies)[0], "foreign-ciphertext") || strings.Contains((*bodies)[1], "foreign-ciphertext") || !strings.Contains((*bodies)[1], `"text":"hi"`) {
		t.Fatalf("refused reasoning was not left out on retry: %q", *bodies)
	}
	// The next turn of the same conversation leaves it out from the start.
	send(scope)
	if len(*bodies) != 3 || strings.Contains((*bodies)[2], "foreign-ciphertext") {
		t.Fatalf("rejection was not remembered for the conversation: %q", (*bodies)[2:])
	}
	// Another conversation keeps its own reasoning.
	send(scope + "/other")
	if len(*bodies) != 4 || !strings.Contains((*bodies)[3], "foreign-ciphertext") {
		t.Fatalf("rejection leaked to another conversation: %q", (*bodies)[3:])
	}
}

func TestForwardExcelBPSRetriesStreamRefusalOfEncryptedReasoning(t *testing.T) {
	t.Setenv("LOG_DISABLED", "true")
	refused := excelBPSOpening + "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-1\",\"status\":\"failed\",\"output\":[],\"error\":{\"code\":\"invalid_encrypted_content\",\"message\":\"The encrypted content could not be decrypted.\"}}}\n\n"
	bodies := excelBPSReplies(t, excelBPSReply(http.StatusOK, refused), excelBPSReply(http.StatusOK, excelBPSAnswered))
	result, body := runExcelBPSStream(t, excelBPSForeignReasoning)
	if len(*bodies) != 2 || result.Terminal != "response.completed" || strings.Contains((*bodies)[1], "foreign-ciphertext") {
		t.Fatalf("sends=%d terminal=%q bodies=%q", len(*bodies), result.Terminal, *bodies)
	}
	if strings.Count(body, `"type":"response.created"`) != 1 || strings.Contains(body, "invalid_encrypted_content") || !strings.Contains(body, "hello") {
		t.Fatalf("client body = %s", body)
	}

	// Without encrypted reasoning a retry would change nothing, so the
	// failure goes to the client.
	bodies = excelBPSReplies(t, excelBPSReply(http.StatusOK, refused))
	result, _ = runExcelBPSStream(t, `{"model":"gpt-5.5","input":"hi","stream":true}`)
	if len(*bodies) != 1 || result.Terminal != "response.failed" {
		t.Fatalf("sends=%d terminal=%q", len(*bodies), result.Terminal)
	}
}

func TestExcelBPSRemembersRefusedInlineToolImages(t *testing.T) {
	previous := excelBPSToolImagesRefusedAt.Load()
	excelBPSToolImagesRefusedAt.Store(0)
	t.Cleanup(func() { excelBPSToolImagesRefusedAt.Store(previous) })
	stub := &excelBPSImageStub{t: t, refuse: 1, uploadID: func(n int) string { return fmt.Sprintf("file-tool-%d", n) }}
	useExcelBPSImageStub(t, stub)
	raw := []byte(`{"model":"gpt-5.5","input":[{"type":"function_call","call_id":"call_view","name":"view_image","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_view","output":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgoAAAABdG9vbA=="}]}]}`)
	send := func() {
		t.Helper()
		upstream, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), raw, "account:91/"+t.Name(), "thread:1", "", false)
		if err != nil {
			t.Fatalf("ExecuteExcelBPSRequest: %v", err)
		}
		upstream.Response.Body.Close()
	}
	send()
	if len(stub.bodies) != 2 || !strings.Contains(stub.bodies[0], "data:image/") || !strings.Contains(stub.bodies[1], `"file_id":"file-tool-1"`) {
		t.Fatalf("inline tool image was not uploaded after the refusal: %q", stub.bodies)
	}
	stub.bodies = nil
	send()
	if len(stub.bodies) != 1 || strings.Contains(stub.bodies[0], "data:image/") {
		t.Fatalf("refusal was not remembered; the next request sent the image inline again: %q", stub.bodies)
	}

	// After the refusal ages out, the inline form is tried again.
	excelBPSToolImagesRefusedAt.Store(time.Now().Add(-excelBPSToolImagesRefusalTTL - time.Minute).UnixNano())
	if excelBPSInitialImageMode() != basispoints.ImagesDefault {
		t.Fatal("stale refusal still forces uploads")
	}
}

func TestExcelBPSSendsAccountUserIDAndBrowserUserAgent(t *testing.T) {
	segment := func(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }
	token := segment(`{"alg":"none"}`) + "." +
		segment(`{"https://api.openai.com/auth":{"chatgpt_account_id":"chatgpt-account","chatgpt_account_user_id":"user-synthetic__chatgpt-account"}}`) + ".sig"
	for _, tc := range []struct {
		token, userAgent, wantUser, wantAgent string
	}{
		{token: token, wantUser: "user-synthetic__chatgpt-account", wantAgent: excelBPSDefaultUserAgent},
		{token: "synthetic-access-token", userAgent: "Custom/1.0", wantAgent: "Custom/1.0"},
	} {
		t.Setenv(excelBPSUserAgentEnv, tc.userAgent)
		request := httptest.NewRequest(http.MethodPost, basispoints.ResponsesURL, nil)
		setExcelBPSHeaders(request, tc.token, "chatgpt-account")
		if got := request.Header.Get("X-Openai-Account-User-Id"); got != tc.wantUser {
			t.Fatalf("X-Openai-Account-User-Id = %q, want %q", got, tc.wantUser)
		}
		if got := request.Header.Get("User-Agent"); got != tc.wantAgent {
			t.Fatalf("User-Agent = %q, want %q", got, tc.wantAgent)
		}
	}
	if !strings.Contains(excelBPSDefaultUserAgent, "Windows NT") || !strings.Contains(excelBPSDefaultUserAgent, "Edg/") {
		t.Fatalf("default User-Agent does not describe desktop Excel's WebView2: %s", excelBPSDefaultUserAgent)
	}
}
