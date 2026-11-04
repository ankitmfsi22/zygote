package trace

import (
	"github.com/mindfire/zygote/journal"
	"github.com/mindfire/zygote/vfs"
)

// Step represents a snapshot of the agent's world at a named boundary.
type Step struct {
	N        int               `json:"n"`
	Name     string            `json:"name"`
	Root     string            `json:"root"`    // hex world hash
	Effects  int               `json:"effects"` // effect count at end of step
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Fidelity records the source adapter's fidelity declaration (SRS §8.5).
type Fidelity struct {
	Tier        string   `json:"tier"`
	Limitations []string `json:"limitations,omitempty"`
}

// Recording is the canonical manifest of a Run, containing steps and effects.
type Recording struct {
	Version          int               `json:"version"`
	HashAlgorithm    string            `json:"hash_algorithm"`
	Producer         string            `json:"producer"`
	RedactionApplied string            `json:"redaction_applied,omitempty"`
	Fidelity         *Fidelity         `json:"fidelity,omitempty"`
	HasRawPrompts    bool              `json:"has_raw_prompts"`
	Meta             map[string]string `json:"meta,omitempty"`
	Steps            []Step            `json:"steps"`
	Effects          []journal.Entry   `json:"effects"`
}

// Check represents the per-step replay verdict, comparing the expected
// recorded world against the actual replayed world.
type Check struct {
	N        int
	Name     string
	Expected vfs.Snapshot
	Actual   vfs.Snapshot
	Match    bool
}

// Bundle combines a Recording with the Store containing all reachable objects.
// This fulfills the type contract required by meiosis (SRS 12.2).
type Bundle struct {
	Recording Recording
	Store     vfs.Store
}
