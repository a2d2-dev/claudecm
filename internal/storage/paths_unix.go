//go:build unix

package storage

import (
	"fmt"
	"os"
	"syscall"
)

// verifyHomeOwnership refuses a HOME that is /root or is root-owned when the
// process is not running as root. This is defense-in-depth against a stray
// sudo run picking up /root/.claudecm/... and clobbering the operator's real
// profile store.
func verifyHomeOwnership(home string, info os.FileInfo) error {
	euid := os.Geteuid()
	if euid == 0 {
		return nil
	}
	if home == "/root" {
		return fmt.Errorf("HOME %q refused: /root while process euid=%d", home, euid)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Non-portable stat; skip rather than false-positive.
		return nil
	}
	if stat.Uid == 0 {
		return fmt.Errorf("HOME %q refused: root-owned while process euid=%d", home, euid)
	}
	return nil
}
