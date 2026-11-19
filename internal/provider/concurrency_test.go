package provider

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestGroupLocksSerializes(t *testing.T) {
	g := &groupLocks{locks: map[string]chan struct{}{}}
	var mu sync.Mutex
	active, maxActive := 0, 0
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := g.acquire(context.Background(), "licensing")
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			mu.Lock()
			active++
			maxActive = max(maxActive, active)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if maxActive != 1 {
		t.Errorf("%d holders at once, want 1", maxActive)
	}
}

func TestGroupLocksIndependentGroups(t *testing.T) {
	g := &groupLocks{locks: map[string]chan struct{}{}}
	releaseA, err := g.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseB, err := g.acquire(ctx, "b")
	if err != nil {
		t.Fatalf("group b waited for group a: %v", err)
	}
	releaseB()
	// No group locks nothing.
	for range 2 {
		release, err := g.acquire(ctx, "")
		if err != nil {
			t.Fatalf("empty group: %v", err)
		}
		defer release()
	}
}

func TestGroupLocksCancel(t *testing.T) {
	g := &groupLocks{locks: map[string]chan struct{}{}}
	release, err := g.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.acquire(ctx, "a"); err == nil {
		t.Fatal("acquired a held group")
	}
	release()
	release2, err := g.acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	release2()
}
