package application

import "time"

// Renderer-hang detection state machine.
//
// This file is deliberately untagged and free of every Windows dependency:
// the decision logic below is what actually has to be right, and keeping it
// behind //go:build windows would mean it could only ever be exercised by
// hand on a Windows box against a live WebView2. The Windows host that drives
// it — timers, COM callbacks, window queries, logging — lives in
// webview_window_windows_renderwatch.go, which owns every side effect and
// delegates every decision here.
//
// All state here is main-thread only: WebView2 raises its callbacks on the
// message loop, and every timer callback hops back onto it with InvokeAsync
// before touching anything. Nothing in this file takes a lock, and nothing in
// it may be called from another goroutine.

const (
	// renderRecoveryNavigateDeadline bounds how long the recovery
	// re-navigation gets to commit before the controller is rebuilt. The
	// re-navigation only ever targets lastNavigatedURL — for a packaged app
	// a local asset-server URL that commits in well under a second on a
	// healthy renderer — so overrunning this means the renderer main thread
	// never picked the navigation up at all.
	renderRecoveryNavigateDeadline = 5 * time.Second

	// renderPingInterval is the liveness-ping cadence while the watchdog is
	// up, and also the rate at which it re-evaluates whether it should be
	// pinging at all.
	renderPingInterval = 5 * time.Second

	// renderStandDownPingInterval is the cadence while the watchdog is stood
	// down. A stood-down tick sends nothing; its only job is to notice that
	// the stand-down condition has cleared. A minimised, suspended window can
	// sit in that state for hours, and 5s ticks there would be 720 pointless
	// main-thread wakeups an hour on a window that is deliberately idle. The
	// events that end a stand-down and matter for detection latency —
	// resume, navigation commit, controller replacement — all re-arm at
	// renderPingInterval directly, so the slow cadence never delays stand-up
	// on any path a user can feel.
	renderStandDownPingInterval = 30 * time.Second

	// renderPingHangMisses and renderPingHangDeadline both have to be
	// exceeded before the renderer is declared hung, and they are not two
	// statements of the same rule.
	//
	// The miss bound is the one that decides: at renderPingInterval = 5s the
	// third consecutive unanswered ping lands ~20s after the last pong, which
	// is already past the 15s deadline, so in normal operation the miss count
	// is what fires. It is the right primary test because it counts actual
	// dispatched-and-unanswered requests, which is the thing being measured.
	//
	// The deadline is a wall-clock floor underneath it. It exists for two
	// cases the miss count alone gets wrong. First, shortening
	// renderPingInterval (or a burst of catch-up ticks) would otherwise let
	// three misses accumulate in a fraction of the time and turn a merely
	// busy main thread — a long GC, a heavy layout, a few seconds of
	// synchronous work — into a rebuild, which destroys all in-page state.
	// Second, machine sleep/resume makes the pending timer fire once, very
	// late: wall time has jumped but the renderer has had no chance to run,
	// so silence is huge while the miss count is 1. Requiring both means
	// neither case can trigger recovery on its own.
	renderPingHangMisses   = 3
	renderPingHangDeadline = 15 * time.Second

	// renderFirstNavigationStandDownBudget bounds the cold-start blind spot.
	// The watchdog stands down until the first navigation commits (see
	// renderStandDownFirstNavigation), which is correct for a slow boot and
	// catastrophic for a renderer that wedges before its boot navigation
	// commits: nothing else would ever end the stand-down and the window
	// would stay frozen forever. After this budget the stand-down is no
	// longer honoured and the watchdog starts pinging regardless. 60s is 4x
	// the hang deadline and far past any legitimate bundle boot, and because
	// standing up restarts the deadline from scratch, a wedge that begins
	// during boot is declared at roughly 60s + 20s rather than never.
	renderFirstNavigationStandDownBudget = 60 * time.Second

	// webviewRebuildRetryDelay is how long a failed controller rebuild waits
	// before its one retry. Long enough for whatever transient condition
	// broke the creation (a browser process still dying, a display
	// transition) to pass, short enough that a user staring at the
	// "rebuilding" indicator does not conclude the app is gone.
	webviewRebuildRetryDelay = 2 * time.Second

	// renderNonPongWarnStreak is how many consecutive non-pong ExecuteScript
	// completions are tolerated quietly. A handful is ordinary (a reload
	// discards whatever was in flight); a sustained run means every ping is
	// being answered by the browser without the renderer evaluating
	// anything, which the state machine deliberately refuses to escalate on
	// (see nonPongReceived) and which would otherwise be invisible.
	renderNonPongWarnStreak = 6
)

