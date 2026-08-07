//go:build windows && !server

package application

import (
	"fmt"
	"strconv"
	"time"

	"github.com/wailsapp/wails/v3/pkg/w32"
)

// Renderer-hang detection and recovery.
//
// Two signals feed one recovery state machine:
//
//  1. WebView2's ProcessFailed notification with kind
//     RENDER_PROCESS_UNRESPONSIVE (see processFailed).
//  2. A host-owned liveness watchdog that pings the renderer main thread on
//     a fixed cadence (this file).
//
// The notification alone is not a usable trigger. Chromium's hang monitor is
// input-driven: components/input/render_input_router.cc starts its timer when
// an input event is dispatched and left unacknowledged, keeps it running only
// while in_flight_event_count_ > 0, and stops it outright while the widget is
// hidden. A renderer that wedges while the user is not touching the window
// therefore raises exactly one notification — the one event that happened to
// be in flight — and never another; a window nobody is touching raises none
// at all. Documentation implying a fixed ~15s re-raise cadence does not match
// that implementation, and the 2026-08-03 incident is the evidence: an
// eight-minute freeze produced a single RENDER_PROCESS_UNRESPONSIVE. Electron,
// CEF and Microsoft's own samples all treat the event as a one-shot trigger
// for a host-owned clock, which is what this file implements.
//
// All state here is main-thread only. WebView2 raises its callbacks on the
// message loop, and every timer callback hops back onto it with InvokeAsync
// before touching anything.

const (
	// renderRecoveryNavigateDeadline bounds how long the recovery
	// re-navigation gets to commit before the controller is rebuilt. The
	// re-navigation only ever targets lastNavigatedURL — for a packaged app
	// a local asset-server URL that commits in well under a second on a
	// healthy renderer — so overrunning this means the renderer main thread
	// never picked the navigation up at all.
	renderRecoveryNavigateDeadline = 5 * time.Second

	// renderPingInterval is the liveness-ping cadence, and also the rate at
	// which the watchdog re-evaluates whether it should be running (window
	// minimised, webview suspended, ...).
	renderPingInterval = 5 * time.Second

	// renderPingHangDeadline and renderPingHangMisses both have to be
	// exceeded before the renderer is declared hung. A main thread that is
	// merely busy — a long GC, a heavy layout, a few seconds of synchronous
	// work — must not trigger a rebuild, because a rebuild destroys all
	// in-page state. The elapsed-time bound is the real test; the
	// consecutive-miss bound covers the case where wall time jumps without
	// the renderer having had any chance to run (machine sleep/resume, which
	// makes the pending timer fire once, very late).
	renderPingHangDeadline = 15 * time.Second
	renderPingHangMisses   = 3

	// renderStandDownSuspendedVisible is the one stand-down reason that
	// indicates a host defect rather than normal operation: the webview was
	// suspended for a minimised window and the window came back without
	// ResumeWebview being called. The window is showing suspended (blank)
	// content and no watchdog can help, so it is logged loudly.
	renderStandDownSuspendedVisible = "webview suspended while the window is visible; ResumeWebview was not called on restore"
)

// renderRecoveryState holds the renderer liveness watchdog's state and the state
// of the recovery episode it (or processFailed) opens. Zero value is the
// not-yet-started state; startRenderWatchdog brings it up.
type renderRecoveryState struct {
	// running is true between startRenderWatchdog and stopRenderWatchdog.
	// stopRenderWatchdog is terminal — it runs when the window is being
	// destroyed and nothing re-arms afterwards.
	running bool

	// suspended mirrors suspendWebview/resumeWebview. A suspended WebView2
	// deliberately runs no script, so pinging it would both fail and defeat
	// the memory trim it exists for. It is the only stand-down condition
	// tied to the window's lifecycle, and deliberately so — see
	// renderPingStandDownReason.
	suspended bool

	// pingCycle identifies the current run of the ping timer. Every armed
	// timer carries the cycle it was armed in; a callback whose cycle no
	// longer matches belongs to a stopped run and is dropped.
	pingCycle uint64
	pingTimer *time.Timer

	// pingSerial is the serial of the most recently dispatched ping, and
	// lastPongSerial the highest serial the renderer has answered. Each ping
	// carries its serial as the script body and gets it back as the script
	// result, so pongs correlate exactly — a completion left over from
	// before a stand-down can never be mistaken for proof of current
	// liveness. lastPongAt is when the renderer last proved it runs script,
	// and is reset (not cleared) whenever the watchdog stands down or an
	// episode ends, so restoring always starts from a full fresh deadline.
	pingSerial     uint64
	lastPongSerial uint64
	lastPongAt     time.Time

	// standDownReason is the reason the watchdog is currently not pinging,
	// or "" while it is. Transitions — including one stand-down reason
	// replacing another — are logged exactly once.
	standDownReason string

	// episode counts recovery episodes; it is the identity a pending
	// navigate deadline is matched against. active is true for the duration
	// of an episode: a second hang signal while one is in flight is
	// informational and must not extend the deadline.
	episode       uint64
	active        bool
	navigateTimer *time.Timer
}

