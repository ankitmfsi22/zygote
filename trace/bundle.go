// Package trace provides trace.
package trace

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/mindfire/zygote/vfs"
	"lukechampine.com/blake3"
)

var (
	// ErrHashMismatch indicates mismatch.
	ErrHashMismatch = errors.New("corrupt bundle: object content does not match its hash filename")
	// ErrMissingObject indicates missing object.
	ErrMissingObject = errors.New("corrupt bundle: recording relies on objects not present in the bundle")
)

// ExportFile writes a highly portable zip bundle containing the Recording manifest
// and all reachable cryptographically hashed vfs objects.
func ExportFile(filename string, store vfs.Store, rec Recording, opts ...ExportOption) error {
	var cfg exportConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	effects, effectObjects, err := spillEffects(store, rec.Effects, cfg.limit())
	if err != nil {
		return err
	}
	rec.Effects = effects

	f, err := os.Create(filename) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	zw := zip.NewWriter(f)

	// 1. Write canonical JSON manifest
	manifestFile, err := zw.Create("trace.json")
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ") // RFC 8785 canonicalization happens at signing M4
	if err != nil {
		return err
	}
	if _, err := manifestFile.Write(b); err != nil {
		return err
	}

	// 2. Identify all reachable objects
	var snaps []vfs.Snapshot
	for _, step := range rec.Steps {
		var h vfs.Hash
		hb, err := hex.DecodeString(step.Root)
		if err != nil || len(hb) != 32 {
			continue
		}
		copy(h[:], hb)
		snaps = append(snaps, vfs.Snapshot{Root: h})
	}

	reachable, err := vfs.Reachable(context.Background(), store, snaps)
	if err != nil {
		return err
	}

	for hash := range effectObjects {
		reachable[hash] = struct{}{}
	}
	hashes := make([]vfs.Hash, 0, len(reachable))
	for hash := range reachable {
		hashes = append(hashes, hash)
	}
	slices.SortFunc(hashes, func(a, b vfs.Hash) int { return bytes.Compare(a[:], b[:]) })

	// 3. Write objects
	for _, hash := range hashes {
		if hash == vfs.EmptyTreeHash {
			continue
		}
		data, ok := effectObjects[hash]
		if !ok {
			data, ok = store.Get(hash)
		}
		if !ok {
			return fmt.Errorf("missing object in store: %x", hash)
		}

		objFile, err := zw.Create("objects/" + hex.EncodeToString(hash[:]))
		if err != nil {
			return err
		}
		if _, err := objFile.Write(data); err != nil {
			return err
		}
	}

	return zw.Close()
}

// ImportFile reads a zip bundle, cryptographically validates every object's
// integrity, and ensures the manifest is fully loadable before returning it.
func ImportFile(filename string, store vfs.Store) (Bundle, error) {
	r, err := zip.OpenReader(filename)
	if err != nil {
		return Bundle{}, err
	}
	defer r.Close() //nolint:errcheck

	var rec Recording

	// 1. Read objects and verify integrity (FR-4.3)
	for _, f := range r.File {
		if f.Name == "trace.json" {
			rc, err := f.Open()
			if err != nil {
				return Bundle{}, err
			}
			dec := json.NewDecoder(rc)
			err = dec.Decode(&rec)
			_ = rc.Close()
			if err != nil {
				return Bundle{}, err
			}
			if rec.Version != 1 {
				return Bundle{}, fmt.Errorf("unsupported format version: %d", rec.Version)
			}
			continue
		}

		if strings.HasPrefix(f.Name, "objects/") {
			hexName := strings.TrimPrefix(f.Name, "objects/")
			expectedHash, err := hex.DecodeString(hexName)
			if err != nil || len(expectedHash) != 32 {
				continue
			}

			rc, err := f.Open()
			if err != nil {
				return Bundle{}, err
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return Bundle{}, err
			}

			actualHash := blake3.Sum256(data)
			if !bytes.Equal(actualHash[:], expectedHash) {
				return Bundle{}, ErrHashMismatch
			}

			if _, err := store.Put(data); err != nil {
				return Bundle{}, err
			}
		}
	}

	if err := validateEffects(store, rec.Effects); err != nil {
		return Bundle{}, err
	}

	// 2. Validate reachability (FR-4.4)
	for _, step := range rec.Steps {
		var h vfs.Hash
		hb, err := hex.DecodeString(step.Root)
		if err == nil && len(hb) == 32 {
			copy(h[:], hb)
			if err := vfs.Verify(context.Background(), store, vfs.Snapshot{Root: h}); err != nil {
				return Bundle{}, ErrMissingObject
			}
		}
	}

	return Bundle{Recording: rec, Store: store}, nil
}