// Stand-down reasons. They are compared by value (the log level for
// renderStandDownSuspendedVisible differs from the rest), so they are
// constants rather than inline strings.
const (
	// renderStandDownStarting is the synthetic reason the watchdog holds
	// between start and its first tick, so that the first tick logs a
	// stand-up transition rather than nothing at all.
	renderStandDownStarting = "watchdog starting"

	renderStandDownRecovering         = "recovery episode in progress"
	renderStandDownNoHandle           = "window has no handle"
	renderStandDownSuspended          = "webview suspended"
	renderStandDownControllerNotReady = "webview controller not ready"
	renderStandDownFirstNavigation    = "first navigation has not completed"

	// renderStandDownSuspendedVisible is the one stand-down reason that
	// indicates a host defect rather than normal operation: the webview was
	// suspended for a minimised window and the window came back without
	// ResumeWebview being called. The window is showing suspended (blank)
	// content and no watchdog can help, so it is logged loudly.
	renderStandDownSuspendedVisible = "webview suspended while the window is visible; ResumeWebview was not called on restore"
)

// renderRecoveryState holds the renderer liveness watchdog's state and the
// state of the recovery episode it (or processFailed) opens. Zero value is
// the not-yet-started state; start brings it up.
type renderRecoveryState struct {
	// running is true between start and stop. stop is terminal — it runs
	// when the window is being destroyed and nothing re-arms afterwards.
	running bool

	// suspended mirrors suspendWebview/resumeWebview. A suspended WebView2
	// deliberately runs no script, so pinging it would both fail and defeat
	// the memory trim it exists for. It is the only stand-down condition
	// tied to the window's lifecycle, and deliberately so — see
	// standDownReasonAt.
	suspended bool

	// firstNavigationDone latches on the first navigation that commits into
	// the CURRENT controller, and is cleared only by controllerReplaced.
	//
	// It deliberately does not reuse windowsWebviewWindow.webviewNavigationCompleted,
	// which looks like the same thing and is not: setURL clears that flag on
	// every navigation, so gating on it would blind the watchdog for the
	// whole of every runtime SetURL, and — because the rebuild path
	// re-navigates through setURL — permanently, if a rebuilt renderer
	// wedged before its boot navigation committed. A controller replacement
	// genuinely is a cold start and genuinely should re-blind; an ordinary
	// re-navigation is not and must not.
	firstNavigationDone bool

	// pingCycle identifies the current run of the ping timer. Every armed
	// timer carries the cycle it was armed in; a callback whose cycle no
	// longer matches belongs to a stopped or restarted run and is dropped.
	pingCycle uint64
	pingTimer *time.Timer

	// pingSerial is the serial of the most recently dispatched ping, and
	// lastPongSerial the highest serial the renderer has answered. Each ping
	// carries its serial as the script body and gets it back as the script
	// result, so pongs correlate exactly — a completion left over from
	// before a stand-down can never be mistaken for proof of current
	// liveness. lastPongAt is when the renderer last proved it runs script.
	pingSerial     uint64
	lastPongSerial uint64
	lastPongAt     time.Time

	// nonPongStreak counts consecutive ExecuteScript completions that were
	// not pongs — a failure HRESULT, or an S_OK whose result is not the
	// serial that was sent. Reset by any real pong and by any full deadline
	// reset. Purely diagnostic: no decision reads it.
	nonPongStreak int

	// standDownReason is the reason the watchdog is currently not pinging,
	// or "" while it is. Transitions — including one stand-down reason
	// replacing another — are logged exactly once.
	//
	// standDownSince is when the CURRENT continuous stand-down began: on the
	// ""-to-non-empty transition, and on the two cold starts (start,
	// controllerReplaced). It is not re-stamped when one stand-down reason
	// replaces another, because the budget it feeds bounds total continuous
	// blindness, not blindness-for-this-particular-reason.
	standDownReason string
	standDownSince  time.Time

	// episode counts recovery episodes; it is the identity a pending
	// navigate deadline is matched against. active is true for the duration
	// of an episode: a second hang signal while one is in flight is
	// informational and must not extend the deadline.
	episode       uint64
	active        bool
	navigateTimer *time.Timer

	// rebuildInFlight spans a controller rebuild: set when rebuildWebView is
	// entered, cleared by the navigation that commits into the controller it
	// built. It is both what makes the host paint its "rebuilding" indicator
	// (the window is otherwise blank for the duration, which is what invited
	// a user to close it mid-rebuild) and what tells the error policy that a
	// reported WebView2 error is a failed rebuild rather than a failed boot.
	//
	// rebuildRetryUsed caps recovery at one retry: it is set when a failed
	// rebuild is retried and cleared only by a rebuild that succeeds, so a
	// controller that cannot be built can never spin.
	rebuildInFlight  bool
	rebuildRetryUsed bool
}