// startRenderWatchdog brings the liveness watchdog up. Idempotent: the
// window creates the watchdog once, and it outlives controller rebuilds
// (it always pings whichever controller w.chromium currently holds).
func (w *windowsWebviewWindow) startRenderWatchdog() {
	r := &w.renderRecovery
	if r.running {
		return
	}
	r.running = true
	r.pingCycle++
	r.standDownReason = "watchdog starting"
	r.resetRenderPingDeadline()
	w.armRenderPingTimer()
	globalApplication.debug("webview2: render watchdog started",
		"window", w.parent.id,
		"interval", renderPingInterval.String(),
		"hangAfter", renderPingHangDeadline.String())
}

// stopRenderWatchdog tears the watchdog down and abandons any recovery
// episode in flight. Terminal — called when the window is being destroyed,
// so that neither a ping tick nor a navigate deadline can fire into a dead
// window. Idempotent.
func (w *windowsWebviewWindow) stopRenderWatchdog() {
	r := &w.renderRecovery
	if !r.running {
		return
	}
	r.running = false
	r.pingCycle++
	if r.pingTimer != nil {
		r.pingTimer.Stop()
		r.pingTimer = nil
	}
	if r.navigateTimer != nil {
		r.navigateTimer.Stop()
		r.navigateTimer = nil
	}
	r.active = false
	globalApplication.debug("webview2: render watchdog stopped", "window", w.parent.id)
}

// setWebviewSuspended records whether the WebView2 is suspended. Pinging a
// suspended webview would wake it every interval and undo the memory trim
// suspending it bought, so the watchdog stands down until it is resumed.
func (w *windowsWebviewWindow) setWebviewSuspended(suspended bool) {
	w.renderRecovery.suspended = suspended
}

// renderNavigationCommitted tells the watchdog that a navigation reached the
// renderer. That both proves the renderer runs script (closing any recovery
// episode) and invalidates every ping issued against the document being
// replaced, which nothing will answer now.
func (w *windowsWebviewWindow) renderNavigationCommitted() {
	w.endRenderRecovery("navigation completed")
	w.renderRecovery.resetRenderPingDeadline()
}

// renderControllerReplaced tells the watchdog that w.chromium now points at
// a different controller. Everything the watchdog knew described the old
// one: its outstanding pings will never be answered (and a late completion
// from it must not be read as the new renderer being alive), and a fresh
// controller is never suspended.
func (w *windowsWebviewWindow) renderControllerReplaced() {
	r := &w.renderRecovery
	r.suspended = false
	r.resetRenderPingDeadline()
}

// armRenderPingTimer schedules the next watchdog tick. The timer runs for as
// long as the watchdog is up, whether or not it is currently pinging: it is
// also what notices that a stand-down condition has cleared.
func (w *windowsWebviewWindow) armRenderPingTimer() {
	r := &w.renderRecovery
	if !r.running {
		// The watchdog was stopped while the tick that re-arms it was on the
		// stack — rebuildWebView pumps a nested message loop, so the window
		// really can be torn down underneath a tick.
		return
	}
	cycle := r.pingCycle
	r.pingTimer = time.AfterFunc(renderPingInterval, func() {
		InvokeAsync(func() { w.renderPingTick(cycle) })
	})
}

