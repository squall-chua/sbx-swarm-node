package apiserver

import (
	"context"
	"testing"
	"time"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/stretchr/testify/require"
)

func TestPublishWork_Branch(t *testing.T) {
	svc, rec, upstream, _ := gitPublishFixture(t)
	svc.publishTimeout = 10 * time.Second

	res, err := svc.PublishWork(context.Background(), &sbxv1.PublishWorkRequest{Id: rec.ID, Strategy: "branch", Target: "feature-x"})
	require.NoError(t, err)
	require.Equal(t, "refs/heads/feature-x", res.Ref)
	require.Empty(t, res.DeliveryUrl)
	require.True(t, upstreamHasBranch(t, upstream, "feature-x"))
}
