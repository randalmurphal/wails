//go:build windows

package edge

import (
	"errors"
	"fmt"
)

// Process enumeration for the host's render-hang forensics.
//
// A WebView2 instance is a process tree: one browser process, one or more
// renderers, a GPU process and assorted utility processes, all children of the
// browser. When the host's render watchdog declares a hang it needs to know
// WHICH of them is the renderer before it can capture anything useful, and the
// only supported way to ask is ICoreWebView2Environment8::GetProcessInfos.

// ErrProcessInfosUnsupported is returned when the installed WebView2 runtime
// has no ICoreWebView2Environment8, or when the environment is not up yet. The
// caller degrades — it records that enumeration was unavailable — rather than
// guessing which child of the browser process is a renderer.
var ErrProcessInfosUnsupported = errors.New("webview2: ICoreWebView2Environment8 is unavailable; process enumeration is not supported by this runtime")

// ProcessInfo is one WebView2 process, flattened out of the COM collection so
// that it can be carried off the main thread. Nothing in it is a COM
// reference, deliberately: the consumer of this list runs on a goroutine that
// outlives the collection.
type ProcessInfo struct {
	ProcessID uint32
	Kind      COREWEBVIEW2_PROCESS_KIND
}

// ProcessInfos returns a snapshot of every process in this instance's WebView2
// environment. Cheap — it materialises a collection and reads two integers per
// entry — but it is a COM call and must be made on the thread that owns the
// environment, i.e. the main/message-loop thread.
func (e *Chromium) ProcessInfos() ([]ProcessInfo, error) {
	if e.environment == nil {
		return nil, ErrProcessInfosUnsupported
	}
	env8 := e.environment.GetICoreWebView2Environment8()
	if env8 == nil {
		return nil, ErrProcessInfosUnsupported
	}
	defer env8.Release()

	collection, err := env8.GetProcessInfos()
	if err != nil {
		return nil, fmt.Errorf("webview2: GetProcessInfos failed: %w", err)
	}
	if collection == nil {
		return nil, ErrProcessInfosUnsupported
	}
	defer collection.Release()

	count, err := collection.GetCount()
	if err != nil {
		return nil, fmt.Errorf("webview2: process info count failed: %w", err)
	}
	infos := make([]ProcessInfo, 0, count)
	for index := uint32(0); index < count; index++ {
		info, err := collection.GetValueAtIndex(index)
		if err != nil || info == nil {
			// One unreadable entry must not cost the whole snapshot: the
			// renderer may well be another one, and a partial list is still
			// evidence.
			continue
		}
		pid, pidErr := info.GetProcessId()
		kind, kindErr := info.GetKind()
		info.Release()
		if pidErr != nil || kindErr != nil {
			continue
		}
		infos = append(infos, ProcessInfo{ProcessID: pid, Kind: kind})
	}
	return infos, nil
}

// BrowserProcessID returns the process id of the browser process backing this
// instance. COM call; main thread only.
func (e *Chromium) BrowserProcessID() (uint32, error) {
	if e.webview == nil {
		return 0, errors.New("webview2: no webview to query the browser process id from")
	}
	return e.webview.GetBrowserProcessID()
}

// RuntimeVersion is the WebView2 runtime version string this instance
// resolved at Embed time, or "" if it never got that far. Safe to read from
// any goroutine once the instance is up: it is written once during Embed and
// never again.
func (e *Chromium) RuntimeVersion() string {
	return e.webview2RuntimeVersion
}
