//go:build darwin && cgo && integrationtest

package notify

// Enabled only in explicitly tagged integration tests; production builds do
// not expose the isolated-Keychain test harness.
func OpenIsolatedTestKeychain(path string) error { return startIsolatedKeychain(path) }
func CloseIsolatedTestKeychain()                 { stopIsolatedKeychain() }
