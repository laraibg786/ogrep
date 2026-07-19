package all

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/laraibg786/ogrep/internal/registry"
)

// TestLockFilesAreNotRecognizedByAnyFormat is a regression test for the
// walker's file-walking having no special-cased knowledge of MS
// Office's transient lock files (e.g. "~$report.docx", created while a
// document is open in Word/PowerPoint/Excel): none of docx/pptx/xlsx's
// Sniff implementations ever validate that tiny placeholder content as
// a real zip package, and text's Sniff explicitly refuses the three
// OOXML extensions regardless of content, so a lock file is silently
// unrecognized by every registered extractor here -- exactly like any
// other file no format claims -- without the walker needing to know
// these three extensions are special at all.
func TestLockFilesAreNotRecognizedByAnyFormat(t *testing.T) {
	for _, name := range []string{"~$report.docx", "~$notes.pptx", "~$budget.xlsx"} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte("Jane Doe"), 0o644); err != nil {
			t.Fatal(err)
		}

		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			t.Fatal(err)
		}

		if _, ok := registry.Default.For(path, f, info.Size()); ok {
			t.Errorf("%s: expected no extractor to claim a lock file, but one did", name)
		}
		f.Close()
	}
}
