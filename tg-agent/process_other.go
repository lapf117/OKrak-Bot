//go:build !unix

package main

import (
	"os"
	"os/exec"
	"time"
)

func configureProcess(cmd *exec.Cmd) { cmd.WaitDelay = 2 * time.Second }
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func inheritOwner(f *os.File, target string) {}
