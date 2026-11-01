package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// The spies are real HTTP servers standing in for ntfy and Vikunja at the
// network boundary, which is where this bridge does all of its trusting.
// Nothing inside the bridge is mocked.

type hit struct {
	method, path, auth, title, actions, click, body string
}

// httpSpy records every request it receives and answers with its configured
// status. Status flips happen only between publishes, notifyWG.Wait settles
// the publish goroutine first, so no lock guards status. GETs (the done
// handler's task read) are answered with getBody and 200 unless getFails.
type httpSpy struct {
	t      *testing.T
	status int

	mu       sync.Mutex
	hits     []hit
	getBody  string
	getFails bool

	server *httptest.Server
}

func newSpy(t *testing.T, status int) *httpSpy {
	s := &httpSpy{t: t, status: status}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.hits = append(s.hits, hit{
			method:  r.Method,
			path:    r.URL.Path,
			auth:    r.Header.Get("Authorization"),
			title:   r.Header.Get("Title"),
			actions: r.Header.Get("Actions"),
			click:   r.Header.Get("Click"),
			body:    string(b),
		})
		s.mu.Unlock()
		if r.Method == http.MethodGet {
			s.mu.Lock()
			fails, getBody := s.getFails, s.getBody
			s.mu.Unlock()
			if fails {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":4002,"message":"This task does not exist"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			if getBody != "" {
				w.Write([]byte(getBody))
			}
			return
		}
		w.WriteHeader(s.status)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *httpSpy) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hits)
}

func (s *httpSpy) last() hit {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.hits) == 0 {
		s.t.Fatal("spy recorded no calls")
	}
	return s.hits[len(s.hits)-1]
}

func bridgeConfig(t *testing.T, ntfyStatus, vikunjaStatus int) (*Config, *httpSpy, *httpSpy) {
	ntfy := newSpy(t, ntfyStatus)
	vik := newSpy(t, vikunjaStatus)
	c := &Config{
		NTFYURL:        ntfy.server.URL,
		NTFYToken:      "ntfy-token",
		VikunjaBaseURL: vik.server.URL,
		VikunjaToken:   "vikunja-token",
		PublicBaseURL:  "https://notifications.example.com",
		ReminderSecret: "reminder-secret",
		DoneSecret:     "done-secret",
		TitlePrefix:    "⏰ ",
		DedupeWindow:   500 * time.Millisecond,
		RequestTimeout: 2 * time.Second,
		ntfyHTTP:       &http.Client{Timeout: 2 * time.Second},
		vikunjaHTTP:    &http.Client{Timeout: 2 * time.Second},
	}
	return c, ntfy, vik
}

// Requests are served through the real mux so route wiring is exercised.
func serve(c *Config, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	newMux(c).ServeHTTP(w, req)
	return w
}

func sendReminder(t *testing.T, c *Config, secret, payload string) int {
	t.Helper()
	w := serve(c, httptest.NewRequest(http.MethodPost, "/v1/reminder/"+secret, strings.NewReader(payload)))
	notifyWG.Wait()
	return w.Code
}

const dedupeWindow = 100 * time.Millisecond

func drainDedupe() { time.Sleep(2 * dedupeWindow) }

const reminderPayload = `{"event_name":"task.reminder.fired","time":"2026-10-27T11:00:00Z",` +
	`"data":{"project":{"title":"Inbox"},"task":{"id":42,"title":"Feed the cat"},` +
	`"reminder":{"reminder":"2026-10-27T07:00:00-04:00"}}}`

