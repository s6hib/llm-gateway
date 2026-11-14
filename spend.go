package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// tally is one caller's totals for one day.
type tally struct {
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"input_tokens"` // the whole prompt, cached parts included
	OutputTokens int64 `json:"output_tokens"`
	CostMicroUSD int64 `json:"cost_micro_usd"` // millionths of a dollar
}

// spendStore keeps what each caller has used per day, in memory and in a JSON
// file, so the totals and the daily cap survive a restart. The file looks like
// {"2026-11-13": {"fineprint": {"requests": 3, ...}}}. Days are UTC.
//
// The whole file is rewritten after every counted call. That is fine for a
// few callers and would be too slow for many.
type spendStore struct {
	mu   sync.Mutex
	path string
	now  func() time.Time // swapped for a fake clock in tests
	days map[string]map[string]*tally
}

// openSpend loads the file at path. A missing file is an empty store. A file
// that cannot be read is an error: starting from zero would quietly lift
// every caller's cap.
func openSpend(path string) (*spendStore, error) {
	s := &spendStore{path: path, now: time.Now, days: map[string]map[string]*tally{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.days); err != nil {
		return nil, fmt.Errorf("%s is not a spend file: %w", path, err)
	}
	if s.days == nil { // the file held the JSON value null
		s.days = map[string]map[string]*tally{}
	}
	return s, nil
}

const dayFormat = "2006-01-02"

// spentToday is the caller's cost so far today, in millionths of a dollar,
// and how long until the day ends and the total starts again. Both come from
// one reading of the clock, so they always describe the same day.
func (s *spendStore) spentToday(name string) (spent int64, resets time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	if t := s.days[now.Format(dayFormat)][name]; t != nil {
		spent = t.CostMicroUSD
	}
	return spent, midnight.Sub(now)
}

// add counts one answered call against the caller and saves the file.
func (s *spendStore) add(name string, u usage, costMicro int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := s.now().UTC().Format(dayFormat)
	if s.days[day] == nil {
		s.days[day] = map[string]*tally{}
	}
	t := s.days[day][name]
	if t == nil {
		t = &tally{}
		s.days[day][name] = t
	}
	t.Requests++
	t.InputTokens += u.promptTokens()
	t.OutputTokens += u.OutputTokens
	t.CostMicroUSD += costMicro
	return s.save()
}

// save writes the totals to a temporary file and then renames it over the
// real one. A rename either happens or does not, so a crash in the middle of
// a save cannot leave half a file behind.
func (s *spendStore) save() error {
	raw, err := json.MarshalIndent(s.days, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".spend-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // does nothing once the rename has happened
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
