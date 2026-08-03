//go:build windows

package edge

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type _ICoreWebView2ProcessFailedEventArgsVtbl struct {
	_IUnknownVtbl
	GetProcessFailedKind ComProc
}

type ICoreWebView2ProcessFailedEventArgs struct {
	vtbl *_ICoreWebView2ProcessFailedEventArgsVtbl
}

func (i *ICoreWebView2ProcessFailedEventArgs) GetProcessFailedKind() (COREWEBVIEW2_PROCESS_FAILED_KIND, error) {
	kind := COREWEBVIEW2_PROCESS_FAILED_KIND(0xffffffff)
	hr, _, _ := i.vtbl.GetProcessFailedKind.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&kind)),
	)

	if windows.Handle(hr) != windows.S_OK {
		return 0, syscall.Errno(hr)
	}

	if kind == 0xffffffff {
		return 0, fmt.Errorf("unknown error")
	}

	return kind, nil
}

type _ICoreWebView2ProcessFailedEventArgs2Vtbl struct {
	_IUnknownVtbl
	GetProcessFailedKind          ComProc
	GetReason                     ComProc
	GetExitCode                   ComProc
	GetProcessDescription         ComProc
	GetFrameInfosForFailedProcess ComProc
}

type ICoreWebView2ProcessFailedEventArgs2 struct {
	vtbl *_ICoreWebView2ProcessFailedEventArgs2Vtbl
}

// GetICoreWebView2ProcessFailedEventArgs2 queries for the extended failure
// diagnostics interface (WebView2 SDK 1.0.1108+). Returns nil when the
// installed runtime does not provide it. The caller owns the returned
// reference and must Release it.
func (i *ICoreWebView2ProcessFailedEventArgs) GetICoreWebView2ProcessFailedEventArgs2() *ICoreWebView2ProcessFailedEventArgs2 {
	var result *ICoreWebView2ProcessFailedEventArgs2

	iidICoreWebView2ProcessFailedEventArgs2 := NewGUID("{4dab9422-46fa-4c3e-a5d2-41d2071d3680}")
	_, _, _ = i.vtbl.QueryInterface.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(iidICoreWebView2ProcessFailedEventArgs2)),
		uintptr(unsafe.Pointer(&result)))

	return result
}

func (i *ICoreWebView2ProcessFailedEventArgs2) Release() uintptr {
	ret, _, _ := i.vtbl.Release.Call(uintptr(unsafe.Pointer(i)))

	return ret
}

func (i *ICoreWebView2ProcessFailedEventArgs2) GetReason() (COREWEBVIEW2_PROCESS_FAILED_REASON, error) {
	var reason COREWEBVIEW2_PROCESS_FAILED_REASON
	hr, _, _ := i.vtbl.GetReason.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&reason)),
	)

	if windows.Handle(hr) != windows.S_OK {
		return 0, syscall.Errno(hr)
	}

	return reason, nil
}

// GetExitCode returns the exit code of the failed process. Only meaningful
// for the *_PROCESS_EXITED kinds; for RENDER_PROCESS_UNRESPONSIVE the
// process is still running and the code is STILL_ACTIVE (259).
func (i *ICoreWebView2ProcessFailedEventArgs2) GetExitCode() (int32, error) {
	var exitCode int32
	hr, _, _ := i.vtbl.GetExitCode.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&exitCode)),
	)

	if windows.Handle(hr) != windows.S_OK {
		return 0, syscall.Errno(hr)
	}

	return exitCode, nil
}

// GetProcessDescription returns the description of the failed process.
// Populated for utility processes (e.g. "Audio Service"); empty for most
// other kinds.
func (i *ICoreWebView2ProcessFailedEventArgs2) GetProcessDescription() (string, error) {
	var _description *uint16
	hr, _, _ := i.vtbl.GetProcessDescription.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&_description)),
	)

	if windows.Handle(hr) != windows.S_OK {
		return "", syscall.Errno(hr)
	}

	description := windows.UTF16PtrToString(_description)
	windows.CoTaskMemFree(unsafe.Pointer(_description))
	return description, nil
}
