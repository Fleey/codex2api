package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy/basispoints"
	"github.com/gin-gonic/gin"
)

func testExcelBPSAccount() *auth.Account {
	return &auth.Account{DBID: 91, AccessToken: "synthetic-access-token", AccountID: "chatgpt-account", ExcelBPSEnabled: true}
}

func TestExecuteExcelBPSRequestUsesProviderHeadersAndDoesNotExposeHTTPBody(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	called := false
	excelBPSDo = func(req *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		called = true
		if got := req.Header.Get("Authorization"); got != "Bearer synthetic-access-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("Chatgpt-Account-Id"); got != "chatgpt-account" {
			t.Fatalf("Chatgpt-Account-Id = %q", got)
		}
		if got := req.Header.Get("X-Basispoints-Auth-Mode"); got != "chatgpt" {
			t.Fatalf("X-Basispoints-Auth-Mode = %q", got)
		}
		if req.Header.Get("X-Openai-Internal-Basispoints-Office-Host") != "Excel" || req.Header.Get("X-Stainless-Runtime") != "browser:chrome" {
			t.Fatalf("Excel client headers missing: %v", req.Header)
		}
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"secret upstream detail"}}`)), Header: make(http.Header)}, nil
	}
	_, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hello"}`), "account:91/key:1/thread:1", "thread:1", "", false)
	if !called || err == nil {
		t.Fatalf("ExecuteExcelBPSRequest called=%t err=%v", called, err)
	}
	if strings.Contains(err.Error(), "secret upstream detail") {
		t.Fatalf("upstream error body leaked: %v", err)
	}
}

func TestForwardExcelBPSWritesOneTerminalFrame(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(_ *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[]}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	result, err := forwardExcelBPS(ctx, ctx, testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`), "account:91/key:1/thread:1", "thread:1", "", false, true, false)
	if err != nil {
		t.Fatalf("forwardExcelBPS: %v", err)
	}
	if result.Terminal != "response.completed" {
		t.Fatalf("terminal = %q", result.Terminal)
	}
	body := recorder.Body.String()
	if strings.Count(body, `"type":"response.completed"`) != 1 {
		t.Fatalf("completed event count = %d, body=%s", strings.Count(body, `"type":"response.completed"`), body)
	}
	if strings.Contains(body, "response.failed") {
		t.Fatalf("unexpected failure terminal: %s", body)
	}
}

func TestForwardExcelBPSSanitizesFailureWithoutAppendingTerminal(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(_ *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		body := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"provider detail\"}}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	result, err := forwardExcelBPS(ctx, ctx, testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`), "account:91/key:1/thread:1", "thread:1", "", false, true, false)
	if err != nil {
		t.Fatalf("forwardExcelBPS: %v", err)
	}
	if result.Terminal != "response.failed" {
		t.Fatalf("terminal = %q", result.Terminal)
	}
	body := recorder.Body.String()
	if strings.Count(body, `"type":"response.failed"`) != 1 {
		t.Fatalf("failure terminal count = %d, body=%s", strings.Count(body, `"type":"response.failed"`), body)
	}
	if strings.Contains(body, "provider detail") || strings.Contains(body, "response.completed") {
		t.Fatalf("failure body was not sanitized or gained a second terminal: %s", body)
	}
}

func TestForwardExcelBPSDoesNotTreatFailedCompletedAsSuccess(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(_ *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"provider detail\"}}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello"}`))
	result, err := forwardExcelBPS(ctx, ctx, testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hello"}`), "account:91/key:1/thread:1", "thread:1", "", false, true, false)
	if err != nil {
		t.Fatalf("stream forwarding should preserve the terminal frame: %v", err)
	}
	if result.Terminal != "response.failed" {
		t.Fatalf("terminal = %q, want response.failed", result.Terminal)
	}
	if strings.Contains(recorder.Body.String(), "provider detail") {
		t.Fatalf("failed completed event leaked provider detail: %s", recorder.Body.String())
	}
}

func TestWriteExcelBPSFailureUsesJSONBeforeStreamingResponseIsCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	writeExcelBPSFailure(ctx, true, http.StatusBadRequest, "basispoints_request_invalid", "unsupported request")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if body := recorder.Body.String(); strings.Contains(body, "response.failed") || !strings.Contains(body, "basispoints_request_invalid") {
		t.Fatalf("unexpected pre-commit failure body: %s", body)
	}
}