// start brings the watchdog up. Reports false if it was already running, in
// which case nothing was changed.
func (r *renderRecoveryState) start(now time.Time) bool {
	if r.running {
		return false
	}
	r.running = true
	r.pingCycle++
	r.standDownReason = renderStandDownStarting
	r.standDownSince = now
	r.resetPingDeadline(now)
	return true
}

// stop tears the watchdog down, cancelling the ping timer. Terminal, and
// idempotent; reports false if it was not running. The caller closes any open
// recovery episode first, so that the episode's own teardown is logged.
func (r *renderRecoveryState) stop() bool {
	if !r.running {
		return false
	}
	r.running = false
	// Invalidate every tick already in flight to the main thread: rebuildWebView
	// pumps a nested message loop, so a tick really can be mid-flight while the
	// window is torn down underneath it.
	r.pingCycle++
	r.stopPingTimer()
	// endEpisode nils navigateTimer with active, so a live one here would mean
	// that invariant broke. Stopping unconditionally means a dead window can
	// never be rebuilt by a timer regardless.
	r.stopNavigateTimer()
	r.active = false
	// The window is being destroyed: no rebuild is in flight any more, and no
	// retry may be scheduled against it.
	r.rebuildInFlight = false
	r.rebuildRetryUsed = false
	return true
}

// restart invalidates the in-flight ping tick and reports the cycle the
// caller must arm the replacement timer with. Used where an event has just
// made the watchdog relevant again (resume, navigation commit, controller
// replacement) and waiting out a stood-down tick would be pointless latency.
func (r *renderRecoveryState) restart() uint64 {
	r.pingCycle++
	r.stopPingTimer()
	return r.pingCycle
}

func (r *renderRecoveryState) stopPingTimer() {
	if r.pingTimer != nil {
		r.pingTimer.Stop()
		r.pingTimer = nil
	}
}

func (r *renderRecoveryState) stopNavigateTimer() {
	if r.navigateTimer != nil {
		r.navigateTimer.Stop()
		r.navigateTimer = nil
	}
}

// acceptTick reports whether a ping tick armed in cycle still belongs to the
// current run.
func (r *renderRecoveryState) acceptTick(cycle uint64) bool {
	return r.running && r.pingCycle == cycle
}

// acceptNavigateDeadline reports whether a navigate deadline that fired for
// episode should still escalate. The deadline can have been cancelled, or
// superseded by a later episode, while its callback was in flight to the main
// thread.
func (r *renderRecoveryState) acceptNavigateDeadline(episode uint64) bool {
	return r.running && r.active && r.episode == episode
}

