package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Handler interface defines how the server responds to JSON-RPC methods.
type Handler interface {
	Handshake(version int) (int, error)
	Effect(op, key string, value []byte) ([]byte, error)
	Step(name string) (*StepResult, error)
}

// ChunkedHandler is an optional Handler extension that carries streamed chunk sizes with an effect.
type ChunkedHandler interface {
	Handler
	EffectChunks(op, key string, value []byte, chunks []int) ([]byte, []int, error)
}

// Server is a JSON-RPC 2.0 server over line-delimited stdio.
type Server struct {
	in      io.Reader
	out     io.Writer
	handler Handler
}

// NewServer creates a new JSON-RPC stdio server.
func NewServer(in io.Reader, out io.Writer, handler Handler) *Server {
	return &Server{
		in:      in,
		out:     out,
		handler: handler,
	}
}

// Serve reads line-by-line from the input. Valid JSON-RPC is processed.
// Invalid JSON is echoed to os.Stderr as agent logs.
func (s *Server) Serve() error {
	scanner := bufio.NewScanner(s.in)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024*10)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintf(os.Stderr, "[Agent] %s\n", string(line))
			continue
		}

		if req.JSONRPC != "2.0" {
			fmt.Fprintf(os.Stderr, "[Agent] %s\n", string(line))
			continue
		}

		if err := s.handleRequest(&req); err != nil {
			return err
		}
	}

	return scanner.Err()
}

func (s *Server) handleRequest(req *Request) error {
	res := Response{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "handshake":
		var p HandshakeParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			res.Error = &Error{Code: ErrInvalidParams, Message: "Invalid handshake params"}
		} else {
			v, err := s.handler.Handshake(p.Version)
			if err != nil {
				res.Error = &Error{Code: ErrInternal, Message: err.Error()}
			} else {
				res.Result = HandshakeResult{Version: v}
			}
		}

	case "effect":
		var p EffectParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			res.Error = &Error{Code: ErrInvalidParams, Message: "Invalid effect params"}
		} else if ch, ok := s.handler.(ChunkedHandler); ok {
			val, chunks, err := ch.EffectChunks(p.Op, p.Key, p.Value, p.Chunks)
			if err != nil {
				return err // Fail closed immediately, propagate Go error
			}
			res.Result = EffectResult{Value: val, Chunks: chunks}
		} else if len(p.Chunks) > 0 {
			res.Error = &Error{Code: ErrInvalidParams, Message: "Handler does not support chunks"}
		} else {
			val, err := s.handler.Effect(p.Op, p.Key, p.Value)
			if err != nil {
				return err // Fail closed immediately, propagate Go error
			}
			res.Result = EffectResult{Value: val}
		}

	case "step":
		var p StepParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			res.Error = &Error{Code: ErrInvalidParams, Message: "Invalid step params"}
		} else {
			result, err := s.handler.Step(p.Name)
			if err != nil {
				return err // Fail closed immediately, propagate Go error
			}
			res.Result = result
		}

	default:
		res.Error = &Error{Code: ErrMethodNotFound, Message: "Method not found"}
	}

	if req.ID == nil {
		if res.Error != nil {
			return fmt.Errorf("fatal protocol error on notification: %s", res.Error.Message)
		}
		return nil
	}

	out, err := json.Marshal(res)
	if err != nil {
		return err
	}
	out = append(out, '\n')
	_, err = s.out.Write(out)

	return err
}
