package dos_test

import (
	"io"
	"testing"

	"github.com/telepresenceio/telepresence/v2/pkg/dos/aferofs"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"

	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/pkg/dos"
)

func TestWithFS(t *testing.T) {
	appFS := afero.NewMemMapFs()
	cData := []byte("file c\n")
	dData := []byte("file d\n")
	require.NoError(t, appFS.MkdirAll("a/b", 0755))
	require.NoError(t, afero.WriteFile(appFS, "/a/b/c.txt", cData, 0644))
	require.NoError(t, afero.WriteFile(appFS, "/a/d.txt", dData, 0644))

	ctx := dos.WithFS(dlog.NewTestContext(t, false), aferofs.Wrap(appFS))

	data, err := dos.ReadFile(ctx, "/a/b/c.txt")
	require.NoError(t, err)
	require.Equal(t, cData, data)

	f, err := dos.Open(ctx, "/a/d.txt")
	require.NoError(t, err)
	data, err = io.ReadAll(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, dData, data)
}
