// Bridges Vikunja task reminders to notifications, and completes tasks
// through the Vikunja API when a notification is acknowledged.
//
// Stateless: no database, dedupe lives in memory, all configuration comes
// from the environment.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	// embeds the time zone database, so TZ works on the shell-less hardened
	// image that has no system tzdata
	_ "time/tzdata"
)

type Config struct {
	ListenAddr     string
	NTFYURL        string // publish URL including topic, e.g. https://ntfy.example.com/vikunja
	NTFYToken      string
	VikunjaBaseURL string
	VikunjaToken   string // API token, scope limited to task updates
	VikunjaIconURL string
	PublicBaseURL  string // base of the callback links
	ReminderSecret string // webhook path secret
	DoneSecret     string // callback path secret
	SignSecret     string // empty disables X-Vikunja-Signature verification
	DedupeWindow   time.Duration
	RequestTimeout time.Duration
	TitlePrefix    string
	ntfyHTTP       *http.Client
	vikunjaHTTP    *http.Client
}

func loadConfig() (*Config, error) {
	req := func(k string) (string, error) {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			return "", fmt.Errorf("missing required env %s", k)
		}
		return v, nil
	}
	opt := func(k, d string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return d
	}

	c := &Config{}
	var err error
	if c.NTFYURL, err = req("NTFY_URL"); err != nil {
		return nil, err
	}
	if c.VikunjaBaseURL, err = req("VIKUNJA_BASE_URL"); err != nil {
		return nil, err
	}
	if c.VikunjaToken, err = req("VIKUNJA_TOKEN"); err != nil {
		return nil, err
	}
	if c.PublicBaseURL, err = req("PUBLIC_BASE_URL"); err != nil {
		return nil, err
	}
	if c.ReminderSecret, err = req("REMINDER_SECRET"); err != nil {
		return nil, err
	}
	if c.DoneSecret, err = req("DONE_SECRET"); err != nil {
		return nil, err
	}
	c.NTFYToken = opt("NTFY_TOKEN", "")
	c.VikunjaIconURL = opt("VIKUNJA_ICON_URL", "https://vikunja.io/favicon.ico")
	c.SignSecret = os.Getenv("VIKUNJA_WEBHOOK_SECRET")
	c.TitlePrefix = opt("NOTIFICATION_PREFIX", "⏰ ")
	c.ListenAddr = opt("LISTEN_ADDR", ":8080")
	c.DedupeWindow = 2 * time.Minute
	if t := os.Getenv("DEDUPE_SECONDS"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			c.DedupeWindow = time.Duration(n) * time.Second
		}
	}
	c.RequestTimeout = 10 * time.Second
	if t := os.Getenv("HTTP_TIMEOUT_SECONDS"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			c.RequestTimeout = time.Duration(n) * time.Second
		}
	}
	if !strings.Contains(c.VikunjaBaseURL, "://") {
		return nil, fmt.Errorf("VIKUNJA_BASE_URL must include scheme (http:// or https://)")
	}
	if !strings.Contains(c.PublicBaseURL, "://") {
		return nil, fmt.Errorf("PUBLIC_BASE_URL must include scheme (http:// or https://)")
	}
	if c.SignSecret == "" && c.ReminderSecret == c.DoneSecret {
		return nil, fmt.Errorf("REMINDER_SECRET and DONE_SECRET must differ")
	}
	c.VikunjaBaseURL = strings.TrimRight(c.VikunjaBaseURL, "/")
	c.PublicBaseURL = strings.TrimRight(c.PublicBaseURL, "/")
	c.ntfyHTTP = &http.Client{Timeout: c.RequestTimeout}
	c.vikunjaHTTP = &http.Client{Timeout: c.RequestTimeout}
	return c, nil
}

var log *slog.Logger

func initLog() {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	log = slog.New(h)
}