// resetRenderPingDeadline discards the outstanding pings and restarts the
// hang deadline from now. Used wherever the renderer is about to get, or has
// just been given, a clean slate — stand-down, stand-up, end of a recovery
// episode — so that a stale outstanding ping can never trip the watchdog the
// moment it comes back.
func (r *renderRecoveryState) resetRenderPingDeadline() {
	r.lastPongSerial = r.pingSerial
	r.lastPongAt = time.Now()
}

// missedRenderPings is how many dispatched pings the renderer has not
// answered. Written as a comparison rather than a subtraction because
// lastPongSerial can legitimately equal pingSerial and must never be allowed
// to exceed it (unsigned underflow).
func (r *renderRecoveryState) missedRenderPings() uint64 {
	if r.pingSerial <= r.lastPongSerial {
		return 0
	}
	return r.pingSerial - r.lastPongSerial
}

// renderPingStandDownReason reports why the watchdog must not ping right
// now, or "" when it should. Conditions are polled each tick rather than
// tracked through window events: the states that matter arrive through
// several unrelated paths, and a watchdog that silently stopped because one
// of them was missed would be worse than no watchdog at all.
//
// Deliberately NOT gated on the window being minimised or hidden, even
// though a hang there is not user-visible and rebuilding is cheapest then.
// Windows flips those bits as a CONSEQUENCE of the hang: under a real
// WebView2, a wedged renderer intermittently leaves the host window
// reporting WS_MINIMIZE with nobody having minimised it (one 90s-wedge run
// flipped within 7s and stayed flipped; the next did not flip at all; an
// idle control window never flipped). A gate the failure itself can trip is
// a gate that disables detection exactly when it is needed. The one state
// that genuinely makes pinging impossible — a suspended webview, which runs
// no script by design — is state this host sets itself, and Windows cannot
// flip it underneath us.
func (w *windowsWebviewWindow) renderPingStandDownReason() string {
	switch {
	case w.renderRecovery.active:
		return "recovery episode in progress"
	case w.hwnd == 0:
		return "window has no handle"
	case w.renderRecovery.suspended:
		if !w.isMinimised() && w.isVisible() {
			// Diagnostic distinction only — both branches stand down. This
			// one means ResumeWebview was never called on restore, so the
			// window is showing suspended content and no watchdog can help.
			return renderStandDownSuspendedVisible
		}
		return "webview suspended"
	case w.chromium == nil || !w.chromium.IsReady():
		return "webview controller not ready"
	case !w.webviewNavigationCompleted:
		// Deliberately blind to a hang during the very first load. A cold
		// start legitimately monopolises the renderer main thread while the
		// bundle parses and boots, and an ExecuteScript issued in that window
		// queues behind it — so watching here risks declaring a slow start a
		// hang, and a rebuild would only restart the same slow start, in a
		// loop. Every later navigation is watched: this flag latches on the
		// first NavigationCompleted and is never cleared, rebuilds included.
		return "first navigation has not completed"
	}
	return ""
}

// windowStyleHex renders the window's current GWL_STYLE for the watchdog's
// stand-down and arm log lines. Purely diagnostic — no watchdog decision
// reads it — but WS_MINIMIZE/WS_VISIBLE flipping under a wedged renderer is
// exactly the kind of thing a post-incident reader of launcher.log needs to
// be able to see.
func (w *windowsWebviewWindow) windowStyleHex() string {
	if w.hwnd == 0 {
		return "none"
	}
	return fmt.Sprintf("0x%08x", uint32(w32.GetWindowLong(w.hwnd, w32.GWL_STYLE)))
}

