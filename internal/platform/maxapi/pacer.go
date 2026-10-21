package maxapi

import (
	"context"
	"sync"
	"time"
)

type pacer struct {
	gate     chan struct{}
	next     time.Time
	interval time.Duration
	parent   *pacer
}

type recipientPacer struct {
	mu        sync.Mutex
	entries   map[int64]*recipientEntry
	interval  time.Duration
	nextSweep time.Time
	parent    *pacer
}

type recipientEntry struct {
	pacer     *pacer
	waiters   int
	idleUntil time.Time
}

func newRecipientPacer(interval time.Duration) *recipientPacer {
	return &recipientPacer{entries: make(map[int64]*recipientEntry), interval: interval}
}

func (p *recipientPacer) wait(ctx context.Context, recipient int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	p.mu.Lock()
	if !now.Before(p.nextSweep) {
		for id, entry := range p.entries {
			if entry.waiters == 0 && !now.Before(entry.idleUntil) {
				delete(p.entries, id)
			}
		}
		p.nextSweep = now.Add(p.interval)
	}
	entry := p.entries[recipient]
	if entry == nil {
		entry = &recipientEntry{pacer: newPacer(p.interval)}
		entry.pacer.parent = p.parent
		p.entries[recipient] = entry
	}
	entry.waiters++
	p.mu.Unlock()
	err := entry.pacer.wait(ctx)
	p.mu.Lock()
	entry.waiters--
	if entry.waiters == 0 {
		entry.idleUntil = time.Now().Add(p.interval)
	}
	p.mu.Unlock()
	return err
}

func newPacer(interval time.Duration) *pacer {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &pacer{gate: gate, interval: interval}
}

func (p *pacer) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.gate:
	}
	defer func() { p.gate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if wait := time.Until(p.next); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if p.parent != nil {
		if err := p.parent.wait(ctx); err != nil {
			return err
		}
	}
	p.next = time.Now().Add(p.interval)
	return nil
}
