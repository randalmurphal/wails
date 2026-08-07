package application

import (
	"testing"
	"time"
)

// Unit tests for the renderer-hang state machine. They run on every platform:
// the logic under test has no Windows or WebView2 dependency, and the reason
// it now lives in renderwatch_state.go is precisely so that it can be driven
// through sequences no manual test on a real WebView2 could reproduce
// reliably (a hang that starts during boot, a discard storm, sleep/resume).

// renderWatchHarness drives renderRecoveryState on a virtual clock.
//
// It reproduces exactly two things the Windows host does that the state
// machine cannot: it supplies the window-derived stand-down inputs, and on a
// renderTickPing outcome it performs sendRenderPing's success path
// (nextPingSerial + pingDispatched). Everything else is the real code.
type renderWatchHarness struct {
	t   *testing.T
	r   renderRecoveryState
	in  renderStandDownInputs
	now time.Time
	// dispatchFails makes the simulated ExecuteScript refuse, i.e. the ping is
	// never sent and so must never count against the deadline.
	dispatchFails bool
	// inFlight holds the serials of pings dispatched but not yet answered, in
	// dispatch order.
	inFlight []uint64
}

// newRenderWatchHarness returns a harness in steady state: watchdog up,
// controller ready, first navigation committed, one ping sent and answered.
// h.now is the moment of that last pong and nothing is outstanding, so a test
// can measure detection latency straight off the clock.
func newRenderWatchHarness(t *testing.T) *renderWatchHarness {
	t.Helper()
	h := &renderWatchHarness{
		t:   t,
		now: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		in:  renderStandDownInputs{hasHandle: true, controllerReady: true},
	}
	h.r.start(h.now)
	h.r.navigationCommitted(h.now)
	// The first tick after start clears the synthetic "watchdog starting"
	// stand-down and sends the first ping.
	h.tick()
	h.pong()
	return h
}

func (h *renderWatchHarness) advance(d time.Duration) {
	h.now = h.now.Add(d)
}

// tick advances the clock by the interval the state machine asked for and
// runs one tick, mirroring renderPingTick.
func (h *renderWatchHarness) tick() renderTickOutcome {
	h.t.Helper()
	out := h.r.tick(h.in, h.now)
	if out.action == renderTickPing {
		serial := h.r.nextPingSerial()
		if h.dispatchFails {
			return out
		}
		h.r.pingDispatched(serial)
		h.inFlight = append(h.inFlight, serial)
	}
	return out
}

// tickAfterInterval waits out the cadence the state machine chose and ticks.
func (h *renderWatchHarness) tickAfterInterval() renderTickOutcome {
	h.t.Helper()
	h.advance(h.r.pingTickInterval())
	return h.tick()
}

// pong answers the oldest outstanding ping, as a healthy renderer does.
func (h *renderWatchHarness) pong() {
	h.t.Helper()
	if len(h.inFlight) == 0 {
		h.t.Fatal("pong with no ping in flight")
	}
	serial := h.inFlight[0]
	h.inFlight = h.inFlight[1:]
	if !h.r.recordPong(serial, h.now) {
		h.t.Fatalf("pong for serial %d was rejected", serial)
	}
}

// nonPong answers the oldest outstanding ping the way the browser answers a
// request whose document went away, and reports the resulting streak. It
// clears every outstanding serial locally too, because that is what
// nonPongReceived does to the state: after it, no previously dispatched
// serial can ever be accepted again.
func (h *renderWatchHarness) nonPong() int {
	h.t.Helper()
	if len(h.inFlight) == 0 {
		h.t.Fatal("non-pong with no ping in flight")
	}
	sent := h.inFlight[0]
	h.inFlight = nil
	streak, counted := h.r.nonPongReceived(sent)
	if !counted {
		h.t.Fatalf("non-pong for outstanding serial %d was not counted", sent)
	}
	return streak
}

// runTicks ticks n times at the state machine's own cadence, answering every
// ping if alive, and returns the first tick that asked for recovery.
func (h *renderWatchHarness) runTicks(n int, alive bool) (renderTickOutcome, bool) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		out := h.tickAfterInterval()
		if out.action == renderTickRecover {
			return out, true
		}
		if alive && out.action == renderTickPing {
			h.pong()
		}
	}
	return renderTickOutcome{}, false
}

