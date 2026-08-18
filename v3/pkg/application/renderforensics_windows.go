//go:build windows && !server

package application

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v3/internal/webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

// Render-hang forensics — the Windows half. Every decision here is delegated
// to renderforensics.go, which is untagged and unit-tested; what lives here is
// the side effects: the COM enumeration, the dbghelp minidump write, and the
// files.
//
// The shape of the thing: beginRenderRecovery calls captureRenderHang on the
// main thread the moment a hang is declared. That does the COM work
// synchronously — GetProcessInfos has to run on the thread that owns the
// environment, and it is a cheap snapshot read — and hands the resulting plain
// values to a goroutine that writes the breadcrumb and the dumps. The
// goroutine is the whole point: the recovery escalation must proceed at its
// own pace whether or not capture succeeds, and a MiniDumpWriteDump of a
// wedged renderer can take seconds. If rebuildWebView's controller Close reaps
// the process tree mid-dump the write fails, the failure is logged and
// breadcrumbed, and the episode record is still on disk.

// MINIDUMP_TYPE flags, from minidumpapiset.h. Only the four that matter for a
// hang are set: what is wanted is every thread's stack and enough module
// information to symbolise it, NOT the address space — MiniDumpWithFullMemory
// on a renderer would be hundreds of megabytes and would take long enough that
// the process would be reaped before the write finished.
const (
	miniDumpNormal                = 0x00000000
	miniDumpWithUnloadedModules   = 0x00000020
	miniDumpWithProcessThreadData = 0x00000100
	miniDumpWithThreadInfo        = 0x00001000

	renderHangMiniDumpType = miniDumpNormal |
		miniDumpWithThreadInfo |
		miniDumpWithProcessThreadData |
		miniDumpWithUnloadedModules
)

var (
	moddbghelp            = syscall.NewLazyDLL("dbghelp.dll")
	procMiniDumpWriteDump = moddbghelp.NewProc("MiniDumpWriteDump")
)

// renderForensicsWriteMu serialises the directory work. Several windows can
// hang at once, and their capture goroutines would otherwise interleave an
// append, a trim and a retention sweep over the same two paths.
var renderForensicsWriteMu sync.Mutex

// renderHangCapture is everything the capture goroutine needs, in plain
// values. Deliberately holds no COM reference and no pointer back into the
// window: by the time it runs, the controller it describes may already have
// been closed and replaced.
type renderHangCapture struct {
	dir            string
	episode        uint64
	window         uint64
	reason         string
	runtimeVersion string
	browserPID     uint32
	processes      []renderForensicsProcess
	captureError   string
}

// captureRenderHang opens the forensic record for a hang episode and kicks off
// the dump writes. Main thread only, and never blocking: the COM calls are
// snapshot reads and everything after them is on a goroutine.
func (w *windowsWebviewWindow) captureRenderHang(episode uint64, reason string) {
	dir := globalApplication.options.Windows.RenderForensicsDir
	if !w.renderForensics.claimCapture(dir, episode, time.Now()) {
		if dir == "" {
			globalApplication.info("webview2: render-hang forensics are disabled; set Options.Windows.RenderForensicsDir to capture renderer minidumps",
				"window", w.parent.id, "episode", episode)
		}
		return
	}

	capture := renderHangCapture{
		dir:     dir,
		episode: episode,
		window:  uint64(w.parent.id),
		reason:  reason,
	}
	if w.chromium == nil {
		capture.captureError = "process enumeration unavailable: the window has no WebView2 instance"
	} else {
		capture.runtimeVersion = w.chromium.RuntimeVersion()
		// The browser process id comes off the ICoreWebView2, which is only
		// safe to call once the controller's COM setup has finished — see
		// edge.Chromium.IsReady. Recovery can open an episode before that.
		if !w.chromium.IsReady() {
			capture.captureError = "browser process id unavailable: the controller is not ready"
		} else if pid, err := w.chromium.BrowserProcessID(); err != nil {
			capture.captureError = "browser process id unavailable: " + err.Error()
		} else {
			capture.browserPID = pid
		}
		// The process snapshot hangs off the environment, not the controller,
		// so it is readable even on the not-ready path above.
		infos, err := w.chromium.ProcessInfos()
		if err != nil {
			// Old runtime, or no environment yet. Degrade: the breadcrumb
			// still records the episode, there is just nothing safe to dump.
			capture.captureError = appendCaptureError(capture.captureError,
				"process enumeration unavailable: "+err.Error())
		} else {
			capture.processes = renderProcessesFromEdge(infos)
		}
	}

	targets := renderDumpTargets(capture.processes)
	if len(targets) == 0 {
		globalApplication.error(
			"webview2: render-hang forensics for window %v episode %d found no renderer process to dump (%s); writing a breadcrumb only into %s",
			w.parent.id, episode, capture.captureError, dir)
	} else {
		globalApplication.error(
			"webview2: capturing render-hang forensics for window %v episode %d into %s; renderer pids %s",
			w.parent.id, episode, dir, renderPIDList(targets))
	}

	go func() {
		defer handlePanic()
		runRenderHangCapture(capture)
	}()
}