func TestReminderWebhook(t *testing.T) {
	t.Run("rejects a wrong path secret without publishing", func(t *testing.T) {
		c, ntfy, _ := bridgeConfig(t, 200, 200)
		if code := sendReminder(t, c, "wrong-secret", reminderPayload); code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", code)
		}
		if n := ntfy.calls(); n != 0 {
			t.Errorf("ntfy got %d calls, want 0", n)
		}
	})

	t.Run("acks at once and publishes with the callback and click targets", func(t *testing.T) {
		c, ntfy, _ := bridgeConfig(t, 200, 200)
		dedupe = newDedupe(dedupeWindow)
		if code := sendReminder(t, c, "reminder-secret", reminderPayload); code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", code)
		}
		h := ntfy.last()
		if h.auth != "Bearer ntfy-token" {
			t.Errorf("authorization = %q", h.auth)
		}
		if h.title != "⏰ Feed the cat" {
			t.Errorf("title = %q", h.title)
		}
		if h.click != c.VikunjaBaseURL+"/tasks/42" {
			t.Errorf("click = %q, want %q", h.click, c.VikunjaBaseURL+"/tasks/42")
		}
		if want := "https://notifications.example.com/v1/done/done-secret?task=42"; !strings.Contains(h.actions, want) {
			t.Errorf("actions %q lacks the callback URL %q", h.actions, want)
		}
	})

	t.Run("silences a redelivery inside the dedupe window", func(t *testing.T) {
		c, ntfy, _ := bridgeConfig(t, 200, 200)
		dedupe = newDedupe(dedupeWindow)
		sendReminder(t, c, "reminder-secret", reminderPayload)
		sendReminder(t, c, "reminder-secret", reminderPayload)
		if n := ntfy.calls(); n != 1 {
			t.Errorf("ntfy got %d calls, want 1", n)
		}
		drainDedupe()
	})

	t.Run("forgets a failed publish so the redelivery retries", func(t *testing.T) {
		c, ntfy, _ := bridgeConfig(t, 500, 200)
		dedupe = newDedupe(dedupeWindow)
		if code := sendReminder(t, c, "reminder-secret", reminderPayload); code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", code)
		}
		if n := ntfy.calls(); n != 1 {
			t.Errorf("first attempt: ntfy got %d calls, want 1", n)
		}
		ntfy.status = 200
		sendReminder(t, c, "reminder-secret", reminderPayload)
		if n := ntfy.calls(); n != 2 {
			t.Errorf("ntfy got %d calls, want 2, the retry must publish again", n)
		}
		drainDedupe()
	})

	t.Run("ignores events that are not reminders", func(t *testing.T) {
		c, ntfy, _ := bridgeConfig(t, 200, 200)
		payload := strings.Replace(reminderPayload, "task.reminder.fired", "task.created", 1)
		if code := sendReminder(t, c, "reminder-secret", payload); code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", code)
		}
		if n := ntfy.calls(); n != 0 {
			t.Errorf("ntfy got %d calls, want 0", n)
		}
	})

	t.Run("ignores an event without a task id", func(t *testing.T) {
		c, ntfy, _ := bridgeConfig(t, 200, 200)
		payload := strings.Replace(reminderPayload, `"task":{"id":42,"title":"Feed the cat"}`, `"task":{}`, 1)
		if code := sendReminder(t, c, "reminder-secret", payload); code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", code)
		}
		if n := ntfy.calls(); n != 0 {
			t.Errorf("ntfy got %d calls, want 0", n)
		}
	})

	t.Run("rejects malformed JSON", func(t *testing.T) {
		c, ntfy, _ := bridgeConfig(t, 200, 200)
		if code := sendReminder(t, c, "reminder-secret", "{not json"); code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", code)
		}
		if n := ntfy.calls(); n != 0 {
			t.Errorf("ntfy got %d calls, want 0", n)
		}
	})

	t.Run("rejects an oversized body", func(t *testing.T) {
		big := `{"data":{"task":{"id":42}}}` + strings.Repeat(" ", 1<<20)
		c, ntfy, _ := bridgeConfig(t, 200, 200)
		if code := sendReminder(t, c, "reminder-secret", big); code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", code)
		}
		if n := ntfy.calls(); n != 0 {
			t.Errorf("ntfy got %d calls, want 0", n)
		}
	})

	t.Run("rejects an unsigned body when signing is configured", func(t *testing.T) {
		c, ntfy, _ := bridgeConfig(t, 200, 200)
		c.SignSecret = "webhook-secret"
		if code := sendReminder(t, c, "reminder-secret", reminderPayload); code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", code)
		}
		if n := ntfy.calls(); n != 0 {
			t.Errorf("ntfy got %d calls, want 0", n)
		}
	})

	t.Run("accepts a valid HMAC signature", func(t *testing.T) {
		c, ntfy, _ := bridgeConfig(t, 200, 200)
		c.SignSecret = "webhook-secret"
		dedupe = newDedupe(dedupeWindow)
		mac := hmac.New(sha256.New, []byte(c.SignSecret))
		mac.Write([]byte(reminderPayload))
		req := httptest.NewRequest(http.MethodPost, "/v1/reminder/reminder-secret", strings.NewReader(reminderPayload))
		req.Header.Set("X-Vikunja-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		w := serve(c, req)
		notifyWG.Wait()
		if w.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", w.Code)
		}
		if n := ntfy.calls(); n != 1 {
			t.Errorf("ntfy got %d calls, want 1", n)
		}
	})

	t.Run("rejects a timestamp prefixed signature outright", func(t *testing.T) {
		c, _, _ := bridgeConfig(t, 200, 200)
		c.SignSecret = "webhook-secret"
		req := httptest.NewRequest(http.MethodPost, "/v1/reminder/reminder-secret", strings.NewReader(reminderPayload))
		req.Header.Set("X-Vikunja-Signature", "1730000000.deadbeef")
		w := serve(c, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401, a replayable format must stay rejected", w.Code)
		}
	})
}

const cannedTask = `{"id":42,"title":"Medication","description":"take pills","done":false,` +
	`"due_date":"2026-10-31T06:00:00-04:00","priority":3,"percent_done":0,` +
	`"repeat_after":86400,"repeat_mode":0,` +
	`"reminders":[{"reminder":"2026-10-31T06:00:00-04:00","relative_period":0,"relative_to":""}],` +
	`"hex_color":"","end_date":"0001-01-01T00:00:00Z","start_date":"0001-01-01T00:00:00Z"}`

