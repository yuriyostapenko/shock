//go:build envtest || e2e_cilium

package ciliume2e

import "os"

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