func TestWriteExcelBPSFailureUsesSSEAfterStreamingResponseIsCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	_, _ = ctx.Writer.Write([]byte("prefix"))

	writeExcelBPSFailure(ctx, true, http.StatusBadRequest, "basispoints_request_invalid", "unsupported request")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed status %d", recorder.Code, http.StatusOK)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "event: response.failed") || !strings.Contains(body, "basispoints_request_invalid") {
		t.Fatalf("unexpected committed failure body: %s", body)
	}
}

func TestExcelBPSFailureInfoExplainsRejectedRequest(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
		t.Fatal("rejected request reached the upstream")
		return nil, nil
	}
	_, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hi","previous_response_id":"resp_1"}`), "account:91/key:1/thread:1", "thread:1", "", false)
	status, code, message := excelBPSFailureInfo(err)
	if status != http.StatusBadRequest || code != "basispoints_request_invalid" {
		t.Fatalf("excelBPSFailureInfo = %d %q", status, code)
	}
	if !strings.Contains(message, "requires expanded history instead of previous_response_id") {
		t.Fatalf("message does not explain the rejection: %q", message)
	}
}

func TestExcelBPSRelaysContextWindowCodeCodexCompactsOn(t *testing.T) {
	t.Setenv("LOG_DISABLED", "true")
	gin.SetMode(gin.TestMode)
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	overflow := `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input of 921375 tokens about project-x exceeds"}}`
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"HTTP 400", http.StatusBadRequest, overflow},
		{"stream response.failed", http.StatusOK, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-1\",\"status\":\"failed\"," + overflow[1:len(overflow)-1] + "}}\n\n"},
	}
	for _, tc := range cases {
		excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
		}
		for _, stream := range []bool{true, false} {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			raw := fmt.Sprintf(`{"model":"gpt-5.5","input":"hello","stream":%t}`, stream)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
			(&Handler{}).handleExcelBPS(ctx, testExcelBPSAccount(), []byte(raw), "account:91/key:1/thread:1", "thread:1", "", false, stream, false, "/v1/responses", "gpt-5.5", "gpt-5.5", "medium", "", auth.SessionAffinityGuard{}, time.Now())
			body := recorder.Body.String()
			if !strings.Contains(body, `"code":"context_length_exceeded"`) || strings.Contains(body, "project-x") || strings.Contains(body, "event: error") {
				t.Fatalf("%s stream=%t: context window code not relayed safely: %s", tc.name, stream, body)
			}
		}
	}
}

type excelBPSImageStub struct {
	t            *testing.T
	uploadStatus int
	uploads      int
	refuse       int
	refusal      int
	refusalMsg   string
	bodies       []string
	uploadID     func(n int) string
}