// pingTickInterval is how long the next tick should be armed for. See
// renderStandDownPingInterval.
func (r *renderRecoveryState) pingTickInterval() time.Duration {
	if r.standDownReason != "" {
		return renderStandDownPingInterval
	}
	return renderPingInterval
}

// renderStandDownInputs are the window-derived facts the stand-down decision
// needs and the state does not hold. Gathered by the host immediately before
// the decision, never cached.
type renderStandDownInputs struct {
	// hasHandle is w.hwnd != 0.
	hasHandle bool
	// controllerReady is w.chromium != nil && w.chromium.IsReady().
	controllerReady bool
	// visibleWhileSuspended is set only when the state is already suspended
	// and the window is neither minimised nor hidden — the host-defect case.
	// Diagnostic only: both suspended branches stand down.
	visibleWhileSuspended bool
}

// standDownReasonAt reports why the watchdog must not ping right now, or ""
// when it should. Conditions are polled each tick rather than tracked through
// window events: the states that matter arrive through several unrelated
// paths, and a watchdog that silently stopped because one of them was missed
// would be worse than no watchdog at all.
//
// Deliberately NOT gated on the window being minimised or hidden, even though
// a hang there is not user-visible and rebuilding is cheapest then. Windows
// flips those bits as a CONSEQUENCE of the hang: under a real WebView2, a
// wedged renderer intermittently leaves the host window reporting WS_MINIMIZE
// with nobody having minimised it (one 90s-wedge run flipped within 7s and
// stayed flipped; the next did not flip at all; an idle control window never
// flipped). A gate the failure itself can trip is a gate that disables
// detection exactly when it is needed. The one state that genuinely makes
// pinging impossible — a suspended webview, which runs no script by design —
// is state this host sets itself, and Windows cannot flip it underneath us.
func (r *renderRecoveryState) standDownReasonAt(in renderStandDownInputs, now time.Time) string {
	switch {
	case r.active:
		return renderStandDownRecovering
	case !in.hasHandle:
		return renderStandDownNoHandle
	case r.suspended:
		if in.visibleWhileSuspended {
			return renderStandDownSuspendedVisible
		}
		return renderStandDownSuspended
	case !in.controllerReady:
		return renderStandDownControllerNotReady
	case !r.firstNavigationDone && now.Sub(r.standDownSince) < renderFirstNavigationStandDownBudget:
		// Blind during the very first load of a controller. A cold start
		// legitimately monopolises the renderer main thread while the bundle
		// parses and boots, and an ExecuteScript issued in that window queues
		// behind it — so watching here risks declaring a slow start a hang,
		// and a rebuild would only restart the same slow start, in a loop.
		// Bounded by renderFirstNavigationStandDownBudget so a renderer that
		// wedges before its boot navigation commits is still eventually
		// caught.
		return renderStandDownFirstNavigation
	}
	return ""
}

// renderTickAction is what a watchdog tick decided the host should do.
type renderTickAction int

const (
	// renderTickStoodDown: a stand-down condition holds; send nothing.
	renderTickStoodDown renderTickAction = iota
	// renderTickPing: dispatch the next liveness ping.
	renderTickPing
	// renderTickRecover: the hang predicate fired; open a recovery episode.
	renderTickRecover
)

// renderTickOutcome carries a tick's decision plus everything the host needs
// to log it. Nothing here is state — the state changes have already been
// applied to the receiver by the time tick returns.
type renderTickOutcome struct {
	action renderTickAction

	// standDownReason is the reason in force, set only for
	// renderTickStoodDown. standDownChanged reports whether it differs from
	// the reason the previous tick was holding, so that a stand-down that
	// persists for hours logs once rather than every tick.
	standDownReason  string
	standDownChanged bool

	// stoodUpFrom is the stand-down reason that ended on this tick, or "" if
	// the watchdog was already up. Independent of action: a tick can stand up
	// and immediately declare a hang.
	stoodUpFrom string

	// missed and silent describe the hang, set only for renderTickRecover.
	missed uint64
	silent time.Duration
}

