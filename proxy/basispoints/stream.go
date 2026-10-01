package basispoints

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type protocolError struct{ error }

type streamBody struct {
	*io.PipeReader
	upstream io.ReadCloser
	once     sync.Once
	err      error
}

func (b *streamBody) closeUpstream() error {
	b.once.Do(func() { b.err = b.upstream.Close() })
	return b.err
}

func (b *streamBody) Close() error {
	readerErr := b.PipeReader.Close()
	return errors.Join(readerErr, b.closeUpstream())
}

// Stream keeps text incremental while withholding native tool events until validated.
// Closing the downstream body interrupts an upstream read or a blocked pipe write.
func (b *Bridge) Stream(upstream io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	body := &streamBody{PipeReader: reader, upstream: upstream}
	go func() {
		err := b.transform(upstream, writer)
		_ = body.closeUpstream()
		_ = writer.CloseWithError(err)
	}()
	return body
}

// defaultKeepalive is how long the upstream may stay silent before the bridge
// repeats response.in_progress. Codex treats a stream idle for five minutes as
// broken and retries the whole turn, which long reasoning can otherwise hit.
const defaultKeepalive = 15 * time.Second

// errorEventGrace is how long the upstream may stay open after an `error`
// event before the bridge ends the response itself. BPS normally follows the
// event with response.failed or closes the connection at once. Tests shorten it.
var errorEventGrace = 10 * time.Second

type upstreamEvent struct {
	event string
	data  []byte
}

type finishedItem struct {
	index int
	item  object
}

// StartSequenceAt numbers the frames the bridge emits from n, so an attempt
// that retries a request continues the sequence its client has already seen.
// It must be called before Stream.
func (b *Bridge) StartSequenceAt(n int) {
	b.sequenceStart = n
}

