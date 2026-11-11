package trace

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/mindfire/zygote/journal"
	"github.com/mindfire/zygote/vfs"
)

// DefaultSpillThreshold is the effect value size in bytes above which export stores the value in the object CAS.
const DefaultSpillThreshold = 32 << 10

// ErrInvalidEffect indicates a malformed effect entry in a recording.
var ErrInvalidEffect = errors.New("corrupt bundle: invalid effect entry")

// ExportOption configures ExportFile.
type ExportOption func(*exportConfig)

type exportConfig struct {
	threshold int
}

// WithSpillThreshold sets the size above which effect values are stored by hash; zero keeps the default and a negative value never spills.
func WithSpillThreshold(n int) ExportOption {
	return func(c *exportConfig) {
		c.threshold = n
	}
}

func (c exportConfig) limit() int {
	switch {
	case c.threshold == 0:
		return DefaultSpillThreshold
	case c.threshold < 0:
		return -1
	default:
		return c.threshold
	}
}

func parseRef(ref string) (vfs.Hash, error) {
	var h vfs.Hash
	if len(ref) != len(h)*2 {
		return h, fmt.Errorf("%w: value_ref %q is not 64 hex characters", ErrInvalidEffect, ref)
	}
	if _, err := hex.Decode(h[:], []byte(ref)); err != nil || hex.EncodeToString(h[:]) != ref {
		return vfs.Hash{}, fmt.Errorf("%w: value_ref %q is not 64 lowercase hex characters", ErrInvalidEffect, ref)
	}
	return h, nil
}

func effectValue(store vfs.Store, e journal.Entry) ([]byte, error) {
	if e.ValueRef == "" {
		return e.Value, nil
	}
	if len(e.Value) > 0 {
		return nil, fmt.Errorf("%w: effect %d carries both value and value_ref", ErrInvalidEffect, e.Seq)
	}
	h, err := parseRef(e.ValueRef)
	if err != nil {
		return nil, fmt.Errorf("effect %d: %w", e.Seq, err)
	}
	if store == nil {
		return nil, fmt.Errorf("effect %d: %w", e.Seq, ErrMissingObject)
	}
	data, ok := store.Get(h)
	if !ok {
		return nil, fmt.Errorf("effect %d: %w", e.Seq, ErrMissingObject)
	}
	if hashBytes(data) != h {
		return nil, fmt.Errorf("effect %d: %w", e.Seq, ErrHashMismatch)
	}
	if e.ValueSize != 0 && e.ValueSize != len(data) {
		return nil, fmt.Errorf("%w: effect %d value_size %d does not match object length %d", ErrInvalidEffect, e.Seq, e.ValueSize, len(data))
	}
	return data, nil
}

func checkEffect(store vfs.Store, e journal.Entry) ([]byte, error) {
	if e.ValueSize < 0 {
		return nil, fmt.Errorf("%w: effect %d has negative value_size", ErrInvalidEffect, e.Seq)
	}
	if e.ValueRef == "" && e.ValueSize != 0 {
		return nil, fmt.Errorf("%w: effect %d has value_size without value_ref", ErrInvalidEffect, e.Seq)
	}
	value, err := effectValue(store, e)
	if err != nil {
		return nil, err
	}
	if err := journal.ValidateChunks(e.Chunks, len(value)); err != nil {
		return nil, fmt.Errorf("%w: effect %d: %v", ErrInvalidEffect, e.Seq, err)
	}
	return value, nil
}

func validateEffects(store vfs.Store, effects []journal.Entry) error {
	for _, e := range effects {
		if _, err := checkEffect(store, e); err != nil {
			return err
		}
	}
	return nil
}

func spillEffects(store vfs.Store, effects []journal.Entry, limit int) ([]journal.Entry, map[vfs.Hash][]byte, error) {
	objects := make(map[vfs.Hash][]byte)
	if len(effects) == 0 {
		return effects, objects, nil
	}
	out := make([]journal.Entry, len(effects))
	for i, e := range effects {
		value, err := checkEffect(store, e)
		if err != nil {
			return nil, nil, err
		}
		out[i] = e
		switch {
		case e.ValueRef != "":
			h, err := parseRef(e.ValueRef)
			if err != nil {
				return nil, nil, err
			}
			objects[h] = value
		case limit >= 0 && len(value) > limit:
			h := hashBytes(value)
			out[i].Value = nil
			out[i].ValueRef = hex.EncodeToString(h[:])
			out[i].ValueSize = len(value)
			objects[h] = value
		}
	}
	return out, objects, nil
}
