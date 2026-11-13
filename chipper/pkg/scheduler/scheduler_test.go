package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

func hourly() Schedule {
	return Schedule{ID: "hourly", Name: "Hourly announcement", ESN: "00abc123", Cron: "0 * * * *", Timezone: "America/Chicago", Enabled: true, Actions: []Action{{Type: "drive_off_charger"}, {Type: "say_text", Text: "It is {hour} o'clock!"}, {Type: "drive_on_charger"}}}
}
func noRun(context.Context, Schedule, time.Time) error { return nil }
func openTest(t *testing.T, run Runner) *Manager {
	t.Helper()
	m, err := Open(filepath.Join(t.TempDir(), "schedules.json"), run)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { <-m.Close().Done() })
	return m
}
func TestPersistenceAndInvalidUpdate(t *testing.T) {
	m := openTest(t, noRun)
	s := hourly()
	if err := m.Save(s); err != nil {
		t.Fatal(err)
	}
	s.Actions[0].Type = "modified by caller"
	if m.List()[0].Actions[0].Type != "drive_off_charger" {
		t.Fatal("actions were not copied")
	}
	before, _ := os.ReadFile(m.path)
	bad := hourly()
	bad.Cron = "60 * * * *"
	if err := m.Save(bad); err == nil {
		t.Fatal("invalid cron accepted")
	}
	after, _ := os.ReadFile(m.path)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid edit changed saved file")
	}
	m2, err := Open(m.path, noRun)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if len(m2.List()) != 1 || m2.List()[0].NextRun.IsZero() {
		t.Fatal(m2.List())
	}
	s = hourly()
	s.Enabled = false
	if err = m2.Save(s); err != nil {
		t.Fatal(err)
	}
	if !m2.List()[0].NextRun.IsZero() {
		t.Fatal("paused job has a next run")
	}
	if err = m2.Delete(s.ID); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(m.path)
	if string(data) != "[]" {
		t.Fatal(string(data))
	}
}
func TestCorruptFilePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	data := []byte("{broken")
	os.WriteFile(path, data, 0600)
	if _, err := Open(path, noRun); err == nil {
		t.Fatal("corrupt file accepted")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, data) {
		t.Fatal("corrupt file overwritten")
	}
}
func TestValidationAndTimezone(t *testing.T) {
	for _, change := range []func(*Schedule){
		func(s *Schedule) { s.Timezone = "Mars/Olympus" }, func(s *Schedule) { s.Timezone = "Local" }, func(s *Schedule) { s.Cron = "* * * * * *" },
		func(s *Schedule) { s.Cron = "0 0 31 2 *" }, func(s *Schedule) { s.Actions = nil },
		func(s *Schedule) { s.Actions = []Action{{Type: "shell"}} }, func(s *Schedule) { s.Actions = []Action{{Type: "say_text"}} },
	} {
		s := hourly()
		change(&s)
		if _, err := validate(s); err == nil {
			t.Fatalf("accepted invalid schedule: %+v", s)
		}
	}
	s := hourly()
	parsed, err := validate(s)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 11, 13, 17, 59, 0, 0, time.UTC)
	next := parsed.Next(when)
	if !next.Equal(time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)) {
		t.Fatal(next)
	}
	if got := expandText("{hour}|{hour24}|{time}", next, s.Timezone); got != "12|12|12:00 PM" {
		t.Fatal(got)
	}
	midnight := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	if got := expandText("{hour}|{hour24}", midnight, s.Timezone); got != "12|0" {
		t.Fatal(got)
	}
	// Spring-forward skips the nonexistent local 2 AM.
	s.Cron = "0 2 * * *"
	parsed, err = validate(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Next(time.Date(2026, 3, 8, 7, 59, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 3, 9, 7, 0, 0, 0, time.UTC)) {
		t.Fatal(got)
	}
}
func TestOverlapAndFailureStatus(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	m := openTest(t, func(ctx context.Context, s Schedule, when time.Time) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("no run deadline")
		}
		close(started)
		<-finish
		return errors.New("offline")
	})
	s := hourly()
	s.Enabled = false
	if err := m.Save(s); err != nil {
		t.Fatal(err)
	}
	// A paused cron callback must do nothing; Run now still works.
	if err := m.start(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if m.List()[0].Running {
		t.Fatal("paused cron ran")
	}
	if err := m.start(s.ID, false); err != nil {
		t.Fatal(err)
	}
	<-started
	other := s
	other.ID = "second"
	if err := m.Save(other); err != nil {
		t.Fatal(err)
	}
	if err := m.start(other.ID, false); err == nil {
		t.Fatal("overlapping robot run accepted")
	}
	// Updating a running job must retain its exclusion, even if its robot changes.
	s.ESN = "newbot"
	if err := m.Save(s); err != nil {
		t.Fatal(err)
	}
	if err := m.start(s.ID, false); err == nil {
		t.Fatal("edited running job overlapped")
	}
	close(finish)
	deadline := time.After(time.Second)
	for {
		var result Status
		for _, status := range m.List() {
			if status.ID == s.ID {
				result = status
			}
		}
		if !result.Running {
			if result.LastError != "offline" {
				t.Fatal(result)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("run did not finish")
		case <-time.After(time.Millisecond):
		}
	}
}
func TestAPI(t *testing.T) {
	m := openTest(t, noRun)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		m.ServeHTTP(r, req)
		return r
	}
	s := hourly()
	s.Enabled = false
	s.ID = ""
	b, _ := json.Marshal(s)
	created := call("POST", "/api-schedules/", string(b))
	if created.Code != 200 {
		t.Fatal(created.Body)
	}
	json.Unmarshal(created.Body.Bytes(), &s)
	if s.ID == "" {
		t.Fatal("missing generated ID")
	}
	if got := call("GET", "/api-schedules/", ""); got.Code != 200 || !strings.Contains(got.Body.String(), s.ID) {
		t.Fatal(got.Body)
	}
	if got := call("POST", "/api-schedules/", string(b)+" {}"); got.Code != 400 {
		t.Fatal(got.Code)
	}
	if got := call("GET", "/api-schedules/"+s.ID+"/run", ""); got.Code != http.StatusMethodNotAllowed {
		t.Fatal(got.Code)
	}
	if got := call("DELETE", "/api-schedules/"+s.ID, ""); got.Code != 204 {
		t.Fatal(got.Body)
	}
	if got := call("POST", "/api-schedules/"+s.ID+"/run", ""); got.Code != 404 {
		t.Fatal(got.Code)
	}
}
func TestWriteFailureDoesNotActivate(t *testing.T) {
	m := openTest(t, noRun)
	m.path = filepath.Join(t.TempDir(), "missing", "schedules.json")
	if err := m.Save(hourly()); err == nil {
		t.Fatal("write should fail")
	}
	if len(m.List()) != 0 {
		t.Fatal("unsaved schedule activated")
	}
}

