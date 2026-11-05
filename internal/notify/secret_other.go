//go:build !windows && (!darwin || !cgo)

package notify

import "errors"

// This delivery is Windows-specific; never silently fall back to plaintext secrets.
func protect([]byte) ([]byte, error)                     { return nil, errors.New("Windows DPAPI is required") }
func unprotect([]byte) ([]byte, error)                   { return nil, errors.New("Windows DPAPI is required") }
func unprotectInteractive(secret []byte) ([]byte, error) { return unprotect(secret) }
