package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The handlers log through the package logger, init it for the whole suite.
func TestMain(m *testing.M) {
	initLog()
	os.Exit(m.Run())
}

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

// setValidEnv loads every required variable with workable values.
func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NTFY_URL", "https://ntfy.example.com/vikunja")
	t.Setenv("VIKUNJA_BASE_URL", "https://todo.example.com/")
	t.Setenv("VIKUNJA_TOKEN", "tok")
	t.Setenv("PUBLIC_BASE_URL", "https://notify.example.com")
	t.Setenv("REMINDER_SECRET", "reminder-secret")
	t.Setenv("DONE_SECRET", "done-secret")
}

func TestLoadConfig(t *testing.T) {
	t.Run("accepts a valid environment with defaults applied", func(t *testing.T) {
		setValidEnv(t)
		c, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if c.VikunjaBaseURL != "https://todo.example.com" {
			t.Errorf("base url = %q, want the trailing slash trimmed", c.VikunjaBaseURL)
		}
		if c.ListenAddr != ":8080" {
			t.Errorf("listen addr = %q, want :8080", c.ListenAddr)
		}
		if c.DedupeWindow != 2*time.Minute {
			t.Errorf("dedupe window = %s, want 2m", c.DedupeWindow)
		}
		if c.RequestTimeout != 10*time.Second {
			t.Errorf("request timeout = %s, want 10s", c.RequestTimeout)
		}
		if c.VikunjaIconURL == "" {
			t.Errorf("icon url = empty, want the built-in default")
		}
	})

	t.Run("rejects a missing required variable", func(t *testing.T) {
		for _, k := range []string{
			"NTFY_URL", "VIKUNJA_BASE_URL", "VIKUNJA_TOKEN",
			"PUBLIC_BASE_URL", "REMINDER_SECRET", "DONE_SECRET",
		} {
			t.Run(k, func(t *testing.T) {
				setValidEnv(t)
				os.Unsetenv(k)
				_, err := loadConfig()
				if err == nil {
					t.Fatalf("missing %s accepted", k)
				}
				if !strings.Contains(err.Error(), k) {
					t.Errorf("error %q does not name the missing %s", err, k)
				}
			})
		}
	})

	t.Run("rejects a base url without a scheme", func(t *testing.T) {
		setValidEnv(t)
		t.Setenv("VIKUNJA_BASE_URL", "todo.example.com")
		if _, err := loadConfig(); err == nil {
			t.Fatal("schemeless base url accepted")
		}
	})

	t.Run("rejects a public base url without a scheme", func(t *testing.T) {
		setValidEnv(t)
		t.Setenv("PUBLIC_BASE_URL", "notify.example.com")
		if _, err := loadConfig(); err == nil {
			t.Fatal("schemeless public url accepted")
		}
	})

	t.Run("rejects equal path secrets when signing is off", func(t *testing.T) {
		setValidEnv(t)
		t.Setenv("REMINDER_SECRET", "same")
		t.Setenv("DONE_SECRET", "same")
		if _, err := loadConfig(); err == nil {
			t.Fatal("equal path secrets accepted, leaking one would forge the other")
		}
	})

	t.Run("allows equal path secrets when signing is configured", func(t *testing.T) {
		setValidEnv(t)
		t.Setenv("REMINDER_SECRET", "same")
		t.Setenv("DONE_SECRET", "same")
		t.Setenv("VIKUNJA_WEBHOOK_SECRET", "sig")
		if _, err := loadConfig(); err != nil {
			t.Fatalf("equal path secrets rejected with signing on: %v", err)
		}
	})

	t.Run("tuning overrides apply and invalid ones fall back cleanly", func(t *testing.T) {
		setValidEnv(t)
		t.Setenv("DEDUPE_SECONDS", "5")
		t.Setenv("HTTP_TIMEOUT_SECONDS", "3")
		c, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if c.DedupeWindow != 5*time.Second || c.RequestTimeout != 3*time.Second {
			t.Errorf("got dedupe=%s timeout=%s, want 5s/3s", c.DedupeWindow, c.RequestTimeout)
		}
		t.Setenv("DEDUPE_SECONDS", "banana")
		t.Setenv("HTTP_TIMEOUT_SECONDS", "-1")
		c, err = loadConfig()
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if c.DedupeWindow != 2*time.Minute || c.RequestTimeout != 10*time.Second {
			t.Errorf("invalid values = dedupe=%s timeout=%s, want the defaults 2m/10s", c.DedupeWindow, c.RequestTimeout)
		}
	})
}
