package cscall

import (
	"github.com/iniwex5/vohive/internal/machelper"
	"os/exec"
)

func audioCommand(mode, device string) (*exec.Cmd, error) { return machelper.Command(mode, device) }