// --- Fix 1: the first-navigation stand-down needs its own latch -------------

// hostSetURL mirrors windowsWebviewWindow.setURL: the host clears its own
// webviewNavigationCompleted flag and navigates. It deliberately touches no
// watchdog state — that decoupling is what this test exists to pin.
func (h *renderWatchHarness) hostSetURL() {}

// TestRenderWatchStateFirstNavLatchSurvivesRenavigation is the regression test
// for the nav-latch defect: the stand-down used to be gated on
// windowsWebviewWindow.webviewNavigationCompleted, whose comment claimed it
// latched forever when in fact setURL clears it on EVERY navigation. The
// watchdog was therefore blind for the whole of every runtime SetURL — and
// terminally blind if the renderer wedged and the navigation never committed.
func TestRenderWatchStateFirstNavLatchSurvivesRenavigation(t *testing.T) {
	h := newRenderWatchHarness(t)

	h.advance(time.Second)
	h.hostSetURL()

	if reason := h.r.standDownReasonAt(h.in, h.now); reason != "" {
		t.Fatalf("watchdog stood down for a runtime re-navigation: %q", reason)
	}
	// The window between the navigate and its commit is exactly where the
	// failure hides: a renderer that never picks the navigation up is the
	// thing being detected, so a stand-down there detects nothing, ever.
	if _, recovered := h.runTicks(10, false); !recovered {
		t.Fatal("a wedge during an uncommitted runtime re-navigation was never detected")
	}

	// A commit that does arrive re-affirms the latch rather than setting it
	// for the first time.
	h.r.navigationCommitted(h.now)
	if !h.r.firstNavigationDone {
		t.Fatal("navigationCommitted did not hold the first-navigation latch")
	}
	if reason := h.r.standDownReasonAt(h.in, h.now); reason != "" {
		t.Fatalf("watchdog stood down after the re-navigation committed: %q", reason)
	}
}

// TestRenderWatchStateFirstNavLatchClearedByControllerReplace: a rebuild
// genuinely is a cold start, so the latch — and the budget clock — reset.
func TestRenderWatchStateFirstNavLatchClearedByControllerReplace(t *testing.T) {
	h := newRenderWatchHarness(t)
	h.advance(30 * time.Second)
	h.r.controllerReplaced(h.now)

	if h.r.firstNavigationDone {
		t.Fatal("controllerReplaced left the first-navigation latch set")
	}
	if got := h.r.standDownReasonAt(h.in, h.now); got != renderStandDownFirstNavigation {
		t.Fatalf("stand-down reason after rebuild = %q, want %q", got, renderStandDownFirstNavigation)
	}
	// The budget restarts with the controller: 30s of pre-rebuild stand-down
	// must not be charged against the new controller's boot.
	h.advance(renderFirstNavigationStandDownBudget - time.Second)
	if got := h.r.standDownReasonAt(h.in, h.now); got != renderStandDownFirstNavigation {
		t.Fatalf("stand-down ended early after rebuild: %q", got)
	}
	h.r.navigationCommitted(h.now)
	if reason := h.r.standDownReasonAt(h.in, h.now); reason != "" {
		t.Fatalf("watchdog still stood down after the rebuild's navigation committed: %q", reason)
	}
}

