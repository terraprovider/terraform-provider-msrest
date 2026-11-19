package provider

import (
	"context"
	"sync"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// writeGroups serializes the writes of resources in the same `concurrency`
// group. It is process-wide: OpenTofu and Terraform start one provider process
// per provider configuration, so a group covers every resource of that
// configuration, but not other processes.
var writeGroups = &groupLocks{locks: map[string]chan struct{}{}}

// groupLocks is a set of named mutexes. A lock is a channel with capacity one,
// so waiting for it can be cancelled.
type groupLocks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func (g *groupLocks) lock(name string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	l, ok := g.locks[name]
	if !ok {
		l = make(chan struct{}, 1)
		g.locks[name] = l
	}
	return l
}

// acquire waits until the group is free and returns the function that releases
// it. An empty group locks nothing. It fails only when ctx ends while waiting.
func (g *groupLocks) acquire(ctx context.Context, group string) (release func(), err error) {
	if group == "" {
		return func() {}, nil
	}
	l := g.lock(group)
	select {
	case l <- struct{}{}:
	default:
		tflog.Debug(ctx, "waiting for the concurrency group", map[string]any{"concurrency": group})
		select {
		case l <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return func() { <-l }, nil
}
