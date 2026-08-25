//go:build windows

package record

func processAlive(_ int) bool {
	// Age-based stale-lock recovery remains available on Windows.
	return true
}
