package basispoints

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
)

// cutReader returns its text, then fails like a connection dropped by the peer.
type cutReader struct {
	text *strings.Reader
}

var errCut = errors.New("connection reset by peer")

func (r cutReader) Read(p []byte) (int, error) {
	n, err := r.text.Read(p)
	if err == io.EOF {
		return n, errCut
	}
	return n, err
}

func (cutReader) Close() error { return nil }

func TestReadEventsKeepsFinalEventWhenConnectionDropsBeforeBlankLine(t *testing.T) {
	const final = `{"type":"response.completed","response":{"status":"completed"}}`
	var got []string
	err := readEvents(cutReader{strings.NewReader("event: response.completed\ndata: " + final + "\n")}, func(_ string, data []byte) error {
		got = append(got, string(data))
		return nil
	})
	if !errors.Is(err, errCut) || len(got) != 1 || got[0] != final {
		t.Fatalf("complete final event was lost: err=%v events=%q", err, got)
	}

	got = nil
	err = readEvents(cutReader{strings.NewReader("data: {\"type\":\"response.comp\n")}, func(_ string, data []byte) error {
		got = append(got, string(data))
		return nil
	})
	if !errors.Is(err, errCut) || len(got) != 0 {
		t.Fatalf("cut-off event was relayed: err=%v events=%q", err, got)
	}
}

func TestStreamKeepsUsageWhenConnectionDropsAfterTerminal(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n"
	out := readStream(t, bridge, cutReader{strings.NewReader(stream)})
	if !strings.Contains(out, `"input_tokens":7`) || bridge.SynthesizedCompletion() {
		t.Fatalf("the real completion was replaced by a rebuilt one without usage: synthesized=%t %s", bridge.SynthesizedCompletion(), out)
	}
}

func TestStartSequenceAtContinuesNumbering(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
	bridge.StartSequenceAt(5)
	out := readStream(t, bridge, io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")))
	if !strings.Contains(out, `"sequence_number":5`) {
		t.Fatalf("sequence did not start at 5: %s", out)
	}
}

func TestCompactThresholdIsConfigurable(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{
		{"", defaultCompactThreshold},
		{"500000", 500000},
		{" 450000 ", 450000},
		{"5", minCompactThreshold},
		{"9999999", maxCompactThreshold},
		{"abc", defaultCompactThreshold},
		{"-1", defaultCompactThreshold},
	} {
		t.Setenv(compactThresholdEnv, tc.env)
		prepared, _ := preparedBody(t, `{"model":"gpt-5.5","input":"hi"}`, &ReplayCache{})
		management, _ := prepared["context_management"].([]any)
		entry, _ := management[0].(object)
		if got := fmt.Sprint(entry["compact_threshold"]); got != strconv.Itoa(tc.want) {
			t.Fatalf("env %q: compact_threshold = %v, want %d", tc.env, got, tc.want)
		}
	}
	t.Setenv(compactThresholdEnv, "500000")
	prepared, _ := preparedBody(t, `{"model":"gpt-5.5","input":"hi","context_management":[{"type":"compaction","compact_threshold":123456}]}`, &ReplayCache{})
	if entry := prepared["context_management"].([]any)[0].(object); fmt.Sprint(entry["compact_threshold"]) != "123456" {
		t.Fatalf("client context_management was overridden: %v", entry)
	}
}

func TestRetryOfFailedTransportTellsModelWhy(t *testing.T) {
	scope := "account:1/" + t.Name()
	raw := []byte(`{"model":"gpt-5.5","input":"list the repo",` + execCommandTools + `}`)
	prepare := func() (object, *Bridge) {
		t.Helper()
		body, bridge, err := Prepare(raw, scope, &ReplayCache{})
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		var prepared object
		if err := decode(body, &prepared); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return prepared, bridge
	}
	lastInput := func(prepared object) string {
		input := prepared["input"].([]any)
		last := input[len(input)-1].(object)
		content := last["content"].([]any)
		return text(last["role"]) + ": " + text(content[0].(object)["text"])
	}

	first, bridge := prepare()
	if strings.Contains(lastInput(first), "Gateway notice") {
		t.Fatalf("first request already carries a notice: %s", lastInput(first))
	}
	malformed := `{"type":"function_call","id":"fc_1","call_id":"c1","name":"run_officejs","arguments":"{\"code\":\"await Excel.run(async (context) => {})\",\"summary\":\"s\"}"}`
	failed := readStream(t, bridge, io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":["+malformed+"]}}\n\n")))
	if !strings.Contains(failed, `"type":"response.failed"`) {
		t.Fatalf("malformed-only answer did not fail: %s", failed)
	}

	retry, bridge := prepare()
	notice := lastInput(retry)
	if !strings.HasPrefix(notice, "developer: Gateway notice") || !strings.Contains(notice, `name="run_officejs" call_id="c1"`) || !strings.Contains(notice, "failed once") {
		t.Fatalf("retry does not explain the failure: %s", notice)
	}
	if strings.Contains(notice, "Excel.run") {
		t.Fatalf("notice echoes the rejected code: %s", notice)
	}
	answered := readStream(t, bridge, io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":["+transportCall("c2", "exec_command", `{"cmd":"ls"}`)+"]}}\n\n")))
	if !strings.Contains(answered, `"name":"exec_command"`) {
		t.Fatalf("retry answer was not relayed: %s", answered)
	}
	if after, _ := prepare(); strings.Contains(lastInput(after), "Gateway notice") {
		t.Fatalf("notice outlived the answered retry: %s", lastInput(after))
	}
}

func TestAppendBeforeTriggerKeepsCompactionTriggerLast(t *testing.T) {
	items := appendBeforeTrigger([]any{object{"type": "message"}, object{"type": "compaction_trigger"}}, object{"type": "note"})
	if len(items) != 3 || text(items[1].(object)["type"]) != "note" || text(items[2].(object)["type"]) != "compaction_trigger" {
		t.Fatalf("items = %v", items)
	}
	if items := appendBeforeTrigger([]any{object{"type": "message"}}, object{"type": "note"}); text(items[1].(object)["type"]) != "note" {
		t.Fatalf("items = %v", items)
	}
}
