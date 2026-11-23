package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"iter"
	"sync"

	"github.com/Autumn-27/norma/llm"
)

// Providers are rebuilt on every profile edit and cached per profile, so the slot
// pool lives in a process-wide registry keyed by endpoint+model+key: calls still in
// flight on a replaced provider keep counting, and two profiles pointing at the same
// account and model share one cap — which is how upstream APIs count it.
var (
	slotPoolsMu sync.Mutex
	slotPools   = map[string]chan struct{}{}
)

func slotPool(baseURL, model, apiKey string, size int) chan struct{} {
	sum := sha256.Sum256([]byte(apiKey))
	key := baseURL + "\x00" + model + "\x00" + hex.EncodeToString(sum[:8])
	slotPoolsMu.Lock()
	defer slotPoolsMu.Unlock()
	if p, ok := slotPools[key]; ok && cap(p) == size {
		return p
	}
	p := make(chan struct{}, size)
	slotPools[key] = p
	return p
}

// concurrencyLimited caps the number of Stream/Complete calls in flight on the
// wrapped provider. A waiting call gives up when its context ends.
type concurrencyLimited struct {
	inner llm.Provider
	slots chan struct{}
}

func limitConcurrency(p llm.Provider, slots chan struct{}) llm.Provider {
	return &concurrencyLimited{inner: p, slots: slots}
}

func (c *concurrencyLimited) acquire(ctx context.Context) error {
	select {
	case c.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *concurrencyLimited) release() { <-c.slots }

func (c *concurrencyLimited) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if err := c.acquire(ctx); err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		defer c.release()
		for ev, err := range c.inner.Stream(ctx, req) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (c *concurrencyLimited) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if err := c.acquire(ctx); err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	defer c.release()
	return c.inner.Complete(ctx, req)
}
