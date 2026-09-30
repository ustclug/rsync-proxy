package cmd

import (
	"testing"

	"github.com/fatih/color"
	"github.com/stretchr/testify/assert"
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
			assert.Equal(t, tt.want, formatSizeColored(tt.size))
		})
	}
}

func TestFormatSizeColoredMutesLastThreeDigits(t *testing.T) {
	originalMutedColor := mutedColor
	mutedColor = color.New(color.FgHiBlack).Add(color.Bold)
	mutedColor.EnableColor()
	t.Cleanup(func() { mutedColor = originalMutedColor })

	muted := func(value string) string {
		return "\x1b[90;1m" + value
	}
	tests := []struct {
		name string
		size int64
		want string
	}{
		{name: "three digits", size: 999, want: muted("999")},
		{name: "four digits", size: 1_999, want: "1" + muted("999")},
		{name: "zero-padded suffix", size: 1_042, want: "1" + muted("042")},
		{name: "zero suffix", size: 1_000, want: "1" + muted("000")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatSizeColored(tt.size))
		})
	}
}

func TestFormatRemoteAddrColored(t *testing.T) {
	originalIPBracketColor := ipBracketColor
	originalIPAddressColor := ipAddressColor
	originalMutedColor := mutedColor
	ipBracketColor = color.New(color.FgRed)
	ipAddressColor = color.New(color.FgYellow)
	mutedColor = color.New(color.FgHiBlack).Add(color.Bold)
	ipBracketColor.EnableColor()
	ipAddressColor.EnableColor()
	mutedColor.EnableColor()
	t.Cleanup(func() {
		ipBracketColor = originalIPBracketColor
		ipAddressColor = originalIPAddressColor
		mutedColor = originalMutedColor
	})

	red := func(value string) string {
		return "\x1b[31m" + value + "\x1b[0m"
	}
	yellow := func(value string) string {
		return "\x1b[33m" + value + "\x1b[0m"
	}
	muted := func(value string) string {
		return "\x1b[90;1m" + value + "\x1b[0;22m"
	}
	tests := []struct {
		name string
		addr string
		want string
	}{
		{
			name: "IPv4",
			addr: "192.0.2.1:873",
			want: yellow("192.0.2.1") + ":" + muted("873"),
		},
		{
			name: "IPv6",
			addr: "[2001:db8::1]:12345",
			want: red("[") + yellow("2001:db8::1") + red("]") + ":" + muted("12345"),
		},
		{name: "invalid address", addr: "unknown", want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatRemoteAddrColored(tt.addr))
		})
	}
}

func TestFormatRemoteAddrColoredWithoutColor(t *testing.T) {
	originalNoColor := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = originalNoColor })

	const addr = "192.0.2.1:873"
	assert.Equal(t, addr, formatRemoteAddrColored(addr))
}
