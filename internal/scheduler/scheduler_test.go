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

func TestSchedule_LeastActualLoad(t *testing.T) {
	cands := []Candidate{
		{NodeID: "A", LimitCPU: 10, LimitMem: 10, LimitDisk: 10, ActualCPU: 0.8, ActualMem: 0.1},
		{NodeID: "B", LimitCPU: 10, LimitMem: 10, LimitDisk: 10, ActualCPU: 0.2, ActualMem: 0.1},
	}
	req := Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "least-actual-load", RequestID: "r"}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, []string{"B", "A"}, order) // lower actual util first
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

func TestPullable(t *testing.T) {
	cases := map[string]bool{
		"ghcr.io/org/img:1":    true,  // registry host: has a dot
		"localhost:5000/img:1": true,  // registry host: localhost
		"registry:5000/img:1":  true,  // registry host: has a colon
		"myimage:v1":           false, // bare tag: only where it was saved
		"org/img:1":            false, // Docker Hub shorthand, deliberately bare
		"alpine":               false,
	}
	for ref, want := range cases {
		require.Equal(t, want, pullable(ref), ref)
	}
}

func TestSchedule_RegistryTemplatePlacesOnANodeWithoutIt(t *testing.T) {
	c := Candidate{NodeID: "n1", LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100}
	// c.Templates is empty: this node holds nothing.
	got, err := Schedule(Request{Template: "ghcr.io/org/img:1", CPU: 1, RequestID: "r"}, []Candidate{c})
	require.NoError(t, err)
	require.Equal(t, []string{"n1"}, got)
}

func TestSchedule_BareTemplateStillFiltered(t *testing.T) {
	c := Candidate{NodeID: "n1", LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100}
	_, err := Schedule(Request{Template: "myimage:v1", CPU: 1, RequestID: "r"}, []Candidate{c})
	require.ErrorIs(t, err, ErrNoEligibleNode)
}

func TestSchedule_HolderBeatsTheEntryNodeOnATie(t *testing.T) {
	// Two identical, unloaded nodes. n2 holds the image; n1 is the entry node.
	mk := func(id string, holds bool) Candidate {
		c := Candidate{NodeID: id, LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100}
		if holds {
			c.Templates = map[string]bool{"ghcr.io/org/img:1": true}
		}
		return c
	}
	req := Request{Template: "ghcr.io/org/img:1", CPU: 1, RequestID: "r", Local: "n1"}
	got, err := Schedule(req, []Candidate{mk("n1", false), mk("n2", true)})
	require.NoError(t, err)
	require.Equal(t, "n2", got[0], "a node holding the image must win over the entry node")
}

// The holder tie-break sits after the score comparison, so it must win a tie
// under every ranking strategy, not just the default. bin-pack, spread and
// least-actual-load all score both nodes equal here, so each one reaches the
// same tie-break as TestSchedule_HolderBeatsTheEntryNodeOnATie.
func TestSchedule_HolderBeatsTheEntryNodeOnATie_BinPack(t *testing.T) {
	mk := func(id string, holds bool) Candidate {
		c := Candidate{NodeID: id, LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100}
		if holds {
			c.Templates = map[string]bool{"ghcr.io/org/img:1": true}
		}
		return c
	}
	req := Request{Template: "ghcr.io/org/img:1", CPU: 1, RequestID: "r", Local: "n1", Strategy: "bin-pack"}
	got, err := Schedule(req, []Candidate{mk("n1", false), mk("n2", true)})
	require.NoError(t, err)
	require.Equal(t, "n2", got[0], "a node holding the image must win over the entry node under bin-pack")
}

func TestSchedule_HolderBeatsTheEntryNodeOnATie_Spread(t *testing.T) {
	mk := func(id string, holds bool) Candidate {
		c := Candidate{NodeID: id, LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100}
		if holds {
			c.Templates = map[string]bool{"ghcr.io/org/img:1": true}
		}
		return c
	}
	req := Request{Template: "ghcr.io/org/img:1", CPU: 1, RequestID: "r", Local: "n1", Strategy: "spread"}
	got, err := Schedule(req, []Candidate{mk("n1", false), mk("n2", true)})
	require.NoError(t, err)
	require.Equal(t, "n2", got[0], "a node holding the image must win over the entry node under spread")
}