func (s *excelBPSImageStub) do(req *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
	switch req.URL.String() {
	case basispoints.AttachmentsURL:
		s.uploads++
		reader, err := req.MultipartReader()
		if err != nil {
			s.t.Fatalf("attachment upload is not multipart: %v", err)
		}
		part, err := reader.NextPart()
		if err != nil || part.FormName() != "file" || part.Header.Get("Content-Type") != "image/png" {
			s.t.Fatalf("attachment part = %v, %v", part, err)
		}
		if req.Header.Get("Authorization") != "Bearer synthetic-access-token" {
			s.t.Fatalf("attachment upload is missing account auth")
		}
		if s.uploadStatus != 0 {
			return &http.Response{StatusCode: s.uploadStatus, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"upload refused"}}`)), Header: make(http.Header)}, nil
		}
		body := `{"openai_file_id":"` + s.uploadID(s.uploads) + `"}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	case basispoints.ResponsesURL:
		body, _ := io.ReadAll(req.Body)
		s.bodies = append(s.bodies, string(body))
		if s.refuse > 0 {
			s.refuse--
			status, message := http.StatusUnprocessableEntity, "422: Invalid request body."
			if s.refusal != 0 {
				status, message = s.refusal, s.refusalMsg
			}
			body := `{"error":{"message":"` + message + `"}}`
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	}
	s.t.Fatalf("unexpected upstream URL %s", req.URL)
	return nil, nil
}

func excelBPSImageRequest(payload string) []byte {
	return []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"data:image/png;base64,` + payload + `"}]}]}`)
}

// useExcelBPSImageStub installs stub as the upstream and isolates the
// process-wide attachment cache so tests do not depend on run order or -count.
func useExcelBPSImageStub(t *testing.T, stub *excelBPSImageStub) {
	t.Helper()
	previous := excelBPSDo
	excelBPSAttachments.reset()
	excelBPSDo = stub.do
	t.Cleanup(func() {
		excelBPSDo = previous
		excelBPSAttachments.reset()
	})
}

func TestExcelBPSUploadsMessageImagesAndFallsBackToNote(t *testing.T) {
	stub := &excelBPSImageStub{t: t, refuse: 1, uploadID: func(n int) string { return fmt.Sprintf("file-note-%d", n) }}
	useExcelBPSImageStub(t, stub)

	upstream, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), excelBPSImageRequest("iVBORw0KGgoAAAABbm90ZQ=="), "account:91/key:1/thread:1", "thread:1", "", false)
	if err != nil {
		t.Fatalf("ExecuteExcelBPSRequest: %v", err)
	}
	upstream.Response.Body.Close()
	if stub.uploads != 1 || len(stub.bodies) != 2 {
		t.Fatalf("uploads=%d sends=%d", stub.uploads, len(stub.bodies))
	}
	if strings.Contains(stub.bodies[0], "data:image/") || !strings.Contains(stub.bodies[0], `"file_id":"file-note-1"`) {
		t.Fatalf("first attempt did not name the uploaded file: %s", stub.bodies[0])
	}
	if strings.Contains(stub.bodies[1], "file-note-1") || !strings.Contains(stub.bodies[1], "image content omitted") {
		t.Fatalf("refused image was not replaced by a note: %s", stub.bodies[1])
	}
}

func TestExcelBPSReusesUploadsAndReuploadsStaleFileIDs(t *testing.T) {
	stub := &excelBPSImageStub{t: t, uploadID: func(n int) string { return fmt.Sprintf("file-reuse-%d", n) }}
	useExcelBPSImageStub(t, stub)
	raw := excelBPSImageRequest("iVBORw0KGgoAAAABcmV1c2U=")

	for attempt := 1; attempt <= 2; attempt++ {
		upstream, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), raw, "account:91/key:1/thread:1", "thread:1", "", false)
		if err != nil {
			t.Fatalf("request %d: %v", attempt, err)
		}
		upstream.Response.Body.Close()
	}
	if stub.uploads != 1 || !strings.Contains(stub.bodies[1], "file-reuse-1") {
		t.Fatalf("repeated image was uploaded again: uploads=%d body=%s", stub.uploads, stub.bodies[1])
	}

	stub.refuse = 1
	stub.bodies = nil
	upstream, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), raw, "account:91/key:1/thread:1", "thread:1", "", false)
	if err != nil {
		t.Fatalf("stale request: %v", err)
	}
	upstream.Response.Body.Close()
	if stub.uploads != 2 || len(stub.bodies) != 2 || !strings.Contains(stub.bodies[0], "file-reuse-1") || !strings.Contains(stub.bodies[1], "file-reuse-2") {
		t.Fatalf("stale file ID was not re-uploaded: uploads=%d bodies=%v", stub.uploads, stub.bodies)
	}
}

func TestExcelBPSReplayBackingRoundTripsThroughRuntimeCache(t *testing.T) {
	backing := excelBPSReplayBacking{cache: cache.NewMemory(1)}
	if err := backing.Store("account:1\x00call-1", []byte(`{"raw":{"type":"function_call"}}`)); err != nil {
		t.Fatalf("Store: %v", err)
	}
	raw, ok, err := backing.Load("account:1\x00call-1")
	if err != nil || !ok || string(raw) != `{"raw":{"type":"function_call"}}` {
		t.Fatalf("Load = %s, %t, %v", raw, ok, err)
	}
	if _, ok, _ := backing.Load("account:2\x00call-1"); ok {
		t.Fatal("replay backing crossed scope")
	}
}

func TestExcelBPSDoesNotRetryImagesForUnrelatedBadRequest(t *testing.T) {
	stub := &excelBPSImageStub{t: t, refuse: 3, refusal: http.StatusBadRequest, refusalMsg: "context length exceeded", uploadID: func(n int) string { return fmt.Sprintf("file-unrelated-%d", n) }}
	useExcelBPSImageStub(t, stub)
	_, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), excelBPSImageRequest("iVBORw0KGgoAAAABdW5yZWw="), "account:91/key:1/thread:1", "thread:1", "", false)
	status, _, _ := excelBPSFailureInfo(err)
	if status != http.StatusBadRequest || len(stub.bodies) != 1 || stub.uploads != 1 {
		t.Fatalf("unrelated 400 was retried: status=%d sends=%d uploads=%d", status, len(stub.bodies), stub.uploads)
	}
}

func TestExcelBPSErrorShapeOmitsProviderMessage(t *testing.T) {
	shape := excelBPSErrorShape([]byte(`{"error":{"message":"the user's secret prompt","code":"bad_image"}}`))
	if strings.Contains(shape, "secret") || !strings.Contains(shape, "bad_image") {
		t.Fatalf("excelBPSErrorShape = %s", shape)
	}
}

func TestForwardExcelBPSMarksSynthesizedCompletion(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(_ *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		body := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	result, err := forwardExcelBPS(ctx, ctx, testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`), "account:91/key:1/thread:1", "thread:1", "", false, true, false)
	if err != nil || result.Terminal != "response.completed" || !result.Synthesized {
		t.Fatalf("forwardExcelBPS = %+v, %v", result, err)
	}
}

