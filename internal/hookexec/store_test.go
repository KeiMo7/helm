/*
Copyright The Helm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hookexec

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir, "my-release", "pre-upgrade", 3)

	ctx := context.Background()
	state, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Nil(t, state, "missing state file yields nil, not an error")

	saved := newState("my-release", "default", 3, "pre-upgrade")
	saved.Records["k1"] = &Record{Key: "k1", Name: "hook", Phase: PhaseSucceeded}
	require.NoError(t, store.Save(ctx, saved))

	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, "my-release", loaded.Release)
	assert.Equal(t, PhaseSucceeded, loaded.Records["k1"].Phase)
}

func TestFileStoreCorrupted(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir, "rel", "pre-upgrade", 1)

	require.NoError(t, os.MkdirAll(filepath.Dir(store.Path()), 0o755))
	require.NoError(t, os.WriteFile(store.Path(), []byte("{not json"), 0o600))

	_, err := store.Load(context.Background())
	assert.ErrorIs(t, err, ErrStateCorrupted)
}

func TestFileStoreSanitizesNames(t *testing.T) {
	store := NewFileStore(t.TempDir(), "rel/with/slashes", "pre-upgrade", 1)
	assert.NotContains(t, filepath.Base(store.Path()), "/")
}
