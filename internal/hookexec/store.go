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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrStateCorrupted is returned (wrapped) by Store.Load when the persisted
// state cannot be decoded. Callers must treat this as "progress unknown"
// and fall back to a full re-run with a visible warning; the corrupted
// state must never silently skip hooks.
var ErrStateCorrupted = errors.New("hook execution state is corrupted")

// Store persists hook execution state independently of cluster resources.
type Store interface {
	// Load returns the stored state, or an empty state when none exists.
	Load(ctx context.Context) (*State, error)
	// Save atomically persists the state.
	Save(ctx context.Context, state *State) error
}

// FileStore is a Store backed by a single JSON file per hook sequence.
type FileStore struct {
	path string
}

// NewFileStore returns a FileStore rooted at dir, namespaced by release,
// event and revision. dir is typically under helmpath.DataPath().
func NewFileStore(dir, releaseName, event string, revision int) *FileStore {
	sanitize := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
				r == '-', r == '_', r == '.':
				return r
			}
			return '_'
		}, s)
	}
	name := fmt.Sprintf("%s-%s-rev%d.json", sanitize(releaseName), sanitize(event), revision)
	return &FileStore{path: filepath.Join(dir, "hookexec", name)}
}

// Path returns the backing file path.
func (s *FileStore) Path() string { return s.path }

// Load implements Store.
func (s *FileStore) Load(_ context.Context) (*State, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateCorrupted, err)
	}
	if state.Records == nil {
		return nil, fmt.Errorf("%w: missing records", ErrStateCorrupted)
	}
	return &state, nil
}

// Save implements Store. The write is atomic (temp file + rename) so a
// crashed process can never leave a half-written state file behind; a
// crash either keeps the previous state or the new one.
func (s *FileStore) Save(_ context.Context, state *State) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".hookexec-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
