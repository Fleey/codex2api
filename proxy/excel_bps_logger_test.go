package proxy

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/proxy/basispoints"
)

func TestExcelBPSLogLinesAreCopiedToTheirOwnFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOG_DIR", dir)
	t.Setenv("LOG_DISABLED", "")
	var stdout bytes.Buffer
	previousOut := log.Writer()
	log.SetOutput(&stdout)
	file := &excelBPSLogFile{name: "excel_bps.log"}
	basispoints.SetLogHook(file.write)
	t.Cleanup(func() {
		basispoints.SetLogHook(excelBPSLogger.write)
		file.close()
		log.SetOutput(previousOut)
	})

	basispoints.Logf("account=%d request_id=%q ended unsuccessfully", 7, "0b8f5c3e-1111-4222-8333-944445555666")
	content, err := os.ReadFile(filepath.Join(dir, "excel_bps.log"))
	if err != nil {
		t.Fatalf("excel_bps.log was not written: %v", err)
	}
	want := `[excel-bps] account=7 request_id="0b8f5c3e-1111-4222-8333-944445555666" ended unsuccessfully`
	if !strings.Contains(string(content), want) || !strings.Contains(stdout.String(), want) {
		t.Fatalf("line missing: file=%q stdout=%q", content, stdout.String())
	}
}

func TestExcelBPSLogFileRotatesAndHonoursLogDisabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOG_DIR", dir)
	t.Setenv("LOG_DISABLED", "")
	file := &excelBPSLogFile{name: "excel_bps.log", maxBytes: 64}
	t.Cleanup(file.close)
	path := filepath.Join(dir, "excel_bps.log")

	file.write("[excel-bps] first line that fills most of the file")
	file.write("[excel-bps] second line")
	current, _ := os.ReadFile(path)
	previous, err := os.ReadFile(path + ".1")
	if err != nil || !strings.Contains(string(previous), "first line") || strings.Contains(string(current), "first line") || !strings.Contains(string(current), "second line") {
		t.Fatalf("rotation failed: current=%q previous=%q err=%v", current, previous, err)
	}

	t.Setenv("LOG_DISABLED", "true")
	file.write("[excel-bps] suppressed")
	if current, _ = os.ReadFile(path); strings.Contains(string(current), "suppressed") {
		t.Fatalf("LOG_DISABLED line was written: %q", current)
	}
}
