package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/basispoints"
)

// Codex compacts a conversation by the window its model manifest names, which
// for official models is far below the ~1M tokens Excel Basispoints accepts.
// Where every account a key can reach for a model is BPS-enabled, the manifest
// names the BPS window instead, so Codex lets the conversation grow to it. A
// key that can also land on a native account keeps the official window: a
// long conversation sent there would be refused.

// codexManifestAccounts lists the Codex OAuth accounts visible to the key, the
// accounts its manifest and requests are served from.
func (h *Handler) codexManifestAccounts(row *database.APIKeyRow) []*auth.Account {
	if h == nil || h.store == nil {
		return nil
	}
	apiKeyID := int64(0)
	if row != nil {
		apiKeyID = row.ID
	}
	now := time.Now()
	var accounts []*auth.Account
	for _, account := range h.store.Accounts() {
		if account == nil || account.IsRelayStyle() || !h.accountVisibleToAPIKey(account, apiKeyID, now) {
			continue
		}
		accounts = append(accounts, account)
	}
	return accounts
}

func anyExcelBPSAccount(accounts []*auth.Account) bool {
	for _, account := range accounts {
		if account.IsExcelBPSEnabled() {
			return true
		}
	}
	return false
}

// excelBPSOnlyModel reports whether every account that serves slug sends it
// through BPS, and at least one does.
func excelBPSOnlyModel(accounts []*auth.Account, slug string) bool {
	served := false
	for _, account := range accounts {
		if !account.SupportsCodexModel(slug) {
			continue
		}
		if !account.IsExcelBPSAvailableForModel(slug) {
			return false
		}
		served = true
	}
	return served
}

// excelBPSContextWindowFields are the manifest fields Codex sizes a
// conversation by, as excel-codex-bridge sets them for its 1M models.
var excelBPSContextWindowFields = map[string]int{
	"context_window":                   basispoints.ContextWindow,
	"max_context_window":               basispoints.ContextWindow,
	"auto_compact_token_limit":         basispoints.AutoCompactTokenLimit,
	"effective_context_window_percent": 95,
}

// withExcelBPSContextWindows gives the models bpsOnly accepts the BPS context
// window and reports whether any changed. Other fields are kept as they came.
func withExcelBPSContextWindows(body []byte, bpsOnly func(slug string) bool) ([]byte, bool, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false, fmt.Errorf("invalid codex manifest JSON: %w", err)
	}
	var models []map[string]json.RawMessage
	if err := json.Unmarshal(root["models"], &models); err != nil {
		return nil, false, fmt.Errorf("unsupported codex manifest schema: %w", err)
	}
	changed := false
	for _, item := range models {
		var slug string
		if json.Unmarshal(item["slug"], &slug) != nil || strings.TrimSpace(slug) == "" || !bpsOnly(slug) {
			continue
		}
		for field, value := range excelBPSContextWindowFields {
			encoded := json.RawMessage(fmt.Sprint(value))
			if string(item[field]) != string(encoded) {
				item[field] = encoded
				changed = true
			}
		}
	}
	if !changed {
		return body, false, nil
	}
	encoded, err := json.Marshal(models)
	if err != nil {
		return nil, false, err
	}
	root["models"] = encoded
	out, err := json.Marshal(root)
	return out, err == nil, err
}

// applyExcelBPSContextWindows returns body with the BPS window on the models
// only BPS serves for this key, logging and keeping body unchanged when the
// manifest cannot be read.
func applyExcelBPSContextWindows(body []byte, accounts []*auth.Account) ([]byte, bool) {
	out, changed, err := withExcelBPSContextWindows(body, func(slug string) bool {
		return excelBPSOnlyModel(accounts, slug)
	})
	if err != nil {
		basispoints.Logf("kept the official context windows in the Codex manifest: %v", err)
		return body, false
	}
	return out, changed
}