func TestExcelBPSFailsOnlyWhenLatestTurnImageCannotBeUploaded(t *testing.T) {
	stub := &excelBPSImageStub{t: t, uploadStatus: http.StatusInternalServerError, uploadID: func(int) string { return "unused" }}
	useExcelBPSImageStub(t, stub)
	_, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), excelBPSImageRequest("iVBORw0KGgoAAAABY3Vycg=="), "account:91/key:1/thread:1", "thread:1", "", false)
	status, code, _ := excelBPSFailureInfo(err)
	if status != http.StatusBadGateway || code != "basispoints_image_upload_failed" || len(stub.bodies) != 0 {
		t.Fatalf("latest-turn upload failure = %d %q, sends=%d", status, code, len(stub.bodies))
	}

	history := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgoAAAABaGlzdA=="}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`)
	upstream, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), history, "account:91/key:1/thread:1", "thread:1", "", false)
	if err != nil {
		t.Fatalf("history upload failure should degrade to a note: %v", err)
	}
	upstream.Response.Body.Close()
	if len(stub.bodies) != 1 || !strings.Contains(stub.bodies[0], "image content omitted") {
		t.Fatalf("history image was not replaced by a note: %v", stub.bodies)
	}

	bad := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/x\u0000y;base64,AAAA"}]}]}`)
	_, err = ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), bad, "account:91/key:1/thread:1", "thread:1", "", false)
	if status, code, message := excelBPSFailureInfo(err); status != http.StatusBadRequest || code != "basispoints_request_invalid" || !strings.Contains(message, "media type") {
		t.Fatalf("invalid latest-turn image = %d %q %q", status, code, message)
	}
}

func TestExcelBPSReplayPersistsOnlyForSessionScopedConversations(t *testing.T) {
	headers := http.Header{}
	if excelBPSConversationScoped(headers, requestSessionIdentity{}) {
		t.Fatal("a request without a session header was treated as conversation-scoped")
	}
	headers.Set("Idempotency-Key", "req-1")
	if excelBPSConversationScoped(headers, requestSessionIdentity{}) {
		t.Fatal("an idempotency key was treated as a conversation identity")
	}
	headers.Set("Session-Id", "s-1")
	if !excelBPSConversationScoped(headers, requestSessionIdentity{}) {
		t.Fatal("a session header did not scope the conversation")
	}
	if excelBPSConversationScoped(headers, requestSessionIdentity{hasDownstreamAffinity: true}) {
		t.Fatal("an affinity header replaced the scope but replay still persisted")
	}
}

