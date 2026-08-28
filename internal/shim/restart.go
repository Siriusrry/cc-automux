package shim

import (
	"os"
	"syscall"
	"time"
)

// restartDelay is how long scheduleSelfRestart waits before replacing the process
// image, giving net/http time to finish writing and flushing the POST response
// (which only completes after the handler returns) so the browser receives its 200
// before the listener goes down. It is a package var purely so tests can shrink it
// to 0 (which also makes the trigger run inline — see scheduleSelfRestart); the
// production value is always positive.
var restartDelay = 300 * time.Millisecond

// restartProcess re-execs the current binary in place so a restart-required config
// change (listen_addr / log_max_bytes — both bound only at startup, in Run) takes
// effect with no manual stop/start. syscall.Exec replaces the process image but
// keeps the SAME pid and inherits os.Environ() (which carries the CC_AUTO_SHIM_*
// vars), so it is transparent to launchd: no child exit, KeepAlive is not
// triggered. The replacement re-runs Run -> loadConfig, which reads the
// just-persisted config and binds the new listen/log values. syscall.Exec returns
// ONLY on error. It is a package var so a test can stub it instead of re-exec'ing
// the test binary.
var restartProcess = func() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

// scheduleSelfRestart triggers the self re-exec after the current POST response has
// been flushed. In production (restartDelay > 0) it returns immediately and the
// re-exec runs from a background goroutine after restartDelay, so the handler can
// return and net/http can finish the response first. restartProcess returns ONLY on
// failure — if it does, the error is logged and the process keeps serving the
// already-hot-swapped config (only the listen port / log cap stay at their old
// bound values, i.e. exactly the prior restart-pending state). A pending restart is
// strictly safer than killing a service the user relies on, so this never exits.
// With restartDelay <= 0 (tests only) the re-exec runs inline, making the trigger
// fully deterministic with no goroutine to outlive the test.
func (p *proxyServer) scheduleSelfRestart() {
	if restartDelay <= 0 {
		runSelfRestart()
		return
	}
	go func() {
		time.Sleep(restartDelay)
		runSelfRestart()
	}()
}

func runSelfRestart() {
	if err := restartProcess(); err != nil {
		errorf("self-restart failed; new listen/log settings stay pending a manual restart, but the shim keeps serving the current config: %v", err)
	}
}
