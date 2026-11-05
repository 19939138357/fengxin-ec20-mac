//go:build !darwin

package cscall

import (
	"os"
	"os/exec"
	"path/filepath"
)

func audioCommand(mode, device string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(filepath.Join(filepath.Dir(exe), "VoHiveAudio.exe"), mode, device), nil
}
