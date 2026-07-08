//go:build !test

package cmd

func readClipboardText() (string, bool, error) {
	return readClipboardTextWithOSCommands()
}
