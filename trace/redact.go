package trace

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/mindfire/zygote/journal"
	"github.com/mindfire/zygote/pkg/redact"
	"github.com/mindfire/zygote/vfs"
	"lukechampine.com/blake3"
)

func hashBytes(b []byte) vfs.Hash {
	h := blake3.Sum256(b)
	return vfs.Hash(h)
}

// RedactBundle takes an input bundle and applies the policy. It returns a new
// bundle with all redactions applied and VFS hashes re-derived.
func RedactBundle(in *Bundle, policy *redact.Policy) (*Bundle, error) {
	eng, err := redact.NewEngine(policy)
	if err != nil {
		return nil, err
	}

	outStore := vfs.NewMemStore()
	hasRawPrompts := false
	var newEffects []journal.Entry

	for i, entry := range in.Recording.Effects {
		value, err := checkEffect(in.Store, entry)
		if err != nil {
			return nil, fmt.Errorf("redaction failed (fail-closed): %w", err)
		}

		res, err := eng.Process(fmt.Sprintf("effect-%d", i), value)
		if err != nil {
			return nil, err
		}

		if entry.Op == "model" && !res.DigestOnly {
			hasRawPrompts = true
		}

		newEntry := entry
		newEntry.ValueRef = ""
		newEntry.ValueSize = 0
		if res.DigestOnly {
			h := hashBytes(value)
			newEntry.Value = []byte(hex.EncodeToString(h[:]))
			newEntry.Chunks = nil
		} else {
			newEntry.Value = res.Content
			if !bytes.Equal(res.Content, value) {
				newEntry.Chunks = nil
			}
		}
		newEffects = append(newEffects, newEntry)
	}

	var newSteps []Step
	memo := make(map[vfs.Hash]vfs.Hash)
	memo[vfs.EmptyTreeHash] = vfs.EmptyTreeHash

	var redactTree func(hash vfs.Hash, currentPath string) (vfs.Hash, error)
	redactTree = func(hash vfs.Hash, currentPath string) (vfs.Hash, error) {
		if newH, ok := memo[hash]; ok {
			return newH, nil
		}

		obj, ok := in.Store.Get(hash)
		if !ok {
			return vfs.Hash{}, fmt.Errorf("object not found: %x", hash[:])
		}

		tree, err := vfs.DecodeTree(obj)
		if err != nil {
			return vfs.Hash{}, err
		}

		var newTree vfs.Tree
		for _, entry := range tree {
			fullPath := filepath.Join(currentPath, entry.Name)
			if entry.Kind == vfs.KindDir {
				newDirHash, err := redactTree(entry.Hash, fullPath)
				if err != nil {
					return vfs.Hash{}, err
				}
				newEntry := entry
				newEntry.Hash = newDirHash
				newTree = append(newTree, newEntry)
			} else {
				fileBytes, ok := in.Store.Get(entry.Hash)
				if !ok {
					return vfs.Hash{}, fmt.Errorf("file object not found: %x", entry.Hash[:])
				}

				res, err := eng.Process(fullPath, fileBytes)
				if err != nil {
					return vfs.Hash{}, err
				}

				var newBytes []byte
				if res.DigestOnly {
					h := hashBytes(fileBytes)
					newBytes = []byte(hex.EncodeToString(h[:]))
				} else {
					newBytes = res.Content
				}

				newFileHash, err := outStore.Put(newBytes)
				if err != nil {
					return vfs.Hash{}, err
				}

				newEntry := entry
				newEntry.Hash = newFileHash
				newEntry.Size = int64(len(newBytes))
				newTree = append(newTree, newEntry)
			}
		}

		newTree.Sort()
		encoded := newTree.Encode()
		newRootHash, err := outStore.Put(encoded)
		if err != nil {
			return vfs.Hash{}, err
		}

		memo[hash] = newRootHash
		return newRootHash, nil
	}

	for _, step := range in.Recording.Steps {
		decoded, err := hex.DecodeString(step.Root)
		if err != nil || len(decoded) != 32 {
			return nil, fmt.Errorf("invalid root hash hex: %s", step.Root)
		}
		var oldRoot vfs.Hash
		copy(oldRoot[:], decoded)

		newRoot, err := redactTree(oldRoot, "")
		if err != nil {
			return nil, err
		}

		newStep := step
		newStep.Root = hex.EncodeToString(newRoot[:])
		newSteps = append(newSteps, newStep)
	}

	out := &Bundle{
		Recording: Recording{
			Version:       in.Recording.Version,
			Meta:          in.Recording.Meta,
			Steps:         newSteps,
			Effects:       newEffects,
			HasRawPrompts: hasRawPrompts,
		},
		Store: outStore,
	}
	return out, nil
}
