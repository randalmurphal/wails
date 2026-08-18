package application

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Render-hang forensics — the decision half.
//
// When the render watchdog declares a hang (webview_window_windows_renderwatch.go)
// the recovery escalation ends in a controller rebuild, and that rebuild's
// Close reaps the whole WebView2 process tree — the wedged renderer with it.
// Three incidents of this class have now been "recovered" by destroying the
// only evidence of why the renderer wedged. Between the moment the hang is
// declared and the moment the tree is reaped there is a window of at least
// renderRecoveryNavigateDeadline (5s), and typically much more, in which the
// renderer is stuck but its process is alive and dumpable. This is what fills
// that window.
//
// Like renderwatch_state.go, this file is deliberately untagged and free of
// every Windows dependency: retention, breadcrumb trimming and capture
// eligibility are the parts that have to be right, and they are unit-tested
// on any host. The COM enumeration and the dbghelp dump write live in
// renderforensics_windows.go, which owns every side effect.

const (
	// renderForensicsBreadcrumbFile is the append-only JSONL log written
	// beside the dumps. One record per event, one flat schema, so that a
	// support engineer can grep it.
	renderForensicsBreadcrumbFile = "render-hang-breadcrumbs.jsonl"

	// renderForensicsDumpKeep is how many .dmp files are retained. Dumps of
	// this shape (thread stacks, no full memory) run tens of megabytes each,
	// and the point of the archive is the most recent incidents; keeping
	// three means a repeat within one session still leaves the first
	// occurrence on disk.
	renderForensicsDumpKeep = 3

	// renderForensicsBreadcrumbMaxBytes caps the breadcrumb log. Records are
	// a few hundred bytes, so this is thousands of episodes — far more than
	// any real deployment produces — and the cap exists so an app that
	// somehow loops cannot fill a disk.
	renderForensicsBreadcrumbMaxBytes = 256 * 1024

	// renderForensicsDumpPrefix / renderForensicsDumpSuffix bound the file
	// names retention is allowed to delete. Retention never removes anything
	// it did not write.
	renderForensicsDumpPrefix = "renderer-hang-"
	renderForensicsDumpSuffix = ".dmp"
)

// Breadcrumb event names. Stable strings — they are what an operator greps
// for — so they are constants rather than literals at the call sites.
const (
	renderForensicsEventHang         = "hang"
	renderForensicsEventDump         = "dump"
	renderForensicsEventEpisodeEnded = "episode-closed"
)

// renderForensicsProcess is one WebView2 process as the breadcrumb sees it.
// Kind is the display spelling; Renderer is what the dump decision reads, so
// that the decision does not depend on how the COM enum happens to render
// itself.
type renderForensicsProcess struct {
	PID      uint32
	Kind     string
	Renderer bool
}

// renderForensicsRecord is the one and only breadcrumb schema. Every event
// shares ts/event/episode/window; the rest is omitempty. One shape means a
// support engineer learns one shape.
type renderForensicsRecord struct {
	Timestamp string `json:"ts"`
	Event     string `json:"event"`
	Episode   uint64 `json:"episode"`
	Window    uint64 `json:"window"`

	// Episode-open fields.
	Reason         string `json:"reason,omitempty"`
	RuntimeVersion string `json:"runtimeVersion,omitempty"`
	BrowserPID     uint32 `json:"browserPid,omitempty"`
	Processes      string `json:"processes,omitempty"`
	DumpTargets    string `json:"dumpTargets,omitempty"`

	// Dump-attempt fields.
	PID    uint32 `json:"pid,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Path   string `json:"path,omitempty"`
	Result string `json:"result,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`

	// Episode-close fields.
	Outcome    string `json:"outcome,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`

	// Set on any record describing something that did not work — a failed
	// enumeration, a refused OpenProcess, a dump that never landed.
	Error string `json:"error,omitempty"`
}

// Result values for dump records.
const (
	renderForensicsResultOK     = "ok"
	renderForensicsResultFailed = "failed"
)

