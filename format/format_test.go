package format

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFormatCorpus runs the formatter against every `before_*.tin` in
// testdata and compares the output to the matching `after_*.tin`.
// Each pair documents one rule's expected behavior; if a rule changes
// shape, the corresponding `after_*.tin` must be updated too.
func TestFormatCorpus(t *testing.T) {
	matches, err := filepath.Glob("testdata/before_*.tin")
	if err != nil {
		t.Fatal(err)
	}

	if len(matches) == 0 {
		t.Fatal("no testdata/before_*.tin files found")
	}

	for _, before := range matches {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(before), "before_"), ".tin")
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(before)
			if err != nil {
				t.Fatal(err)
			}

			afterPath := filepath.Join("testdata", "after_"+name+".tin")

			want, err := os.ReadFile(afterPath)
			if err != nil {
				t.Fatal(err)
			}

			got, _ := Format(string(src))
			if got != string(want) {
				t.Errorf("Format(%s) =\n%s\nwant:\n%s", before, got, string(want))
			}
		})
	}
}

// TestFormatIdempotent verifies that Format(Format(x)) == Format(x) for
// every after_*.tin in the corpus. A non-idempotent rule would silently
// produce ever-growing files and is the most common formatter bug.
func TestFormatIdempotent(t *testing.T) {
	matches, err := filepath.Glob("testdata/after_*.tin")
	if err != nil {
		t.Fatal(err)
	}

	if len(matches) == 0 {
		t.Fatal("no testdata/after_*.tin files found")
	}

	for _, after := range matches {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(after), "after_"), ".tin")
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(after)
			if err != nil {
				t.Fatal(err)
			}

			pass1, _ := Format(string(src))

			pass2, changed := Format(pass1)
			if changed {
				t.Errorf("Format is not idempotent on %s - pass2 differs from pass1:\npass1:\n%s\npass2:\n%s",
					after, pass1, pass2)
			}
		})
	}
}