// tick runs one watchdog tick: re-evaluate the stand-down conditions, apply
// the resulting transition, and — if the watchdog is up — decide between
// pinging and declaring the renderer hung. The host does the I/O the outcome
// asks for; every decision is here.
func (r *renderRecoveryState) tick(in renderStandDownInputs, now time.Time) renderTickOutcome {
	if reason := r.standDownReasonAt(in, now); reason != "" {
		return renderTickOutcome{
			action:           renderTickStoodDown,
			standDownReason:  reason,
			standDownChanged: r.applyStandDown(reason, now),
		}
	}
	out := renderTickOutcome{stoodUpFrom: r.applyStandUp(now)}
	missed, silent, hung := r.hangDetected(now)
	if hung {
		out.action = renderTickRecover
		out.missed, out.silent = missed, silent
		return out
	}
	out.action = renderTickPing
	return out
}

// applyStandDown records that the watchdog is standing down for reason, and
// reports whether that is a transition worth logging.
//
// Anything dispatched before or during the stand-down is written off here
// rather than at stand-up, so the deadline is always measured from the most
// recent tick that could have been answered.
func (r *renderRecoveryState) applyStandDown(reason string, now time.Time) bool {
	if r.standDownReason == "" {
		r.standDownSince = now
	}
	changed := r.standDownReason != reason
	r.standDownReason = reason
	r.resetPingDeadline(now)
	return changed
}

// applyStandUp clears the stand-down and returns the reason that ended, or ""
// if the watchdog was already up (in which case nothing was changed).
func (r *renderRecoveryState) applyStandUp(now time.Time) string {
	previous := r.standDownReason
	if previous == "" {
		return ""
	}
	r.standDownReason = ""
	r.resetPingDeadline(now)
	return previous
}

// resetPingDeadline discards the outstanding pings and restarts the hang
// deadline from now. Used wherever the renderer is about to get, or has just
// been given, a clean slate — stand-down, stand-up, end of a recovery
// episode, navigation commit, controller replacement — so that a stale
// outstanding ping can never trip the watchdog the moment it comes back.
//
// Note what it does that nonPongReceived deliberately does not: it
// moves lastPongAt forward, which is a claim that the renderer is known-good
// as of now. Only call it where that claim is true.
func (r *renderRecoveryState) resetPingDeadline(now time.Time) {
	r.lastPongSerial = r.pingSerial
	r.lastPongAt = now
	r.nonPongStreak = 0
}

// missedPings is how many dispatched pings the renderer has not answered.
// Written as a comparison rather than a subtraction because lastPongSerial
// can legitimately equal pingSerial and must never be allowed to exceed it
// (unsigned underflow).
func (r *renderRecoveryState) missedPings() uint64 {
	if r.pingSerial <= r.lastPongSerial {
		return 0
	}
	return r.pingSerial - r.lastPongSerial
}

// nextPingSerial is the serial the next ping should carry. The state is not
// advanced until pingDispatched confirms the request actually went out.
func (r *renderRecoveryState) nextPingSerial() uint64 {
	return r.pingSerial + 1
}

// pingDispatched records that the ping carrying serial reached the browser. A
// ping that was never dispatched must not count against the hang deadline —
// no completion can arrive for it.
func (r *renderRecoveryState) pingDispatched(serial uint64) {
	r.pingSerial = serial
}

// pingIsOutstanding reports whether the ping carrying serial is one the
// watchdog is still waiting on. A completion for a ping already written off —
// by a deadline reset, or by an earlier discard that forgot everything in
// flight — says nothing new, and a serial never dispatched at all cannot be
// genuine.
func (r *renderRecoveryState) pingIsOutstanding(serial uint64) bool {
	return serial > r.lastPongSerial && serial <= r.pingSerial
}

