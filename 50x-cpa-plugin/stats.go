package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// reqStats counts what this plugin handled. 50x charges free models $0, so
// GET /key always reports zero spend — local counters are the only signal.
type reqStats struct {
	Total       int64  `json:"total"`
	Today       int64  `json:"today"`
	Errors      int64  `json:"errors"`
	TokensTotal int64  `json:"tokens_total"`
	TokensToday int64  `json:"tokens_today"`
	Last        int64  `json:"last"`
	Day         string `json:"day"`
}

var (
	statsMu   sync.Mutex
	statsData = map[string]*reqStats{}
	statsPath = filepath.Join(os.TempDir(), "50x-cpa-stats.json")
)

func init() {
	loadStats()
}

func loadStats() {
	raw, err := os.ReadFile(statsPath)
	if err != nil {
		return
	}
	statsMu.Lock()
	defer statsMu.Unlock()
	_ = json.Unmarshal(raw, &statsData)
	if statsData == nil {
		statsData = map[string]*reqStats{}
	}
}

func saveStats() {
	statsMu.Lock()
	raw, err := json.Marshal(statsData)
	statsMu.Unlock()
	if err != nil {
		return
	}
	tmp := statsPath + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, statsPath)
	}
}

// bump records one request for uid.
func bump(uid string) {
	withStats(uid, func(s *reqStats) {
		s.Total++
		s.Today++
		s.Last = time.Now().Unix()
	})
}

// bumpError records a failed request for uid.
func bumpError(uid string) {
	withStats(uid, func(s *reqStats) { s.Errors++ })
}

// addTokens records token usage from one response.
func addTokens(uid string, u usageBlock) {
	n := u.TotalTokens
	if n == 0 {
		n = u.PromptTokens + u.CompletionTokens
	}
	if n <= 0 {
		return
	}
	withStats(uid, func(s *reqStats) {
		s.TokensTotal += n
		s.TokensToday += n
	})
}

func withStats(uid string, fn func(*reqStats)) {
	uid = normalizeStatsKey(uid)
	today := time.Now().UTC().Format("2006-01-02")
	statsMu.Lock()
	defer statsMu.Unlock()
	s := statsData[uid]
	if s == nil {
		s = &reqStats{Day: today}
		statsData[uid] = s
	}
	if s.Day != today {
		s.Day = today
		s.Today = 0
		s.TokensToday = 0
	}
	fn(s)
}

func getStats(uid string) reqStats {
	uid = normalizeStatsKey(uid)
	today := time.Now().UTC().Format("2006-01-02")
	statsMu.Lock()
	defer statsMu.Unlock()
	s := statsData[uid]
	if s == nil {
		return reqStats{Day: today}
	}
	out := *s
	if out.Day != today {
		out.Day = today
		out.Today = 0
		out.TokensToday = 0
	}
	return out
}

func normalizeStatsKey(uid string) string {
	uid = sanitizeUIDForFileName(uid)
	if uid == "" {
		return providerName
	}
	return uid
}
