package proxy

import (
	"encoding/json"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/basispoints"
)

const officialManifest = `{"models":[` +
	`{"slug":"gpt-6-sol","display_name":"6-Sol","context_window":272000,"max_context_window":272000,"auto_compact_token_limit":244800,"future_field":{"kept":true}},` +
	`{"slug":"gpt-5.6-luna","display_name":"5.6-Luna","context_window":272000}],"etag_hint":"x"}`

func manifestModels(t *testing.T, body []byte) map[string]map[string]any {
	t.Helper()
	var root struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	models := make(map[string]map[string]any, len(root.Models))
	for _, model := range root.Models {
		models[model["slug"].(string)] = model
	}
	return models
}

func hasBPSWindow(model map[string]any) bool {
	return model["context_window"] == float64(basispoints.ContextWindow) &&
		model["max_context_window"] == float64(basispoints.ContextWindow) &&
		model["auto_compact_token_limit"] == float64(basispoints.AutoCompactTokenLimit) &&
		model["effective_context_window_percent"] == float64(95)
}

func TestManifestNamesTheBPSWindowOnlyForModelsOnlyBPSServes(t *testing.T) {
	bps := func(id int64, models ...string) *auth.Account {
		return &auth.Account{DBID: id, AccessToken: "bps-token", ExcelBPSEnabled: true, Models: models}
	}
	native := func(id int64, models ...string) *auth.Account {
		return &auth.Account{DBID: id, AccessToken: "native-token", Models: models}
	}
	cases := []struct {
		name     string
		accounts []*auth.Account
		want     map[string]bool
	}{
		{"BPS only", []*auth.Account{bps(1), bps(2)}, map[string]bool{"gpt-6-sol": true, "gpt-5.6-luna": true}},
		{"mixed pool", []*auth.Account{bps(1), native(2)}, map[string]bool{}},
		{"native serves one model", []*auth.Account{bps(1), native(2, "gpt-5.6-luna")}, map[string]bool{"gpt-6-sol": true}},
		{"no BPS account", []*auth.Account{native(1)}, map[string]bool{}},
	}
	for _, tc := range cases {
		store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
		for _, account := range tc.accounts {
			store.AddAccount(account)
		}
		handler := &Handler{store: store}
		accounts := handler.codexManifestAccounts(&database.APIKeyRow{ID: 1})
		if got := anyExcelBPSAccount(accounts); got != (tc.name != "no BPS account") {
			t.Fatalf("%s: anyExcelBPSAccount = %t", tc.name, got)
		}
		body, changed := applyExcelBPSContextWindows([]byte(officialManifest), accounts)
		if changed != (len(tc.want) > 0) {
			t.Fatalf("%s: changed = %t", tc.name, changed)
		}
		models := manifestModels(t, body)
		for slug, model := range models {
			if hasBPSWindow(model) != tc.want[slug] {
				t.Fatalf("%s: %s window = %v, want BPS=%t", tc.name, slug, model, tc.want[slug])
			}
		}
		if sol := models["gpt-6-sol"]; sol["display_name"] != "6-Sol" || sol["future_field"] == nil {
			t.Fatalf("%s: other manifest fields were lost: %v", tc.name, sol)
		}
	}
}

func TestBPSContextWindowFollowsExcelCodexBridge(t *testing.T) {
	// excel-codex-bridge 1M models: 918k window, Codex compacts at 90%, the
	// backend at 95%.
	if basispoints.ContextWindow != 918000 || basispoints.AutoCompactTokenLimit != 826000 {
		t.Fatalf("window=%d auto_compact=%d", basispoints.ContextWindow, basispoints.AutoCompactTokenLimit)
	}
	unchanged, changed, err := withExcelBPSContextWindows([]byte(officialManifest), func(string) bool { return false })
	if err != nil || changed || string(unchanged) != officialManifest {
		t.Fatalf("manifest rewritten without a BPS-only model: changed=%t err=%v", changed, err)
	}
	if _, _, err := withExcelBPSContextWindows([]byte(`{"models":"nope"}`), func(string) bool { return true }); err == nil {
		t.Fatal("malformed manifest was accepted")
	}
}
