package cmd

import (
	"testing"

	"github.com/fatih/color"
)

func TestFormatSizeColored(t *testing.T) {
	originalNoColor := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = originalNoColor })

	tests := []struct {
		name string
		size int64
		want string
	}{
		{name: "zero", size: 0, want: "0"},
		{name: "small value", size: 42, want: "42"},
		{name: "five digit value", size: 99_999, want: "99999"},
		{name: "six digit value", size: 100_000, want: "100000"},
		{name: "megabyte grouping", size: 1_000_042, want: "1000042"},
		{name: "gigabyte grouping", size: 1_002_000_042, want: "1002000042"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatSizeColored(tt.size); got != tt.want {
				t.Fatalf("formatSizeColored(%d) = %q, want %q", tt.size, got, tt.want)
			}
		})
	}
}