// recordRenderEpisodeClosed appends the episode's disposition. Called from
// endRenderRecovery for every path that closes an episode — a re-navigation
// that committed, the controller rebuild, a failed rebuild's retry, the window
// going away — so the breadcrumb always says how the episode ended and how
// long it took. No-op unless this episode was captured.
func (w *windowsWebviewWindow) recordRenderEpisodeClosed(episode uint64, outcome string) {
	dir := globalApplication.options.Windows.RenderForensicsDir
	if dir == "" {
		return
	}
	duration, ok := w.renderForensics.closeCapture(episode, time.Now())
	if !ok {
		return
	}
	record := renderForensicsRecord{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Event:      renderForensicsEventEpisodeEnded,
		Episode:    episode,
		Window:     uint64(w.parent.id),
		Outcome:    outcome,
		DurationMs: duration.Milliseconds(),
	}
	go func() {
		defer handlePanic()
		appendRenderBreadcrumb(dir, record)
	}()
}

// renderProcessesFromEdge flattens the COM snapshot into the value type the
// breadcrumb and the dump decision use.
func renderProcessesFromEdge(infos []edge.ProcessInfo) []renderForensicsProcess {
	processes := make([]renderForensicsProcess, 0, len(infos))
	for _, info := range infos {
		processes = append(processes, renderForensicsProcess{
			PID:      info.ProcessID,
			Kind:     info.Kind.String(),
			Renderer: info.Kind == edge.COREWEBVIEW2_PROCESS_KIND_RENDERER,
		})
	}
	return processes
}

// appendCaptureError joins two capture problems into one breadcrumb field.
func appendCaptureError(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
}

// runRenderHangCapture is the goroutine body: breadcrumb first, then dumps,
// then retention. The breadcrumb goes down before any dump is attempted on
// purpose — a dump can be killed halfway by the recovery reaping the process
// tree, and the record of what was hung and why must survive that.
func runRenderHangCapture(capture renderHangCapture) {
	if err := os.MkdirAll(capture.dir, 0o755); err != nil {
		globalApplication.error("webview2: render-hang forensics directory %s is unusable: %v", capture.dir, err)
		return
	}
	now := time.Now().UTC()
	targets := renderDumpTargets(capture.processes)

	appendRenderBreadcrumb(capture.dir, renderForensicsRecord{
		Timestamp:      now.Format(time.RFC3339Nano),
		Event:          renderForensicsEventHang,
		Episode:        capture.episode,
		Window:         capture.window,
		Reason:         capture.reason,
		RuntimeVersion: capture.runtimeVersion,
		BrowserPID:     capture.browserPID,
		Processes:      renderProcessSummary(capture.processes),
		DumpTargets:    renderPIDList(targets),
		Error:          capture.captureError,
	})

	for _, target := range targets {
		path := filepath.Join(capture.dir, renderDumpFileName(now, target.PID))
		record := renderForensicsRecord{
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Event:     renderForensicsEventDump,
			Episode:   capture.episode,
			Window:    capture.window,
			PID:       target.PID,
			Kind:      target.Kind,
			Path:      path,
		}
		size, err := writeRenderMiniDump(target.PID, path)
		if err != nil {
			record.Result = renderForensicsResultFailed
			record.Error = err.Error()
			globalApplication.error("webview2: render-hang minidump of pid %d failed: %v", target.PID, err)
		} else {
			record.Result = renderForensicsResultOK
			record.Bytes = size
			globalApplication.info("webview2: render-hang minidump written",
				"pid", target.PID, "path", path, "bytes", size)
		}
		appendRenderBreadcrumb(capture.dir, record)
	}

	evictOldRenderDumps(capture.dir)
}