type reminderEvent struct {
	EventName string `json:"event_name"`
	Time      string `json:"time"`
	Data      struct {
		Project struct {
			Title string `json:"title"`
		} `json:"project"`
		Task struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"task"`
		Reminder struct {
			Reminder string `json:"reminder"`
		} `json:"reminder"`
	} `json:"data"`
}

var notifyWG sync.WaitGroup

var dedupe *deduper

type deduper struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	window time.Duration
}

func newDedupe(window time.Duration) *deduper {
	return &deduper{seen: make(map[string]time.Time), window: window}
}

// The window counts from the first observation, no hit refreshes it, so a
// looping key leaks through again.
func (d *deduper) claim(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.seen) > 1000 {
		now := time.Now()
		for k, t := range d.seen {
			if now.Sub(t) > d.window {
				delete(d.seen, k)
			}
		}
	}
	if t, ok := d.seen[key]; ok && time.Since(t) < d.window {
		return false
	}
	// shortcut: the map is uncapped, the sweep keeps it bounded at personal volume
	d.seen[key] = time.Now()
	return true
}

func (d *deduper) forget(key string) {
	d.mu.Lock()
	delete(d.seen, key)
	d.mu.Unlock()
}

// sanitizeTitle keeps task titles from smuggling line breaks or other control
// characters into outbound ntfy headers.
func sanitizeTitle(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func (c *Config) handleReminder(w http.ResponseWriter, r *http.Request) {
	if !equalSecret(strings.Trim(r.PathValue("secret"), "/"), c.ReminderSecret) {
		log.Info("reminder auth rejected")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// MaxBytesReader rejects with 413 instead of handing us truncated JSON.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			log.Warn("reminder body too large", "limit", mb.Limit)
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		log.Error("reminder read body", "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if c.SignSecret != "" {
		sig := r.Header.Get("X-Vikunja-Signature")
		if !verifySignature(c.SignSecret, sig, body) {
			log.Info("reminder signature mismatch", "sig", redact(sig))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	var ev reminderEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		log.Error("reminder unmarshal", "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if ev.EventName != "task.reminder.fired" {
		// Vikunja may have the same webhook URL subscribed to other events.
		log.Info("ignoring non-reminder event", "event", ev.EventName)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	taskID := ev.Data.Task.ID
	if taskID <= 0 {
		log.Warn("reminder event missing task id")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	dedupeKey := fmt.Sprintf("task=%d@%s", taskID, ev.Data.Reminder.Reminder)
	if !dedupe.claim(dedupeKey) {
		log.Info("duplicate reminder suppressed", "task_id", taskID, "key", dedupeKey)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.WriteHeader(http.StatusNoContent)

	taskTitle := ev.Data.Task.Title
	projectTitle := ev.Data.Project.Title
	reminder := ev.Data.Reminder.Reminder
	icon := c.VikunjaIconURL
	notifyWG.Add(1)
	go func() {
		defer notifyWG.Done()
		// Outside net/http's per-connection panic recovery, a panic here would
		// kill the process after we already answered Vikunja.
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic while sending notification", "task_id", taskID, "panic", fmt.Sprint(p),
					"stack", string(debug.Stack()))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), c.RequestTimeout)
		defer cancel()
		if err := c.sendNotification(ctx, taskID, taskTitle, projectTitle, reminder, icon); err != nil {
			dedupe.forget(dedupeKey)
			log.Error("ntfy publish failed", "err", err, "task_id", taskID, "title", taskTitle)
			return
		}
		log.Info("notification sent", "task_id", taskID, "title", taskTitle, "project", projectTitle, "reminder", reminder)
	}()
}

func (c *Config) formatReminderTime(ts string) string {
	t, err := time.ParseInLocation(time.RFC3339, ts, time.Local)
	if err != nil {
		t = time.Now()
		log.Warn("invalid reminder timestamp, using now", "ts", ts, "err", err)
	}
	return t.Format("Mon, 01/02 at 15:04")
}

func (c *Config) sendNotification(ctx context.Context, taskID int64, rawTitle, projectTitle, reminder, icon string) error {
	title := sanitizeTitle(c.TitlePrefix + sanitizeTitle(rawTitle))
	body := fmt.Sprintf("Reminder at %s · *%s*\n\nTap to open.", c.formatReminderTime(reminder), sanitizeTitle(projectTitle))
	publishURL := strings.TrimRight(c.NTFYURL, "/")
	actionURL := fmt.Sprintf("%s/v1/done/%s?task=%d",
		c.PublicBaseURL, c.DoneSecret, taskID)
	actions := fmt.Sprintf("http, %q, %s, method=POST, clear=true", "Mark as done", actionURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, publishURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "alarm,vikunja")
	req.Header.Set("Click", fmt.Sprintf("%s/tasks/%d", c.VikunjaBaseURL, taskID))
	req.Header.Set("X-Markdown", "yes")
	if icon != "" {
		req.Header.Set("X-Icon", icon)
	}
	req.Header.Set("Actions", actions)
	req.Header.Set("Content-Type", "text/plain")
	if c.NTFYToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.NTFYToken)
	}
	resp, err := c.ntfyHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("ntfy unexpected status %d: %s", resp.StatusCode, string(b))
	}
	// Drain a little of the body so the keep-alive connection survives.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	return nil
}

func (c *Config) handleDone(w http.ResponseWriter, r *http.Request) {
	if !equalSecret(strings.Trim(r.PathValue("secret"), "/"), c.DoneSecret) {
		log.Info("done auth rejected")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	q := r.URL.Query().Get("task")
	taskID, err := strconv.ParseInt(q, 10, 64)
	if err != nil || taskID <= 0 {
		log.Info("done bad task param", "task", q)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if err := c.markTaskDone(r.Context(), taskID); err != nil {
		log.Error("done mark failed", "task_id", taskID, "err", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	log.Info("task marked done", "task_id", taskID)
	w.WriteHeader(http.StatusOK)
}

func (c *Config) markTaskDone(ctx context.Context, taskID int64) error {
	url := fmt.Sprintf("%s/api/v1/tasks/%d", c.VikunjaBaseURL, taskID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"done":true}`))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.VikunjaToken)
	req.Header.Set("Content-Type", "application/json")
	// Sending only the `done` field works. PUT and PATCH return 405.
	resp, err := c.vikunjaHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	return fmt.Errorf("vikunja unexpected status %d: %s", resp.StatusCode, string(b))
}

