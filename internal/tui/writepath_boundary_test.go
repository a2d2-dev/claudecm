package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectorPackageDoesNotImportWritepathOrCommit(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(body)
		for _, forbidden := range []string{
			"github.com/a2d2-dev/claudecm/internal/writepath",
			"github.com/a2d2-dev/claudecm/internal/commit",
		} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s imports forbidden write pipeline package %s", file, forbidden)
			}
		}
	}
}
