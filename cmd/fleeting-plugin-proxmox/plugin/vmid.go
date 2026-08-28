package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// vmidAllocateAttempts bounds how many candidate VMIDs vmidAllocator.Allocate
// tries before giving up. /cluster/nextid always returns the lowest free ID and
// has no idea what this process has reserved locally, so a burst of
// concurrent Allocate calls can be handed the exact same starting candidate
// over and over. Allocate does not repeat that same nextID call on a local
// collision - it advances the candidate locally instead (candidate,
// candidate+1, ...), spending its checkID round trips on genuinely different
// IDs. This bounds a burst of N concurrent Allocate calls to
// min(N, vmidAllocateAttempts) successes instead of degrading toward a single
// success; a call only fails once
// vmidAllocateAttempts consecutive candidates starting there are all taken,
// locally or on the cluster.
const vmidAllocateAttempts = 10

var ErrVMIDAllocationFailed = errors.New("failed to allocate a free vmid")

// vmidAllocator hands out VMIDs for new clones and keeps a local reservation set so
// two concurrent Allocate calls in this process can never be handed the same ID.
// /cluster/nextid reserves nothing server-side (Cluster.pm:1037-1040) and Proxmox
// only writes the target config inside its forked clone worker, after the clone
// POST returns - the reservation set is what actually closes that window for
// clones issued by this process; passing the result as an explicit NewID is what
// makes a cross-process collision fail the clone POST instead of creating a
// duplicate VM.
//
// checkID and nextID are injected so the allocator is unit-testable without a live
// Proxmox API.
type vmidAllocator struct {
	mu       sync.Mutex
	reserved map[int]struct{}

	checkID func(ctx context.Context, vmid int) (bool, error)
	nextID  func(ctx context.Context) (int, error)
}

func newVMIDAllocator(
	checkID func(ctx context.Context, vmid int) (bool, error),
	nextID func(ctx context.Context) (int, error),
) *vmidAllocator {
	return &vmidAllocator{
		reserved: make(map[int]struct{}),
		checkID:  checkID,
		nextID:   nextID,
	}
}

// Allocate reserves and returns a VMID that is not currently reserved by this
// allocator. The caller must pass the result as an explicit NewID on the clone
// request and call Release if the clone POST fails or once the collector confirms
// the resulting instance has been deleted.
func (a *vmidAllocator) Allocate(ctx context.Context) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	candidate, err := a.nextID(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get next free vmid: %w", err)
	}

	for attempt := range vmidAllocateAttempts {
		vmid := candidate + attempt

		if _, taken := a.reserved[vmid]; taken {
			continue
		}

		free, err := a.checkID(ctx, vmid)
		if err != nil {
			return 0, fmt.Errorf("failed to check vmid='%d': %w", vmid, err)
		}

		if !free {
			continue
		}

		a.reserved[vmid] = struct{}{}

		return vmid, nil
	}

	return 0, ErrVMIDAllocationFailed
}

// Release returns vmid to the pool of allocatable IDs. Releasing a vmid that is
// not currently reserved is a no-op.
//
// Release policy across the plugin (kept in one place): a clone POST failure
// releases immediately (cloneTemplate in instances.go, the id was never sent
// to Proxmox); a clone task failure releases via ReleaseIfFree
// (runDeployWorker in deploy.go, the id may or may not have landed); a
// confirmed instance delete releases immediately (finishRemoval in
// collector.go); every other failure to stop or fetch an instance leaves the
// reservation in place rather than risk freeing an id that is still live. A
// deployed instance that fails to locate its VM or write its creating tag
// (deployClonedInstance in deploy.go) is never renamed to
// InstanceNameRemoving, so the collector never sees it to release either - a
// slow reservation drain, not a brick, since it only consumes
// vmidAllocateAttempts worth of headroom rather than repeating on every
// future clone.
func (a *vmidAllocator) Release(vmid int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.reserved, vmid)
}

// ReleaseIfFree returns vmid to the pool only once the cluster confirms it is
// actually free. It exists for outcomes where the reservation's fate is
// unknown - e.g. a clone task wait that timed out rather than confirming
// failure, where the clone may still land - so that releasing early can never
// hand a still-live id to a concurrent Allocate. A checkID error, or the
// cluster reporting the id as taken, leaves the reservation in place: a
// leaked local reservation is bounded by vmidAllocateAttempts (see its doc
// comment); a duplicate VM is not recoverable.
func (a *vmidAllocator) ReleaseIfFree(ctx context.Context, vmid int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, reserved := a.reserved[vmid]; !reserved {
		return
	}

	free, err := a.checkID(ctx, vmid)
	if err != nil || !free {
		return
	}

	delete(a.reserved, vmid)
}
