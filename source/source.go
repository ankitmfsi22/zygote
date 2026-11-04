// Package source provides source.
package source

import (
	"context"

	"github.com/mindfire/zygote/vfs"
)

// Tier classifies how faithfully an adapter observes an agent's environment
type Tier string

// Fidelity tiers as defined in SRS §8.5.
const (
	TierA Tier = "A"
	TierB Tier = "B"
	TierC Tier = "C"
)

// Fidelity declares what an adapter can and cannot capture (FR-7.7).
type Fidelity struct {
	Tier        Tier
	Limitations []string
}

// Source defines the interface.
type Source interface {
	Load(ctx context.Context, w *vfs.World) error
	Apply(ctx context.Context, w *vfs.World) error
	Fidelity() Fidelity
}
