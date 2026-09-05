//go:build !windows

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"
)

// unixPTY is a shell running on a Unix pseudo-terminal. The master fd is both
// the read and write side (the PTY interleaves stdout/stderr the way a real
// terminal does), so Read/Write go straight to it.
type unixPTY struct {
	master *os.File
	cmd    *exec.Cmd

	waitOnce sync.Once
	waitErr  error
	exitCode int
}

// startPlatformPTY starts shell (or the platform default) in a Unix PTY.
//
// Shell resolution: the requested shell, else $SHELL, else /bin/bash, else
// /bin/sh. The shell is started as an interactive login shell ("-l -i") so the
// operator gets their normal profile/prompt, matching an SSH login.
//
// cwd empty means the node user's home directory.
func startPlatformPTY(shell, cwd string, cols, rows uint16, extraEnv []string) (ptyHandle, error) {
	shellPath, args := resolveUnixShell(shell)
	workDir, err := resolveTerminalCwd(cwd)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(shellPath, args...)
	cmd.Dir = workDir
	// TERM is required for curses/colour apps to behave; the browser side is
	// xterm.js, so advertise xterm-256color. extraEnv (HOME/USERPROFILE for a
	// maintenance shell in a shared environment) is appended last so it wins
	// over the inherited host environ.
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Env = append(cmd.Env, extraEnv...)

	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, fmt.Errorf("start pty shell %s: %w", shellPath, err)
	}
	return &unixPTY{master: master, cmd: cmd}, nil
}

// resolveUnixShell picks the shell binary and its argv. An explicit request wins
// (as-is, so an operator can ask for a specific shell); otherwise $SHELL, then
// bash, then sh.
func resolveUnixShell(requested string) (string, []string) {
	if s := strings.TrimSpace(requested); s != "" {
		return s, []string{"-l", "-i"}
	}
	if s := strings.TrimSpace(os.Getenv("SHELL")); s != "" {
		return s, []string{"-l", "-i"}
	}
	for _, candidate := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, []string{"-l", "-i"}
		}
	}
	return "/bin/sh", []string{"-l", "-i"}
}

func (p *unixPTY) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *unixPTY) Write(b []byte) (int, error) { return p.master.Write(b) }

func (p *unixPTY) Resize(cols, rows uint16) error {
	return pty.Setsize(p.master, &pty.Winsize{Cols: cols, Rows: rows})
}

// Close kills the shell process group and releases the master fd. Killing the
// process (not just closing the fd) ensures no orphan shell survives a dropped
// console connection.
func (p *unixPTY) Close() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return p.master.Close()
}

// Wait reaps the shell and returns its exit code. Safe to call repeatedly: the
// result is memoised, since both the pump goroutine and Close may race here.
func (p *unixPTY) Wait() (int, error) {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		p.waitErr = err
		if p.cmd.ProcessState != nil {
			p.exitCode = p.cmd.ProcessState.ExitCode()
		} else {
			p.exitCode = -1
		}
		// A killed/normally-exited shell is not an error worth surfacing to the
		// console; only report genuinely unexpected wait failures.
		if _, ok := err.(*exec.ExitError); ok {
			p.waitErr = nil
		}
	})
	return p.exitCode, p.waitErr
}
