//go:build windows && !server

package application

import (
	"fmt"
	"os"
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
	if w.renderRecovery.navigationCommitted(time.Now()) {
		// The rebuilt controller has content: take the indicator down.
		globalApplication.info("webview2: controller rebuild completed", "window", w.parent.id)
		w.repaintRenderRecoveryIndicator()
	}
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

// WebView2 error policy — the host half of edge.Chromium.SetErrorPolicy.
//
// edge reports errors it cannot recover from itself and no longer decides
// whether the process survives them; every one of them used to end in
// os.Exit(1). The 2026-08-18 incident is what that costs: a rebuild triggered
// by the escalation above had its CreateCoreWebView2Controller aborted with
// E_ABORT (0x80004004) because the user closed the window while it ran, and
// the app vanished. Closing a window is not a crash.
//
// The classification is in classifyWebviewError (renderwatch_state.go); this
// carries it out.

const (
	webviewFatalStartupMessage = "The application could not start: its WebView2 view failed to initialise."
	webviewFatalRuntimeMessage = "The WebView2 view stopped responding and could not be recovered. " +
		"The application has to close."
)

// webviewErrorReported is installed on every Chromium this window owns. It
// runs on the COM/main thread, where a modal message box is both safe and the
// point: a fatal WebView2 failure must never be a silent vanish.
func (w *windowsWebviewWindow) webviewErrorReported(err error) {
	in := webviewErrorInputs{
		shuttingDown:     w.webviewTearingDown(),
		controllerReady:  w.chromium != nil && w.chromium.IsReady(),
		rebuildInFlight:  w.renderRecovery.rebuildInFlight,
		rebuildRetryUsed: w.renderRecovery.rebuildRetryUsed,
	}
	switch classifyWebviewError(in) {
	case webviewErrorIgnore:
		globalApplication.info("webview2: error during teardown, ignored",
			"window", w.parent.id, "error", err.Error())
	case webviewErrorRetryRebuild:
		w.renderRecovery.useRebuildRetry()
		globalApplication.error("webview2: controller rebuild failed for window %v (%v); retrying once in %s",
			w.parent.id, err, webviewRebuildRetryDelay)
		// Not inline: this fires from inside the failed creation, which is
		// itself inside rebuildWebView's nested message pump. The retry has to
		// start after that call has unwound.
		time.AfterFunc(webviewRebuildRetryDelay, func() {
			InvokeAsync(w.retryWebviewRebuild)
		})
	case webviewErrorFatalStartup:
		w.fatalWebviewError(webviewFatalStartupMessage, err)
	case webviewErrorFatalRuntime:
		w.fatalWebviewError(webviewFatalRuntimeMessage, err)
	}
}

// webviewTearingDown reports whether this window is on its way out, in which
// case a reported error is teardown noise. Two independent facts, because
// either can be true first: the host announces the teardown to the controller
// (WM_CLOSE), and the HWND stops existing (WM_DESTROY and everything after).
func (w *windowsWebviewWindow) webviewTearingDown() bool {
	if w.chromium == nil || w.chromium.IsShuttingDown() {
		return true
	}
	return w.hwnd == 0 || !w32.IsWindow(w.hwnd)
}

// retryWebviewRebuild runs the one retry a failed rebuild gets, on the main
// thread. Everything it re-checks can have changed during the delay: the
// window can have been closed, and the rebuild can have completed after all
// (a creation error is not always terminal — the composition-hosting fallback
// reports one and then succeeds on the HWND path).
func (w *windowsWebviewWindow) retryWebviewRebuild() {
	if w.webviewTearingDown() {
		return
	}
	if !w.renderRecovery.rebuildInFlight {
		globalApplication.debug("webview2: rebuild retry skipped; the controller recovered",
			"window", w.parent.id)
		return
	}
	w.rebuildWebView("retrying a controller rebuild that failed")
}

// fatalWebviewError ends the process the only defensible way: after telling
// the user why. The message box blocks the main thread deliberately — the
// alternative is the window disappearing with no explanation, which is the
// behaviour this replaces.
func (w *windowsWebviewWindow) fatalWebviewError(message string, err error) {
	globalApplication.error("webview2: unrecoverable error for window %v: %s: %v",
		w.parent.id, message, err)
	owner := w.hwnd
	if owner != 0 && !w32.IsWindow(owner) {
		owner = 0
	}
	caption := globalApplication.options.Name
	if caption == "" {
		caption = "Application"
	}
	w32.MessageBox(owner, message+"\n\n"+err.Error(), caption, w32.MB_OK|w32.MB_ICONERROR)
	os.Exit(1)
}

// The "rebuilding" indicator.
//
// A controller rebuild closes the old controller and blocks for as long as
// the new one takes to create — ~22s in the incident, during which the window
// showed nothing but its background colour and the user, reasonably,
// concluded the app was dead and closed it. The child WebView2 HWND is gone
// by then, so the host window's own WM_PAINT is visible and is all it takes
// to say "still alive, wait".

const renderRecoveryIndicatorText = "Renderer stopped responding — rebuilding the view…"

// repaintRenderRecoveryIndicator asks for a repaint when the indicator flag
// has just flipped, in either direction.
func (w *windowsWebviewWindow) repaintRenderRecoveryIndicator() {
	if w.hwnd == 0 {
		return
	}
	w32.InvalidateRect(w.hwnd, nil, true)
}

// paintRenderRecoveryIndicator services WM_PAINT while a rebuild is in
// flight, and reports whether it did — the caller falls through to the
// default handling when it did not.
func (w *windowsWebviewWindow) paintRenderRecoveryIndicator() bool {
	if !w.renderRecovery.rebuildInFlight || w.hwnd == 0 {
		return false
	}
	var ps w32.PAINTSTRUCT
	hdc := w32.BeginPaint(w.hwnd, &ps)
	if hdc == 0 {
		return false
	}
	defer w32.EndPaint(w.hwnd, &ps)

	// GetClientRect returns nil for a window in a transient state, and
	// FillRect/DrawText on a nil rect crash. The paint is still consumed:
	// BeginPaint has already validated the region.
	rc := w32.GetClientRect(w.hwnd)
	if rc == nil {
		return true
	}
	col := w.parent.options.BackgroundColour
	background := w32.COLORREF(uint32(col.Red) | uint32(col.Green)<<8 | uint32(col.Blue)<<16)
	brush := w32.CreateSolidBrush(background)
	w32.FillRect(hdc, rc, brush)
	w32.DeleteObject(w32.HGDIOBJ(brush))

	previousFont := w32.SelectObject(hdc, w32.GetStockObject(w32.DEFAULT_GUI_FONT))
	defer w32.SelectObject(hdc, previousFont)
	w32.SetBkMode(hdc, w32.TRANSPARENT)
	w32.SetTextColor(hdc, indicatorTextColour(col))
	text := w32.MustStringToUTF16(renderRecoveryIndicatorText)
	w32.DrawText(hdc, text, -1, rc, w32.DT_CENTER|w32.DT_VCENTER|w32.DT_SINGLELINE|w32.DT_NOPREFIX)
	return true
}

// indicatorTextColour picks black or white against the window's own
// background colour, which is the only theming available here — the app's
// stylesheet lives in the renderer that just died. Rec. 601 luma, the same
// rule the Windows shell uses for accent-coloured text.
func indicatorTextColour(background RGBA) w32.COLORREF {
	luma := (299*uint32(background.Red) + 587*uint32(background.Green) + 114*uint32(background.Blue)) / 1000
	if luma < 128 {
		return w32.COLORREF(0x00FFFFFF)
	}
	return w32.COLORREF(0)
}
