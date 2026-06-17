// Package coordinator places provision requests: it scores candidates and
// attempts them in order, retrying on a target NACK (admission failure).
package coordinator

import (
	"context"
	"errors"

	"github.com/squall-chua/sbx-swarm-node/internal/scheduler"
)

// ErrNack is returned by an attempt when the target refuses admission.
var ErrNack = errors.New("target nacked")

// ErrNoCapacity means every eligible candidate nacked.
var ErrNoCapacity = errors.New("no node accepted the provision")

// AttemptFunc provisions on a node, returning the new sandbox id or ErrNack.
// It is supplied per request so it can capture that request's create spec.
type AttemptFunc func(ctx context.Context, nodeID string) (sandboxID string, err error)

// Coordinator places provisions over the current candidate view.
type Coordinator struct {
	candidates func() []scheduler.Candidate
}

// New builds a coordinator over a candidate-view function.
func New(candidates func() []scheduler.Candidate) *Coordinator {
	return &Coordinator{candidates: candidates}
}

// Provision runs the scheduler and tries candidates best-first until one
// accepts, returning the sandbox id. ErrNoEligibleNode passes through; all-NACK
// becomes ErrNoCapacity; any non-NACK attempt error is surfaced immediately.
func (c *Coordinator) Provision(ctx context.Context, req scheduler.Request, attempt AttemptFunc) (string, error) {
	order, err := scheduler.Schedule(req, c.candidates())
	if err != nil {
		return "", err
	}
	for _, nodeID := range order {
		sbID, aerr := attempt(ctx, nodeID)
		if aerr == nil {
			return sbID, nil
		}
		if !errors.Is(aerr, ErrNack) {
			return "", aerr
		}
	}
	return "", ErrNoCapacity
}