// writeRenderMiniDump dumps one process to path and returns the bytes written.
// The dump type is thread stacks plus module/unloaded-module lists — see
// renderHangMiniDumpType — which is what a hang investigation needs and is
// small and fast enough to have a chance of finishing before the recovery
// reaps the target.
func writeRenderMiniDump(pid uint32, path string) (int64, error) {
	// Find rather than Call-and-pray: LazyProc.Call panics when the DLL or
	// the entry point is missing, and this runs on a goroutine.
	if err := procMiniDumpWriteDump.Find(); err != nil {
		return 0, fmt.Errorf("dbghelp!MiniDumpWriteDump unavailable: %w", err)
	}
	// PROCESS_VM_READ is the part that matters and the part a
	// LIMITED_INFORMATION fallback could not supply: without it dbghelp
	// cannot read the target's stacks, so there is no degraded mode to fall
	// back to. A refusal here is recorded and the capture moves on.
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return 0, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", path, err)
	}

	written, _, callErr := procMiniDumpWriteDump.Call(
		uintptr(handle),
		uintptr(pid),
		file.Fd(),
		uintptr(renderHangMiniDumpType),
		0, // ExceptionParam — none; this is a hang, not a crash
		0, // UserStreamParam
		0, // CallbackParam
	)
	if written == 0 {
		_ = file.Close()
		// A failed dump leaves a truncated file behind that looks like
		// evidence and is not.
		_ = os.Remove(path)
		return 0, fmt.Errorf("MiniDumpWriteDump(%d): %w", pid, callErr)
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if closeErr != nil {
		return 0, fmt.Errorf("close %s: %w", path, closeErr)
	}
	if statErr != nil {
		// The dump landed; only its size is unknown.
		return 0, nil
	}
	return info.Size(), nil
}

// appendRenderBreadcrumb appends one record to the JSONL log and enforces its
// size cap. Failures are logged and never fatal — forensics must not be able
// to take the app down.
func appendRenderBreadcrumb(dir string, record renderForensicsRecord) {
	line, err := renderForensicsLine(record)
	if err != nil {
		globalApplication.error("webview2: could not encode a render-hang breadcrumb: %v", err)
		return
	}
	path := filepath.Join(dir, renderForensicsBreadcrumbFile)

	renderForensicsWriteMu.Lock()
	defer renderForensicsWriteMu.Unlock()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		globalApplication.error("webview2: could not open the render-hang breadcrumb log %s: %v", path, err)
		return
	}
	_, writeErr := file.Write(line)
	closeErr := file.Close()
	if writeErr != nil {
		globalApplication.error("webview2: could not append to the render-hang breadcrumb log %s: %v", path, writeErr)
		return
	}
	if closeErr != nil {
		globalApplication.error("webview2: could not close the render-hang breadcrumb log %s: %v", path, closeErr)
		return
	}
	trimRenderBreadcrumbFile(path)
}

// trimRenderBreadcrumbFile rewrites the log with its newest half once it is
// over cap. Caller holds renderForensicsWriteMu.
func trimRenderBreadcrumbFile(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= renderForensicsBreadcrumbMaxBytes {
		return
	}
	content, err := os.ReadFile(path)
	if err != nil {
		globalApplication.error("webview2: could not read the render-hang breadcrumb log %s to trim it: %v", path, err)
		return
	}
	trimmed, changed := trimRenderBreadcrumbs(content, renderForensicsBreadcrumbMaxBytes)
	if !changed {
		return
	}
	if err := os.WriteFile(path, trimmed, 0o600); err != nil {
		globalApplication.error("webview2: could not trim the render-hang breadcrumb log %s: %v", path, err)
		return
	}
	globalApplication.info("webview2: trimmed the render-hang breadcrumb log",
		"path", path, "was", info.Size(), "now", len(trimmed))
}

// evictOldRenderDumps enforces the dump retention bound. Only files this
// package named are candidates; anything else in the directory is left alone.
func evictOldRenderDumps(dir string) {
	renderForensicsWriteMu.Lock()
	defer renderForensicsWriteMu.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		globalApplication.error("webview2: could not list the render-hang forensics directory %s: %v", dir, err)
		return
	}
	files := make([]renderDumpFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isRenderDumpFile(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, renderDumpFile{Name: entry.Name(), ModTime: info.ModTime()})
	}
	for _, name := range renderDumpsToEvict(files, renderForensicsDumpKeep) {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			globalApplication.error("webview2: could not delete the old render-hang dump %s: %v", name, err)
			continue
		}
		globalApplication.info("webview2: deleted an old render-hang dump", "file", name)
	}
}