func TestHandleExcelBPSWritesUpstreamFailuresToErrorLogs(t *testing.T) {
	stubExcelBPSSleep(t)
	dir := t.TempDir()
	t.Setenv("LOG_DIR", dir)
	t.Setenv("LOG_DISABLED", "")
	previousBad, previousServer := badRequestLogger, serverErrorLogger
	badRequestLogger = &fileLogger{path: "bad_request.log"}
	serverErrorLogger = &fileLogger{path: "server_error.log"}
	t.Cleanup(func() {
		CloseErrorLogger()
		badRequestLogger, serverErrorLogger = previousBad, previousServer
	})
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	gin.SetMode(gin.TestMode)
	run := func(status int, body string) string {
		excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		raw := `{"model":"gpt-5.5","input":"hello","stream":true}`
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
		var h *Handler
		h.handleExcelBPS(ctx, testExcelBPSAccount(), []byte(raw), "account:91/key:1/thread:1", "thread:1", "", false, true, false, "/v1/responses", "gpt-5.5", "gpt-5.5", "", "", auth.SessionAffinityGuard{}, time.Now())
		return recorder.Body.String()
	}

	client := run(http.StatusOK, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"code\":\"server_error\",\"message\":\"stream provider detail\"}}\n\n")
	if strings.Contains(client, "stream provider detail") {
		t.Fatalf("provider message reached the client: %s", client)
	}
	run(http.StatusTooManyRequests, `{"error":{"code":"rate_limit_exceeded","message":"http provider detail"}}`)

	serverLog, _ := os.ReadFile(filepath.Join(dir, "server_error.log"))
	if !strings.Contains(string(serverLog), "stream provider detail") || !strings.Contains(string(serverLog), "Status: 500") {
		t.Fatalf("server_error.log missing the stream failure: %s", serverLog)
	}
	badLog, _ := os.ReadFile(filepath.Join(dir, "bad_request.log"))
	if !strings.Contains(string(badLog), "http provider detail") || !strings.Contains(string(badLog), "Status: 429") {
		t.Fatalf("bad_request.log missing the HTTP 429 failure: %s", badLog)
	}
}

func TestHandleExcelBPSReturnsRateLimitWithRetryDelayOnly(t *testing.T) {
	stubExcelBPSSleep(t)
	t.Setenv("LOG_DISABLED", "true")
	// No waiting: the limit goes to the client as the provider gave it.
	t.Setenv(excelBPSRateLimitWaitEnv, "0")
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	gin.SetMode(gin.TestMode)
	run := func(stream bool, status int, header http.Header, body string) *httptest.ResponseRecorder {
		excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: header}, nil
		}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		raw := fmt.Sprintf(`{"model":"gpt-5.5","input":"hello","stream":%t}`, stream)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
		var h *Handler
		h.handleExcelBPS(ctx, testExcelBPSAccount(), []byte(raw), "account:91/key:1/thread:1", "thread:1", "", false, stream, false, "/v1/responses", "gpt-5.5", "gpt-5.5", "", "", auth.SessionAffinityGuard{}, time.Now())
		return recorder
	}
	check := func(name string, recorder *httptest.ResponseRecorder, want string) {
		t.Helper()
		body := recorder.Body.String()
		if recorder.Code != http.StatusTooManyRequests || !strings.Contains(body, `"code":"rate_limit_exceeded"`) || !strings.Contains(body, want) {
			t.Fatalf("%s: status=%d body=%s", name, recorder.Code, body)
		}
		if strings.Contains(body, "org-synthetic") || strings.Contains(body, "Used 40000000") {
			t.Fatalf("%s: provider detail reached the client: %s", name, body)
		}
	}

	header := http.Header{}
	header.Set("Retry-After", "3")
	check("HTTP 429", run(true, http.StatusTooManyRequests, header, `{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached in organization org-synthetic: Used 40000000"}}`), "Please try again in 3s.")

	event := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"tokens\",\"code\":\"rate_limit_exceeded\",\"headers\":{\"retry-after-ms\":\"74\"},\"message\":\"Rate limit reached in organization org-synthetic: Used 40000000\"}}\n\n"
	check("non-stream error event", run(false, http.StatusOK, make(http.Header), event), "Please try again in 74ms.")
}

