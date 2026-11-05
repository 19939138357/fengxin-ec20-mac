//go:build !windows

package cscall

import "os/exec"

func hideConsole(cmd *exec.Cmd) {}
