// Package journal provides effects.
package journal

import "fmt"

// Entry represents a single nondeterministic effect recorded during an agent's run.
// This struct strictly conforms to the zygote SRS format specification.
type Entry struct {
	Seq       int    `json:"seq"`
	Op        string `json:"op"`  // e.g., "model", "clock", "tool:<name>", "http"
	Key       string `json:"key"` // request identity within the category
	Value     []byte `json:"value,omitempty"`
	ValueRef  string `json:"value_ref,omitempty"`
	ValueSize int    `json:"value_size,omitempty"`
	Chunks    []int  `json:"chunks,omitempty"`
}

// ValidateChunks reports whether chunk sizes are all positive and sum to total; no chunks is always valid.
func ValidateChunks(chunks []int, total int) error {
	if len(chunks) == 0 {
		return nil
	}
	sum := 0
	for i, n := range chunks {
		if n <= 0 {
			return fmt.Errorf("chunk %d: size %d must be positive", i, n)
		}
		if n > total-sum {
			return fmt.Errorf("chunks exceed value length %d", total)
		}
		sum += n
	}
	if sum != total {
		return fmt.Errorf("chunks sum to %d bytes, want value length %d", sum, total)
	}
	return nil
}

// DivergenceClass represents the taxonomy of divergence defined in SRS §8.4.
type DivergenceClass string

const (
	// ClassEffectMismatch indicates an effect mismatch.
	ClassEffectMismatch DivergenceClass = "effect-mismatch"
	// ClassWorldMismatch indicates mismatch.
	ClassWorldMismatch DivergenceClass = "world-mismatch"
	// ClassStepMismatch indicates mismatch.
	ClassStepMismatch DivergenceClass = "step-mismatch"
	// ClassExhausted indicates exhausted.
	ClassExhausted DivergenceClass = "exhausted"
	// ClassMissingObject indicates missing object.
	ClassMissingObject DivergenceClass = "missing-object"
	// ClassUninstrumented indicates uninstrumented.
	ClassUninstrumented DivergenceClass = "uninstrumented"
)

// DivergenceError is returned during replay when an agent's behavior deviates
// from the recorded timeline.
type DivergenceError struct {
	Class   DivergenceClass
	Message string
	Step    int // the step number where divergence occurred
}

func (e *DivergenceError) Error() string {
	return fmt.Sprintf("divergence [%s] at step %d: %s", e.Class, e.Step, e.Message)
}
