//go:build windows

package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/UserExistsError/conpty"
)

// windowsPTY is a shell running on a Windows pseudo console (ConPTY). ConPTY
// exposes Read/Write over the console's pipes and a Resize taking int dims.
type windowsPTY struct {
	cpty *conpty.ConPty

	waitOnce sync.Once
	waitErr  error
	exitCode int
}

// startPlatformPTY starts shell (or the platform default) in a Windows ConPTY.
//
// Shell resolution: the requested shell, else powershell.exe, else cmd.exe.
// cwd empty means the node user's home directory. Requires Windows 10 1809+
// (ConPTY); older hosts return a clear "unsupported" error the console shows.
func startPlatformPTY(shell, cwd string, cols, rows uint16, extraEnv []string) (ptyHandle, error) {
	if !conpty.IsConPtyAvailable() {
		return nil, fmt.Errorf("interactive terminal requires Windows 10 1809+ (ConPTY unavailable)")
	}
	cmdLine := resolveWindowsShell(shell)
	workDir, err := resolveTerminalCwd(cwd)
	if err != nil {
		return nil, err
	}

	opts := []conpty.ConPtyOption{
		conpty.ConPtyDimensions(int(cols), int(rows)),
	}
	if workDir != "" {
		opts = append(opts, conpty.ConPtyWorkDir(workDir))
	}
	// extraEnv (HOME/USERPROFILE for a maintenance shell) wins over the host
	// environ: append it after os.Environ() so the environment's home is the one
	// the shell lands in.
	env := os.Environ()
	env = append(env, extraEnv...)
	opts = append(opts, conpty.ConPtyEnv(env))

	cpty, err := conpty.Start(cmdLine, opts...)
	if err != nil {
		return nil, fmt.Errorf("start conpty shell %q: %w", cmdLine, err)
	}
	return &windowsPTY{cpty: cpty}, nil
}

// resolveWindowsShell picks the shell command line. An explicit request wins;
// otherwise powershell, then cmd.
func resolveWindowsShell(requested string) string {
	if s := strings.TrimSpace(requested); s != "" {
		return s
	}
	// PowerShell is the modern default; cmd is the universal fallback.
	if _, err := os.Stat(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`); err == nil {
		return "powershell.exe"
	}
	return "cmd.exe"
}

func (p *windowsPTY) Read(b []byte) (int, error)  { return p.cpty.Read(b) }
func (p *windowsPTY) Write(b []byte) (int, error) { return p.cpty.Write(b) }

func (p *windowsPTY) Resize(cols, rows uint16) error {
	return p.cpty.Resize(int(cols), int(rows))
}

// Close terminates the console process and releases all handles.
func (p *windowsPTY) Close() error {
	return p.cpty.Close()
}

// Wait blocks for the shell to exit and returns its code. Memoised so the pump
// goroutine and Close can both call it safely.
func (p *windowsPTY) Wait() (int, error) {
	p.waitOnce.Do(func() {
		code, err := p.cpty.Wait(context.Background())
		p.exitCode = int(code)
		p.waitErr = err
	})
	return p.exitCode, p.waitErr
}
