//go:build windows

package edge

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ICoreWebView2ProcessInfoCollection is the snapshot returned by
// ICoreWebView2Environment8::GetProcessInfos.
//
// Vtable layout from internal/webview2/scripts/WebView2.1.0.2903.40.idl:
//
//	[uuid(402b99cd-a0cc-4fa5-b7a5-51d86a1d2339)]
//	interface ICoreWebView2ProcessInfoCollection : IUnknown {
//	  [propget] HRESULT Count([out, retval] UINT32* value);      // slot 3
//	  HRESULT GetValueAtIndex([in] UINT32 index,                 // slot 4
//	      [out, retval] ICoreWebView2ProcessInfo** value);
//	}
type iCoreWebView2ProcessInfoCollectionVtbl struct {
	_IUnknownVtbl
	GetCount        ComProc
	GetValueAtIndex ComProc
}

type ICoreWebView2ProcessInfoCollection struct {
	vtbl *iCoreWebView2ProcessInfoCollectionVtbl
}

func (i *ICoreWebView2ProcessInfoCollection) AddRef() uint32 {
	ret, _, _ := i.vtbl.AddRef.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

func (i *ICoreWebView2ProcessInfoCollection) Release() uint32 {
	ret, _, _ := i.vtbl.Release.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

func (i *ICoreWebView2ProcessInfoCollection) GetCount() (uint32, error) {
	var value uint32
	hr, _, _ := i.vtbl.GetCount.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&value)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return 0, syscall.Errno(hr)
	}
	return value, nil
}

// GetValueAtIndex returns the process info at index. The index is an [in]
// UINT32 passed by value — not by address; the generated binding in
// internal/webview2/pkg/webview2 gets this wrong and is not used here.
func (i *ICoreWebView2ProcessInfoCollection) GetValueAtIndex(index uint32) (*ICoreWebView2ProcessInfo, error) {
	var value *ICoreWebView2ProcessInfo
	hr, _, _ := i.vtbl.GetValueAtIndex.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(index),
		uintptr(unsafe.Pointer(&value)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return nil, syscall.Errno(hr)
	}
	return value, nil
}