func TestSchedule_HolderBeatsTheEntryNodeOnATie_LeastActualLoad(t *testing.T) {
	mk := func(id string, holds bool) Candidate {
		c := Candidate{NodeID: id, LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100}
		if holds {
			c.Templates = map[string]bool{"ghcr.io/org/img:1": true}
		}
		return c
	}
	req := Request{Template: "ghcr.io/org/img:1", CPU: 1, RequestID: "r", Local: "n1", Strategy: "least-actual-load"}
	got, err := Schedule(req, []Candidate{mk("n1", false), mk("n2", true)})
	require.NoError(t, err)
	require.Equal(t, "n2", got[0], "a node holding the image must win over the entry node under least-actual-load")
}

// Two identical unloaded nodes. n1 advertises the daemon's tagged form of an
// untagged request, so only the canonical lookup finds it.
func TestSchedule_TieBreakMatchesTheCanonicalName(t *testing.T) {
	mk := func(id string, tmpl map[string]bool) Candidate {
		return Candidate{NodeID: id, LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100, Templates: tmpl}
	}
	n1 := mk("n1", map[string]bool{"ghcr.io/org/img:latest": true})
	n2 := mk("n2", nil)

	// RequestID chosen so the hash tie-break (what decides if holds falls back
	// to a raw map read) would rank n2 first, the opposite of the expected
	// n1 — so this test fails if holds stops using the canonical name.
	req := Request{Template: "ghcr.io/org/img", CPU: 1, RequestID: "req-tiebreak"}
	got, err := Schedule(req, []Candidate{n1, n2})
	require.NoError(t, err)
	require.Equal(t, "n1", got[0], "only the canonical lookup finds n1's tagged form")
}

func TestCanonical(t *testing.T) {
	cases := map[string]string{
		"myimage:v1":           "docker.io/library/myimage:v1",     // bare tag, as the daemon reports it
		"org/img:1":            "docker.io/org/img:1",              // Docker Hub shorthand
		"ghcr.io/org/img:1":    "ghcr.io/org/img:1",                // already qualified: unchanged
		"localhost:5000/img:1": "localhost:5000/img:1",             // already qualified: unchanged
		"myimage":              "docker.io/library/myimage:latest", // no tag: Docker defaults to latest
		"localhost:5000/img":   "localhost:5000/img:latest",        // registry port colon is not a tag
	}
	for in, want := range cases {
		require.Equal(t, want, canonical(in), in)
	}
}

// A node that saved "myimage:v1" advertises "docker.io/library/myimage:v1", because the
// daemon canonicalizes an unqualified repository. A request for the bare tag must still
// place there.
func TestSchedule_BareTagMatchesTheCanonicalAdvertisedName(t *testing.T) {
	c := Candidate{NodeID: "n1", LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100,
		Templates: map[string]bool{"docker.io/library/myimage:v1": true}}
	got, err := Schedule(Request{Template: "myimage:v1", CPU: 1, RequestID: "r"}, []Candidate{c})
	require.NoError(t, err)
	require.Equal(t, []string{"n1"}, got)
}

// A bare tag still does not travel: a node that holds nothing is not eligible.
func TestSchedule_BareTagStillDoesNotTravel(t *testing.T) {
	c := Candidate{NodeID: "n1", LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100}
	// c.Templates is empty: this node holds nothing.
	_, err := Schedule(Request{Template: "myimage:v1", CPU: 1, RequestID: "r"}, []Candidate{c})
	require.ErrorIs(t, err, ErrNoEligibleNode)
}

func TestSchedule_KitFilter(t *testing.T) {
	has := Candidate{NodeID: "a", LimitCPU: 4, LimitMem: 4_000_000, LimitDisk: 100,
		Kits: map[string]bool{"tools": true}}
	lacks := Candidate{NodeID: "b", LimitCPU: 4, LimitMem: 4_000_000, LimitDisk: 100,
		Kits: map[string]bool{"other": true}}

	if _, err := Schedule(Request{CPU: 1, Mem: 1, Disk: 1, Kits: []string{"tools"}, RequestID: "r"}, []Candidate{lacks}); err == nil {
		t.Fatal("a node without the kit must not be a candidate")
	}

	order, err := Schedule(Request{CPU: 1, Mem: 1, Disk: 1, Kits: []string{"tools"}, RequestID: "r"}, []Candidate{lacks, has})
	if err != nil {
		t.Fatalf("want a placement, got %v", err)
	}
	if order[0] != "a" {
		t.Fatalf("want node a, got %v", order)
	}
}
