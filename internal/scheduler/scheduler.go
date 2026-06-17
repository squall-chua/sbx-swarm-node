// Package scheduler performs constraint-based placement: filter by hard
// predicates (Placement constraints), score survivors by dominant-resource
// ratio over CPU/mem/disk, break ties by hash(requestID ⊕ nodeID) (ADR-0007).
package scheduler

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
)

// ErrNoEligibleNode means no candidate passed every Placement constraint.
var ErrNoEligibleNode = errors.New("no eligible node")

// Candidate is a node's schedulable view (self from local capacity, peers from gossip).
// Units: CPU cores, memory KB, disk GB.
type Candidate struct {
	NodeID       string
	Workspaces   map[string]bool
	Templates    map[string]bool
	Capabilities map[string]bool
	Labels       map[string]string
	LimitCPU     float64
	LimitMem     float64
	LimitDisk    float64
	AllocCPU     float64
	AllocMem     float64
	AllocDisk    float64
	Sandboxes    int
	Cordoned     bool
}

// Request is a provision request's scheduling constraints. Units match Candidate.
type Request struct {
	CPU, Mem, Disk float64
	Workspaces     []string
	Template       string
	Capabilities   []string
	Affinity       map[string]string
	AntiAffinity   map[string]string
	Strategy       string // least-loaded(default)|bin-pack|spread
	RequestID      string
}

// Schedule returns eligible node ids best-first.
func Schedule(req Request, cands []Candidate) ([]string, error) {
	var ok []Candidate
	for _, c := range cands {
		if fits(req, c) {
			ok = append(ok, c)
		}
	}
	if len(ok) == 0 {
		return nil, ErrNoEligibleNode
	}
	sort.SliceStable(ok, func(i, j int) bool {
		si, sj := score(req, ok[i]), score(req, ok[j])
		if si != sj {
			if req.Strategy == "bin-pack" {
				return si > sj // fuller first
			}
			return si < sj // least-loaded / spread: lighter first
		}
		return tie(req.RequestID, ok[i].NodeID) < tie(req.RequestID, ok[j].NodeID)
	})
	out := make([]string, len(ok))
	for i, c := range ok {
		out[i] = c.NodeID
	}
	return out, nil
}

func fits(req Request, c Candidate) bool {
	if c.Cordoned {
		return false
	}
	for _, w := range req.Workspaces {
		if !c.Workspaces[w] {
			return false
		}
	}
	if req.Template != "" && !c.Templates[req.Template] {
		return false
	}
	for _, cap := range req.Capabilities {
		if !c.Capabilities[cap] {
			return false
		}
	}
	for k, v := range req.Affinity {
		if c.Labels[k] != v {
			return false
		}
	}
	for k, v := range req.AntiAffinity {
		if c.Labels[k] == v {
			return false
		}
	}
	return capFits(c.AllocCPU+req.CPU, c.LimitCPU) &&
		capFits(c.AllocMem+req.Mem, c.LimitMem) &&
		capFits(c.AllocDisk+req.Disk, c.LimitDisk)
}

// capFits reports whether used ≤ limit; a 0 limit is non-binding (unknown/unlimited).
func capFits(used, limit float64) bool { return limit == 0 || used <= limit }

// score is the post-placement dominant-resource ratio (or sandbox count for spread).
func score(req Request, c Candidate) float64 {
	if req.Strategy == "spread" {
		return float64(c.Sandboxes)
	}
	return max3(
		ratio(c.AllocCPU+req.CPU, c.LimitCPU),
		ratio(c.AllocMem+req.Mem, c.LimitMem),
		ratio(c.AllocDisk+req.Disk, c.LimitDisk),
	)
}

func max3(a, b, c float64) float64 {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}

// ratio is used/limit; an unknown (0) limit sorts as fully loaded.
func ratio(a, b float64) float64 {
	if b == 0 {
		return 1
	}
	return a / b
}

func tie(requestID, nodeID string) uint64 {
	h := sha256.Sum256([]byte(requestID + "\x00" + nodeID))
	return binary.BigEndian.Uint64(h[:8])
}
