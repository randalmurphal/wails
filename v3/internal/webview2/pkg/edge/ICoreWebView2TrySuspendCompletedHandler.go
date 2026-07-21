package edge

import (
	"unsafe"
)

type _ICoreWebView2TrySuspendCompletedHandlerVtbl struct {
	_IUnknownVtbl
	Invoke ComProc
}

type iCoreWebView2TrySuspendCompletedHandler struct {
	vtbl *_ICoreWebView2TrySuspendCompletedHandlerVtbl
	impl _ICoreWebView2TrySuspendCompletedHandlerImpl
}

func (i *iCoreWebView2TrySuspendCompletedHandler) AddRef() uint32 {
	ret, _, _ := i.vtbl.AddRef.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

func (i *iCoreWebView2TrySuspendCompletedHandler) Release() uint32 {
	ret, _, _ := i.vtbl.Release.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

func _ICoreWebView2TrySuspendCompletedHandlerIUnknownQueryInterface(this *iCoreWebView2TrySuspendCompletedHandler, refiid, object uintptr) uintptr {
	return this.impl.QueryInterface(refiid, object)
}

func _ICoreWebView2TrySuspendCompletedHandlerIUnknownAddRef(this *iCoreWebView2TrySuspendCompletedHandler) uintptr {
	return this.impl.AddRef()
}

func _ICoreWebView2TrySuspendCompletedHandlerIUnknownRelease(this *iCoreWebView2TrySuspendCompletedHandler) uintptr {
	return this.impl.Release()
}

func iCoreWebView2TrySuspendCompletedHandlerInvoke(this *iCoreWebView2TrySuspendCompletedHandler, errorCode uintptr, isSuccessful uintptr) uintptr {
	return this.impl.TrySuspendCompleted(errorCode, isSuccessful != 0)
}

type _ICoreWebView2TrySuspendCompletedHandlerImpl interface {
	_IUnknownImpl
	TrySuspendCompleted(errorCode uintptr, isSuccessful bool) uintptr
}

var _ICoreWebView2TrySuspendCompletedHandlerFn = _ICoreWebView2TrySuspendCompletedHandlerVtbl{
	_IUnknownVtbl{
		NewComProc(_ICoreWebView2TrySuspendCompletedHandlerIUnknownQueryInterface),
		NewComProc(_ICoreWebView2TrySuspendCompletedHandlerIUnknownAddRef),
		NewComProc(_ICoreWebView2TrySuspendCompletedHandlerIUnknownRelease),
	},
	NewComProc(iCoreWebView2TrySuspendCompletedHandlerInvoke),
}

func newICoreWebView2TrySuspendCompletedHandler(impl _ICoreWebView2TrySuspendCompletedHandlerImpl) *iCoreWebView2TrySuspendCompletedHandler {
	return &iCoreWebView2TrySuspendCompletedHandler{
		vtbl: &_ICoreWebView2TrySuspendCompletedHandlerFn,
		impl: impl,
	}
}
