//go:build test

package cmd

var readClipboardText addAutoClipboardReader = readClipboardTextWithOSCommands

func setAddAutoClipboardForTest(fn addAutoClipboardReader) func() {
	prev := readClipboardText
	readClipboardText = fn
	return func() { readClipboardText = prev }
}