// recordPong applies a completion that carried a ping serial back, and
// reports whether it advanced the liveness proof.
func (r *renderRecoveryState) recordPong(serial uint64, now time.Time) bool {
	// Getting a serial back at all means the renderer evaluated script, which
	// is exactly what the non-pong streak counts the absence of — even when
	// the pong is too old to move the deadline.
	r.nonPongStreak = 0
	if !r.pingIsOutstanding(serial) {
		// Superseded (an older ping answering after a newer one) or from a run
		// the watchdog has already written off. Either way it says nothing
		// about the renderer's current state.
		return false
	}
	r.lastPongSerial = serial
	r.lastPongAt = now
	return true
}

// nonPongReceived applies a completion that was NOT a pong: a failure HRESULT,
// or the S_OK/"null" WebView2 answers still-pending ExecuteScript requests
// with when their target document goes away. It reports the consecutive
// non-pong streak, and false if the completion was for a ping already written
// off, in which case nothing changed.
//
// It writes the outstanding pings off without touching lastPongAt, and that
// split is the whole point. Advancing lastPongAt (which a full
// resetPingDeadline would) credits the renderer with proof it never gave: a
// recurring non-pong source — a document that is torn down and never commits
// — would hold measured silence near zero forever and the watchdog could
// never fire. Leaving the outstanding pings counted instead would let an
// ordinary burst of reloads accumulate misses and trip the watchdog on a
// perfectly healthy renderer. Forgetting the serials and keeping the clock
// gives neither: a discard storm cannot trip the miss gate, and it cannot
// manufacture liveness either, so the moment the storm stops the accumulated
// silence is already counted against the renderer and detection is immediate.
//
// The cost is that a PERMANENT discard storm suppresses detection — misses
// keep resetting to zero and the miss gate never reaches
// renderPingHangMisses. That is deliberate: every discarded completion is the
// browser process answering, so the failure is a broken document rather than
// a wedged main thread, and a controller rebuild is the wrong response. The
// streak this returns is what makes it visible instead.
func (r *renderRecoveryState) nonPongReceived(sent uint64) (int, bool) {
	if !r.pingIsOutstanding(sent) {
		return r.nonPongStreak, false
	}
	r.lastPongSerial = r.pingSerial
	r.nonPongStreak++
	return r.nonPongStreak, true
}

// hangDetected applies the two-condition hang predicate. See
// renderPingHangMisses for why it is two conditions and which one leads.
func (r *renderRecoveryState) hangDetected(now time.Time) (missed uint64, silent time.Duration, hung bool) {
	missed = r.missedPings()
	silent = now.Sub(r.lastPongAt)
	return missed, silent, missed >= renderPingHangMisses && silent >= renderPingHangDeadline
}

// navigationCommitted records that a navigation reached the renderer. That
// both proves the renderer runs script and invalidates every ping issued
// against the document being replaced, which nothing will answer now.
//
// Reports whether it ended a rebuild: a navigation committing IS the proof
// that the rebuilt controller works, so it retires both the indicator and the
// retry budget. The host repaints when that flips.
func (r *renderRecoveryState) navigationCommitted(now time.Time) bool {
	r.firstNavigationDone = true
	r.resetPingDeadline(now)
	if !r.rebuildInFlight {
		return false
	}
	r.rebuildInFlight = false
	r.rebuildRetryUsed = false
	return true
}

// rebuildStarted records that the host is replacing the controller, and
// reports whether that is a change (the host paints its indicator on the
// transition). Deliberately does not touch rebuildRetryUsed: the retry IS a
// second rebuild, and clearing the budget here would make it a loop.
func (r *renderRecoveryState) rebuildStarted() bool {
	if r.rebuildInFlight {
		return false
	}
	r.rebuildInFlight = true
	return true
}

// useRebuildRetry claims the single retry a recovery episode gets, reporting
// false if it has already been spent.
func (r *renderRecoveryState) useRebuildRetry() bool {
	if r.rebuildRetryUsed {
		return false
	}
	r.rebuildRetryUsed = true
	return true
}