// TestRenderWatchStateFirstNavStandDownIsBounded: a renderer that wedges
// before its boot navigation commits must still be caught. Without the
// budget, nothing ever clears the stand-down and the window stays frozen for
// the life of the process.
func TestRenderWatchStateFirstNavStandDownIsBounded(t *testing.T) {
	var r renderRecoveryState
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.start(now)
	in := renderStandDownInputs{hasHandle: true, controllerReady: true}

	h := &renderWatchHarness{t: t, r: r, in: in, now: now}

	// Boot never commits. Tick until the budget expires.
	deadline := now.Add(renderFirstNavigationStandDownBudget + renderStandDownPingInterval)
	stoodUp := false
	for h.now.Before(deadline) {
		out := h.tickAfterInterval()
		if out.action != renderTickStoodDown {
			stoodUp = true
			break
		}
		if out.standDownReason != renderStandDownFirstNavigation &&
			out.standDownReason != renderStandDownStarting {
			t.Fatalf("unexpected stand-down reason during boot: %q", out.standDownReason)
		}
	}
	if !stoodUp {
		t.Fatalf("watchdog never stood up; still blind %s after start", h.now.Sub(now))
	}
	if blind := h.now.Sub(now); blind < renderFirstNavigationStandDownBudget {
		t.Fatalf("watchdog stood up after %s, before the %s budget",
			blind, renderFirstNavigationStandDownBudget)
	}
	// And having stood up, it declares the wedge.
	if _, recovered := h.runTicks(10, false); !recovered {
		t.Fatal("wedge that began during boot was never declared")
	}
}

// TestRenderWatchStateStandDownSinceStampedOnce: standDownSince marks the
// start of the current CONTINUOUS stand-down, not of the current reason, so a
// stand-down that changes reason cannot keep resetting the budget.
func TestRenderWatchStateStandDownSinceStampedOnce(t *testing.T) {
	var r renderRecoveryState
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.start(now)
	start := r.standDownSince

	now = now.Add(10 * time.Second)
	r.applyStandDown(renderStandDownControllerNotReady, now)
	if !r.standDownSince.Equal(start) {
		t.Fatalf("standDownSince re-stamped on a reason change: %v -> %v", start, r.standDownSince)
	}

	// Standing up and back down starts a fresh continuous stand-down.
	now = now.Add(10 * time.Second)
	r.applyStandUp(now)
	now = now.Add(10 * time.Second)
	r.applyStandDown(renderStandDownSuspended, now)
	if !r.standDownSince.Equal(now) {
		t.Fatalf("standDownSince = %v after a fresh stand-down, want %v", r.standDownSince, now)
	}
}

// --- Fix 4: non-pong completions must not fabricate liveness ---------------

// TestRenderWatchStateNonPongDoesNotManufactureLiveness is the regression test
// for the second defect: the errorCode!=0 and unparseable-result branches used
// to run a full deadline reset, which moved lastPongAt to now and so credited
// the renderer with proof it never gave. A recurring non-pong source held
// measured silence at ~0 forever.
func TestRenderWatchStateNonPongDoesNotManufactureLiveness(t *testing.T) {
	h := newRenderWatchHarness(t)
	lastRealPong := h.now

	// Every ping from here on is answered by the browser, not the renderer.
	for i := 0; i < 20; i++ {
		out := h.tickAfterInterval()
		if out.action != renderTickPing {
			t.Fatalf("tick %d: action = %v, want a ping", i, out.action)
		}
		h.nonPong()
	}

	if _, silent, _ := h.r.hangDetected(h.now); silent != h.now.Sub(lastRealPong) {
		t.Fatalf("measured silence = %s, want the full %s since the last real pong",
			silent, h.now.Sub(lastRealPong))
	}
	if h.r.lastPongAt != lastRealPong {
		t.Fatalf("lastPongAt moved to %v on non-pong completions; last real pong was %v",
			h.r.lastPongAt, lastRealPong)
	}

	// The instant the storm stops, the accumulated silence is already counted
	// against the renderer, so only the miss bound is left to satisfy —
	// detection takes the minimum possible number of ticks rather than
	// restarting the deadline from zero.
	if _, recovered := h.runTicks(renderPingHangMisses+1, false); !recovered {
		t.Fatalf("hang not declared within %d ticks of the discard storm ending",
			renderPingHangMisses+1)
	}
}

// TestRenderWatchStateNonPongStormDoesNotTripMissGate is the other half of the
// same split: forgetting the outstanding serials is what stops an ordinary
// burst of reloads from accumulating misses on a healthy renderer.
func TestRenderWatchStateNonPongStormDoesNotTripMissGate(t *testing.T) {
	h := newRenderWatchHarness(t)
	for i := 0; i < 20; i++ {
		out := h.tickAfterInterval()
		if out.action == renderTickRecover {
			t.Fatalf("tick %d: discard storm tripped the miss gate", i)
		}
		h.nonPong()
		if missed := h.r.missedPings(); missed != 0 {
			t.Fatalf("tick %d: %d misses outstanding after a discard", i, missed)
		}
	}
}

