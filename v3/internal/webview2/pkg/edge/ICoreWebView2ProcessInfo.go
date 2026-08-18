//go:build windows

package edge

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ICoreWebView2ProcessInfo describes one process in the WebView2 environment.
//
// Vtable layout from internal/webview2/scripts/WebView2.1.0.2903.40.idl:
//
//	[uuid(84FA7612-3F3D-4FBF-889D-FAD000492D72)]
//	interface ICoreWebView2ProcessInfo : IUnknown {
//	  [propget] HRESULT ProcessId([out, retval] INT32* value);   // slot 3
//	  [propget] HRESULT Kind([out, retval] COREWEBVIEW2_PROCESS_KIND* kind); // slot 4
//	}
//
// It derives from IUnknown directly, so the three IUnknown slots are the whole
// of the inherited prefix.
type iCoreWebView2ProcessInfoVtbl struct {
	_IUnknownVtbl
	GetProcessId ComProc
	GetKind      ComProc
}

type ICoreWebView2ProcessInfo struct {
	vtbl *iCoreWebView2ProcessInfoVtbl
}

func (i *ICoreWebView2ProcessInfo) AddRef() uint32 {
	ret, _, _ := i.vtbl.AddRef.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

func (i *ICoreWebView2ProcessInfo) Release() uint32 {
	ret, _, _ := i.vtbl.Release.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

// GetProcessId returns the OS process id. The IDL types it INT32; every
// caller here wants a Windows process id, which is unsigned, so the sign is
// dropped at the boundary rather than being propagated.
func (i *ICoreWebView2ProcessInfo) GetProcessId() (uint32, error) {
	var value int32
	hr, _, _ := i.vtbl.GetProcessId.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&value)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return 0, syscall.Errno(hr)
	}
	return uint32(value), nil
}

// GetKind returns whether this is the browser, a renderer, the GPU process
// and so on.
func (i *ICoreWebView2ProcessInfo) GetKind() (COREWEBVIEW2_PROCESS_KIND, error) {
	var kind COREWEBVIEW2_PROCESS_KIND
	hr, _, _ := i.vtbl.GetKind.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&kind)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return 0, syscall.Errno(hr)
	}
	return kind, nil
}
