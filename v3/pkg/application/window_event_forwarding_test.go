package application

import (
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/events"
)

type eventForwardingImpl struct{ webviewWindowImpl }

func (*eventForwardingImpl) on(uint) {}

func TestWindowEventForwardingPreservesNativeCallbacks(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "disabled"}[disabled], func(t *testing.T) {
			window := &WebviewWindow{
				options:        WebviewWindowOptions{DisableWindowEventForwarding: disabled},
				impl:           &eventForwardingImpl{},
				eventListeners: make(map[uint][]*WindowEventListener),
				eventHooks:     make(map[uint][]*WindowEventListener),
			}
			called := make(chan struct{}, 1)
			hooked := false
			window.RegisterHook(events.Common.WindowDidResize, func(*WindowEvent) { hooked = true })
			window.OnWindowEvent(events.Common.WindowDidResize, func(*WindowEvent) { called <- struct{}{} })
			window.HandleWindowEvent(uint(events.Common.WindowDidResize))
			if !hooked {
				t.Fatal("native hook did not run")
			}
			select {
			case <-called:
			case <-time.After(time.Second):
				t.Fatal("native listener did not run")
			}
			want := 1
			if disabled {
				want = 0
			}
			if len(window.pendingJS) != want {
				t.Fatalf("queued %d scripts, want %d", len(window.pendingJS), want)
			}
			// Explicit event delivery and bootstrap ExecJS remain available.
			window.DispatchWailsEvent(&CustomEvent{Name: "explicit"})
			window.ExecJS("bootstrap()")
			if len(window.pendingJS) != want+2 || !strings.Contains(window.pendingJS[want], "explicit") || window.pendingJS[want+1] != "bootstrap()" {
				t.Fatalf("explicit JavaScript delivery changed: %v", window.pendingJS)
			}
		})
	}
}
