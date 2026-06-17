package sandbox

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCapacity_TryReserveAdmitRelease(t *testing.T) {
	c := NewCapacity(4, 8, 10) // 4 cores, 8 KB, 10 GB

	id1, ok := c.TryReserve(2, 4, 5)
	require.True(t, ok)
	_, ok = c.TryReserve(3, 1, 1) // only 2 cores left
	require.False(t, ok)
	id2, ok := c.TryReserve(2, 4, 5)
	require.True(t, ok)

	c.Release(id1)
	c.Release(id2)
	_, ok = c.TryReserve(4, 8, 10)
	require.True(t, ok)
}

func TestCapacity_CommitMovesReservationToBase(t *testing.T) {
	c := NewCapacity(4, 8, 10)
	id, ok := c.TryReserve(2, 4, 5)
	require.True(t, ok)
	c.Commit(id) // create succeeded: reservation becomes base

	cpu, mem, disk := c.Snapshot()
	require.Equal(t, 2.0, cpu)
	require.Equal(t, 4.0, mem)
	require.Equal(t, 5.0, disk)
	// committed load still counts against the limit
	_, ok = c.TryReserve(3, 1, 1)
	require.False(t, ok)
}

func TestCapacity_SetBaseFromRecords(t *testing.T) {
	c := NewCapacity(4, 8, 10)
	c.SetBase(3, 6, 9) // reconciled from List()
	_, ok := c.TryReserve(2, 1, 1)
	require.False(t, ok)
	_, ok = c.TryReserve(1, 2, 1)
	require.True(t, ok)
}

func TestCapacity_ZeroLimitIsUnlimited(t *testing.T) {
	c := NewCapacity(0, 0, 0) // detection-failed / standalone
	_, ok := c.TryReserve(1e9, 1e9, 1e9)
	require.True(t, ok)
}

func TestCapacity_CommitBaseSetsAbsoluteAndReleases(t *testing.T) {
	c := NewCapacity(10, 1e9, 1e9)
	id, ok := c.TryReserve(3, 0, 0)
	require.True(t, ok)
	// Resync base absolutely (e.g. records total 3 cpu) and drop the reservation.
	c.CommitBase(3, 0, 0, id)
	cpu, _, _ := c.Snapshot()
	require.Equal(t, 3.0, cpu) // exactly 3 (not 3 reservation + 3 base = 6)
}

func TestCapacity_TryReserveAtomicUnderRace(t *testing.T) {
	c := NewCapacity(10, 1e9, 1e9) // exactly 5 of size-2 cpu fit
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := c.TryReserve(2, 0, 0); ok {
				mu.Lock()
				got++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 5, got) // never over-admits
}
