//go:build windows
// +build windows

package main

import (
	"os/exec"
	"syscall"
)

const CREATE_NEW_PROCESS_GROUP = 0x00000200

func setCmdSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: CREATE_NEW_PROCESS_GROUP,
	}
}
