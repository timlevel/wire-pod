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
		func(s *Schedule) { s.Timezone = "Mars/Olympus" }, func(s *Schedule) { s.Cron = "* * * * * *" },
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
		m.ServeHTTP(r, httptest.NewRequest(method, path, strings.NewReader(body)))
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