// renderPingTick is one watchdog tick, on the main thread. It re-evaluates
// the stand-down conditions, checks the hang deadline, and dispatches the
// next ping.
func (w *windowsWebviewWindow) renderPingTick(cycle uint64) {
	r := &w.renderRecovery
	if !r.running || r.pingCycle != cycle {
		// The run this tick belongs to was stopped or restarted while the
		// tick was in flight to the main thread.
		return
	}
	// Keep the cadence going no matter which branch below runs: while stood
	// down this timer is the only thing that will notice the condition
	// clearing.
	defer w.armRenderPingTimer()

	if reason := w.renderPingStandDownReason(); reason != "" {
		if r.standDownReason != reason {
			if reason == renderStandDownSuspendedVisible {
				globalApplication.warning(
					"webview2: render watchdog standing down for window %v: %s", w.parent.id, reason)
			} else {
				globalApplication.debug("webview2: render watchdog standing down",
					"window", w.parent.id, "reason", reason, "windowStyle", w.windowStyleHex())
			}
			r.standDownReason = reason
		}
		// Anything dispatched before or during the stand-down is written off
		// here rather than at stand-up, so the deadline is always measured
		// from the most recent tick that could have been answered.
		r.resetRenderPingDeadline()
		return
	}

	if r.standDownReason != "" {
		globalApplication.debug("webview2: render watchdog armed",
			"window", w.parent.id, "after", r.standDownReason, "windowStyle", w.windowStyleHex())
		r.standDownReason = ""
		r.resetRenderPingDeadline()
	}

	if missed := r.missedRenderPings(); missed >= renderPingHangMisses {
		if silent := time.Since(r.lastPongAt); silent >= renderPingHangDeadline {
			w.beginRenderRecovery(fmt.Sprintf(
				"renderer ran no script for %s (%d consecutive liveness pings unanswered)",
				silent.Round(time.Second), missed))
			return
		}
	}

	w.sendRenderPing()
}

// sendRenderPing dispatches one liveness ping. The script is the ping's
// serial number: evaluating an integer literal is the cheapest thing a
// renderer main thread can be asked to do, and ExecuteScript hands the
// JSON-encoded result back to the completion handler, so the pong carries
// its own correlation token at no extra cost.
func (w *windowsWebviewWindow) sendRenderPing() {
	r := &w.renderRecovery
	serial := r.pingSerial + 1
	if err := w.chromium.EvalWithCompletion(strconv.FormatUint(serial, 10)); err != nil {
		// No completion can arrive for a request that was never dispatched,
		// so this ping must not count against the hang deadline. Transient
		// refusals are expected (the controller reconfiguring during a DPI or
		// visibility transition); the next tick retries.
		globalApplication.debug("webview2: render liveness ping not dispatched",
			"window", w.parent.id, "serial", serial, "error", err.Error())
		return
	}
	r.pingSerial = serial
}

// renderPongReceived handles an ExecuteScript completion. WebView2 raises it
// on the message loop, so this runs on the main thread like the rest of the
// watchdog.
func (w *windowsWebviewWindow) renderPongReceived(errorCode uintptr, result string) {
	r := &w.renderRecovery
	if errorCode != 0 {
		// The request was completed without the renderer evaluating it — the
		// usual cause is the document being replaced while the ping was in
		// flight. That is not evidence of liveness, but it is not evidence of
		// a hang either, so restart the deadline and let the next tick ping a
		// settled document.
		globalApplication.debug("webview2: render liveness ping failed",
			"window", w.parent.id, "hresult", fmt.Sprintf("0x%x", errorCode))
		r.resetRenderPingDeadline()
		return
	}
	serial, err := strconv.ParseUint(result, 10, 64)
	if err != nil {
		// WebView2 completes still-pending ExecuteScript requests with S_OK
		// and a JSON "null" when their target goes away — every outstanding
		// ping is answered that way the moment the controller is closed or
		// the document is replaced. The renderer did not evaluate anything,
		// so this is not a pong; but leaving those pings counted would let an
		// ordinary reload accumulate enough misses to trip the watchdog, so
		// the deadline restarts here too.
		globalApplication.debug("webview2: render liveness ping discarded by the browser",
			"window", w.parent.id, "result", result)
		r.resetRenderPingDeadline()
		return
	}
	if serial <= r.lastPongSerial || serial > r.pingSerial {
		// Superseded (an older ping answering after a newer one) or from a
		// run the watchdog has already written off. Either way it says
		// nothing about the renderer's current state.
		return
	}
	r.lastPongSerial = serial
	r.lastPongAt = time.Now()
}