// controllerReplaced records that the host now points at a different
// controller. Everything the watchdog knew described the old one: its
// outstanding pings will never be answered (and a late completion from it must
// not be read as the new renderer being alive), a fresh controller is never
// suspended, and its first navigation has not committed yet — a rebuild is a
// cold start in every sense, including for the first-navigation budget, which
// restarts here.
func (r *renderRecoveryState) controllerReplaced(now time.Time) {
	r.suspended = false
	r.firstNavigationDone = false
	r.standDownSince = now
	r.resetPingDeadline(now)
}

// beginEpisode opens a recovery episode and returns its identity. Reports
// false when one is already open: repeat hang signals during an episode are
// informational, and extending the deadline for each one would let a renderer
// that keeps reporting itself unresponsive postpone the rebuild indefinitely.
func (r *renderRecoveryState) beginEpisode() (uint64, bool) {
	if r.active {
		return r.episode, false
	}
	r.active = true
	r.episode++
	return r.episode, true
}

// endEpisode closes the open episode and cancels its navigate deadline,
// reporting whether one was open.
func (r *renderRecoveryState) endEpisode(now time.Time) bool {
	if !r.active {
		return false
	}
	r.stopNavigateTimer()
	r.active = false
	// The renderer this window is about to watch is either freshly navigated
	// or freshly built; either way the previous episode's outstanding pings
	// say nothing about it.
	r.resetPingDeadline(now)
	return true
}

// WebView2 error policy.
//
// edge.Chromium reports errors it cannot recover from itself and no longer
// decides what happens next — it used to end every one of them with
// os.Exit(1), which is how a user closing the window during a controller
// rebuild (E_ABORT on the pending CreateCoreWebView2Controller) became a
// silent process kill. The decision needs host state, and lives here.

// webviewErrorInputs are the facts the disposition needs. Gathered by the
// host at the moment the error is reported, never cached.
type webviewErrorInputs struct {
	// shuttingDown is true once the window is on its way out: the host called
	// edge.Chromium.ShuttingDown, or the HWND is gone. Errors after that are
	// teardown noise.
	shuttingDown bool
	// controllerReady is w.chromium.IsReady() — whether a usable controller
	// exists right now. False during boot and between a failed creation and
	// its replacement.
	controllerReady bool
	// rebuildInFlight and rebuildRetryUsed mirror renderRecoveryState.
	rebuildInFlight  bool
	rebuildRetryUsed bool
}

// webviewErrorDisposition is what the host should do about a reported error.
type webviewErrorDisposition int

const (
	// webviewErrorIgnore: log it and carry on. The window is closing; there
	// is nothing left to recover and nothing to tell the user about.
	webviewErrorIgnore webviewErrorDisposition = iota
	// webviewErrorRetryRebuild: rebuild the controller once more after a
	// short delay. Only ever chosen when the retry budget is unspent.
	webviewErrorRetryRebuild
	// webviewErrorFatalStartup: no controller has ever come up for this
	// window. The app cannot run without a webview, so this ends the process
	// — visibly, never silently.
	webviewErrorFatalStartup
	// webviewErrorFatalRuntime: a controller was up and the view is now
	// unusable — a rebuild that failed twice, or a fatal COM error on a live
	// controller. Also ends the process visibly.
	webviewErrorFatalRuntime
)

// classifyWebviewError applies the policy. The order is the argument: a
// window that is going away outranks everything (a user closing it mid-rebuild
// is a normal close, not a crash), then a rebuild with budget left is worth
// one more attempt, and only then does the absence of a controller mean the
// app never got off the ground.
func classifyWebviewError(in webviewErrorInputs) webviewErrorDisposition {
	switch {
	case in.shuttingDown:
		return webviewErrorIgnore
	case in.rebuildInFlight && !in.rebuildRetryUsed:
		return webviewErrorRetryRebuild
	case in.rebuildInFlight || in.controllerReady:
		return webviewErrorFatalRuntime
	default:
		return webviewErrorFatalStartup
	}
}
