// Package harness provides the JSON-RPC 2.0 stdio protocol.
package harness

import "encoding/json"

// Request represents a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
}

// Response represents a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	Result  interface{}      `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
}

// Error represents a JSON-RPC 2.0 error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC 2.0 error codes.
const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
)

// HandshakeParams payload.
type HandshakeParams struct {
	Version int `json:"version"`
}

// HandshakeResult payload.
type HandshakeResult struct {
	Version int `json:"version"`
}

// EffectParams payload.
type EffectParams struct {
	Op     string `json:"op"`
	Key    string `json:"key"`
	Value  []byte `json:"value,omitempty"` // populated on record
	Chunks []int  `json:"chunks,omitempty"`
}

// EffectResult payload.
type EffectResult struct {
	Value  []byte `json:"value"`
	Chunks []int  `json:"chunks,omitempty"`
}

// StepParams payload.
type StepParams struct {
	Name string `json:"name"`
}

// StepResult payload.
type StepResult struct {
	Match    bool   `json:"match"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}
