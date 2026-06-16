package apiserver

import (
	"context"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/stretchr/testify/require"
)

func TestNodeService_GetNodeInfo(t *testing.T) {
	svc := NewNodeService("node-abc", "alpha", "v9")
	out, err := svc.GetNodeInfo(context.Background(), &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "node-abc", out.NodeId)
	require.Equal(t, "alpha", out.NodeName)
	require.Equal(t, "v9", out.Version)
}