// renderForensicsLine marshals a record into the single newline-terminated
// line that gets appended to the JSONL log.
func renderForensicsLine(record renderForensicsRecord) ([]byte, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// renderForensicsState tracks which episode a capture has already been taken
// for, so that a second hang signal inside one episode cannot start a second
// set of dumps against the same processes. Main-thread only, like the rest of
// the render watchdog state.
type renderForensicsState struct {
	// lastEpisode is the highest episode number a capture has been claimed
	// for. Episode numbers only ever increase, so comparing against it is
	// both the same-episode guard and the out-of-order guard.
	lastEpisode uint64

	// open spans claimCapture to closeCapture, and is what stops an episode
	// close being recorded twice (endRenderRecovery is a no-op-if-not-active
	// call that several unrelated paths make).
	open bool

	// startedAt is when the captured episode was declared, and the base for
	// the duration in the close record.
	startedAt time.Time
}

// claimCapture reports whether this window should capture forensics for
// episode now. False when forensics are disabled (no directory configured),
// and false for an episode already claimed.
func (s *renderForensicsState) claimCapture(dir string, episode uint64, now time.Time) bool {
	if dir == "" || episode == 0 || episode <= s.lastEpisode {
		return false
	}
	s.lastEpisode = episode
	s.open = true
	s.startedAt = now
	return true
}

// closeCapture reports how long the captured episode lasted, and false when
// this episode was never captured or has already been closed out.
func (s *renderForensicsState) closeCapture(episode uint64, now time.Time) (time.Duration, bool) {
	if !s.open || episode == 0 || episode != s.lastEpisode {
		return 0, false
	}
	s.open = false
	elapsed := now.Sub(s.startedAt)
	if elapsed < 0 {
		// A clock step backwards must not produce a negative duration in the
		// record; the episode is at least instantaneous.
		elapsed = 0
	}
	return elapsed, true
}

// renderDumpTargets picks the processes worth a minidump. Renderer-kind only:
// the browser process is alive and responsive by definition (it is what
// reported the hang), and the GPU and utility processes are not where a
// wedged main thread is.
func renderDumpTargets(processes []renderForensicsProcess) []renderForensicsProcess {
	var targets []renderForensicsProcess
	for _, process := range processes {
		if process.Renderer {
			targets = append(targets, process)
		}
	}
	return targets
}

// renderProcessSummary renders the full process list for the breadcrumb, in
// the order the runtime reported it: "browser:1234 renderer:5678 gpu:9012".
func renderProcessSummary(processes []renderForensicsProcess) string {
	if len(processes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(processes))
	for _, process := range processes {
		parts = append(parts, fmt.Sprintf("%s:%d", process.Kind, process.PID))
	}
	return strings.Join(parts, " ")
}

// renderPIDList renders just the pids, for the dumpTargets field and for the
// "capture starting" log line.
func renderPIDList(processes []renderForensicsProcess) string {
	if len(processes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(processes))
	for _, process := range processes {
		parts = append(parts, fmt.Sprintf("%d", process.PID))
	}
	return strings.Join(parts, " ")
}

// renderDumpFileName is the name one dump gets. The stamp is UTC and sorts
// lexicographically, so the directory listing is chronological even before
// retention looks at mtimes.
func renderDumpFileName(now time.Time, pid uint32) string {
	return fmt.Sprintf("%s%s-pid%d%s",
		renderForensicsDumpPrefix,
		now.UTC().Format("20060102T150405.000Z"),
		pid,
		renderForensicsDumpSuffix)
}

// isRenderDumpFile reports whether name is one of ours. Retention is only
// ever allowed to delete files this returns true for — the directory is
// operator-visible and may well hold other things.
func isRenderDumpFile(name string) bool {
	return len(name) > len(renderForensicsDumpPrefix)+len(renderForensicsDumpSuffix) &&
		strings.HasPrefix(name, renderForensicsDumpPrefix) &&
		strings.HasSuffix(name, renderForensicsDumpSuffix)
}

// renderDumpFile is a dump on disk, as retention sees it.
type renderDumpFile struct {
	Name    string
	ModTime time.Time
}

// renderDumpsToEvict names the dumps to delete so that at most keep remain,
// oldest first. Ties on mtime break on name, which is stamped, so the result
// is deterministic even on a filesystem with coarse timestamps. The input is
// not mutated.
func renderDumpsToEvict(files []renderDumpFile, keep int) []string {
	if keep < 0 {
		keep = 0
	}
	if len(files) <= keep {
		return nil
	}
	ordered := make([]renderDumpFile, len(files))
	copy(ordered, files)
	sort.Slice(ordered, func(a, b int) bool {
		if ordered[a].ModTime.Equal(ordered[b].ModTime) {
			return ordered[a].Name < ordered[b].Name
		}
		return ordered[a].ModTime.Before(ordered[b].ModTime)
	})
	evict := make([]string, 0, len(ordered)-keep)
	for _, file := range ordered[:len(ordered)-keep] {
		evict = append(evict, file.Name)
	}
	return evict
}

// trimRenderBreadcrumbs drops the oldest records from an over-cap breadcrumb
// log, keeping the newest half, and reports whether it changed anything. The
// cut is moved forward to the next record boundary so the result is always
// valid JSONL — a half-line at the head would break every reader.
//
// If the newest half contains no boundary at all (one pathological record
// larger than half the cap) everything is dropped rather than a partial line
// being kept: an empty log is readable, a truncated one is not.
func trimRenderBreadcrumbs(content []byte, maxBytes int) ([]byte, bool) {
	if maxBytes <= 0 || len(content) <= maxBytes {
		return content, false
	}
	cut := len(content) / 2
	if boundary := bytes.IndexByte(content[cut:], '\n'); boundary >= 0 {
		cut += boundary + 1
	} else {
		cut = len(content)
	}
	return content[cut:], true
}
