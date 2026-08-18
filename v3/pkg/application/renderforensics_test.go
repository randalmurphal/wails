package application

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRenderForensicsClaimCapture(t *testing.T) {
	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		dir     string
		before  renderForensicsState
		episode uint64
		want    bool
	}{
		{
			name:    "disabled when no directory is configured",
			dir:     "",
			episode: 1,
			want:    false,
		},
		{
			name:    "first episode is claimed",
			dir:     `C:\forensics`,
			episode: 1,
			want:    true,
		},
		{
			name:    "episode zero is never claimed",
			dir:     `C:\forensics`,
			episode: 0,
			want:    false,
		},
		{
			name:    "the same episode is not claimed twice",
			dir:     `C:\forensics`,
			before:  renderForensicsState{lastEpisode: 4, open: true},
			episode: 4,
			want:    false,
		},
		{
			name:    "a stale episode number is refused",
			dir:     `C:\forensics`,
			before:  renderForensicsState{lastEpisode: 7},
			episode: 3,
			want:    false,
		},
		{
			name:    "the next episode is claimed after the previous closed",
			dir:     `C:\forensics`,
			before:  renderForensicsState{lastEpisode: 4},
			episode: 5,
			want:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := test.before
			if got := state.claimCapture(test.dir, test.episode, base); got != test.want {
				t.Fatalf("claimCapture = %v, want %v", got, test.want)
			}
			if test.want && state.lastEpisode != test.episode {
				t.Fatalf("lastEpisode = %d, want %d", state.lastEpisode, test.episode)
			}
			if test.want && !state.open {
				t.Fatal("a claimed capture must leave the episode open")
			}
		})
	}
}

func TestRenderForensicsCloseCapture(t *testing.T) {
	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	t.Run("reports the episode duration once", func(t *testing.T) {
		var state renderForensicsState
		if !state.claimCapture(`C:\forensics`, 2, base) {
			t.Fatal("claimCapture refused a fresh episode")
		}
		duration, ok := state.closeCapture(2, base.Add(7*time.Second))
		if !ok {
			t.Fatal("closeCapture refused the open episode")
		}
		if duration != 7*time.Second {
			t.Fatalf("duration = %v, want 7s", duration)
		}
		// endRenderRecovery is called by several unrelated paths; only the
		// first may produce a record.
		if _, ok := state.closeCapture(2, base.Add(9*time.Second)); ok {
			t.Fatal("closeCapture reported the same episode twice")
		}
	})

	t.Run("refuses an episode that was never captured", func(t *testing.T) {
		var state renderForensicsState
		if _, ok := state.closeCapture(1, base); ok {
			t.Fatal("closeCapture accepted an uncaptured episode")
		}
	})

	t.Run("refuses a different episode", func(t *testing.T) {
		var state renderForensicsState
		state.claimCapture(`C:\forensics`, 3, base)
		if _, ok := state.closeCapture(2, base.Add(time.Second)); ok {
			t.Fatal("closeCapture accepted a mismatched episode")
		}
	})

	t.Run("clamps a backwards clock step to zero", func(t *testing.T) {
		var state renderForensicsState
		state.claimCapture(`C:\forensics`, 1, base)
		duration, ok := state.closeCapture(1, base.Add(-time.Minute))
		if !ok {
			t.Fatal("closeCapture refused the open episode")
		}
		if duration != 0 {
			t.Fatalf("duration = %v, want 0", duration)
		}
	})
}

func TestRenderDumpTargets(t *testing.T) {
	processes := []renderForensicsProcess{
		{PID: 100, Kind: "browser"},
		{PID: 200, Kind: "renderer", Renderer: true},
		{PID: 300, Kind: "gpu"},
		{PID: 400, Kind: "renderer", Renderer: true},
		{PID: 500, Kind: "utility"},
	}
	targets := renderDumpTargets(processes)
	if len(targets) != 2 || targets[0].PID != 200 || targets[1].PID != 400 {
		t.Fatalf("renderDumpTargets = %+v, want the two renderer pids", targets)
	}
	if got := renderDumpTargets(nil); got != nil {
		t.Fatalf("renderDumpTargets(nil) = %+v, want nil", got)
	}
	if got := renderDumpTargets([]renderForensicsProcess{{PID: 1, Kind: "browser"}}); got != nil {
		t.Fatalf("renderDumpTargets with no renderer = %+v, want nil", got)
	}
}

