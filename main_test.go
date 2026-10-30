package main

import (
	"testing"
	"time"
)

// Real sleeps instead of a fake clock because the deduper is exercised
// against the wall clock in production, no injection point exists.
func TestDedupeClaimForget(t *testing.T) {
	tests := []struct {
		name   string
		run    func(d *deduper) bool
		window time.Duration
		want   bool
	}{
		{
			name: "first claim passes",
			run: func(d *deduper) bool {
				return d.claim("k1")
			},
			want: true,
		},
		{
			name: "second claim inside window is suppressed",
			run: func(d *deduper) bool {
				d.claim("k2")
				return d.claim("k2")
			},
			want: false,
		},
		{
			name: "claim after the window passes again",
			run: func(d *deduper) bool {
				d.claim("k3")
				time.Sleep(60 * time.Millisecond)
				return d.claim("k3")
			},
			want: true,
		},
		{
			name: "forgotten key means the failed publish can be retried",
			run: func(d *deduper) bool {
				d.claim("k4")
				d.forget("k4")
				return d.claim("k4")
			},
			want: true,
		},
		{
			name: "forget of an unknown key is a harmless no-op",
			run: func(d *deduper) bool {
				d.forget("k5")
				return d.claim("k5")
			},
			want: true,
		},
	}
	const window = 50 * time.Millisecond
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newDedupe(window)
			if got := tc.run(d); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSanitizeTitle(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"drops control characters", "a\x01b\x7fc", "abc"},
		{"newlines become spaces", "hello\nworld", "hello world"},
		{"tabs become spaces", "a\tb", "a b"},
		{"collapses whitespace runs", "  many   spaces  here ", "many spaces here"},
		{"clean title untouched", "Fix the thing", "Fix the thing"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeTitle(tc.in); got != tc.want {
				t.Errorf("sanitizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestEqualSecret(t *testing.T) {
	tests := []struct {
		name           string
		provided, want string
		expect         bool
	}{
		{"equal pair accepts", "abc", "abc", true},
		{"mismatch rejects", "abc", "abd", false},
		{"empty provided rejects", "", "abc", false},
		{"empty expected rejects", "abc", "", false},
		{"both empty rejects", "", "", false},
		{"prefix is not equal", "abc", "abcd", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := equalSecret(tc.provided, tc.want); got != tc.expect {
				t.Errorf("equalSecret(%q, %q) = %v, want %v", tc.provided, tc.want, got, tc.expect)
			}
		})
	}
}
