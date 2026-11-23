package mcphttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeMCP answers JSON-RPC only for the credentials in good; the rest get 429.
type fakeMCP struct {
	mu   sync.Mutex
	good map[string]bool
	seen []string
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	f.mu.Lock()
	f.seen = append(f.seen, auth)
	ok := f.good[auth]
	f.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"You have used today's 200 calls."}`, http.StatusTooManyRequests)
		return
	}
	var req rpcRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ID == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Mcp-Session-Id", "sess-"+auth)
	w.Header().Set("Content-Type", "application/json")
	var result any = map[string]any{}
	if req.Method == "tools/call" {
		result = map[string]any{"content": []map[string]string{{"type": "text", "text": "ok via " + auth}}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func (f *fakeMCP) setGood(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.good = map[string]bool{}
	for _, k := range keys {
		f.good[k] = true
	}
}

func (f *fakeMCP) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.seen
	f.seen = nil
	return out
}

func TestRotatesToNextKeyOnQuota(t *testing.T) {
	f := &fakeMCP{}
	f.setGood("Bearer k2", "Bearer k3")
	srv := httptest.NewServer(f)
	defer srv.Close()
	headers := map[string]string{"Authorization": "Bearer k1 || Bearer k2 ||Bearer k3", "X-Other": "x"}

	c, err := New(context.Background(), "rot1", srv.URL, headers)
	if err != nil {
		t.Fatalf("initialize should rotate past the spent key: %v", err)
	}
	if got := f.take(); len(got) < 2 || got[0] != "Bearer k1" || got[1] != "Bearer k2" {
		t.Fatalf("expected k1 then k2 during initialize, got %v", got)
	}
	out, err := c.Call(context.Background(), "search", map[string]any{})
	if err != nil || out != "ok via Bearer k2" {
		t.Fatalf("call: out=%q err=%v", out, err)
	}

	// k2 runs out mid-session: the call must re-initialize on k3 and succeed.
	f.setGood("Bearer k3")
	out, err = c.Call(context.Background(), "search", map[string]any{})
	if err != nil || out != "ok via Bearer k3" {
		t.Fatalf("rotation mid-session: out=%q err=%v", out, err)
	}

	// A brand-new client (agents build one per run) starts on the key that works.
	f.take()
	if _, err := New(context.Background(), "rot1", srv.URL, headers); err != nil {
		t.Fatal(err)
	}
	if got := f.take(); len(got) == 0 || got[0] != "Bearer k3" {
		t.Fatalf("new client should start on k3, got %v", got)
	}
}

func TestAllKeysSpentReturnsError(t *testing.T) {
	f := &fakeMCP{}
	f.setGood()
	srv := httptest.NewServer(f)
	defer srv.Close()
	_, err := New(context.Background(), "rot2", srv.URL, map[string]string{"Authorization": "Bearer a||Bearer b"})
	if err == nil {
		t.Fatal("expected an error when every key is spent")
	}
	if got := f.take(); len(got) != 2 {
		t.Fatalf("each key should be tried exactly once, got %v", got)
	}
}

func TestSingleKeyUnchanged(t *testing.T) {
	static, name, keys := splitRotatingHeader(map[string]string{"Authorization": "Bearer only", "X-Api-Key": "abc"})
	if name != "" || keys != nil || static["Authorization"] != "Bearer only" || static["X-Api-Key"] != "abc" {
		t.Fatalf("single credential must pass through untouched: %v %q %v", static, name, keys)
	}
}

func TestQuotaSpentUntilUTCMidnight(t *testing.T) {
	r := &keyRing{keys: []string{"a", "b"}, spentUntil: make([]time.Time, 2)}
	now := time.Date(2026, 11, 23, 8, 30, 0, 0, time.UTC)
	if !r.markSpent(0, 429, now) {
		t.Fatal("b should still be usable")
	}
	if want := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC); !r.spentUntil[0].Equal(want) {
		t.Fatalf("429 should last until UTC midnight, got %v", r.spentUntil[0])
	}
	if idx, _ := r.pick(now); idx != 1 {
		t.Fatalf("pick should skip the spent key, got %d", idx)
	}
	if idx, _ := r.pick(time.Date(2026, 11, 24, 0, 0, 1, 0, time.UTC)); idx != 1 {
		t.Fatalf("after reset the current key stays in use, got %d", idx)
	}
}
