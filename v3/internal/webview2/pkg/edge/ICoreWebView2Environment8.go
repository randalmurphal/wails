//go:build windows

package edge

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ICoreWebView2Environment8 adds the ProcessInfos APIs to the environment.
// The only one bound here is GetProcessInfos, which is what the render-hang
// forensics uses to tell the renderer processes apart from the browser, GPU
// and utility processes before it dumps them.
//
// The vtable below is the FULL flattened chain, not just Environment8's own
// three slots: a QueryInterface'd ICoreWebView2Environment8 points at a
// vtable that begins with IUnknown and every ancestor's methods in
// declaration order, and a struct that omitted them would call the wrong
// slot. Order taken from internal/webview2/scripts/WebView2.1.0.2903.40.idl —
// the version-pinned IDL this package is derived from — reading the
// `interface ICoreWebView2EnvironmentN : ICoreWebView2EnvironmentN-1` chain:
//
//	IUnknown                       QueryInterface, AddRef, Release          slots 0-2
//	ICoreWebView2Environment       CreateCoreWebView2Controller,            slots 3-7
//	                               CreateWebResourceResponse,
//	                               get_BrowserVersionString,
//	                               add_NewBrowserVersionAvailable,
//	                               remove_NewBrowserVersionAvailable
//	ICoreWebView2Environment2      CreateWebResourceRequest                 slot 8
//	ICoreWebView2Environment3      CreateCoreWebView2CompositionController, slots 9-10
//	                               CreateCoreWebView2PointerInfo
//	ICoreWebView2Environment4      GetAutomationProviderForWindow           slot 11
//	ICoreWebView2Environment5      add_BrowserProcessExited,                slots 12-13
//	                               remove_BrowserProcessExited
//	ICoreWebView2Environment6      CreatePrintSettings                      slot 14
//	ICoreWebView2Environment7      get_UserDataFolder                       slot 15
//	ICoreWebView2Environment8      add_ProcessInfosChanged,                 slots 16-18
//	                               remove_ProcessInfosChanged,
//	                               GetProcessInfos
//
// The first ten slots agree with iCoreWebView2Environment3Vtbl in
// ICoreWebView2Environment3.go, which is the existing cross-check that this
// prefix is right.
type iCoreWebView2Environment8Vtbl struct {
	_IUnknownVtbl
	CreateCoreWebView2Controller            ComProc
	CreateWebResourceResponse               ComProc
	GetBrowserVersionString                 ComProc
	AddNewBrowserVersionAvailable           ComProc
	RemoveNewBrowserVersionAvailable        ComProc
	CreateWebResourceRequest                ComProc
	CreateCoreWebView2CompositionController ComProc
	CreateCoreWebView2PointerInfo           ComProc
	GetAutomationProviderForWindow          ComProc
	AddBrowserProcessExited                 ComProc
	RemoveBrowserProcessExited              ComProc
	CreatePrintSettings                     ComProc
	GetUserDataFolder                       ComProc
	AddProcessInfosChanged                  ComProc
	RemoveProcessInfosChanged               ComProc
	GetProcessInfos                         ComProc
}

type ICoreWebView2Environment8 struct {
	vtbl *iCoreWebView2Environment8Vtbl
}

func (e *ICoreWebView2Environment8) AddRef() uintptr {
	ret, _, _ := e.vtbl.AddRef.Call(uintptr(unsafe.Pointer(e)))

	return ret
}

func (e *ICoreWebView2Environment8) Release() uintptr {
	ret, _, _ := e.vtbl.Release.Call(uintptr(unsafe.Pointer(e)))

	return ret
}

// GetICoreWebView2Environment8 returns nil when the installed WebView2 runtime
// predates the interface (Runtime 99.0.1150.38 / SDK 1.0.1150.38). Callers
// degrade rather than fail: without it there is no supported way to tell a
// renderer process from a GPU one.
func (e *ICoreWebView2Environment) GetICoreWebView2Environment8() *ICoreWebView2Environment8 {
	var result *ICoreWebView2Environment8

	iidICoreWebView2Environment8 := NewGUID("{d6eb91dd-c3d2-45e5-bd29-6dc2bc4de9cf}")
	_, _, _ = e.vtbl.QueryInterface.Call(
		uintptr(unsafe.Pointer(e)),
		uintptr(unsafe.Pointer(iidICoreWebView2Environment8)),
		uintptr(unsafe.Pointer(&result)))

	return result
}

// GetProcessInfos returns a snapshot of every process using this
// environment's user data folder, except crashpad. Cheap: the collection is
// materialised at call time and nothing about it is live.
func (e *ICoreWebView2Environment8) GetProcessInfos() (*ICoreWebView2ProcessInfoCollection, error) {
	var value *ICoreWebView2ProcessInfoCollection
	hr, _, _ := e.vtbl.GetProcessInfos.Call(
		uintptr(unsafe.Pointer(e)),
		uintptr(unsafe.Pointer(&value)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return nil, syscall.Errno(hr)
	}
	return value, nil
}
