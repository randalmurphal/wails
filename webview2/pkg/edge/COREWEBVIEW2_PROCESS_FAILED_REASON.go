//go:build windows

package edge

import "strconv"

// COREWEBVIEW2_PROCESS_FAILED_REASON is the reason for the process failure,
// from ICoreWebView2ProcessFailedEventArgs2.
type COREWEBVIEW2_PROCESS_FAILED_REASON uint32

const (
	// An unexpected process failure occurred.
	COREWEBVIEW2_PROCESS_FAILED_REASON_UNEXPECTED = 0

	// The process became unresponsive. This only applies to the main frame's
	// render process.
	COREWEBVIEW2_PROCESS_FAILED_REASON_UNRESPONSIVE = 1

	// The process was terminated, for example from Task Manager.
	COREWEBVIEW2_PROCESS_FAILED_REASON_TERMINATED = 2

	// The process crashed.
	COREWEBVIEW2_PROCESS_FAILED_REASON_CRASHED = 3

	// The process failed to launch.
	COREWEBVIEW2_PROCESS_FAILED_REASON_LAUNCH_FAILED = 4

	// The process terminated due to running out of memory.
	COREWEBVIEW2_PROCESS_FAILED_REASON_OUT_OF_MEMORY = 5

	// The process exited because its corresponding profile was deleted.
	COREWEBVIEW2_PROCESS_FAILED_REASON_PROFILE_DELETED = 6

	// The process exited normally.
	COREWEBVIEW2_PROCESS_FAILED_REASON_NORMAL_EXIT = 7

	// The process exited abnormally.
	COREWEBVIEW2_PROCESS_FAILED_REASON_ABNORMAL_EXIT = 8

	// The process failed an integrity check.
	COREWEBVIEW2_PROCESS_FAILED_REASON_INTEGRITY_FAILURE = 9
)

// String returns the SDK constant suffix for the reason, for logs.
func (r COREWEBVIEW2_PROCESS_FAILED_REASON) String() string {
	switch r {
	case COREWEBVIEW2_PROCESS_FAILED_REASON_UNEXPECTED:
		return "UNEXPECTED"
	case COREWEBVIEW2_PROCESS_FAILED_REASON_UNRESPONSIVE:
		return "UNRESPONSIVE"
	case COREWEBVIEW2_PROCESS_FAILED_REASON_TERMINATED:
		return "TERMINATED"
	case COREWEBVIEW2_PROCESS_FAILED_REASON_CRASHED:
		return "CRASHED"
	case COREWEBVIEW2_PROCESS_FAILED_REASON_LAUNCH_FAILED:
		return "LAUNCH_FAILED"
	case COREWEBVIEW2_PROCESS_FAILED_REASON_OUT_OF_MEMORY:
		return "OUT_OF_MEMORY"
	case COREWEBVIEW2_PROCESS_FAILED_REASON_PROFILE_DELETED:
		return "PROFILE_DELETED"
	case COREWEBVIEW2_PROCESS_FAILED_REASON_NORMAL_EXIT:
		return "NORMAL_EXIT"
	case COREWEBVIEW2_PROCESS_FAILED_REASON_ABNORMAL_EXIT:
		return "ABNORMAL_EXIT"
	case COREWEBVIEW2_PROCESS_FAILED_REASON_INTEGRITY_FAILURE:
		return "INTEGRITY_FAILURE"
	}
	return "UNKNOWN(" + strconv.FormatUint(uint64(r), 10) + ")"
}
