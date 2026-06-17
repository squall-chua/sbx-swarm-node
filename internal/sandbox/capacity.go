package sandbox

import (
	"runtime"
	"sync"
)

// Capacity tracks soft, in-memory CPU/mem/disk accounting against a per-node
// provision limit. Units: CPU cores, memory KB, disk GB. The durable truth is
// Manager.List(); SetBase is called by reconcile. A 0 limit is non-binding
// (unlimited / detection-failed). Admission is a single atomic op (TryReserve)
// to avoid a check-then-reserve TOCTOU race.
type Capacity struct {
	mu                            sync.Mutex
	limitCPU, limitMem, limitDisk float64
	baseCPU, baseMem, baseDisk    float64
	resv                          map[int]reservation
	next                          int
}

type reservation struct{ cpu, mem, disk float64 }

// NewCapacity builds a tracker with the given (already-resolved) limits.
func NewCapacity(limitCPU, limitMem, limitDisk float64) *Capacity {
	return &Capacity{limitCPU: limitCPU, limitMem: limitMem, limitDisk: limitDisk, resv: map[int]reservation{}}
}

func (c *Capacity) usedLocked() (cpu, mem, disk float64) {
	cpu, mem, disk = c.baseCPU, c.baseMem, c.baseDisk
	for _, r := range c.resv {
		cpu += r.cpu
		mem += r.mem
		disk += r.disk
	}
	return
}

func fitsLimit(used, limit float64) bool { return limit == 0 || used <= limit }

// TryReserve atomically checks used+req ≤ limit (all three; 0 limit non-binding)
// and reserves. Returns (id, true) on success or (0, false) if it does not fit.
func (c *Capacity) TryReserve(cpu, mem, disk float64) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	uc, um, ud := c.usedLocked()
	if !fitsLimit(uc+cpu, c.limitCPU) || !fitsLimit(um+mem, c.limitMem) || !fitsLimit(ud+disk, c.limitDisk) {
		return 0, false
	}
	id := c.next
	c.next++
	c.resv[id] = reservation{cpu: cpu, mem: mem, disk: disk}
	return id, true
}

// Commit promotes a reservation into the base (create succeeded) and drops it,
// atomically — no double-count, no gap.
func (c *Capacity) Commit(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.resv[id]
	if !ok {
		return
	}
	c.baseCPU += r.cpu
	c.baseMem += r.mem
	c.baseDisk += r.disk
	delete(c.resv, id)
}

// CommitBase sets the absolute base (from reconciled durable records) and drops
// the reservation in one lock hold. Used on create success: the new record is
// reflected in base exactly once — never double-counted with its reservation,
// never dropped — and because it is absolute (like SetBase), a concurrent
// Reconcile cannot double-count it.
func (c *Capacity) CommitBase(cpu, mem, disk float64, id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseCPU, c.baseMem, c.baseDisk = cpu, mem, disk
	delete(c.resv, id)
}

// Release frees a reservation (create failed).
func (c *Capacity) Release(id int) { c.mu.Lock(); delete(c.resv, id); c.mu.Unlock() }

// SetBase sets the reconciled allocation from durable records.
func (c *Capacity) SetBase(cpu, mem, disk float64) {
	c.mu.Lock()
	c.baseCPU, c.baseMem, c.baseDisk = cpu, mem, disk
	c.mu.Unlock()
}

// Limits returns the resolved limits (for gossip advertisement).
func (c *Capacity) Limits() (cpu, mem, disk float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limitCPU, c.limitMem, c.limitDisk
}

// Snapshot returns current allocated cpu/mem/disk (base + reservations).
func (c *Capacity) Snapshot() (cpu, mem, disk float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usedLocked()
}

// numCPU is wrapped here (a build-tag-free file in this package) so the
// build-tagged host-detection files (Task 3) can call it on every platform.
func numCPU() int { return runtime.NumCPU() }