func (b *Bridge) transform(reader io.Reader, writer io.Writer) error {
	sequence := b.sequenceStart
	terminal := false
	lastWrite := time.Now()
	var writeErr error
	emitted := make(map[string]bool)
	pendingTools := make(map[string]bool)
	// started, itemsAdded and finished describe the response so far, for
	// keepalive frames and for completing a stream cut off after its last item.
	var started object
	itemsAdded := 0
	var finished []finishedItem
	// reported is the sanitized error of an upstream `error` event. Codex
	// ignores that event type and only reads response.failed, so it is held
	// until the upstream ends the response, then sent as response.failed.
	var reported object
	var reportedGrace <-chan time.Time
	emit := func(kind string, payload object) error {
		payload["type"] = kind
		payload["sequence_number"] = sequence
		sequence++
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", kind, raw); err != nil {
			writeErr = err
			return err
		}
		lastWrite = time.Now()
		return nil
	}
	emitTool := func(item object, index any) error {
		id := text(item["id"])
		if emitted[id] {
			return nil
		}
		emitted[id] = true
		if isBuiltinCall(item) || isToolSearchCall(item) {
			// Built-in and tool_search calls carry structured fields and have
			// no argument delta events; the finished item is the whole call.
			added := make(object, len(item))
			for k, v := range item {
				added[k] = v
			}
			added["status"] = "in_progress"
			if err := emit("response.output_item.added", object{"output_index": index, "item": added}); err != nil {
				return err
			}
			return emit("response.output_item.done", object{"output_index": index, "item": item})
		}
		field, prefix := "arguments", "response.function_call_arguments"
		if text(item["type"]) == "custom_tool_call" {
			field, prefix = "input", "response.custom_tool_call_input"
		}
		added := make(object, len(item))
		for k, v := range item {
			added[k] = v
		}
		added[field], added["status"] = "", "in_progress"
		if err := emit("response.output_item.added", object{"output_index": index, "item": added}); err != nil {
			return err
		}
		if err := emit(prefix+".delta", object{"output_index": index, "item_id": id, "delta": item[field]}); err != nil {
			return err
		}
		if err := emit(prefix+".done", object{"output_index": index, "item_id": id, field: item[field]}); err != nil {
			return err
		}
		return emit("response.output_item.done", object{"output_index": index, "item": item})
	}
	process := func(event string, data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		var payload object
		if decode(data, &payload) != nil || payload == nil {
			return fmt.Errorf("invalid Basispoints SSE event (event=%q bytes=%d)", truncateRunes(event, 64), len(data))
		}
		kind := text(payload["type"])
		if kind == "" {
			kind = event
		}
		failure := ""
		if kind == "response.completed" {
			if response, ok := payload["response"].(object); ok {
				switch strings.ToLower(strings.TrimSpace(text(response["status"]))) {
				case "failed":
					failure = "response.failed"
				case "incomplete":
					failure = "response.incomplete"
				}
			}
		}
		if kind == "response.failed" || kind == "response.incomplete" || kind == "error" {
			failure = kind
		}
		if failure != "" {
			raw := append([]byte(nil), data...)
			shape, notice, retryAfter, clientCode := sanitizeTerminalError(payload, failure)
			b.logf("upstream %s: %s", failure, shape)
			b.upstreamFailure.Store(&TerminalFailure{Shape: shape, Raw: raw, RateLimit: notice, RetryAfter: retryAfter, ClientCode: clientCode})
		}
		if kind == "error" {
			// sanitizeTerminalError put the client-safe error in one of these.
			reported, _ = payload["error"].(object)
			if response, ok := payload["response"].(object); ok && reported == nil {
				reported, _ = response["error"].(object)
			}
			if reportedGrace == nil {
				reportedGrace = time.After(errorEventGrace)
			}
			return nil
		}
		if isToolEvent(kind) {
			return nil
		}
		item, _ := payload["item"].(object)
		switch kind {
		case "response.output_item.added":
			itemsAdded++
		case "response.output_item.done":
			index := len(finished)
			if number, ok := payload["output_index"].(json.Number); ok {
				if value, err := number.Int64(); err == nil {
					index = int(value)
				}
			}
			if item != nil {
				finished = append(finished, finishedItem{index: index, item: item})
			} else {
				b.logf("upstream response.output_item.done at output_index=%d carried no item", index)
			}
		}
		if kind == "response.output_item.added" && isTool(item) {
			return nil
		}
		if kind == "response.output_item.done" && isTool(item) {
			// Only the terminal response contains the authoritative native item.
			// Text keeps streaming; tool calls wait until the whole response validates.
			if len(pendingTools) >= 1024 {
				return fmt.Errorf("basispoints response contains too many tool items")
			}
			pendingTools[text(item["call_id"])+"\x00"+text(item["id"])] = true
			return nil
		}
		if text(item["type"]) == "reasoning" {
			normalizeReasoningForClient(item)
		}
		if response, ok := payload["response"].(object); ok {
			if kind == "response.completed" {
				output, _ := response["output"].([]any)
				outputTools := 0
				for _, raw := range output {
					item, _ := raw.(object)
					if isTool(item) {
						outputTools++
						delete(pendingTools, text(item["call_id"])+"\x00"+text(item["id"]))
					}
				}
				if len(pendingTools) != 0 {
					missing := make([]string, 0, len(pendingTools))
					for key := range pendingTools {
						callID, itemID, _ := strings.Cut(key, "\x00")
						missing = append(missing, fmt.Sprintf("call_id=%q id=%q", truncateRunes(callID, 64), truncateRunes(itemID, 64)))
					}
					sort.Strings(missing)
					if len(missing) > 4 {
						missing = missing[:4]
					}
					return fmt.Errorf("basispoints completed response omitted %d streamed tool item(s); its output has %d tool item(s); missing %s",
						len(pendingTools), outputTools, strings.Join(missing, ", "))
				}
				if err := b.translateResponse(response); err != nil {
					return err
				}
				output, _ = response["output"].([]any)
				for i, raw := range output {
					item, _ := raw.(object)
					if isClientCall(item) {
						if err := emitTool(item, i); err != nil {
							return err
						}
					}
				}
			} else {
				// Never expose native or incomplete tool arguments to the client.
				output, _ := response["output"].([]any)
				filtered := make([]any, 0, len(output))
				for _, raw := range output {
					item, _ := raw.(object)
					if !isTool(item) {
						filtered = append(filtered, raw)
					}
				}
				response["output"] = filtered
				response["reasoning"] = object{"effort": b.Effort}
				if kind == "response.created" || kind == "response.in_progress" {
					started = response
				}
			}
		}
		terminal = kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed"
		return emit(kind, payload)
	}
	// failReported ends the response with the error an `error` event reported.
	failReported := func(reason string) error {
		b.logf("upstream reported an error event and %s; ending the response with response.failed", reason)
		response := make(object, len(started)+3)
		for k, v := range started {
			response[k] = v
		}
		response["status"], response["output"], response["error"] = "failed", []any{}, reported
		terminal = true
		return emit("response.failed", object{"response": response})
	}
	// completion rebuilds response.completed when the upstream closed after
	// finishing every item it started and the last one ends a turn: a final
	// answer message or a tool call. Anything else (reasoning, commentary
	// before tool calls) means the stream was cut mid-turn, which is left to
	// the client's retry instead of being reported as a finished answer.
	completion := func() ([]byte, string) {
		if len(finished) == 0 {
			return nil, fmt.Sprintf("no output item finished (started=%d)", itemsAdded)
		}
		if len(finished) < itemsAdded {
			return nil, fmt.Sprintf("only %d of %d started output items finished", len(finished), itemsAdded)
		}
		sort.SliceStable(finished, func(i, j int) bool { return finished[i].index < finished[j].index })
		output := make([]any, 0, len(finished))
		for _, entry := range finished {
			output = append(output, entry.item)
		}
		last := finished[len(finished)-1].item
		endsTurn := isTool(last) || text(last["type"]) == "message" && text(last["phase"]) != "commentary"
		if !endsTurn {
			return nil, fmt.Sprintf("last finished item type=%q phase=%q does not end a turn", truncateRunes(text(last["type"]), 32), truncateRunes(text(last["phase"]), 32))
		}
		response := make(object, len(started)+2)
		for k, v := range started {
			response[k] = v
		}
		response["status"], response["output"] = "completed", output
		raw, err := json.Marshal(object{"type": "response.completed", "response": response})
		if err != nil {
			return nil, "the rebuilt response could not be encoded: " + err.Error()
		}
		return raw, ""
	}
	fail := func(err error) error {
		if writeErr != nil {
			b.logf("downstream write failed while relaying the response: %v", writeErr)
			return writeErr
		}
		b.logf("Basispoints response could not be translated: %v", err)
		reason := truncateRunes(err.Error(), 512)
		b.translationFailure.Store(&reason)
		if emitErr := emit("response.failed", object{"response": object{
			"status": "failed", "output": []any{},
			"error": object{"code": "basispoints_protocol_error", "message": "Basispoints response could not be translated"},
		}}); emitErr != nil {
			b.logf("downstream write failed while reporting the translation failure: %v", emitErr)
			return emitErr
		}
		return nil
	}

	events := make(chan upstreamEvent)
	readDone := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		readDone <- readEvents(reader, func(event string, data []byte) error {
			select {
			case events <- upstreamEvent{event: event, data: data}:
				return nil
			case <-stop:
				return io.EOF
			}
		})
	}()
	interval := b.keepalive
	if interval <= 0 {
		interval = defaultKeepalive
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case upstream := <-events:
			if err := process(upstream.event, upstream.data); err != nil {
				return fail(err)
			}
			if terminal {
				return nil
			}
		case err := <-readDone:
			var invalid protocolError
			if errors.As(err, &invalid) {
				return fail(invalid.error)
			}
			if closedLocally(err) {
				// The client went away and the body was closed on our side;
				// there is nobody to complete the response for.
				b.logf("downstream closed before the upstream finished: %v", err)
				return err
			}
			reason := "closed"
			if err != nil {
				reason = err.Error()
			}
			if reported != nil {
				return failReported("the stream then ended (" + reason + ")")
			}
			raw, incomplete := completion()
			if raw != nil {
				b.logf("upstream stream ended before response.completed (%s); completing it from %d finished items", reason, len(finished))
				b.synthesized.Store(true)
				if perr := process("response.completed", raw); perr != nil {
					return fail(perr)
				}
				return nil
			}
			b.logf("upstream stream ended before a terminal event (%s) and was left to the client retry: %s", reason, incomplete)
			if err != nil {
				return err
			}
			return io.ErrUnexpectedEOF
		case <-reportedGrace:
			return failReported(fmt.Sprintf("no terminal event followed within %s", errorEventGrace))
		case <-ticker.C:
			if started != nil && time.Since(lastWrite) >= interval {
				if err := emit("response.in_progress", object{"response": started}); err != nil {
					b.logf("keepalive write failed; downstream is gone: %v", err)
					return err
				}
			}
		}
	}
}

