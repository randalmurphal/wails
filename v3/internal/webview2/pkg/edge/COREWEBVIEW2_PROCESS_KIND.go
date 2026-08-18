//go:build windows

package edge

import "strconv"

// COREWEBVIEW2_PROCESS_KIND indicates the process type reported by
// ICoreWebView2ProcessInfo.
//
// Values are the declaration order of `typedef enum COREWEBVIEW2_PROCESS_KIND`
// in internal/webview2/scripts/WebView2.1.0.2903.40.idl (line 1093), which is
// the version-pinned IDL this package is derived from.
type COREWEBVIEW2_PROCESS_KIND uint32

const (
	// The browser process — the one that owns the WebView2 environment.
	COREWEBVIEW2_PROCESS_KIND_BROWSER COREWEBVIEW2_PROCESS_KIND = 0

	// A renderer process. This is the one that runs page script, and the one
	// worth a minidump when the render watchdog declares a hang.
	COREWEBVIEW2_PROCESS_KIND_RENDERER COREWEBVIEW2_PROCESS_KIND = 1

	// A utility process (network service, audio, storage, ...).
	COREWEBVIEW2_PROCESS_KIND_UTILITY COREWEBVIEW2_PROCESS_KIND = 2

	// A sandbox helper process.
	COREWEBVIEW2_PROCESS_KIND_SANDBOX_HELPER COREWEBVIEW2_PROCESS_KIND = 3

	// The GPU process.
	COREWEBVIEW2_PROCESS_KIND_GPU COREWEBVIEW2_PROCESS_KIND = 4

	// A PPAPI plugin process.
	COREWEBVIEW2_PROCESS_KIND_PPAPI_PLUGIN COREWEBVIEW2_PROCESS_KIND = 5

	// A PPAPI broker process.
	COREWEBVIEW2_PROCESS_KIND_PPAPI_BROKER COREWEBVIEW2_PROCESS_KIND = 6
)

// String returns the SDK constant suffix for the process kind, lowercased for
// logs and forensic breadcrumbs.
func (k COREWEBVIEW2_PROCESS_KIND) String() string {
	switch k {
	case COREWEBVIEW2_PROCESS_KIND_BROWSER:
		return "browser"
	case COREWEBVIEW2_PROCESS_KIND_RENDERER:
		return "renderer"
	case COREWEBVIEW2_PROCESS_KIND_UTILITY:
		return "utility"
	case COREWEBVIEW2_PROCESS_KIND_SANDBOX_HELPER:
		return "sandbox_helper"
	case COREWEBVIEW2_PROCESS_KIND_GPU:
		return "gpu"
	case COREWEBVIEW2_PROCESS_KIND_PPAPI_PLUGIN:
		return "ppapi_plugin"
	case COREWEBVIEW2_PROCESS_KIND_PPAPI_BROKER:
		return "ppapi_broker"
	}
	return "unknown(" + strconv.FormatUint(uint64(k), 10) + ")"
}
