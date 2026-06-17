package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func cand(id string, cpuLim, cpuAlloc, memLim, memAlloc, diskLim, diskAlloc float64, ws ...string) Candidate {
	m := map[string]bool{}
	for _, w := range ws {
		m[w] = true
	}
	return Candidate{
		NodeID: id, Workspaces: m,
		LimitCPU: cpuLim, AllocCPU: cpuAlloc,
		LimitMem: memLim, AllocMem: memAlloc,
		LimitDisk: diskLim, AllocDisk: diskAlloc,
	}
}

func TestSchedule_FiltersWorkspaceAndCapacity(t *testing.T) {
	req := Request{CPU: 2, Mem: 4, Disk: 1, Workspaces: []string{"repo-foo"}, Strategy: "least-loaded", RequestID: "r1"}
	cands := []Candidate{
		cand("A", 8, 6, 16, 11, 100, 10, "repo-foo", "data"), // eligible, loaded
		cand("B", 16, 1, 32, 1, 100, 1, "repo-bar"),          // missing workspace -> filtered
		cand("C", 16, 4, 32, 6, 100, 5, "repo-foo"),          // eligible, light
	}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, []string{"C", "A"}, order) // least-loaded: C before A; B excluded
}

func TestSchedule_DiskIsDominant(t *testing.T) {
	// A is light on cpu/mem but nearly full on disk; B is the opposite. The
	// dominant-resource max() must pick A as the more-loaded node.
	req := Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "least-loaded", RequestID: "r"}
	cands := []Candidate{
		cand("A", 100, 1, 100, 1, 10, 9), // disk ratio (9+1)/10 = 1.0  -> dominant
		cand("B", 100, 50, 100, 50, 100, 1),
	}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, "B", order[0]) // B less dominant-loaded
}

func TestSchedule_NoEligibleNode(t *testing.T) {
	req := Request{CPU: 100, Mem: 1, Disk: 1, Strategy: "least-loaded", RequestID: "r"}
	_, err := Schedule(req, []Candidate{cand("A", 8, 0, 16, 0, 100, 0)})
	require.ErrorIs(t, err, ErrNoEligibleNode)
}

func TestSchedule_CordonedExcluded(t *testing.T) {
	c := cand("A", 8, 0, 16, 0, 100, 0)
	c.Cordoned = true
	_, err := Schedule(Request{CPU: 1, Mem: 1, Disk: 1, RequestID: "r"}, []Candidate{c})
	require.ErrorIs(t, err, ErrNoEligibleNode)
}

func TestSchedule_BinPackPrefersFuller(t *testing.T) {
	req := Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "bin-pack", RequestID: "r"}
	cands := []Candidate{cand("A", 4, 3, 4, 3, 4, 3), cand("C", 4, 0, 4, 0, 4, 0)}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, "A", order[0]) // fuller node first
}

func TestSchedule_CapabilityAndTemplateFilter(t *testing.T) {
	c := Candidate{NodeID: "A", LimitCPU: 8, LimitMem: 8, LimitDisk: 8,
		Capabilities: map[string]bool{"clone": true}, Templates: map[string]bool{"base:1": true}}
	// needs a template the node lacks
	_, err := Schedule(Request{CPU: 1, Mem: 1, Disk: 1, Template: "other:1", RequestID: "r"}, []Candidate{c})
	require.ErrorIs(t, err, ErrNoEligibleNode)
	// needs a capability the node lacks
	_, err = Schedule(Request{CPU: 1, Mem: 1, Disk: 1, Capabilities: []string{"gpu"}, RequestID: "r"}, []Candidate{c})
	require.ErrorIs(t, err, ErrNoEligibleNode)
	// both satisfied
	order, err := Schedule(Request{CPU: 1, Mem: 1, Disk: 1, Template: "base:1", Capabilities: []string{"clone"}, RequestID: "r"}, []Candidate{c})
	require.NoError(t, err)
	require.Equal(t, []string{"A"}, order)
}

func TestSchedule_TieBreakDeterministicAcrossCalls(t *testing.T) {
	req := Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "least-loaded", RequestID: "same"}
	cands := []Candidate{cand("A", 10, 0, 10, 0, 10, 0), cand("B", 10, 0, 10, 0, 10, 0)}
	o1, _ := Schedule(req, cands)
	o2, _ := Schedule(req, cands)
	require.Equal(t, o1, o2) // hash(requestID ⊕ nodeID) is stable
}

func TestSchedule_PrefersLocalOnTie(t *testing.T) {
	// A and B are equally unloaded -> score tie. The local (entry) node wins,
	// so an unconstrained create stays where it was requested.
	req := Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "least-loaded", RequestID: "r", Local: "B"}
	cands := []Candidate{cand("A", 10, 0, 10, 0, 10, 0), cand("B", 10, 0, 10, 0, 10, 0)}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, "B", order[0]) // local B preferred over A on the tie
}

func TestSchedule_LoadedLocalStillOffloads(t *testing.T) {
	// Local B is heavily loaded; A is lighter, so A wins on score despite the
	// locality bias (which only breaks exact ties).
	req := Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "least-loaded", RequestID: "r", Local: "B"}
	cands := []Candidate{cand("A", 10, 0, 10, 0, 10, 0), cand("B", 10, 8, 10, 8, 10, 8)}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, "A", order[0]) // lighter peer beats the loaded local node
}

func TestSchedule_NodeAffinityFiltersByLabel(t *testing.T) {
	cands := []Candidate{
		{NodeID: "A", Labels: map[string]string{"zone": "us"}, LimitCPU: 10, LimitMem: 10, LimitDisk: 10},
		{NodeID: "B", Labels: map[string]string{"zone": "eu"}, LimitCPU: 10, LimitMem: 10, LimitDisk: 10},
	}
	req := Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "least-loaded", RequestID: "r",
		Affinity: map[string]string{"zone": "eu"}}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, []string{"B"}, order) // only the eu node is eligible

	req.Affinity = nil
	req.AntiAffinity = map[string]string{"zone": "eu"}
	order, err = Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, []string{"A"}, order) // eu excluded
}
