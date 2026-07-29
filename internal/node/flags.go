package node

import (
	"encoding/json"
	"log/slog"

	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

const (
	flagsBucket = "node"
	flagsKey    = "flags"
)

// nodeFlags is the operator state that must outlive the process. A restart used
// to discard it, which silently put a cordoned node back into service across the
// whole swarm (ADR-0023).
type nodeFlags struct {
	Cordoned bool `json:"cordoned"`
	Draining bool `json:"draining"`
}

// loadNodeFlags reads the stored flags. A missing value is the normal case on a
// fresh node and reads as all-false.
//
// A read error also falls back to all-false, which un-cordons the node — the
// very failure this feature exists to stop. It is logged at error level rather
// than failing the boot: a node that refuses to start is worse than one that
// starts uncordoned and says so loudly.
func loadNodeFlags(st *store.Store, log *slog.Logger) nodeFlags {
	raw, ok, err := st.Get(flagsBucket, flagsKey)
	if err != nil {
		log.Error("node flags unreadable, starting UNCORDONED", "err", err)
		return nodeFlags{}
	}
	if !ok {
		return nodeFlags{}
	}
	var f nodeFlags
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Error("node flags corrupt, starting UNCORDONED", "err", err)
		return nodeFlags{}
	}
	return f
}

// saveNodeFlags persists the flags. Best effort: a write failure is logged and
// the RPC still succeeds, because refusing an operator's cordon because the disk
// is unhappy would be worse than losing it on the next restart.
func saveNodeFlags(st *store.Store, log *slog.Logger, f nodeFlags) {
	raw, err := json.Marshal(f)
	if err != nil { // unreachable for two bools; kept so the error is never dropped silently
		log.Error("node flags marshal failed", "err", err)
		return
	}
	if err := st.Put(flagsBucket, flagsKey, raw); err != nil {
		log.Error("node flags not saved; a restart will lose this state", "err", err)
	}
}
