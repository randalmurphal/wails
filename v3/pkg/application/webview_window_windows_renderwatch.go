//go:build windows && !server

package application

import (
	"fmt"
	"strconv"
	"time"

	"github.com/wailsapp/wails/v3/pkg/w32"
)

// Renderer-hang detection and recovery — the Windows host side. Every
// decision this file makes is delegated to renderRecoveryState in
// renderwatch_state.go, which is untagged and unit-tested; what lives here is
// the side effects: timers, COM calls, window queries and logging.
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

// startRenderWatchdog brings the liveness watchdog up. Idempotent: the
// window creates the watchdog once, and it outlives controller rebuilds
// (it always pings whichever controller w.chromium currently holds).
func (w *windowsWebviewWindow) startRenderWatchdog() {
	if !w.renderRecovery.start(time.Now()) {
		return
	}
	w.armRenderPingTimer(renderPingInterval)
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
	if !w.renderRecovery.running {
		return
	}
	w.endRenderRecovery("watchdog stopped")
	w.renderRecovery.stop()
	globalApplication.debug("webview2: render watchdog stopped", "window", w.parent.id)
}

// setWebviewSuspended records whether the WebView2 is suspended. Pinging a
// suspended webview would wake it every interval and undo the memory trim
// suspending it bought, so the watchdog stands down until it is resumed.
//
// Un-suspending re-arms the tick at the short interval here rather than at
// each call site: both the ordinary restore path and the declined-TrySuspend
// path have to stand the watchdog back up promptly, and a call site that
// forgot would leave the window unwatched for up to
// renderStandDownPingInterval.
func (w *windowsWebviewWindow) setWebviewSuspended(suspended bool) {
	r := &w.renderRecovery
	if r.suspended == suspended {
		return
	}
	r.suspended = suspended
	if !suspended {
		w.restartRenderPingTimer()
	}
}

// renderNavigationCommitted tells the watchdog that a navigation reached the
// renderer. That both proves the renderer runs script (closing any recovery
// episode) and invalidates every ping issued against the document being
// replaced, which nothing will answer now. It is also what ends the
// first-navigation stand-down, so the tick is re-armed at the short interval.
func (w *windowsWebviewWindow) renderNavigationCommitted() {
	w.endRenderRecovery("navigation completed")
	w.renderRecovery.navigationCommitted(time.Now())
	w.restartRenderPingTimer()
}

// renderControllerReplaced tells the watchdog that w.chromium now points at a
// different controller, closing any recovery episode still open in the same
// step — the rebuild IS the terminal action of an episode, and a caller that
// did those two in the wrong order would leave a navigate deadline armed
// against a controller that no longer exists.
func (w *windowsWebviewWindow) renderControllerReplaced(reason string) {
	w.endRenderRecovery(reason)
	w.renderRecovery.controllerReplaced(time.Now())
	w.restartRenderPingTimer()
}

// armRenderPingTimer schedules the next watchdog tick. The timer runs for as
// long as the watchdog is up, whether or not it is currently pinging: it is
// also what notices that a stand-down condition has cleared.
func (w *windowsWebviewWindow) armRenderPingTimer(after time.Duration) {
	r := &w.renderRecovery
	if !r.running {
		// The watchdog was stopped while the tick that re-arms it was on the
		// stack — rebuildWebView pumps a nested message loop, so the window
		// really can be torn down underneath a tick.
		return
	}
	// Stopping first makes a double-arm structurally harmless: whichever path
	// armed the timer previously, only one is left pending.
	r.stopPingTimer()
	cycle := r.pingCycle
	r.pingTimer = time.AfterFunc(after, func() {
		InvokeAsync(func() { w.renderPingTick(cycle) })
	})
}

// restartRenderPingTimer drops the pending tick and schedules a fresh one at
// the short interval. Called from the main thread by the events that end a
// stand-down — resume, navigation commit, controller replacement — so that
// stand-up latency is renderPingInterval and not renderStandDownPingInterval.
func (w *windowsWebviewWindow) restartRenderPingTimer() {
	if !w.renderRecovery.running {
		return
	}
	w.renderRecovery.restart()
	w.armRenderPingTimer(renderPingInterval)
}

// renderStandDownInputs gathers the window-derived facts the stand-down
// decision needs. isMinimised/isVisible are only consulted in the suspended
// case, and only once there is an HWND to ask about.
func (w *windowsWebviewWindow) renderStandDownInputs() renderStandDownInputs {
	in := renderStandDownInputs{
		hasHandle:       w.hwnd != 0,
		controllerReady: w.chromium != nil && w.chromium.IsReady(),
	}
	if in.hasHandle && w.renderRecovery.suspended {
		in.visibleWhileSuspended = !w.isMinimised() && w.isVisible()
	}
	return in
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
	if !r.acceptTick(cycle) {
		// The run this tick belongs to was stopped or restarted while the
		// tick was in flight to the main thread.
		return
	}
	// Keep the cadence going no matter which branch below runs: while stood
	// down this timer is the only thing that will notice the condition
	// clearing. The interval is read after the branches, so a tick that
	// stands down slows the cadence and one that stands up restores it.
	defer func() { w.armRenderPingTimer(r.pingTickInterval()) }()

	outcome := r.tick(w.renderStandDownInputs(), time.Now())
	if outcome.stoodUpFrom != "" {
		globalApplication.debug("webview2: render watchdog armed",
			"window", w.parent.id, "after", outcome.stoodUpFrom, "windowStyle", w.windowStyleHex())
	}
	switch outcome.action {
	case renderTickStoodDown:
		if !outcome.standDownChanged {
			return
		}
		if outcome.standDownReason == renderStandDownSuspendedVisible {
			globalApplication.warning(
				"webview2: render watchdog standing down for window %v: %s",
				w.parent.id, outcome.standDownReason)
		} else {
			globalApplication.debug("webview2: render watchdog standing down",
				"window", w.parent.id, "reason", outcome.standDownReason,
				"windowStyle", w.windowStyleHex())
		}
	case renderTickRecover:
		w.beginRenderRecovery(fmt.Sprintf(
			"renderer ran no script for %s (%d consecutive liveness pings unanswered)",
			outcome.silent.Round(time.Second), outcome.missed))
	case renderTickPing:
		w.sendRenderPing()
	}
}