// TestRenderWatchStateNonPongStreak: the streak is what makes a permanent
// discard storm (which the state machine deliberately does not escalate on)
// diagnosable, so it has to count consecutively and reset on real evidence.
func TestRenderWatchStateNonPongStreak(t *testing.T) {
	h := newRenderWatchHarness(t)
	for want := 1; want <= renderNonPongWarnStreak; want++ {
		h.tickAfterInterval()
		if got := h.nonPong(); got != want {
			t.Fatalf("streak = %d, want %d", got, want)
		}
	}
	// A real pong is evidence the renderer runs script; the streak restarts.
	h.tickAfterInterval()
	h.pong()
	if h.r.nonPongStreak != 0 {
		t.Fatalf("streak = %d after a real pong, want 0", h.r.nonPongStreak)
	}

	// So is a superseded pong: it is too old to move the deadline, but the
	// renderer still evaluated it.
	h.tickAfterInterval()
	first := h.inFlight[0]
	h.nonPong()
	h.tickAfterInterval()
	if h.r.recordPong(first, h.now) {
		t.Fatal("a superseded pong advanced the deadline")
	}
	if h.r.nonPongStreak != 0 {
		t.Fatalf("streak = %d after a superseded pong, want 0", h.r.nonPongStreak)
	}

	// A full reset clears it too.
	h.tickAfterInterval()
	h.nonPong()
	h.r.resetPingDeadline(h.now)
	if h.r.nonPongStreak != 0 {
		t.Fatalf("streak = %d after a deadline reset, want 0", h.r.nonPongStreak)
	}
}

// TestRenderWatchStateNonPongForWrittenOffPingIsIgnored: a controller
// teardown answers every ping still in flight with "null" at once. Those
// completions are all for pings the first one already wrote off, so counting
// each of them would let an ordinary rebuild manufacture a discard-storm
// warning out of nothing.
func TestRenderWatchStateNonPongForWrittenOffPingIsIgnored(t *testing.T) {
	h := newRenderWatchHarness(t)
	h.runTicks(3, false)
	if len(h.inFlight) != 3 {
		t.Fatalf("%d pings in flight, want 3", len(h.inFlight))
	}
	inFlight := h.inFlight

	// The first completion writes all three off.
	if streak, counted := h.r.nonPongReceived(inFlight[0]); !counted || streak != 1 {
		t.Fatalf("first discard: streak=%d counted=%v, want 1/true", streak, counted)
	}
	// Its siblings say nothing new.
	for _, serial := range inFlight[1:] {
		streak, counted := h.r.nonPongReceived(serial)
		if counted {
			t.Fatalf("discard for already-written-off serial %d was counted", serial)
		}
		if streak != 1 {
			t.Fatalf("streak = %d after an ignored discard, want 1", streak)
		}
	}
	// And a completion for a serial that was never dispatched is not evidence
	// of anything at all.
	if _, counted := h.r.nonPongReceived(h.r.pingSerial + 100); counted {
		t.Fatal("discard for a never-dispatched serial was counted")
	}
}

// --- Serial correlation ----------------------------------------------------

func TestRenderWatchStatePongSerials(t *testing.T) {
	var r renderRecoveryState
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.start(now)
	r.pingDispatched(1)
	r.pingDispatched(2)
	r.pingDispatched(3)

	tests := []struct {
		name   string
		serial uint64
		want   bool
	}{
		{"in range advances", 2, true},
		{"superseded is rejected", 1, false},
		{"repeat of the current is rejected", 2, false},
		{"newer in range advances", 3, true},
		{"never dispatched is rejected", 4, false},
		{"far future is rejected", 1 << 40, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.recordPong(tc.serial, now); got != tc.want {
				t.Fatalf("recordPong(%d) = %v, want %v", tc.serial, got, tc.want)
			}
		})
	}
}

