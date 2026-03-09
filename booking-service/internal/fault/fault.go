package fault

import (
	"sync"
	"time"
)

// Mode represents an injectable fault type.
type Mode string

const (
	ModeNone            Mode = ""
	ModeSlowDB          Mode = "slow_db"          // artificial delay on every DB query
	ModeBookingFailure  Mode = "booking_failure"   // 5xx on POST /bookings
	ModePoolExhaustion  Mode = "pool_exhaustion"   // acquire conn delay simulating pool full
)

// State holds the current fault configuration.
type State struct {
	mu      sync.RWMutex
	active  map[Mode]bool
	slowDB  time.Duration // how long to sleep per query when slow_db is on
}

var global = &State{
	active: make(map[Mode]bool),
	slowDB: 2 * time.Second,
}

// Get returns the global fault state singleton.
func Get() *State { return global }

func (s *State) Enable(m Mode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[m] = true
}

func (s *State) Disable(m Mode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, m)
}

func (s *State) IsActive(m Mode) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active[m]
}

// ActiveModes returns a copy of currently active fault modes.
func (s *State) ActiveModes() []Mode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Mode, 0, len(s.active))
	for m := range s.active {
		out = append(out, m)
	}
	return out
}

// SlowDBDelay returns how long DB operations should sleep when slow_db is active.
func (s *State) SlowDBDelay() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.slowDB
}

// SetSlowDBDelay lets the control panel configure the delay duration.
func (s *State) SetSlowDBDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slowDB = d
}
