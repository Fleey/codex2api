package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/codex2api/proxy/basispoints"
	"github.com/codex2api/security"
)

// excelBPSLogMaxBytes caps logs/excel_bps.log; a full file moves to
// excel_bps.log.1 (replacing the previous one), so at most two files remain.
const excelBPSLogMaxBytes = 20 << 20

// excelBPSLogFile keeps a copy of the [excel-bps] operator lines, which
// otherwise only reach the process output, in logs/excel_bps.log.
type excelBPSLogFile struct {
	mu   sync.Mutex
	name string
	file *os.File
	size int64
	// maxBytes overrides excelBPSLogMaxBytes when positive.
	maxBytes int64
	// failed stops retrying after the file could not be opened.
	failed bool
}

var excelBPSLogger = &excelBPSLogFile{name: "excel_bps.log"}

func init() {
	basispoints.SetLogHook(excelBPSLogger.write)
}

func (l *excelBPSLogFile) write(line string) {
	if security.FileLogsDisabled() {
		return
	}
	// Lines already name only shapes and identifiers; they are kept exactly as
	// printed so request IDs still match the process output.
	entry := time.Now().Format("2006/01/02 15:04:05") + " " + line + "\n"
	limit := int64(excelBPSLogMaxBytes)
	if l.maxBytes > 0 {
		limit = l.maxBytes
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil && l.size > 0 && l.size+int64(len(entry)) > limit {
		l.rotate()
	}
	if l.file == nil && !l.open() {
		return
	}
	n, err := l.file.WriteString(entry)
	l.size += int64(n)
	if err != nil {
		fmt.Fprintf(os.Stderr, "写入日志文件 %s 失败: %v\n", l.name, err)
	}
}

// open must be called with mu held.
func (l *excelBPSLogFile) open() bool {
	if l.failed {
		return false
	}
	dir := errorLogDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		l.failed = true
		fmt.Fprintf(os.Stderr, "创建日志目录失败: %v\n", err)
		return false
	}
	f, err := os.OpenFile(filepath.Join(dir, l.name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		l.failed = true
		fmt.Fprintf(os.Stderr, "打开日志文件 %s 失败: %v\n", l.name, err)
		return false
	}
	size := int64(0)
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	l.file, l.size = f, size
	return true
}

// rotate must be called with mu held; the next write reopens the file.
func (l *excelBPSLogFile) rotate() {
	path := filepath.Join(errorLogDir(), l.name)
	_ = l.file.Close()
	l.file, l.size = nil, 0
	if err := os.Rename(path, path+".1"); err != nil {
		fmt.Fprintf(os.Stderr, "轮转日志文件 %s 失败: %v\n", l.name, err)
	}
}

func (l *excelBPSLogFile) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	if err := l.file.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "关闭日志文件 %s 失败: %v\n", l.name, err)
	}
	l.file = nil
}