func equalSecret(provided, expected string) bool {
	if provided == "" || expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func verifySignature(secret, sigHeader string, payload []byte) bool {
	if sigHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	wantRaw := hex.EncodeToString(mac.Sum(nil))
	got := strings.TrimPrefix(sigHeader, "sha256=")
	return subtle.ConstantTimeCompare([]byte(got), []byte(wantRaw)) == 1
}

// Prefixes leak secrets, so only log the length.
func redact(s string) string {
	return fmt.Sprintf("len=%d", len(s))
}

func runHealthcheck() int {
	addr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	// Parse the port instead of stripping a leading colon, an explicit host
	// in LISTEN_ADDR must still probe the port the server listens on.
	port := "8080"
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		port = p
	}
	return healthcheck(port)
}

func healthcheck(port string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			fmt.Fprintf(os.Stderr, "healthcheck: %s -> %s\n", port, resp.Status)
		} else {
			fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		}
		return 1
	}
	resp.Body.Close()
	return 0
}

func newMux(c *Config) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /v1/reminder/{secret}", c.handleReminder)
	mux.HandleFunc("POST /v1/done/{secret}", c.handleDone)
	return mux
}

func main() {
	// HEALTHCHECK runs without a shell, so the binary probes itself.
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}
	initLog()
	cfg, err := loadConfig()
	if err != nil {
		log.Error("config error", "err", err)
		os.Exit(1)
	}
	dedupe = newDedupe(cfg.DedupeWindow)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           newMux(cfg),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       120 * time.Second, // stop immortal keep-alive connections
	}
	log.Info("listening", "addr", cfg.ListenAddr,
		"ntfy_url", cfg.NTFYURL,
		"vikunja", cfg.VikunjaBaseURL,
		"public", cfg.PublicBaseURL,
		"dedupe_window", cfg.DedupeWindow.String())

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			log.Error("server exited", "err", err)
			os.Exit(1)
		}
		return
	case <-shutdownCtx.Done():
		log.Info("shutdown signal received: draining")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		log.Warn("http shutdown incomplete", "err", err)
	}
	notifyWG.Wait()
	log.Info("bye")
}
