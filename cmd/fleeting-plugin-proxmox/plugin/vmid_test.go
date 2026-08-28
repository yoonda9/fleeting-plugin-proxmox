package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// alwaysFreeCheckID is a checkID stub that reports every candidate as free.
func alwaysFreeCheckID(context.Context, int) (bool, error) {
	return true, nil
}

// onlyFreeCheckID is a checkID stub that reports vmid, and nothing else, as free: once vmid is
// reserved locally, every other candidate in the attempt window is rejected, so Allocate must
// exhaust its attempts rather than wander off to a different free id.
func onlyFreeCheckID(vmid int) func(context.Context, int) (bool, error) {
	return func(_ context.Context, candidate int) (bool, error) {
		return candidate == vmid, nil
	}
}

// constantNextID is a nextID stub that always proposes vmid, as /cluster/nextid does while
// nothing is created: reservation is process-local and the endpoint has no idea about it.
func constantNextID(vmid int) func(context.Context) (int, error) {
	return func(context.Context) (int, error) {
		return vmid, nil
	}
}

// unusedNextID is a nextID stub for tests of paths that must never propose a new id.
func unusedNextID(context.Context) (int, error) {
	return 0, errors.New("nextID should not be called by ReleaseIfFree")
}

// TestVMIDAllocator_allocateConcurrentIsUnique is a regression test for
// Allocate's local-collision check. nextID/checkID model /cluster/nextid and
// Cluster.CheckID against a simulated cluster where nothing is ever actually
// created during the test: reserving a vmid locally never advances what the
// (simulated) cluster reports, since reservation is process-local and the
// real endpoint has no idea about it. A nextID stub that instead
// auto-advances (e.g. via atomic.Add) would make the allocator's own
// bookkeeping unnecessary for uniqueness, which is exactly the bug this test
// caught: deleting the local-collision check in Allocate left the old
// version of this test green.
//
// Because the simulated cluster never advances, every candidate search starts
// back at vmid 1 and Allocate's local-advance can only walk
// vmidAllocateAttempts candidates per call - so of the concurrency-many
// racing callers, only vmidAllocateAttempts can succeed, deterministically
// claiming 1..vmidAllocateAttempts. That bound, not "everyone succeeds", is
// the contract: it replaces an earlier behaviour where a burst degraded
// toward a single success.
func TestVMIDAllocator_allocateConcurrentIsUnique(t *testing.T) {
	const concurrency = 50

	var (
		clusterMu sync.Mutex
		created   = make(map[int]struct{})
	)

	nextID := func(context.Context) (int, error) {
		clusterMu.Lock()
		defer clusterMu.Unlock()

		for id := 1; ; id++ {
			if _, taken := created[id]; !taken {
				return id, nil
			}
		}
	}

	checkID := func(_ context.Context, vmid int) (bool, error) {
		clusterMu.Lock()
		defer clusterMu.Unlock()

		_, taken := created[vmid]

		return !taken, nil
	}

	allocator := newVMIDAllocator(checkID, nextID)

	// runParallel gives each caller its own slot for both the error and the vmid, so the
	// results need no lock of their own to stay matched to the caller that produced them.
	allocated := make([]int, concurrency)

	errs := runParallel(concurrency, func(index int) error {
		vmid, err := allocator.Allocate(context.Background())
		allocated[index] = vmid

		return err
	})

	results := make(map[int]struct{}, concurrency)
	failures := 0

	for index, err := range errs {
		if err != nil {
			require.ErrorIs(t, err, ErrVMIDAllocationFailed)

			failures++

			continue
		}

		_, duplicate := results[allocated[index]]
		require.False(t, duplicate, "vmid %d allocated more than once", allocated[index])
		results[allocated[index]] = struct{}{}
	}

	require.Len(t, results, vmidAllocateAttempts,
		"a burst sharing one unadvancing candidate should succeed up to vmidAllocateAttempts times, not just once")
	require.Equal(t, concurrency-vmidAllocateAttempts, failures)
}

func TestVMIDAllocator_allocateSkipsIDsRejectedByCheckID(t *testing.T) {
	var nextIDCalls int

	nextID := func(context.Context) (int, error) {
		nextIDCalls++

		return 100, nil
	}

	checkID := func(_ context.Context, vmid int) (bool, error) {
		// 100 is rejected (e.g. it exists on the cluster already); Allocate
		// advances the candidate locally instead of calling nextID again.
		return vmid != 100, nil
	}

	allocator := newVMIDAllocator(checkID, nextID)

	vmid, err := allocator.Allocate(context.Background())

	require.NoError(t, err)
	require.Equal(t, 101, vmid)
	require.Equal(t, 1, nextIDCalls)
}

// TestVMIDAllocator_allocateAdvancesPastALocalCollision is a regression test:
// nextID not advancing once this process has already reserved the lowest
// free id must not exhaust Allocate's attempts on repeats of the same
// candidate - Allocate has to walk forward locally instead.
func TestVMIDAllocator_allocateAdvancesPastALocalCollision(t *testing.T) {
	allocator := newVMIDAllocator(alwaysFreeCheckID, constantNextID(100))

	first, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 100, first)

	second, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 101, second)
}

