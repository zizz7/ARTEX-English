package agent

import (
	"context"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

type countingProvider struct {
	cur, peak atomic.Int64
}

func (p *countingProvider) enter() {
	n := p.cur.Add(1)
	for {
		old := p.peak.Load()
		if n <= old || p.peak.CompareAndSwap(old, n) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	p.cur.Add(-1)
}

func (p *countingProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.enter()
		yield(llm.StreamEvent{}, nil)
	}
}

func (p *countingProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.enter()
	return llm.Message{}, "", llm.Usage{}, nil
}

func TestConcurrencyLimitSharedAcrossProviders(t *testing.T) {
	inner := &countingProvider{}
	// Two providers built from the same endpoint+model+key must share one pool.
	a := limitConcurrency(inner, slotPool("https://x.test/v1", "m", "key-1", 3))
	b := limitConcurrency(inner, slotPool("https://x.test/v1", "m", "key-1", 3))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := a
			if i%2 == 1 {
				p = b
			}
			if i%3 == 0 {
				_, _, _, _ = p.Complete(context.Background(), llm.CompletionRequest{})
				return
			}
			for range p.Stream(context.Background(), llm.CompletionRequest{}) {
			}
		}(i)
	}
	wg.Wait()
	if got := inner.peak.Load(); got > 3 {
		t.Fatalf("peak in-flight calls = %d, want <= 3", got)
	}
	if got := inner.peak.Load(); got < 2 {
		t.Fatalf("peak in-flight calls = %d, calls were not running in parallel", got)
	}
}

func TestConcurrencyLimitWaitHonoursContext(t *testing.T) {
	slots := slotPool("https://y.test/v1", "m", "key-2", 1)
	slots <- struct{}{} // occupy the only slot
	defer func() { <-slots }()
	p := limitConcurrency(&countingProvider{}, slots)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var gotErr error
	for _, err := range p.Stream(ctx, llm.CompletionRequest{}) {
		gotErr = err
	}
	if gotErr == nil {
		t.Fatal("expected the waiting call to fail when its context expired")
	}
}

func TestNoLimitWhenZero(t *testing.T) {
	c := Config{Format: llm.FormatOpenAI, BaseURL: "https://z.test/v1", Model: "m", APIKey: "k"}
	prov, err := c.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	if _, wrapped := prov.(*concurrencyLimited); wrapped {
		t.Fatal("MaxConcurrency=0 must not wrap the provider")
	}
	c.MaxConcurrency = 4
	prov, err = c.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	if _, wrapped := prov.(*concurrencyLimited); !wrapped {
		t.Fatal("MaxConcurrency>0 must wrap the provider")
	}
}
