//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
}
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// inheritOwner gives a new file the owner of the file it replaces (or of its
// directory). It only acts for root, e.g. "sudo tg-agent admin", so the bot's
// service user can still read what the terminal wrote.
func inheritOwner(f *os.File, target string) {
	if os.Geteuid() != 0 {
		return
	}
	info, err := os.Stat(target)
	if err != nil {
		info, err = os.Stat(filepath.Dir(target))
	}
	if err != nil {
		return
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		_ = f.Chown(int(st.Uid), int(st.Gid))
	}
}