func TestVMIDAllocator_allocateFailsWhenAttemptsExhausted(t *testing.T) {
	// Every candidate from the base through base+vmidAllocateAttempts-1 is
	// rejected, e.g. because the cluster already has a contiguous run of VMs
	// there; Allocate must give up rather than loop forever.
	allocator := newVMIDAllocator(func(_ context.Context, vmid int) (bool, error) {
		return vmid >= 100+vmidAllocateAttempts, nil
	}, constantNextID(100))

	_, err := allocator.Allocate(context.Background())
	require.ErrorIs(t, err, ErrVMIDAllocationFailed)
}

func TestVMIDAllocator_releaseReturnsTheIDToThePool(t *testing.T) {
	// Only 100 is ever free, or Allocate would wander off to a different free id and defeat
	// the point of testing Release.
	allocator := newVMIDAllocator(onlyFreeCheckID(100), constantNextID(100))

	vmid, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 100, vmid)

	_, err = allocator.Allocate(context.Background())
	require.ErrorIs(t, err, ErrVMIDAllocationFailed)

	allocator.Release(vmid)

	reallocated, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 100, reallocated)
}

func TestVMIDAllocator_allocatePropagatesNextIDError(t *testing.T) {
	wantErr := errors.New("nextid unavailable")

	allocator := newVMIDAllocator(alwaysFreeCheckID, func(context.Context) (int, error) {
		return 0, wantErr
	})

	_, err := allocator.Allocate(context.Background())
	require.ErrorIs(t, err, wantErr)
}

func TestVMIDAllocator_allocatePropagatesCheckIDError(t *testing.T) {
	wantErr := errors.New("checkid unavailable")

	allocator := newVMIDAllocator(func(context.Context, int) (bool, error) {
		return false, wantErr
	}, constantNextID(100))

	_, err := allocator.Allocate(context.Background())
	require.ErrorIs(t, err, wantErr)
}

// TestVMIDAllocator_releaseIfFree is a regression test: ReleaseIfFree exists
// only to behave differently from Release when the cluster still reports the
// id as taken, so that difference needs its own coverage rather than relying
// on instances_test.go merely observing that checkID was called. Collapsing
// the guard to an unconditional delete after the checkID round trip leaves
// the "reports the id as taken" and "checkID errors" cases green with the
// old suite; both fail here.
func TestVMIDAllocator_releaseIfFree(t *testing.T) {
	tests := []struct {
		name         string
		reserved     bool
		checkIDFree  bool
		checkIDErr   error
		wantReserved bool
		wantCheckID  bool
	}{
		{
			name:         "cluster reports the id free: reservation is dropped",
			reserved:     true,
			checkIDFree:  true,
			wantReserved: false,
			wantCheckID:  true,
		},
		{
			name:         "cluster reports the id taken: reservation is kept",
			reserved:     true,
			checkIDFree:  false,
			wantReserved: true,
			wantCheckID:  true,
		},
		{
			name:         "checkID errors: reservation is kept",
			reserved:     true,
			checkIDErr:   errors.New("checkid unavailable"),
			wantReserved: true,
			wantCheckID:  true,
		},
		{
			name:         "id was never reserved: no-op, checkID is not called",
			reserved:     false,
			checkIDFree:  true,
			wantReserved: false,
			wantCheckID:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var checkIDCalled bool

			checkID := func(context.Context, int) (bool, error) {
				checkIDCalled = true

				return tt.checkIDFree, tt.checkIDErr
			}

			allocator := newVMIDAllocator(checkID, unusedNextID)

			const vmid = 100

			if tt.reserved {
				allocator.reserved[vmid] = struct{}{}
			}

			allocator.ReleaseIfFree(context.Background(), vmid)

			require.Equal(t, tt.wantCheckID, checkIDCalled)

			_, stillReserved := allocator.reserved[vmid]
			require.Equal(t, tt.wantReserved, stillReserved)
		})
	}
}

// ReleaseIfFree runs on the path where the caller's context has usually just been cancelled -
// a cancelled wait is what leaves the reservation's fate unknown in the first place - so it
// must confirm on a context of its own. Confirming on the caller's would fail instantly with
// "context canceled" and leak the reservation every single time.
func TestVMIDAllocator_releaseIfFreeConfirmsOnACancelledCallerContext(t *testing.T) {
	const vmid = 100

	var confirmed bool

	allocator := newVMIDAllocator(func(ctx context.Context, _ int) (bool, error) {
		// what a real client does when handed a dead context
		if err := ctx.Err(); err != nil {
			return false, err
		}

		confirmed = true

		return true, nil
	}, unusedNextID)

	allocator.reserved[vmid] = struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	allocator.ReleaseIfFree(ctx, vmid)

	require.True(t, confirmed, "the confirmation must still reach the cluster")

	_, stillReserved := allocator.reserved[vmid]
	require.False(t, stillReserved, "an id the cluster confirms free must be released")
}
