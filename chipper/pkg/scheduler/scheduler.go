// Package scheduler persists and runs per-robot cron action sequences.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

type Action struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Animation string `json:"animation,omitempty"`
}

type Schedule struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	ESN      string   `json:"esn"`
	Cron     string   `json:"cron"`
	Timezone string   `json:"timezone"`
	Enabled  bool     `json:"enabled"`
	Actions  []Action `json:"actions"`
}

type Status struct {
	Schedule
	NextRun   time.Time `json:"nextRun,omitempty"`
	LastRun   time.Time `json:"lastRun,omitempty"`
	LastError string    `json:"lastError,omitempty"`
	LoadError string    `json:"loadError,omitempty"`
	Running   bool      `json:"running"`
}

type entry struct {
	Schedule
	parsed    cron.Schedule
	job       cron.EntryID
	lastRun   time.Time
	lastError string
	loadError string
	running   bool
}

type Runner func(context.Context, Schedule, time.Time) error

type Manager struct {
	mu      sync.Mutex
	path    string
	cron    *cron.Cron
	entries map[string]*entry
	busy    map[string]bool
	run     Runner
}

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func validate(s Schedule) (cron.Schedule, error) {
	if s.ID == "" || len(s.ID) > 100 || strings.ContainsAny(s.ID, "/\\") {
		return nil, errors.New("invalid schedule ID")
	}
	if strings.TrimSpace(s.Name) == "" || len(s.Name) > 100 {
		return nil, errors.New("name is required (maximum 100 characters)")
	}
	if strings.TrimSpace(s.ESN) == "" || len(s.ESN) > 100 {
		return nil, errors.New("robot serial is required")
	}
	if len(strings.Fields(s.Cron)) != 5 {
		return nil, errors.New("cron must contain five fields: minute hour day month weekday")
	}
	if s.Timezone == "" || s.Timezone == "Local" {
		return nil, errors.New("timezone is required, for example America/Chicago")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return nil, fmt.Errorf("invalid timezone: %w", err)
	}
	parsed, err := parser.Parse("CRON_TZ=" + s.Timezone + " " + s.Cron)
	if err != nil {
		return nil, err
	}
	if parsed.Next(time.Now()).IsZero() {
		return nil, errors.New("cron has no upcoming occurrence")
	}
	if len(s.Actions) == 0 || len(s.Actions) > 20 {
		return nil, errors.New("provide between 1 and 20 actions")
	}
	for _, a := range s.Actions {
		switch a.Type {
		case "say_text":
			if strings.TrimSpace(a.Text) == "" || len(a.Text) > 1000 {
				return nil, errors.New("speech needs text (maximum 1000 characters)")
			}
		case "play_animation":
			if strings.TrimSpace(a.Animation) == "" || len(a.Animation) > 200 {
				return nil, errors.New("animation name is required (maximum 200 characters)")
			}
		case "drive_off_charger", "drive_on_charger":
		default:
			return nil, fmt.Errorf("unknown action %q", a.Type)
		}
	}
	return parsed, nil
}

// Open preserves the file and quarantines invalid entries for repair through the API.
// Unparseable JSON still fails closed without overwriting the file.
func Open(path string, run Runner) (*Manager, error) {
	m := &Manager{path: path, cron: cron.New(), entries: make(map[string]*entry), busy: make(map[string]bool), run: run}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		var saved []Schedule
		if err = json.Unmarshal(b, &saved); err != nil {
			return nil, err
		}
		for _, s := range saved {
			parsed, err := validate(s)
			_, duplicate := m.entries[s.ID]
			if duplicate || s.ID == "" || len(s.ID) > 100 || strings.ContainsAny(s.ID, "/\\") {
				// Assign an addressable ID so a malformed/duplicate entry can be deleted.
				s.ID = uuid.NewString()
				err = errors.Join(err, errors.New("invalid or duplicate saved ID; replacement ID assigned"))
			}
			e := &entry{Schedule: s, parsed: parsed}
			if err != nil {
				e.loadError = err.Error()
				e.Enabled = false
			}
			m.entries[s.ID] = e
		}
	}
	for _, e := range m.entries {
		m.register(e)
	}
	m.cron.Start()
	return m, nil
}

func (m *Manager) Close() context.Context { return m.cron.Stop() }