// TestRenderWatchStateStalePongAfterStandDown: a completion left over from
// before a stand-down must never be read as proof of current liveness. It is
// the whole reason pings carry a serial.
func TestRenderWatchStateStalePongAfterStandDown(t *testing.T) {
	h := newRenderWatchHarness(t)
	h.tickAfterInterval()
	stale := h.inFlight[0]

	// Minimise and suspend: the watchdog stands down and writes the ping off.
	h.r.suspended = true
	h.tickAfterInterval()

	// Restore, and the stale completion arrives just after stand-up.
	h.r.suspended = false
	h.advance(time.Minute)
	h.tickAfterInterval()
	if h.r.recordPong(stale, h.now) {
		t.Fatal("a pong from before the stand-down was accepted as current liveness")
	}
}

func TestRenderWatchStateMissCountNeverUnderflows(t *testing.T) {
	var r renderRecoveryState
	if got := r.missedPings(); got != 0 {
		t.Fatalf("zero value missedPings = %d, want 0", got)
	}
	r.pingSerial, r.lastPongSerial = 5, 5
	if got := r.missedPings(); got != 0 {
		t.Fatalf("equal serials missedPings = %d, want 0", got)
	}
	// lastPongSerial should never exceed pingSerial, but the subtraction it
	// replaces would wrap to ~1.8e19 and declare an instant hang if it did.
	r.pingSerial, r.lastPongSerial = 5, 9
	if got := r.missedPings(); got != 0 {
		t.Fatalf("lastPongSerial > pingSerial: missedPings = %d, want 0", got)
	}
	r.pingSerial, r.lastPongSerial = 9, 5
	if got := r.missedPings(); got != 4 {
		t.Fatalf("missedPings = %d, want 4", got)
	}
}

// TestRenderWatchStateUndispatchedPingIsNotAMiss: a ping the browser refused
// can never be answered, so counting it would let a run of transient refusals
// (a DPI transition) rebuild a healthy renderer.
func TestRenderWatchStateUndispatchedPingIsNotAMiss(t *testing.T) {
	h := newRenderWatchHarness(t)
	h.dispatchFails = true
	before := h.r.pingSerial
	for i := 0; i < 10; i++ {
		if out := h.tickAfterInterval(); out.action == renderTickRecover {
			t.Fatalf("tick %d declared a hang on pings that were never dispatched", i)
		}
	}
	if missed := h.r.missedPings(); missed != 0 {
		t.Fatalf("missedPings = %d after 10 refused dispatches, want 0", missed)
	}
	if h.r.pingSerial != before {
		t.Fatalf("pingSerial advanced to %d on refused dispatches, want %d", h.r.pingSerial, before)
	}
}

// --- Hang predicate --------------------------------------------------------

func TestRenderWatchStateHangPredicate(t *testing.T) {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		missed         uint64
		silent         time.Duration
		wantHung       bool
		whyNotDetected string
	}{
		{name: "healthy", missed: 0, silent: 0},
		{
			name: "busy main thread: misses accrued far too fast", missed: renderPingHangMisses + 2,
			silent:         renderPingHangDeadline - time.Second,
			whyNotDetected: "the wall-clock floor keeps a shortened interval honest",
		},
		{
			name: "sleep/resume: one very late tick", missed: 1,
			silent:         time.Hour,
			whyNotDetected: "wall time jumped but the renderer never had a chance to run",
		},
		{
			name: "wedged", missed: renderPingHangMisses, silent: renderPingHangDeadline,
			wantHung: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r renderRecoveryState
			r.pingSerial = tc.missed
			r.lastPongSerial = 0
			r.lastPongAt = base
			missed, silent, hung := r.hangDetected(base.Add(tc.silent))
			if missed != tc.missed || silent != tc.silent {
				t.Fatalf("hangDetected reported missed=%d silent=%s, want %d/%s",
					missed, silent, tc.missed, tc.silent)
			}
			if hung != tc.wantHung {
				t.Fatalf("hung = %v, want %v (%s)", hung, tc.wantHung, tc.whyNotDetected)
			}
		})
	}
}

