package fault

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

type Mode string

const (
	ModeMissingAds  Mode = "missing_ads"   // GET /api/ads returns 503
	ModeSlowCDN     Mode = "slow_db"       // reuse slow_db signal → slow CDN fetch
)

type State struct {
	mu     sync.RWMutex
	active map[Mode]bool
}

var global = &State{active: make(map[Mode]bool)}

func Get() *State { return global }

func (s *State) IsActive(m Mode) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active[m]
}

func (s *State) set(modes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = make(map[Mode]bool)
	for _, m := range modes {
		s.active[Mode(m)] = true
	}
}

// StartPoller polls booking-service /api/faults every interval and mirrors state.
func StartPoller(ctx context.Context, bookingServiceURL string, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				poll(bookingServiceURL)
			}
		}
	}()
}

func poll(url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/faults", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn().Err(err).Msg("fault poll failed")
		return
	}
	defer resp.Body.Close()

	var payload struct {
		ActiveFaults []string `json:"active_faults"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		log.Warn().Err(err).Msg("fault poll decode failed")
		return
	}

	global.set(payload.ActiveFaults)
	if len(payload.ActiveFaults) > 0 {
		log.Debug().Strs("faults", payload.ActiveFaults).Msg("fault state synced")
	}
}