// stubExcelBPSSleep makes rate-limit retries immediate and records the waits.
func stubExcelBPSSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	previous := excelBPSSleep
	t.Cleanup(func() { excelBPSSleep = previous })
	var waits []time.Duration
	excelBPSSleep = func(_ context.Context, wait time.Duration) error {
		waits = append(waits, wait)
		return nil
	}
	return &waits
}

func TestForwardExcelBPSRetriesRateLimitBeforeAnyOutput(t *testing.T) {
	t.Setenv("LOG_DISABLED", "true")
	gin.SetMode(gin.TestMode)
	const opening = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
		"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp-1\",\"status\":\"in_progress\",\"output\":[]}}\n\n"
	limited := func(retryAfterMS string) string {
		return opening + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"tokens\",\"code\":\"rate_limit_exceeded\",\"headers\":{\"retry-after-ms\":\"" + retryAfterMS + "\"},\"message\":\"org-synthetic is over its TPM\"}}\n\n"
	}
	const answered = opening + "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[]}}\n\n"
	const limitedAfterOutput = opening + "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"tokens\",\"code\":\"rate_limit_exceeded\",\"headers\":{\"retry-after-ms\":\"74\"}}}\n\n"
	type reply struct {
		status int
		body   string
	}
	cases := []struct {
		name     string
		budget   string
		replies  []reply
		attempts int
		waits    []time.Duration
		terminal string
		want     []string
		absent   []string
	}{
		{
			name:     "stream limit then success",
			replies:  []reply{{http.StatusOK, limited("74")}, {http.StatusOK, answered}},
			attempts: 2, waits: []time.Duration{time.Second}, terminal: "response.completed",
			want: []string{"hello"}, absent: []string{"rate_limit", "org-synthetic"},
		},
		{
			name:     "HTTP 429 then success",
			replies:  []reply{{http.StatusTooManyRequests, `{"error":{"code":"rate_limit_exceeded"}}`}, {http.StatusOK, answered}},
			attempts: 2, waits: []time.Duration{time.Second}, terminal: "response.completed",
			want: []string{"hello"}, absent: []string{"rate_limit"},
		},
		{
			name:     "provider hint above the backoff",
			replies:  []reply{{http.StatusOK, limited("5000")}, {http.StatusOK, answered}},
			attempts: 2, waits: []time.Duration{5 * time.Second}, terminal: "response.completed",
			want: []string{"hello"}, absent: []string{"rate_limit"},
		},
		{
			name:     "provider wait above the budget",
			budget:   "3",
			replies:  []reply{{http.StatusOK, limited("5000")}},
			attempts: 1, terminal: "response.failed",
			want: []string{`"code":"rate_limit_exceeded"`, "Please try again in 5s."}, absent: []string{"org-synthetic"},
		},
		{
			name:     "limit after output",
			replies:  []reply{{http.StatusOK, limitedAfterOutput}},
			attempts: 1, terminal: "response.failed",
			want: []string{"hel", `"code":"rate_limit_exceeded"`},
		},
		{
			// The request already waited, so the client must not retry it.
			name:     "budget exhausted",
			budget:   "4",
			replies:  []reply{{http.StatusOK, limited("74")}},
			attempts: 4, waits: []time.Duration{time.Second, 2 * time.Second, time.Second}, terminal: "response.failed",
			want: []string{`"code":"invalid_prompt"`, "waited 4s"}, absent: []string{"org-synthetic", "rate_limit_exceeded"},
		},
	}
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	for _, tc := range cases {
		t.Setenv(excelBPSRateLimitWaitEnv, tc.budget)
		waits := stubExcelBPSSleep(t)
		attempts := 0
		excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
			next := tc.replies[min(attempts, len(tc.replies)-1)]
			attempts++
			return &http.Response{StatusCode: next.status, Body: io.NopCloser(strings.NewReader(next.body)), Header: make(http.Header)}, nil
		}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		raw := `{"model":"gpt-5.5","input":"hello","stream":true}`
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
		result, err := forwardExcelBPS(ctx, ctx, testExcelBPSAccount(), []byte(raw), "account:91/key:1/thread:1", "thread:1", "", false, true, false)
		if err != nil {
			t.Fatalf("%s: forwardExcelBPS: %v", tc.name, err)
		}
		body := recorder.Body.String()
		if attempts != tc.attempts || result.Terminal != tc.terminal || fmt.Sprint(*waits) != fmt.Sprint(tc.waits) {
			t.Fatalf("%s: attempts=%d terminal=%q waits=%v body=%s", tc.name, attempts, result.Terminal, *waits, body)
		}
		if strings.Count(body, `"type":"response.created"`) != 1 {
			t.Fatalf("%s: client should see exactly one response.created: %s", tc.name, body)
		}
		// Codex ignores a bare error event and retries the "unfinished" stream.
		if strings.Contains(body, "event: error") {
			t.Fatalf("%s: client got an error event instead of response.failed: %s", tc.name, body)
		}
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Fatalf("%s: missing %s in %s", tc.name, want, body)
			}
		}
		for _, absent := range tc.absent {
			if strings.Contains(body, absent) {
				t.Fatalf("%s: unexpected %s in %s", tc.name, absent, body)
			}
		}
	}
}