// TestRenderWatchStateWedgeDetectionTiming pins the documented constant
// relationship: at the 5s cadence it is the miss bound that decides, at ~20s.
func TestRenderWatchStateWedgeDetectionTiming(t *testing.T) {
	h := newRenderWatchHarness(t)
	start := h.now
	out, recovered := h.runTicks(10, false)
	if !recovered {
		t.Fatal("a wedged renderer was never declared hung")
	}
	elapsed := h.now.Sub(start)
	if elapsed < renderPingHangDeadline {
		t.Fatalf("declared hung after %s, inside the %s floor", elapsed, renderPingHangDeadline)
	}
	if want := renderPingInterval * (renderPingHangMisses + 1); elapsed != want {
		t.Fatalf("declared hung after %s, want %s (miss bound decides at the 5s cadence)",
			elapsed, want)
	}
	if out.missed != renderPingHangMisses {
		t.Fatalf("declared hung on %d misses, want %d", out.missed, renderPingHangMisses)
	}
}

// TestRenderWatchStateHealthyRendererIsNeverDeclaredHung is the false-positive
// guard: a renderer answering every ping must survive indefinitely.
func TestRenderWatchStateHealthyRendererIsNeverDeclaredHung(t *testing.T) {
	h := newRenderWatchHarness(t)
	if _, recovered := h.runTicks(500, true); recovered {
		t.Fatal("a renderer answering every ping was declared hung")
	}
}

// --- Stand-down conditions -------------------------------------------------

