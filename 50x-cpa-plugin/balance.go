package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Account balance from the 50x control plane (GET /api/billing). It needs the
// dashboard session token (Authorization: Token …), which the farm stores at
// registration; if that expired we log in again with the stored password.
//
// /api/* is routinely slow (2–30s) or hanging, so the panel never waits on a
// fetch: handleUsage serves the cached snapshot and this file refreshes stale
// entries in the background.

type balanceSnap struct {
	BalanceUSD   string `json:"balance_usd,omitempty"`
	TokensToday  int64  `json:"tokens_used_today,omitempty"`
	CostToday    string `json:"cost_today,omitempty"`
	Cost7d       string `json:"cost_7d,omitempty"`
	Requests7d   int64  `json:"requests_7d,omitempty"`
	RequestsDay  int64  `json:"requests_today,omitempty"`
	InTokensDay  int64  `json:"in_tokens_today,omitempty"`
	OutTokensDay int64  `json:"out_tokens_today,omitempty"`
	At           int64  `json:"at,omitempty"`
	Err          string `json:"error,omitempty"`
}

const (
	balanceTTL   = 10 * time.Minute
	balanceFetch = 12 * time.Second
)

var balanceState = struct {
	sync.Mutex
	m    map[string]balanceSnap
	busy map[string]bool
}{m: map[string]balanceSnap{}, busy: map[string]bool{}}

func balanceFor(sa *storedAuth, force bool) balanceSnap {
	if sa == nil {
		return balanceSnap{}
	}
	if force {
		return balanceForced(sa)
	}
	uid := sa.Account.UID
	balanceState.Lock()
	snap := balanceState.m[uid]
	busy := balanceState.busy[uid]
	balanceState.Unlock()
	fresh := snap.At > 0 && time.Since(time.Unix(snap.At, 0)) < balanceTTL
	if (force || !fresh) && !busy {
		balanceState.Lock()
		balanceState.busy[uid] = true
		balanceState.Unlock()
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			defer func() {
				balanceState.Lock()
				delete(balanceState.busy, uid)
				balanceState.Unlock()
			}()
			out := fetchBalance(sa)
			balanceState.Lock()
			balanceState.m[uid] = out
			balanceState.Unlock()
		}()
	}
	return snap
}

// balanceForced refreshes inline: the panel only sets force on an explicit card
// click, so waiting up to balanceFetch is fine and gives a truthful number.
var balanceForceMu sync.Mutex

func balanceForced(sa *storedAuth) balanceSnap {
	balanceForceMu.Lock()
	defer balanceForceMu.Unlock()
	uid := sa.Account.UID
	balanceState.Lock()
	snap := balanceState.m[uid]
	balanceState.Unlock()
	if snap.At > 0 && time.Since(time.Unix(snap.At, 0)) < 3*time.Second {
		return snap
	}
	out := fetchBalance(sa)
	balanceState.Lock()
	balanceState.m[uid] = out
	balanceState.Unlock()
	return out
}

func fetchBalance(sa *storedAuth) balanceSnap {
	now := time.Now().Unix()
	session := strings.TrimSpace(sa.Account.Session)
	if session == "" {
		s, err := loginSession(sa)
		if err != nil {
			return balanceSnap{At: now, Err: shortErr(err.Error())}
		}
		session = s
	}
	snap, status, err := billingWith(session)
	if err == nil && status == http.StatusUnauthorized && sa.Account.Password != "" {
		if s, lerr := loginSession(sa); lerr == nil {
			snap, status, err = billingWith(s)
		}
	}
	if err != nil {
		return balanceSnap{At: now, Err: shortErr(err.Error())}
	}
	if status >= 400 {
		return balanceSnap{At: now, Err: fmt.Sprintf("billing HTTP %d", status)}
	}
	// /api/usage is the only source of actual spend (cost_usd). It is best
	// effort: a failed aggregation must not hide the balance we already have.
	if u, err := usageWith(session); err == nil {
		snap.CostToday, snap.Cost7d = u.costToday, u.cost7d
		snap.RequestsDay, snap.Requests7d = u.requestsToday, u.requests7d
		snap.InTokensDay, snap.OutTokensDay = u.inToday, u.outToday
	}
	return snap
}

type usageTotals struct {
	costToday                 string
	cost7d                    string
	requestsToday, requests7d int64
	inToday, outToday         int64
}

// usageWith sums /api/usage?days=7 rows: today's row plus the whole window.
func usageWith(session string) (usageTotals, error) {
	ctx, cancel := context.WithTimeout(bgCtx, balanceFetch)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, controlBase+"/usage?days=7", nil)
	if err != nil {
		return usageTotals{}, err
	}
	req.Header.Set("Authorization", "Token "+session)
	resp, err := hostHTTPDoWithCallback(req, "")
	if err != nil {
		return usageTotals{}, err
	}
	if resp.StatusCode >= 400 {
		return usageTotals{}, fmt.Errorf("usage HTTP %d", resp.StatusCode)
	}
	var out struct {
		Rows []struct {
			Date         string `json:"date"`
			Requests     int64  `json:"requests"`
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
			CostUSD      string `json:"cost_usd"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return usageTotals{}, err
	}
	today := time.Now().UTC().Format("2006-01-02")
	var costToday, cost7d float64
	var t usageTotals
	for _, r := range out.Rows {
		c, _ := strconv.ParseFloat(r.CostUSD, 64)
		cost7d += c
		t.requests7d += r.Requests
		if r.Date == today {
			costToday += c
			t.requestsToday += r.Requests
			t.inToday += r.InputTokens
			t.outToday += r.OutputTokens
		}
	}
	t.costToday = strconv.FormatFloat(costToday, 'f', 8, 64)
	t.cost7d = strconv.FormatFloat(cost7d, 'f', 8, 64)
	return t, nil
}

func billingWith(session string) (balanceSnap, int, error) {
	ctx, cancel := context.WithTimeout(bgCtx, balanceFetch)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, controlBase+"/billing", nil)
	if err != nil {
		return balanceSnap{}, 0, err
	}
	req.Header.Set("Authorization", "Token "+session)
	resp, err := hostHTTPDoWithCallback(req, "")
	if err != nil {
		return balanceSnap{}, 0, err
	}
	if resp.StatusCode >= 400 {
		return balanceSnap{}, resp.StatusCode, nil
	}
	var out struct {
		BalanceUSD      string `json:"balance_usd"`
		TokensUsedToday int64  `json:"tokens_used_today"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return balanceSnap{}, resp.StatusCode, fmt.Errorf("billing 解析失败: %w", err)
	}
	return balanceSnap{
		BalanceUSD:  out.BalanceUSD,
		TokensToday: out.TokensUsedToday,
		At:          time.Now().Unix(),
	}, resp.StatusCode, nil
}

func loginSession(sa *storedAuth) (string, error) {
	if sa == nil || strings.TrimSpace(sa.Account.Email) == "" || strings.TrimSpace(sa.Account.Password) == "" {
		return "", fmt.Errorf("没有邮箱/密码，无法拉余额")
	}
	body, _ := json.Marshal(map[string]string{
		"email":    sa.Account.Email,
		"password": sa.Account.Password,
	})
	ctx, cancel := context.WithTimeout(bgCtx, balanceFetch)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controlBase+"/auth/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hostHTTPDoWithCallback(req, "")
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("login HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("login 响应里没有 token")
	}
	return out.Token, nil
}
