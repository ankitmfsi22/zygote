// Package journal provides effects.
package journal

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"time"
)

// ClockRecord records or replays the current time.
// Since time.Time serializes well to text, we marshal it as RFC3339Nano.
func ClockRecord(r *Recorder, key string) (time.Time, error) {
	val, err := r.Record("clock", key, func() ([]byte, error) {
		t := time.Now()
		b, err := t.MarshalText()
		return b, err
	})
	if err != nil {
		return time.Time{}, err
	}
	var t time.Time
	err = t.UnmarshalText(val)
	return t, err
}

// ClockReplay replays the clock.
func ClockReplay(r *Replayer, key string) (time.Time, error) {
	val, err := r.Replay("clock", key)
	if err != nil {
		return time.Time{}, err
	}
	var t time.Time
	err = t.UnmarshalText(val)
	return t, err
}

// ModelRecord records or replays an LLM response.
func ModelRecord(r *Recorder, key string, call func() (string, error)) (string, error) {
	val, err := r.Record("model", key, func() ([]byte, error) {
		resp, err := call()
		if err != nil {
			return nil, err
		}
		return []byte(resp), nil
	})
	if err != nil {
		return "", err
	}
	return string(val), nil
}

// ModelReplay replays the model.
func ModelReplay(r *Replayer, key string) (string, error) {
	val, err := r.Replay("model", key)
	if err != nil {
		return "", err
	}
	return string(val), nil
}

// ModelStreamRecord records a streamed LLM response as one effect with the assembled value and the chunk sizes.
func ModelStreamRecord(r *Recorder, key string, call func() ([][]byte, error)) ([][]byte, error) {
	var streamed [][]byte
	_, _, err := r.RecordChunks("model", key, func() ([]byte, []int, error) {
		parts, err := call()
		if err != nil {
			return nil, nil, err
		}
		streamed = parts
		var assembled []byte
		var sizes []int
		for _, p := range parts {
			if len(p) == 0 {
				continue
			}
			assembled = append(assembled, p...)
			sizes = append(sizes, len(p))
		}
		return assembled, sizes, nil
	})
	if err != nil {
		return nil, err
	}
	return streamed, nil
}

// ModelStreamReplay replays a streamed LLM response, re-split at the recorded chunk boundaries.
func ModelStreamReplay(r *Replayer, key string) ([][]byte, error) {
	val, chunks, err := r.ReplayChunks("model", key)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return [][]byte{val}, nil
	}
	if err := ValidateChunks(chunks, len(val)); err != nil {
		return nil, fmt.Errorf("recorded chunks do not match value: %w", err)
	}
	parts := make([][]byte, 0, len(chunks))
	off := 0
	for _, n := range chunks {
		parts = append(parts, val[off:off+n:off+n])
		off += n
	}
	return parts, nil
}

// ToolRecord records or replays a named tool call.
func ToolRecord(r *Recorder, name, key string, call func() ([]byte, error)) ([]byte, error) {
	return r.Record("tool:"+name, key, call)
}

// ToolReplay replays the tool.
func ToolReplay(r *Replayer, name, key string) ([]byte, error) {
	return r.Replay("tool:"+name, key)
}

// ReplayRoundTripper intercepts HTTP requests and replays recorded responses.
type ReplayRoundTripper struct {
	next     http.RoundTripper
	recorder *Recorder
	replayer *Replayer
}

// WrapHTTPClient injects zygote recording and replaying into an http.Client.
func WrapHTTPClient(client *http.Client, recorder *Recorder, replayer *Replayer) {
	next := client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	client.Transport = &ReplayRoundTripper{
		next:     next,
		recorder: recorder,
		replayer: replayer,
	}
}

// RoundTrip executes the HTTP request or replays a recorded response.
func (rt *ReplayRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	key := fmt.Sprintf("%s %s", req.Method, req.URL.String())

	if rt.replayer != nil {
		data, err := rt.replayer.Replay("http", key)
		if err != nil {
			return nil, err
		}

		buf := bytes.NewBuffer(data)
		res, err := http.ReadResponse(bufio.NewReader(buf), req)
		if err != nil {
			return nil, fmt.Errorf("failed to decode recorded response: %w", err)
		}
		return res, nil
	}

	data, err := rt.recorder.Record("http", key, func() ([]byte, error) {
		res, err := rt.next.RoundTrip(req)
		if res != nil && res.Body != nil {
			defer func() { _ = res.Body.Close() }()
		}
		if err != nil {
			return nil, err
		}

		dump, err := httputil.DumpResponse(res, true)
		if err != nil {
			return nil, err
		}
		return dump, nil
	})
	if err != nil {
		return nil, err
	}

	buf := bytes.NewBuffer(data)
	return http.ReadResponse(bufio.NewReader(buf), req)
}

// WrapRand seeds a random number generator deterministically.
func WrapRand(recorder *Recorder, replayer *Replayer) *rand.Rand {
	var seed int64

	if replayer != nil {
		data, err := replayer.Replay("rand", "seed")
		if err != nil {
			panic(err)
		}
		seed = int64(data[0]) | int64(data[1])<<8 | int64(data[2])<<16 | int64(data[3])<<24 | int64(data[4])<<32 | int64(data[5])<<40 | int64(data[6])<<48 | int64(data[7])<<56
	} else {
		seed = rand.Int63() //nolint:gosec

		var data [8]byte
		binary.LittleEndian.PutUint64(data[:], uint64(seed)) //nolint:gosec

		_, _ = recorder.Record("rand", "seed", func() ([]byte, error) {
			return data[:], nil
		})
	}

	return rand.New(rand.NewSource(seed)) //nolint:gosec
}