// beginRenderRecovery opens a renderer-recovery episode. It is the single
// entry point for both hang signals — the RENDER_PROCESS_UNRESPONSIVE
// notification and the liveness watchdog — so that they can never run two
// overlapping recoveries.
//
// The episode re-navigates to the last host-requested URL and gives it
// renderRecoveryNavigateDeadline to commit. A renderer whose main thread
// still pumps IPC recovers there: the navigation commits, navigationCompleted
// closes the episode, and the page's state is the only thing lost. A wedged
// main thread never picks the navigation up, and the deadline escalates to a
// controller rebuild, which reaps the hung process tree.
//
// Main thread only.
func (w *windowsWebviewWindow) beginRenderRecovery(reason string) {
	r := &w.renderRecovery
	if !r.running {
		globalApplication.debug("webview2: render recovery ignored; watchdog not running",
			"window", w.parent.id, "signal", reason)
		return
	}
	if r.active {
		// Repeat signals during an episode are informational. Extending the
		// deadline for each one would let a renderer that keeps reporting
		// itself unresponsive postpone the rebuild indefinitely.
		globalApplication.debug("webview2: render recovery already in progress",
			"window", w.parent.id, "episode", r.episode, "signal", reason)
		return
	}
	r.active = true
	r.episode++
	episode := r.episode
	globalApplication.error("webview2: render recovery episode %d started for window %v: %s",
		episode, w.parent.id, reason)

	if w.chromium == nil || !w.chromium.IsReady() {
		globalApplication.error(
			"webview2: render recovery episode %d found no ready controller; rebuilding", episode)
		w.rebuildWebView("renderer unresponsive before the controller was ready")
		return
	}
	url := w.lastNavigatedURL
	if url == "" {
		globalApplication.error(
			"webview2: render recovery episode %d has no recorded URL to re-navigate to; rebuilding", episode)
		w.rebuildWebView("renderer unresponsive with no recorded URL")
		return
	}

	globalApplication.debug("webview2: render recovery re-navigating",
		"window", w.parent.id,
		"episode", episode,
		"url", url,
		"deadline", renderRecoveryNavigateDeadline.String())
	w.chromium.Navigate(url)
	r.navigateTimer = time.AfterFunc(renderRecoveryNavigateDeadline, func() {
		InvokeAsync(func() { w.renderRecoveryDeadlineExpired(episode) })
	})
}

// endRenderRecovery closes the current episode, cancelling its navigate
// deadline. Called when a navigation commits (proof of a responsive
// renderer), when the controller is rebuilt (the escalation has happened),
// and when the window goes away. A no-op when no episode is open, so callers
// on the ordinary path do not need to check.
//
// Main thread only.
func (w *windowsWebviewWindow) endRenderRecovery(reason string) {
	r := &w.renderRecovery
	if !r.active {
		return
	}
	if r.navigateTimer != nil {
		r.navigateTimer.Stop()
		r.navigateTimer = nil
	}
	r.active = false
	// The renderer this window is about to watch is either freshly navigated
	// or freshly built; either way the previous episode's outstanding pings
	// say nothing about it.
	r.resetRenderPingDeadline()
	globalApplication.info("webview2: render recovery episode closed",
		"window", w.parent.id, "episode", r.episode, "reason", reason)
}

// renderRecoveryDeadlineExpired escalates an episode whose re-navigation
// never committed. Re-checks the episode state on the main thread: the
// deadline can already have been cancelled, or superseded by a later
// episode, while this callback was in flight.
func (w *windowsWebviewWindow) renderRecoveryDeadlineExpired(episode uint64) {
	r := &w.renderRecovery
	if !r.running || !r.active || r.episode != episode {
		globalApplication.debug("webview2: render recovery deadline fired for a closed episode",
			"window", w.parent.id, "episode", episode)
		return
	}
	w.rebuildWebView(fmt.Sprintf(
		"renderer unresponsive; re-navigation did not commit within %s", renderRecoveryNavigateDeadline))
}
