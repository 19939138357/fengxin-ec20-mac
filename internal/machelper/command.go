package machelper

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// The runtime path is installed locally, never resolved through a shell.
func Command(mode, device string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(exe)
	raw, err := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if err != nil {
		return nil, err
	}
	var runtime struct {
		Python string `json:"python"`
	}
	if err := json.Unmarshal(raw, &runtime); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(runtime.Python) {
		return nil, fmt.Errorf("runtime Python must be an absolute path")
	}
	return exec.Command(runtime.Python, "-u", filepath.Join(dir, "vohive_mac_helper.py"), mode, device), nil
}
