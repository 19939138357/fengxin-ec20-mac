//go:build darwin && cgo && integrationtest

package main

import (
	"fmt"
	"github.com/iniwex5/vohive/internal/notify"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fengxin-api-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = notify.OpenIsolatedTestKeychain(filepath.Join(dir, "mail.keychain")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	notify.CloseIsolatedTestKeychain()
	os.RemoveAll(dir)
	os.Exit(code)
}
