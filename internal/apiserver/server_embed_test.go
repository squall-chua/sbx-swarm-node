package apiserver

import (
	"io/fs"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/web"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedSPA_HasIndex(t *testing.T) {
	_, err := fs.Stat(web.FS(), "index.html")
	require.NoError(t, err, "run web/scripts/build.sh to produce web/dist before building the binary")
}
