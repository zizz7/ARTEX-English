package mcphttp

import (
	"strings"
	"sync"
	"time"
)

// KeySeparator splits one header value into alternative credentials, e.g.
// {"Authorization": "Bearer key-1 || Bearer key-2"}. When the server answers with
// 401/402/403/429 the client moves to the next alternative and retries.
const KeySeparator = "||"

// keyRing is the rotation state for one server's alternative credentials. It is
// shared process-wide (keyed by server+url+header) so every Client — agents build
// a new one per run — starts on the last credential that worked.
type keyRing struct {
	mu         sync.Mutex
	keys       []string
	cur        int
	spentUntil []time.Time
}

var (
	ringsMu sync.Mutex
	rings   = map[string]*keyRing{}
)

// splitRotatingHeader pulls the first header whose value holds several
// alternatives out of headers, returning the header name and its keys.
func splitRotatingHeader(headers map[string]string) (static map[string]string, name string, keys []string) {
	static = make(map[string]string, len(headers))
	for k, v := range headers {
		if name == "" && strings.Contains(v, KeySeparator) {
			for _, part := range strings.Split(v, KeySeparator) {
				if p := strings.TrimSpace(part); p != "" {
					keys = append(keys, p)
				}
			}
			if len(keys) > 1 {
				name = k
				continue
			}
			keys = nil
		}
		static[k] = v
	}
	return static, name, keys
}

func ringFor(server, url, header string, keys []string) *keyRing {
	id := server + "\x00" + url + "\x00" + header + "\x00" + strings.Join(keys, "\x00")
	ringsMu.Lock()
	defer ringsMu.Unlock()
	if r, ok := rings[id]; ok {
		return r
	}
	r := &keyRing{keys: keys, spentUntil: make([]time.Time, len(keys))}
	rings[id] = r
	return r
}

// pick returns the current credential, skipping ones still marked spent. When all
// are spent it returns the current one anyway, so a quota that reset early is found.
func (r *keyRing) pick(now time.Time) (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i < len(r.keys); i++ {
		idx := (r.cur + i) % len(r.keys)
		if !now.Before(r.spentUntil[idx]) {
			r.cur = idx
			return idx, r.keys[idx]
		}
	}
	return r.cur, r.keys[r.cur]
}

// markSpent records that credential idx was refused and reports whether another
// credential is still usable. Quota/billing refusals (402/429) last until the next
// UTC midnight, when daily quotas reset; auth refusals (401/403) for an hour.
func (r *keyRing) markSpent(idx, status int, now time.Time) (another bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	until := now.Add(time.Hour)
	if status == 402 || status == 429 {
		y, m, d := now.UTC().Date()
		until = time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
	}
	r.spentUntil[idx] = until
	for i := range r.keys {
		if !now.Before(r.spentUntil[i]) {
			r.cur = i
			return true
		}
	}
	return false
}

func rotatable(status int) bool {
	return status == 401 || status == 402 || status == 403 || status == 429
}
