package harness

import (
	"encoding/hex"

	"github.com/mindfire/zygote/trace"
)

// ReplayHandler bridges the JSON-RPC interface to a trace.RunReplayer.
type ReplayHandler struct {
	r *trace.RunReplayer
}

// NewReplayHandler creates a new handler for replaying.
func NewReplayHandler(r *trace.RunReplayer) *ReplayHandler {
	return &ReplayHandler{r: r}
}

// Handshake validates version.
func (h *ReplayHandler) Handshake(_ int) (int, error) {
	return 1, nil // we support v1
}

// Effect fetches the recorded effect, ignoring the requested value.
func (h *ReplayHandler) Effect(op, key string, _ []byte) ([]byte, error) {
	return h.r.ReplayEffect(op, key)
}

// Step verifies the agent's world against the recording.
func (h *ReplayHandler) Step(name string) (*StepResult, error) {
	chk, err := h.r.Step(name)
	if err != nil {
		return nil, err
	}

	return &StepResult{
		Match:    chk.Match,
		Expected: hex.EncodeToString(chk.Expected.Root[:]),
		Actual:   hex.EncodeToString(chk.Actual.Root[:]),
	}, nil
}