func TestRenderProcessSummary(t *testing.T) {
	processes := []renderForensicsProcess{
		{PID: 100, Kind: "browser"},
		{PID: 200, Kind: "renderer", Renderer: true},
		{PID: 300, Kind: "gpu"},
	}
	if got, want := renderProcessSummary(processes), "browser:100 renderer:200 gpu:300"; got != want {
		t.Fatalf("renderProcessSummary = %q, want %q", got, want)
	}
	if got := renderProcessSummary(nil); got != "" {
		t.Fatalf("renderProcessSummary(nil) = %q, want empty", got)
	}
	if got, want := renderPIDList(processes), "100 200 300"; got != want {
		t.Fatalf("renderPIDList = %q, want %q", got, want)
	}
	if got := renderPIDList(nil); got != "" {
		t.Fatalf("renderPIDList(nil) = %q, want empty", got)
	}
}

func TestRenderDumpFileNameAndRecognition(t *testing.T) {
	stamp := time.Date(2026, 8, 18, 17, 4, 5, 123000000, time.UTC)
	name := renderDumpFileName(stamp, 4242)
	if want := "renderer-hang-20260818T170405.123Z-pid4242.dmp"; name != want {
		t.Fatalf("renderDumpFileName = %q, want %q", name, want)
	}
	if !isRenderDumpFile(name) {
		t.Fatalf("isRenderDumpFile(%q) = false, want true", name)
	}
	// Retention must never touch anything this package did not write.
	for _, other := range []string{
		"render-hang-breadcrumbs.jsonl",
		"renderer-hang-.dmp",
		"crash.dmp",
		"renderer-hang-20260818T170405.123Z-pid1.dmp.tmp",
		"",
	} {
		if isRenderDumpFile(other) {
			t.Fatalf("isRenderDumpFile(%q) = true, want false", other)
		}
	}
	// A local-zone timestamp must still produce a UTC name.
	local := stamp.In(time.FixedZone("UTC+5", 5*3600))
	if got := renderDumpFileName(local, 4242); got != name {
		t.Fatalf("renderDumpFileName is not UTC-normalised: %q vs %q", got, name)
	}
}

func TestRenderDumpsToEvict(t *testing.T) {
	at := func(minute int) time.Time {
		return time.Date(2026, 8, 18, 12, minute, 0, 0, time.UTC)
	}

	tests := []struct {
		name  string
		files []renderDumpFile
		keep  int
		want  []string
	}{
		{
			name: "nothing to do under the bound",
			files: []renderDumpFile{
				{Name: "b.dmp", ModTime: at(2)},
				{Name: "a.dmp", ModTime: at(1)},
			},
			keep: 3,
		},
		{
			name: "nothing to do exactly at the bound",
			files: []renderDumpFile{
				{Name: "a.dmp", ModTime: at(1)},
				{Name: "b.dmp", ModTime: at(2)},
				{Name: "c.dmp", ModTime: at(3)},
			},
			keep: 3,
		},
		{
			name: "oldest first, newest kept",
			files: []renderDumpFile{
				{Name: "c.dmp", ModTime: at(3)},
				{Name: "a.dmp", ModTime: at(1)},
				{Name: "e.dmp", ModTime: at(5)},
				{Name: "b.dmp", ModTime: at(2)},
				{Name: "d.dmp", ModTime: at(4)},
			},
			keep: 3,
			want: []string{"a.dmp", "b.dmp"},
		},
		{
			name: "equal mtimes break on name for determinism",
			files: []renderDumpFile{
				{Name: "z.dmp", ModTime: at(1)},
				{Name: "a.dmp", ModTime: at(1)},
				{Name: "m.dmp", ModTime: at(1)},
			},
			keep: 1,
			want: []string{"a.dmp", "m.dmp"},
		},
		{
			name: "keep zero evicts everything",
			files: []renderDumpFile{
				{Name: "a.dmp", ModTime: at(1)},
			},
			keep: 0,
			want: []string{"a.dmp"},
		},
		{
			name: "a negative bound is treated as zero",
			files: []renderDumpFile{
				{Name: "a.dmp", ModTime: at(1)},
			},
			keep: -2,
			want: []string{"a.dmp"},
		},
		{
			name: "empty directory",
			keep: 3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := make([]renderDumpFile, len(test.files))
			copy(input, test.files)
			got := renderDumpsToEvict(input, test.keep)
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("renderDumpsToEvict = %v, want %v", got, test.want)
			}
			for i := range input {
				if input[i] != test.files[i] {
					t.Fatal("renderDumpsToEvict reordered its input")
				}
			}
		})
	}
}

