package cmd

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

func readClipboardTextWithOSCommands() (string, bool, error) {
	tools := []struct {
		name string
		args []string
	}{
		{name: "pbpaste"},
		{name: "wl-paste"},
		{name: "xclip", args: []string{"-o", "-selection", "clipboard"}},
		{name: "xsel", args: []string{"-b"}},
		{name: "powershell", args: []string{"-NoProfile", "-Command", "Get-Clipboard"}},
		{name: "powershell.exe", args: []string{"-NoProfile", "-Command", "Get-Clipboard"}},
	}
	var missing []string
	for _, tool := range tools {
		path, err := exec.LookPath(tool.name)
		if err != nil {
			missing = append(missing, tool.name)
			continue
		}
		out, err := exec.Command(path, tool.args...).Output()
		if err != nil {
			return "", false, fmt.Errorf("%s failed: %w", tool.name, err)
		}
		if strings.TrimSpace(string(out)) == "" {
			return "", false, nil
		}
		return string(out), true, nil
	}
	sort.Strings(missing)
	return "", false, fmt.Errorf("no clipboard tool found (%s)", strings.Join(missing, ", "))
}