// sendRenderPing dispatches one liveness ping. The script is the ping's
// serial number: evaluating an integer literal is the cheapest thing a
// renderer main thread can be asked to do, and ExecuteScript hands the
// JSON-encoded result back to the completion handler, so the pong carries
// its own correlation token at no extra cost.
func (w *windowsWebviewWindow) sendRenderPing() {
	r := &w.renderRecovery
	serial := r.nextPingSerial()
	err := w.chromium.EvalWithCompletion(strconv.FormatUint(serial, 10),
		func(errorCode uintptr, result string) {
			w.renderPongReceived(serial, errorCode, result)
		})
	if err != nil {
		// No completion can arrive for a request that was never dispatched,
		// so this ping must not count against the hang deadline. Transient
		// refusals are expected (the controller reconfiguring during a DPI or
		// visibility transition); the next tick retries.
		globalApplication.debug("webview2: render liveness ping not dispatched",
			"window", w.parent.id, "serial", serial, "error", err.Error())
		return
	}
	r.pingDispatched(serial)
}

// renderPongReceived handles the ExecuteScript completion for the ping that
// carried sent. WebView2 raises it on the message loop, so this runs on the
// main thread like the rest of the watchdog.
func (w *windowsWebviewWindow) renderPongReceived(sent uint64, errorCode uintptr, result string) {
	r := &w.renderRecovery
	if errorCode != 0 {
		// The request was completed without the renderer evaluating it — the
		// usual cause is the document being replaced while the ping was in
		// flight. That is not evidence of liveness, but it is not evidence of
		// a hang either.
		globalApplication.debug("webview2: render liveness ping failed",
			"window", w.parent.id, "serial", sent, "hresult", fmt.Sprintf("0x%x", errorCode))
		w.renderNonPongReceived(sent)
		return
	}
	serial, err := strconv.ParseUint(result, 10, 64)
	if err != nil {
		// WebView2 completes still-pending ExecuteScript requests with S_OK
		// and a JSON "null" when their target goes away — every outstanding
		// ping is answered that way the moment the controller is closed or
		// the document is replaced. The renderer did not evaluate anything,
		// so this is not a pong.
		globalApplication.debug("webview2: render liveness ping discarded by the browser",
			"window", w.parent.id, "serial", sent, "result", result)
		w.renderNonPongReceived(sent)
		return
	}
	r.recordPong(serial, time.Now())
}

// renderNonPongReceived applies a completion for the ping carrying sent that
// carried no serial back. The state change is deliberately not a full deadline
// reset (see nonPongReceived); a sustained run of them is the one thing that
// silently suppresses detection, so it is warned about — once per run, on the
// tick the streak first gets long enough to mean something, like every other
// transition the watchdog logs.
func (w *windowsWebviewWindow) renderNonPongReceived(sent uint64) {
	streak, counted := w.renderRecovery.nonPongReceived(sent)
	if !counted || streak != renderNonPongWarnStreak {
		return
	}
	globalApplication.warning(
		"webview2: %d consecutive render liveness pings answered without the renderer evaluating them "+
			"for window %v; the document is not running script and hang detection is suppressed",
		streak, w.parent.id)
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
	episode, opened := r.beginEpisode()
	if !opened {
		globalApplication.debug("webview2: render recovery already in progress",
			"window", w.parent.id, "episode", episode, "signal", reason)
		return
	}
	globalApplication.error("webview2: render recovery episode %d started for window %v: %s",
		episode, w.parent.id, reason)

	if w.chromium == nil || !w.chromium.IsReady() {
		globalApplication.error(
			"webview2: render recovery episode %d for window %v found no ready controller; rebuilding",
			episode, w.parent.id)
		w.rebuildWebView("renderer unresponsive before the controller was ready")
		return
	}
	url := w.lastNavigatedURL
	if url == "" {
		globalApplication.error(
			"webview2: render recovery episode %d for window %v has no recorded URL to re-navigate to; rebuilding",
			episode, w.parent.id)
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
	if !w.renderRecovery.endEpisode(time.Now()) {
		return
	}
	globalApplication.info("webview2: render recovery episode closed",
		"window", w.parent.id, "episode", w.renderRecovery.episode, "reason", reason)
}

// renderRecoveryDeadlineExpired escalates an episode whose re-navigation
// never committed. Re-checks the episode state on the main thread: the
// deadline can already have been cancelled, or superseded by a later
// episode, while this callback was in flight.
func (w *windowsWebviewWindow) renderRecoveryDeadlineExpired(episode uint64) {
	if !w.renderRecovery.acceptNavigateDeadline(episode) {
		globalApplication.debug("webview2: render recovery deadline fired for a closed episode",
			"window", w.parent.id, "episode", episode)
		return
	}
	w.rebuildWebView(fmt.Sprintf(
		"renderer unresponsive; re-navigation did not commit within %s", renderRecoveryNavigateDeadline))
}