func (m *Manager) register(e *entry) {
	if e.Enabled && e.loadError == "" {
		id := e.ID
		e.job = m.cron.Schedule(e.parsed, cron.FuncJob(func() { m.start(id, true) }))
	}
}

func (m *Manager) persist(all map[string]*entry) error {
	saved := make([]Schedule, 0, len(all))
	for _, e := range all {
		saved = append(saved, e.Schedule)
	}
	sort.Slice(saved, func(i, j int) bool { return saved[i].ID < saved[j].ID })
	b, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".schedules-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), m.path)
}

func (m *Manager) Save(s Schedule) error {
	s.ESN = strings.ToLower(strings.TrimSpace(s.ESN))
	parsed, err := validate(s)
	if err != nil {
		return err
	}
	s.Actions = append([]Action(nil), s.Actions...)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) >= 100 && m.entries[s.ID] == nil {
		return errors.New("maximum 100 schedules")
	}
	next := make(map[string]*entry, len(m.entries)+1)
	for id, e := range m.entries {
		next[id] = e
	}
	e := &entry{Schedule: s, parsed: parsed}
	old := m.entries[s.ID]
	if old != nil {
		e.lastRun = old.lastRun
		e.lastError = old.lastError
		e.running = old.running
	}
	next[s.ID] = e
	if err = m.persist(next); err != nil {
		return err
	}
	if old != nil && old.job != 0 {
		m.cron.Remove(old.job)
	}
	m.entries = next
	m.register(e)
	return nil
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return os.ErrNotExist
	}
	next := make(map[string]*entry, len(m.entries))
	for key, value := range m.entries {
		if key != id {
			next[key] = value
		}
	}
	if err := m.persist(next); err != nil {
		return err
	}
	if e.job != 0 {
		m.cron.Remove(e.job)
	}
	m.entries = next
	return nil
}

func (m *Manager) List() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Status, 0, len(m.entries))
	for _, e := range m.entries {
		s := Status{Schedule: e.Schedule, LastRun: e.lastRun, LastError: e.lastError, LoadError: e.loadError, Running: e.running}
		s.Actions = append([]Action(nil), e.Actions...)
		if e.loadError != "" {
			s.Enabled = false
		}
		if e.Enabled && e.loadError == "" {
			s.NextRun = e.parsed.Next(time.Now())
		}
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// start claims the robot before spawning so manual and cron runs cannot queue up.
func (m *Manager) start(id string, scheduled bool) error {
	m.mu.Lock()
	e, ok := m.entries[id]
	if !ok {
		m.mu.Unlock()
		return os.ErrNotExist
	}
	if e.loadError != "" {
		m.mu.Unlock()
		return fmt.Errorf("repair the saved schedule before running: %s", e.loadError)
	}
	if scheduled && !e.Enabled {
		m.mu.Unlock()
		return nil
	}
	if e.running || m.busy[e.ESN] {
		m.mu.Unlock()
		return errors.New("robot already has a scheduled action running; occurrence skipped")
	}
	s := e.Schedule
	when := time.Now()
	e.running = true
	e.lastRun = when
	e.lastError = ""
	m.busy[s.ESN] = true
	m.mu.Unlock()
	go func() {
		var runErr error
		defer func() {
			if p := recover(); p != nil {
				runErr = fmt.Errorf("action panic: %v", p)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			delete(m.busy, s.ESN)
			if current := m.entries[id]; current != nil {
				current.running = false
				if runErr != nil {
					current.lastError = runErr.Error()
				}
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		runErr = m.run(ctx, s, when)
	}()
	return nil
}

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
	}
	id := strings.TrimPrefix(r.URL.Path, "/api-schedules/")
	switch {
	case r.Method == http.MethodGet && id == "":
		json.NewEncoder(w).Encode(m.List())
	case r.Method == http.MethodPost && id == "":
		var s Schedule
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "expected one JSON object", 400)
			return
		}
		if s.ID == "" {
			s.ID = uuid.NewString()
		}
		if err := m.Save(s); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		json.NewEncoder(w).Encode(s)
	case r.Method == http.MethodDelete && id != "":
		if err := m.Delete(id); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
			} else {
				http.Error(w, err.Error(), 500)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(id, "/run"):
		if err := m.start(strings.TrimSuffix(id, "/run"), false); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
			} else {
				http.Error(w, err.Error(), 409)
			}
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		http.Error(w, "unsupported schedule endpoint or method", http.StatusMethodNotAllowed)
	}
}