func TestForwardExcelBPSAnswersJSONWhenStreamEndsWhileFramesAreHeld(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
		body := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\",\"status\":\"in_progress\",\"output\":[]}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	raw := `{"model":"gpt-5.5","input":"hello","stream":true}`
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
	var h *Handler
	h.handleExcelBPS(ctx, testExcelBPSAccount(), []byte(raw), "account:91/key:1/thread:1", "thread:1", "", false, true, false, "/v1/responses", "gpt-5.5", "gpt-5.5", "", "", auth.SessionAffinityGuard{}, time.Now())
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, body=%s", got, recorder.Body.String())
	}
}

func TestHandleExcelBPSLogsFailureSummaryWithRequestTag(t *testing.T) {
	stubExcelBPSSleep(t)
	t.Setenv("LOG_DISABLED", "true")
	var logs bytes.Buffer
	previousOut, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousOut)
		log.SetFlags(previousFlags)
	})
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	gin.SetMode(gin.TestMode)
	run := func() {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		raw := `{"model":"gpt-5.5","input":"hello","stream":true}`
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
		var h *Handler
		h.handleExcelBPS(ctx, testExcelBPSAccount(), []byte(raw), "account:91/key:1/thread:1", "thread:1", "", false, true, false, "/v1/responses", "gpt-5.5", "gpt-5.5", "", "", auth.SessionAffinityGuard{}, time.Now())
	}

	excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
		body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"c\",\"name\":\"excel_native\",\"arguments\":\"{}\"}]}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	run()
	summary := regexp.MustCompile(`\[excel-bps\] (account=91 req=[0-9a-f]{8}) /v1/responses ended unsuccessfully: .*terminal="response.failed" kind="basispoints_protocol_error" message="Basispoints response could not be translated: basispoints response contained only tool calls that could not be relayed \(dropped=1 output_items=1\)"`).FindStringSubmatch(logs.String())
	if summary == nil {
		t.Fatalf("request summary missing: %s", logs.String())
	}
	if !strings.Contains(logs.String(), summary[1]+` dropped an untranslatable tool call (type="function_call" name="excel_native" call_id="c")`) {
		t.Fatalf("bridge detail does not carry the summary's request tag %q: %s", summary[1], logs.String())
	}

	logs.Reset()
	excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	run()
	// A transport failure before any output goes to the native path, which
	// logs why instead of a failure summary.
	if !regexp.MustCompile(`\[excel-bps\] account=91 req=[0-9a-f]{8} upstream request failed`).MatchString(logs.String()) {
		t.Fatalf("transport failure log does not carry the request tag: %s", logs.String())
	}
	if !regexp.MustCompile(`account=91 req=[0-9a-f]{8} native fallback reason=transport_error`).MatchString(logs.String()) {
		t.Fatalf("native fallback log does not carry the request tag: %s", logs.String())
	}
	for _, want := range []string{"connection refused"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("transport failure log is missing %q: %s", want, logs.String())
		}
	}
}