func TestTrimRenderBreadcrumbs(t *testing.T) {
	t.Run("under the cap is left alone", func(t *testing.T) {
		content := []byte("a\nb\nc\n")
		got, changed := trimRenderBreadcrumbs(content, 1024)
		if changed {
			t.Fatal("trimmed a log that was under the cap")
		}
		if string(got) != string(content) {
			t.Fatalf("content changed: %q", got)
		}
	})

	t.Run("exactly at the cap is left alone", func(t *testing.T) {
		content := []byte("abcde\n")
		if _, changed := trimRenderBreadcrumbs(content, len(content)); changed {
			t.Fatal("trimmed a log that was exactly at the cap")
		}
	})

	t.Run("keeps the newest half on a record boundary", func(t *testing.T) {
		content := []byte("one\ntwo\nthree\nfour\nfive\n")
		got, changed := trimRenderBreadcrumbs(content, 10)
		if !changed {
			t.Fatal("did not trim an over-cap log")
		}
		if len(got) >= len(content) {
			t.Fatalf("trim did not shrink the log: %q", got)
		}
		if len(got) > 0 && got[len(got)-1] != '\n' {
			t.Fatalf("trimmed log does not end on a record boundary: %q", got)
		}
		for _, record := range strings.Split(strings.TrimSuffix(string(got), "\n"), "\n") {
			if record == "" {
				t.Fatalf("trimmed log has an empty record: %q", got)
			}
		}
		if !strings.HasSuffix(string(got), "five\n") {
			t.Fatalf("trim dropped the newest records: %q", got)
		}
		if strings.Contains(string(got), "one") {
			t.Fatalf("trim kept the oldest record: %q", got)
		}
	})

	t.Run("a pathological single record is dropped rather than truncated", func(t *testing.T) {
		content := []byte(strings.Repeat("x", 100))
		got, changed := trimRenderBreadcrumbs(content, 10)
		if !changed {
			t.Fatal("did not trim an over-cap log")
		}
		if len(got) != 0 {
			t.Fatalf("kept a partial record: %q", got)
		}
	})

	t.Run("a non-positive cap disables trimming", func(t *testing.T) {
		content := []byte("one\ntwo\n")
		if _, changed := trimRenderBreadcrumbs(content, 0); changed {
			t.Fatal("trimmed with a zero cap")
		}
	})
}

func TestRenderForensicsLine(t *testing.T) {
	record := renderForensicsRecord{
		Timestamp:      "2026-08-18T17:04:05.123456789Z",
		Event:          renderForensicsEventHang,
		Episode:        2,
		Window:         1,
		Reason:         "renderer ran no script for 20s",
		RuntimeVersion: "150.0.3296.83",
		BrowserPID:     1234,
		Processes:      "browser:1234 renderer:5678",
		DumpTargets:    "5678",
	}
	line, err := renderForensicsLine(record)
	if err != nil {
		t.Fatalf("renderForensicsLine: %v", err)
	}
	if line[len(line)-1] != '\n' {
		t.Fatal("breadcrumb line is not newline terminated")
	}
	// One flat object per line: a support engineer greps this.
	var decoded map[string]any
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatalf("breadcrumb line is not valid JSON: %v", err)
	}
	for key, value := range decoded {
		switch value.(type) {
		case map[string]any, []any:
			t.Fatalf("breadcrumb field %q is nested; the schema must stay flat", key)
		}
	}
	for _, key := range []string{"ts", "event", "episode", "window", "reason", "runtimeVersion", "browserPid", "processes", "dumpTargets"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("breadcrumb is missing %q: %s", key, line)
		}
	}
	// Fields belonging to other event shapes must not appear.
	for _, key := range []string{"pid", "path", "result", "bytes", "outcome", "durationMs", "error"} {
		if _, ok := decoded[key]; ok {
			t.Fatalf("hang breadcrumb carries %q, which belongs to another event: %s", key, line)
		}
	}
}