// TerminalErrorShape describes a provider error for operator logs using only
// its enum-like code and type fields. Messages are never logged because they
// can echo request content, connector arguments, or credential metadata.
func TerminalErrorShape(payload object) string {
	source := terminalErrorSource(payload)
	return fmt.Sprintf("code=%q type=%q", truncateRunes(text(source["code"]), 64), truncateRunes(text(source["type"]), 64))
}

// closedLocally reports read errors caused by this side closing the stream
// (client disconnect or cancellation), as opposed to the upstream dropping it.
func closedLocally(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, http.ErrBodyReadAfterClose) || errors.Is(err, io.ErrClosedPipe)
}

// incompleteReason returns the enum-like reason of an incomplete response
// (max_output_tokens, content_filter, ...), or "" when there is none.
func incompleteReason(payload object) string {
	response, _ := payload["response"].(object)
	details, _ := response["incomplete_details"].(object)
	return truncateRunes(text(details["reason"]), 64)
}

// clientErrors are the provider error codes Codex acts on by code, each with
// the gateway's own message. A full context window makes Codex compact the
// conversation; a refused prompt is shown instead of retried. Both depend on
// the request alone, so another try or another account cannot fix them. Codes
// about the account (quota, plan, overload) stay generic so the client keeps
// retrying, which can land on a healthy account.
var clientErrors = map[string]string{
	"context_length_exceeded": "Your input exceeds the context window of this model. Please adjust your input and try again.",
	"invalid_prompt":          "Basispoints rejected the prompt as invalid.",
}

