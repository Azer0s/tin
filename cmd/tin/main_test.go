package main

import (
	"reflect"
	"testing"
)

func TestParseFileDirectives_LinkerFlagTokenization(t *testing.T) {
	// Regression for issue #23: `//!-framework Cocoa` reached the
	// linker as one argv element ("-framework Cocoa") and ld rejected
	// it, because ld looks up `-framework` and `Cocoa` independently.
	// The directive parser must tokenize the linker flag on
	// whitespace AFTER arch-gating and $ENV expansion.
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "single-token flag stays single",
			src:  "//!-lm\n",
			want: []string{"-lm"},
		},
		{
			name: "macOS -framework splits into two tokens",
			src:  "//!-framework Cocoa\n",
			want: []string{"-framework", "Cocoa"},
		},
		{
			name: "-Xlinker -rpath -Xlinker PATH splits into four tokens",
			src:  "//!-Xlinker -rpath -Xlinker /opt/lib\n",
			want: []string{"-Xlinker", "-rpath", "-Xlinker", "/opt/lib"},
		},
		{
			name: "comma-separated -Wl form stays one token",
			src:  "//!-Wl,-z,now\n",
			want: []string{"-Wl,-z,now"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, _ := parseFileDirectives(c.src, ".", ".")
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseFileDirectives(%q): got %v, want %v", c.src, got, c.want)
			}
		})
	}
}

func TestParseFileDirectives_LinkerFlagArchGate(t *testing.T) {
	// Verify the [arch] qualifier still gates the *whole* multi-token
	// flag group: an arch the host can't be at runtime drops the entire
	// flag instead of leaking a partial argv to the linker.
	mismatch := "x86_64"
	if targetGOARCH == "amd64" {
		mismatch = "aarch64"
	}

	src := "//!-framework Cocoa [" + mismatch + "]\n"

	got, _, _ := parseFileDirectives(src, ".", ".")
	if len(got) != 0 {
		t.Fatalf("expected arch-mismatched directive to drop, got %v", got)
	}
}