// Exercise dispatch through the cron engine without waiting for a wall-clock minute.
func TestCronDispatch(t *testing.T) {
	fired := make(chan Schedule, 1)
	m := openTest(t, func(_ context.Context, s Schedule, _ time.Time) error {
		select {
		case fired <- s:
		default:
		}
		return nil
	})
	s := hourly()
	s.Enabled = false
	if err := m.Save(s); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	e := m.entries[s.ID]
	e.Enabled = true
	e.parsed = cron.Every(time.Second)
	m.register(e)
	m.mu.Unlock()
	select {
	case got := <-fired:
		if got.ID != s.ID {
			t.Fatal(got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cron did not dispatch")
	}
}

func TestInvalidSavedEntriesAreIsolated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	good, bad := hourly(), hourly()
	bad.ID, bad.Cron = "bad", "60 * * * *"
	data, _ := json.Marshal([]Schedule{good, bad})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Open(path, noRun)
	if err != nil {
		t.Fatalf("one invalid entry disabled all schedules: %v", err)
	}
	defer m.Close()
	for _, status := range m.List() {
		if status.ID == good.ID && status.NextRun.IsZero() {
			t.Fatal("valid schedule disabled")
		}
		if status.ID == bad.ID {
			encoded, _ := json.Marshal(status)
			if status.Enabled || !status.NextRun.IsZero() || !bytes.Contains(encoded, []byte(`"loadError"`)) {
				t.Fatalf("invalid entry not quarantined: %s", encoded)
			}
		}
	}
	if err := m.start(bad.ID, false); err == nil {
		t.Fatal("invalid saved entry ran")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(data, got) {
		t.Fatal("load rewrote original file")
	}
	bad.Cron = good.Cron
	if err := m.Save(bad); err != nil {
		t.Fatal(err)
	}
	for _, status := range m.List() {
		if status.ID == bad.ID && status.NextRun.IsZero() {
			t.Fatal("fixed schedule still disabled")
		}
	}
	bad.Cron = "invalid"
	data, _ = json.Marshal([]Schedule{good, bad})
	os.WriteFile(path, data, 0600)
	m2, err := Open(path, noRun)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if err := m2.Delete(bad.ID); err != nil {
		t.Fatal(err)
	}
	if len(m2.List()) != 1 {
		t.Fatal(m2.List())
	}
}

func TestWriteAPIRequiresJSON(t *testing.T) {
	m := openTest(t, noRun)
	for _, path := range []string{"/api-schedules/", "/api-schedules/hourly/run"} {
		for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
			r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
			r.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			m.ServeHTTP(w, r)
			if w.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("%s %s: %d", path, contentType, w.Code)
			}
		}
	}
}
func TestInvalidSavedIdentityAndActions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	good, duplicate, malformed := hourly(), hourly(), hourly()
	malformed.ID, malformed.Timezone, malformed.Actions = "/bad/id", "Mars/Olympus", nil
	data, _ := json.Marshal([]Schedule{good, duplicate, malformed})
	os.WriteFile(path, data, 0600)
	m, err := Open(path, noRun)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if len(m.List()) != 3 {
		t.Fatal("lost entries", m.List())
	}
	invalid := 0
	for _, status := range m.List() {
		if status.LoadError != "" {
			invalid++
			if status.Enabled || strings.Contains(status.ID, "/") {
				t.Fatal(status)
			}
			if err := m.start(status.ID, false); err == nil {
				t.Fatal("invalid entry ran")
			}
		}
	}
	if invalid != 2 {
		t.Fatal(m.List())
	}
	if err := m.Save(good); err != nil {
		t.Fatal(err)
	}
	m2, err := Open(path, noRun)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	enabled := 0
	for _, status := range m2.List() {
		if status.Enabled {
			enabled++
		}
	}
	if enabled != 1 {
		t.Fatal("unrelated save activated invalid entry on restart", m2.List())
	}
}
func TestFallBackCronRepeatsLocalHour(t *testing.T) {
	s := hourly()
	s.Cron = "0 1 * * *"
	parsed, err := validate(s)
	if err != nil {
		t.Fatal(err)
	}
	first := parsed.Next(time.Date(2026, 11, 1, 5, 59, 0, 0, time.UTC))
	second := parsed.Next(first)
	if !first.Equal(time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)) || !second.Equal(first.Add(time.Hour)) {
		t.Fatalf("repeated local hour: %v, %v", first, second)
	}
}