// ClientError returns the client-safe message for a provider error code that
// may cross the gateway boundary unchanged, and whether code is one.
func ClientError(code string) (string, bool) {
	message, ok := clientErrors[strings.TrimSpace(code)]
	return message, ok
}

// sanitizeTerminalError removes provider error text before a failed BPS event
// crosses the gateway boundary. Provider errors can echo request content,
// connector arguments, or credential metadata. A rate limit keeps its code and
// retry delay so clients can back off, and a code in clientErrors keeps its
// code with the gateway's message. It returns the error's shape as logged,
// taken before the provider fields are replaced (with an incomplete reason
// when present), the rate-limit notice with its retry delay, and the kept
// client error code, if any.
func sanitizeTerminalError(payload object, kind string) (string, string, time.Duration, string) {
	source := terminalErrorSource(payload)
	shape := TerminalErrorShape(payload)
	if reason := incompleteReason(payload); reason != "" {
		shape += fmt.Sprintf(" reason=%q", reason)
	}
	failure := object{"code": "basispoints_upstream_error", "message": "Basispoints upstream returned an unsuccessful response"}
	if kind == "response.incomplete" {
		failure = object{"code": "basispoints_incomplete", "message": "Basispoints upstream ended the response before completion"}
	}
	notice, retryAfter, clientCode := "", time.Duration(0), ""
	if isRateLimit(source) {
		notice, retryAfter = rateLimitNotice(source, nil)
		failure = object{"code": "rate_limit_exceeded", "message": notice}
		if limit := text(source["type"]); limit == "tokens" || limit == "requests" {
			failure["type"] = limit
		}
	} else if message, ok := ClientError(text(source["code"])); ok && kind != "response.incomplete" {
		clientCode = strings.TrimSpace(text(source["code"]))
		failure = object{"code": clientCode, "message": message}
	}
	if response, ok := payload["response"].(object); ok {
		delete(response, "status_details")
		response["error"] = failure
	} else {
		payload["error"] = failure
	}
	delete(payload, "message")
	delete(payload, "code")
	delete(payload, "param")
	return shape, notice, retryAfter, clientCode
}

func readEvents(reader io.Reader, consume func(string, []byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	var data strings.Builder
	event := ""
	flush := func() error {
		if data.Len() == 0 {
			event = ""
			return nil
		}
		err := consume(event, []byte(strings.TrimSuffix(data.String(), "\n")))
		data.Reset()
		event = ""
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
		} else if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			_, _ = data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			_ = data.WriteByte('\n')
			if data.Len() > 16<<20 {
				return protocolError{fmt.Errorf("basispoints SSE event exceeds 16 MiB")}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return protocolError{fmt.Errorf("basispoints SSE line exceeds 16 MiB")}
		}
		// Basispoints often drops the connection right after its final event,
		// before the blank line that ends it. Complete data lines holding valid
		// JSON are still the whole event; keeping it keeps the real terminal
		// and its usage. A cut-off payload fails json.Valid and is dropped.
		if data.Len() > 0 && json.Valid([]byte(strings.TrimSuffix(data.String(), "\n"))) {
			if flushErr := flush(); flushErr != nil {
				return flushErr
			}
		}
		return err
	}
	return flush()
}
