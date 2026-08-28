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

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		results  = make(map[int]struct{}, concurrency)
		failures int
	)

	for range concurrency {
		wg.Add(1)

		go func() {
			defer wg.Done()

			vmid, err := allocator.Allocate(context.Background())

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				require.ErrorIs(t, err, ErrVMIDAllocationFailed)
				failures++

				return
			}

			_, duplicate := results[vmid]
			require.False(t, duplicate, "vmid %d allocated more than once", vmid)
			results[vmid] = struct{}{}
		}()
	}

	wg.Wait()

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
	allocator := newVMIDAllocator(alwaysFreeCheckID, func(context.Context) (int, error) {
		return 100, nil
	})

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
	}, func(context.Context) (int, error) {
		return 100, nil
	})

	_, err := allocator.Allocate(context.Background())
	require.ErrorIs(t, err, ErrVMIDAllocationFailed)
}

func TestVMIDAllocator_releaseReturnsTheIDToThePool(t *testing.T) {
	// Only 100 is ever free on the (simulated) cluster, so once it is reserved
	// locally, every other candidate in the attempt window is rejected and
	// Allocate must exhaust its attempts rather than wander off to a
	// different free id - that would defeat the point of testing Release.
	checkID := func(_ context.Context, vmid int) (bool, error) {
		return vmid == 100, nil
	}
	nextID := func(context.Context) (int, error) {
		return 100, nil
	}

	allocator := newVMIDAllocator(checkID, nextID)

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
	}, func(context.Context) (int, error) {
		return 100, nil
	})

	_, err := allocator.Allocate(context.Background())
	require.ErrorIs(t, err, wantErr)
}

// TestVMIDAllocator_rangeAllocateNoReissueBeforeWrap is a regression
// test: /cluster/nextid always returns the lowest free ID, so a nextid-mode
// allocator would hand a just-released ID straight back out. Range mode's
// forward-only cursor must not do that until the cursor actually wraps.
func TestVMIDAllocator_rangeAllocateNoReissueBeforeWrap(t *testing.T) {
	allocator := newVMIDAllocatorRange(alwaysFreeCheckID, 100, 103)

	first, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 100, first)

	allocator.Release(first)

	second, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 101, second, "a freed id must not be reissued before the cursor wraps")
}

// TestVMIDAllocator_rangeAllocateWrapsAround proves the cursor comes back to lo
// once it passes hi, and that it picks up the released id on the way round.
func TestVMIDAllocator_rangeAllocateWrapsAround(t *testing.T) {
	allocator := newVMIDAllocatorRange(alwaysFreeCheckID, 100, 102)

	first, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 100, first)

	second, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 101, second)

	allocator.Release(first)

	// The range is fully reserved except 100 (just released); the cursor must
	// wrap past hi=102 back to lo=100 to find it.
	third, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 100, third)
}

// TestVMIDAllocator_rangeAllocateSkipsLocallyReservedIDs proves an id already
// reserved by an earlier, still-outstanding Allocate call is never handed out
// again by a later call, even though checkID alone would report it free (the
// cluster has no idea about a purely local reservation).
func TestVMIDAllocator_rangeAllocateSkipsLocallyReservedIDs(t *testing.T) {
	allocator := newVMIDAllocatorRange(alwaysFreeCheckID, 100, 102)

	first, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 100, first)

	second, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 101, second)

	// Both ids in the range are still reserved (never released): a third call
	// must exhaust rather than reissue either one.
	_, err = allocator.Allocate(context.Background())
	require.ErrorIs(t, err, ErrVMIDExhausted)
}

// TestVMIDAllocator_rangeAllocateExhaustionReturnsErrVMIDExhausted covers a range
// that is entirely reserved locally or taken on the cluster: Allocate must give
// up after one full pass over [lo, hi) rather than looping forever.
func TestVMIDAllocator_rangeAllocateExhaustionReturnsErrVMIDExhausted(t *testing.T) {
	neverFreeCheckID := func(context.Context, int) (bool, error) {
		return false, nil
	}

	allocator := newVMIDAllocatorRange(neverFreeCheckID, 100, 103)

	_, err := allocator.Allocate(context.Background())
	require.ErrorIs(t, err, ErrVMIDExhausted)
}

// TestVMIDAllocator_rangeAllocateSkipsIDsRejectedByCheckID proves candidates
// already present on the cluster (checkID reports taken) are skipped without
// being reserved, and the cursor keeps advancing past them.
func TestVMIDAllocator_rangeAllocateSkipsIDsRejectedByCheckID(t *testing.T) {
	checkID := func(_ context.Context, vmid int) (bool, error) {
		// 100 already exists on the cluster outside this allocator's bookkeeping.
		return vmid != 100, nil
	}

	allocator := newVMIDAllocatorRange(checkID, 100, 103)

	vmid, err := allocator.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 101, vmid)

	_, taken := allocator.reserved[100]
	require.False(t, taken, "a checkID-rejected id must not be reserved")
}

func TestVMIDAllocator_rangeAllocatePropagatesCheckIDError(t *testing.T) {
	wantErr := errors.New("checkid unavailable")

	allocator := newVMIDAllocatorRange(func(context.Context, int) (bool, error) {
		return false, wantErr
	}, 100, 103)

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

			allocator := newVMIDAllocator(checkID, func(context.Context) (int, error) {
				return 0, errors.New("nextID should not be called by ReleaseIfFree")
			})

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