func TestRenderWatchStateStandDownConditions(t *testing.T) {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	ready := renderStandDownInputs{hasHandle: true, controllerReady: true}

	tests := []struct {
		name  string
		setup func(r *renderRecoveryState)
		in    renderStandDownInputs
		want  string
	}{
		{
			name: "recovery episode wins over everything",
			setup: func(r *renderRecoveryState) {
				r.beginEpisode()
				r.suspended = true
			},
			in:   renderStandDownInputs{},
			want: renderStandDownRecovering,
		},
		{
			name:  "no window handle",
			setup: func(r *renderRecoveryState) {},
			in:    renderStandDownInputs{controllerReady: true},
			want:  renderStandDownNoHandle,
		},
		{
			name:  "suspended while minimised is routine",
			setup: func(r *renderRecoveryState) { r.suspended = true },
			in:    ready,
			want:  renderStandDownSuspended,
		},
		{
			name:  "suspended while visible is a host defect",
			setup: func(r *renderRecoveryState) { r.suspended = true },
			in: renderStandDownInputs{
				hasHandle: true, controllerReady: true, visibleWhileSuspended: true,
			},
			want: renderStandDownSuspendedVisible,
		},
		{
			name:  "controller not ready",
			setup: func(r *renderRecoveryState) {},
			in:    renderStandDownInputs{hasHandle: true},
			want:  renderStandDownControllerNotReady,
		},
		{
			name:  "first navigation pending",
			setup: func(r *renderRecoveryState) {},
			in:    ready,
			want:  renderStandDownFirstNavigation,
		},
		{
			name:  "watching",
			setup: func(r *renderRecoveryState) { r.navigationCommitted(base) },
			in:    ready,
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r renderRecoveryState
			r.start(base)
			tc.setup(&r)
			if got := r.standDownReasonAt(tc.in, base); got != tc.want {
				t.Fatalf("standDownReasonAt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderWatchStateStandDownIsNotGatedOnMinimise: a wedged renderer can flip
// WS_MINIMIZE on the host window with nobody having minimised it, so a
// minimise gate would disable detection exactly when it is needed. The only
// window state that stands the watchdog down is the one the host sets itself.
func TestRenderWatchStateStandDownIsNotGatedOnMinimise(t *testing.T) {
	h := newRenderWatchHarness(t)
	// A minimised (but not suspended) window: visibleWhileSuspended is
	// irrelevant because suspended is false, and nothing else changes.
	if reason := h.r.standDownReasonAt(h.in, h.now); reason != "" {
		t.Fatalf("stood down on a minimised, unsuspended window: %q", reason)
	}
	if _, recovered := h.runTicks(10, false); !recovered {
		t.Fatal("a wedge on a minimised window was never detected")
	}
}

// TestRenderWatchStateStandDownTransitionsLogOnce: the log line is driven by
// standDownChanged, so a stand-down holding for hours must report a change
// exactly once, and a reason replacing another must report one too.
func TestRenderWatchStateStandDownTransitionsLogOnce(t *testing.T) {
	h := newRenderWatchHarness(t)
	h.r.suspended = true

	first := h.tickAfterInterval()
	if !first.standDownChanged {
		t.Fatal("the first stand-down tick did not report a transition")
	}
	for i := 0; i < 5; i++ {
		if out := h.tickAfterInterval(); out.standDownChanged {
			t.Fatalf("repeat stand-down tick %d reported a transition", i)
		}
	}
	// One reason replacing another is a transition worth logging.
	h.in.visibleWhileSuspended = true
	swapped := h.tickAfterInterval()
	if !swapped.standDownChanged || swapped.standDownReason != renderStandDownSuspendedVisible {
		t.Fatalf("reason swap reported changed=%v reason=%q",
			swapped.standDownChanged, swapped.standDownReason)
	}

	h.r.suspended = false
	h.in.visibleWhileSuspended = false
	up := h.tickAfterInterval()
	if up.stoodUpFrom != renderStandDownSuspendedVisible {
		t.Fatalf("stoodUpFrom = %q, want %q", up.stoodUpFrom, renderStandDownSuspendedVisible)
	}
	if again := h.tickAfterInterval(); again.stoodUpFrom != "" {
		t.Fatalf("stoodUpFrom = %q on an already-armed watchdog, want \"\"", again.stoodUpFrom)
	}
}

// TestRenderWatchStateStandUpStartsAFreshDeadline reproduces spike case D: a
// wedge that began while suspended must not trip the instant the window is
// restored on the strength of stale outstanding pings.
func TestRenderWatchStateStandUpStartsAFreshDeadline(t *testing.T) {
	h := newRenderWatchHarness(t)
	// Accumulate outstanding pings, then suspend for an hour.
	h.runTicks(2, false)
	h.r.suspended = true
	h.tickAfterInterval()
	h.advance(time.Hour)

	h.r.suspended = false
	if out := h.tick(); out.action == renderTickRecover {
		t.Fatal("stand-up declared a hang on stale pings")
	}
	// Detection then takes a full fresh deadline.
	start := h.now
	if _, recovered := h.runTicks(10, false); !recovered {
		t.Fatal("wedge not detected after restore")
	}
	if elapsed := h.now.Sub(start); elapsed < renderPingHangDeadline {
		t.Fatalf("declared hung %s after restore, inside the %s deadline",
			elapsed, renderPingHangDeadline)
	}
}

// --- Cadence ---------------------------------------------------------------

func TestRenderWatchStatePingTickInterval(t *testing.T) {
	h := newRenderWatchHarness(t)
	if got := h.r.pingTickInterval(); got != renderPingInterval {
		t.Fatalf("armed interval = %s, want %s", got, renderPingInterval)
	}
	h.r.suspended = true
	h.tickAfterInterval()
	if got := h.r.pingTickInterval(); got != renderStandDownPingInterval {
		t.Fatalf("stood-down interval = %s, want %s", got, renderStandDownPingInterval)
	}
	h.r.suspended = false
	h.tickAfterInterval()
	if got := h.r.pingTickInterval(); got != renderPingInterval {
		t.Fatalf("interval after stand-up = %s, want %s", got, renderPingInterval)
	}
}

// --- Identity guards -------------------------------------------------------

func TestRenderWatchStateTickIdentityGuard(t *testing.T) {
	var r renderRecoveryState
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.start(now)
	cycle := r.pingCycle
	if !r.acceptTick(cycle) {
		t.Fatal("the current cycle's tick was rejected")
	}
	if r.restart(); r.acceptTick(cycle) {
		t.Fatal("a tick from before a restart was accepted")
	}
	if !r.acceptTick(r.pingCycle) {
		t.Fatal("the restarted cycle's tick was rejected")
	}
	r.stop()
	if r.acceptTick(r.pingCycle) {
		t.Fatal("a tick was accepted after the watchdog stopped")
	}
	// Restarting a stopped watchdog must not resurrect it.
	if r.restart(); r.acceptTick(r.pingCycle) {
		t.Fatal("restart resurrected a stopped watchdog")
	}
}

func TestRenderWatchStateNavigateDeadlineIdentityGuard(t *testing.T) {
	var r renderRecoveryState
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.start(now)

	episode, opened := r.beginEpisode()
	if !opened {
		t.Fatal("the first episode did not open")
	}
	if !r.acceptNavigateDeadline(episode) {
		t.Fatal("the open episode's deadline was rejected")
	}
	// A repeat hang signal inside an episode must not open a second one, or
	// the rebuild could be postponed indefinitely.
	if again, opened := r.beginEpisode(); opened || again != episode {
		t.Fatalf("a second episode opened inside the first: %d, opened=%v", again, opened)
	}
	r.endEpisode(now)
	if r.acceptNavigateDeadline(episode) {
		t.Fatal("a closed episode's deadline was accepted")
	}
	// Superseded by a later episode.
	next, _ := r.beginEpisode()
	if r.acceptNavigateDeadline(episode) {
		t.Fatalf("episode %d's deadline was accepted while episode %d is open", episode, next)
	}
	r.stop()
	if r.acceptNavigateDeadline(next) {
		t.Fatal("a deadline was accepted after the watchdog stopped")
	}
}

// --- Lifecycle -------------------------------------------------------------

func TestRenderWatchStateStartStopIdempotent(t *testing.T) {
	var r renderRecoveryState
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	if !r.start(now) {
		t.Fatal("start on a fresh state reported no change")
	}
	if r.start(now) {
		t.Fatal("start on a running watchdog reported a change")
	}
	if got := r.standDownReason; got != renderStandDownStarting {
		t.Fatalf("standDownReason after start = %q, want %q", got, renderStandDownStarting)
	}
	if !r.stop() {
		t.Fatal("stop on a running watchdog reported no change")
	}
	if r.stop() {
		t.Fatal("stop on a stopped watchdog reported a change")
	}
}

// TestRenderWatchStateEpisodeEndResetsDeadline: closing an episode gives the
// renderer that is about to be watched a clean slate, in both directions —
// navigation commit (recovered) and rebuild (replaced).
func TestRenderWatchStateEpisodeEndResetsDeadline(t *testing.T) {
	h := newRenderWatchHarness(t)
	h.runTicks(2, false)
	h.r.beginEpisode()
	h.advance(time.Minute)

	if !h.r.endEpisode(h.now) {
		t.Fatal("endEpisode on an open episode reported nothing closed")
	}
	if h.r.endEpisode(h.now) {
		t.Fatal("endEpisode on a closed episode reported a close")
	}
	if missed := h.r.missedPings(); missed != 0 {
		t.Fatalf("missedPings = %d after the episode closed, want 0", missed)
	}
	if !h.r.lastPongAt.Equal(h.now) {
		t.Fatalf("lastPongAt = %v after the episode closed, want %v", h.r.lastPongAt, h.now)
	}
}

// TestRenderWatchStateControllerReplaceClearsSuspend: a fresh controller is
// never suspended, so a rebuild that happens while the old one was suspended
// must not leave the watchdog stood down against the new one.
func TestRenderWatchStateControllerReplaceClearsSuspend(t *testing.T) {
	h := newRenderWatchHarness(t)
	h.r.suspended = true
	h.tickAfterInterval()

	h.r.controllerReplaced(h.now)
	if h.r.suspended {
		t.Fatal("controllerReplaced left the suspended flag set")
	}
	h.r.navigationCommitted(h.now)
	if reason := h.r.standDownReasonAt(h.in, h.now); reason != "" {
		t.Fatalf("watchdog stood down against a fresh controller: %q", reason)
	}
}