func TestDoneCallback(t *testing.T) {
	t.Run("rejects a wrong path secret without calling Vikunja", func(t *testing.T) {
		c, _, vik := bridgeConfig(t, 200, 200)
		w := serve(c, httptest.NewRequest(http.MethodPost, "/v1/done/wrong-secret?task=42", nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
		if n := vik.calls(); n != 0 {
			t.Errorf("vikunja got %d calls, want 0", n)
		}
	})

	t.Run("rejects a malformed task id", func(t *testing.T) {
		c, _, vik := bridgeConfig(t, 200, 200)
		w := serve(c, httptest.NewRequest(http.MethodPost, "/v1/done/done-secret?task=banana", nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
		if n := vik.calls(); n != 0 {
			t.Errorf("vikunja got %d calls, want 0", n)
		}
	})

	t.Run("completes the task by echoing it back with done flipped", func(t *testing.T) {
		// A partial body erases fields Vikunja does not receive, so the read
		// then full write is the contract (go-vikunja issue 1459).
		c, _, vik := bridgeConfig(t, 200, 200)
		vik.getBody = cannedTask
		w := serve(c, httptest.NewRequest(http.MethodPost, "/v1/done/done-secret?task=42", nil))
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
		get := vik.hits[0]
		if get.method != http.MethodGet || get.path != "/api/v1/tasks/42" {
			t.Errorf("first call = %s %s, want GET /api/v1/tasks/42", get.method, get.path)
		}
		post := vik.last()
		if post.method != http.MethodPost || post.path != "/api/v1/tasks/42" {
			t.Errorf("last call = %s %s, want POST /api/v1/tasks/42", post.method, post.path)
		}
		if post.auth != "Bearer vikunja-token" {
			t.Errorf("authorization = %q", post.auth)
		}

		// The raw record must come back whole, only done flipped and done_at
		// dropped for Vikunja to restamp.
		var got, want map[string]any
		if err := json.Unmarshal([]byte(post.body), &got); err != nil {
			t.Fatalf("posted body is not json: %v", err)
		}
		if err := json.Unmarshal([]byte(cannedTask), &want); err != nil {
			t.Fatal(err)
		}
		want["done"] = true
		delete(want, "done_at")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("posted body should be the task with done flipped, diff got %v want %v", got, want)
		}
	})

	t.Run("reports the missing task as 502 without posting", func(t *testing.T) {
		c, _, vik := bridgeConfig(t, 200, 200)
		vik.getFails = true
		w := serve(c, httptest.NewRequest(http.MethodPost, "/v1/done/done-secret?task=999999999", nil))
		if w.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", w.Code)
		}
	})

	t.Run("surfaces a Vikunja failure as 502", func(t *testing.T) {
		c, _, vik := bridgeConfig(t, 200, 500)
		vik.getBody = cannedTask
		w := serve(c, httptest.NewRequest(http.MethodPost, "/v1/done/done-secret?task=42", nil))
		if w.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", w.Code)
		}
	})
}

func startBridge(t *testing.T, c *Config) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: newMux(c)}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close(); l.Close() })
	return "http://" + l.Addr().String()
}

func TestRunningBridge(t *testing.T) {
	t.Run("a reminder flows through to a completed task end to end", func(t *testing.T) {
		c, ntfy, vik := bridgeConfig(t, 200, 200)
		vik.getBody = cannedTask
		dedupe = newDedupe(dedupeWindow)
		base := startBridge(t, c)

		res, err := http.Post(base+"/v1/reminder/reminder-secret", "application/json", strings.NewReader(reminderPayload))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		notifyWG.Wait()
		if res.StatusCode != http.StatusNoContent {
			t.Errorf("reminder status = %d, want 204", res.StatusCode)
		}
		if n := ntfy.calls(); n != 1 {
			t.Errorf("ntfy got %d calls, want 1", n)
		}

		drainDedupe()
		done, err := http.Post(base+"/v1/done/done-secret?task=42", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		done.Body.Close()
		if done.StatusCode != http.StatusOK {
			t.Errorf("done status = %d, want 200", done.StatusCode)
		}
		post := vik.last()
		if post.method != http.MethodPost {
			t.Errorf("last vikunja call = %s, want POST", post.method)
		}
		if !strings.Contains(post.body, `"done":true`) {
			t.Errorf("vikunja body = %q, want carried done true", post.body)
		}
	})

	t.Run("the health endpoint answers ok", func(t *testing.T) {
		base := startBridge(t, &Config{})
		res, err := http.Get(base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || string(b) != "ok" {
			t.Errorf("healthz = %d %q, want 200 ok", res.StatusCode, b)
		}
	})
}

func TestHealthcheck(t *testing.T) {
	t.Run("probes a running bridge and reports healthy", func(t *testing.T) {
		base := startBridge(t, &Config{})
		port := strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "127.0.0.1:")
		if code := healthcheck(port); code != 0 {
			t.Errorf("healthcheck(%q) = %d, want 0 against a running bridge", port, code)
		}
	})

	t.Run("reports unhealthy when nothing listens on the configured port", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
		l.Close()
		t.Setenv("LISTEN_ADDR", ":"+port)
		if code := runHealthcheck(); code != 1 {
			t.Errorf("runHealthcheck() = %d, want 1", code)
		}
	})
}
