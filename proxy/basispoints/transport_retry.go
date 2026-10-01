package basispoints

import (
	"container/list"
	"fmt"
	"strings"
	"sync"
	"time"
)

// A response whose only tool calls could not be relayed fails, and the client
// retries the same request. Sampling again blind tends to repeat the same
// malformed run_officejs payload, so the failure reasons are remembered under
// the request's retry key and the retry tells the model what went wrong. The
// reasons are the gateway's own structural descriptions of the model's output;
// they never carry the rejected code or arguments.
const (
	transportRetryTTL     = 15 * time.Minute
	transportRetryEntries = 1024
	// transportRetryReasons caps the reasons kept per failed response.
	transportRetryReasons = 4
)

type transportRetryEntry struct {
	key      string
	reasons  []string
	failures int
	expires  time.Time
}

// transportRetryMemory is a bounded, process-local LRU of failed requests.
type transportRetryMemory struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
	now     func() time.Time
}

var transportRetries = transportRetryMemory{now: time.Now}

// record remembers why the response to the request named key had no usable
// tool call. Repeated failures of the same request are counted.
func (m *transportRetryMemory) record(key string, reasons []string) {
	if key == "" || len(reasons) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[string]*list.Element)
	}
	now := m.now()
	element := m.entries[key]
	if element == nil {
		for len(m.entries) >= transportRetryEntries {
			oldest := m.order.Back()
			delete(m.entries, oldest.Value.(*transportRetryEntry).key)
			m.order.Remove(oldest)
		}
		element = m.order.PushFront(&transportRetryEntry{key: key})
		m.entries[key] = element
	}
	entry := element.Value.(*transportRetryEntry)
	if !now.Before(entry.expires) {
		entry.failures = 0
	}
	entry.failures++
	entry.reasons = append([]string(nil), reasons...)
	entry.expires = now.Add(transportRetryTTL)
	m.order.MoveToFront(element)
}

// forget drops the failure of the request named key once it was answered.
func (m *transportRetryMemory) forget(key string) {
	if key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if element := m.entries[key]; element != nil {
		delete(m.entries, key)
		m.order.Remove(element)
	}
}

// note returns the developer notice for a retry of the request named key, or
// "" when no earlier answer to it failed.
func (m *transportRetryMemory) note(key string) string {
	if key == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	element := m.entries[key]
	if element == nil {
		return ""
	}
	entry := element.Value.(*transportRetryEntry)
	if !m.now().Before(entry.expires) {
		delete(m.entries, key)
		m.order.Remove(element)
		return ""
	}
	times := "once"
	if entry.failures > 1 {
		times = fmt.Sprintf("%d times", entry.failures)
	}
	return "Gateway notice: your previous answer to this same request failed " + times +
		" because its run_officejs calls could not be relayed, so the client sent the request again. Reasons: " +
		strings.Join(entry.reasons, "; ") + ". " +
		"Make the needed client tool calls again now, following the transport rules exactly: one catalog tool per run_officejs call; " +
		"code is JSON text holding one object such as {\"name\":\"CATALOG_NAME\",\"arguments\":{...}}, with every quote and backslash inside its string values escaped; " +
		"for a tool with a raw transport, text containing quotes or backslashes goes in code unchanged with summary codex2api.raw/CATALOG_NAME/FIELD. " +
		"Do not send the rejected payload again."
}
