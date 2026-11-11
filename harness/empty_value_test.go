package harness_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mindfire/zygote/harness"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

// An empty effect value is dropped from trace.json (omitempty) and decodes as
func TestReplayEmptyValueRoundTripsAsEmptyString(t *testing.T) {
	r, store := newAutoRecorder()
	if _, err := serveReplies(t, harness.NewRecordHandler(r), effectLine(t, 1, "tool:noop", "k", "")); err != nil {
		t.Fatal(err)
	}
	r.Finish()

	path := filepath.Join(t.TempDir(), "run.zip")
	if err := trace.ExportFile(path, store, r.Recording()); err != nil {
		t.Fatal(err)
	}
	fresh := vfs.NewMemStore()
	bundle, err := trace.ImportFile(path, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Recording.Effects[0].Value != nil {
		t.Fatalf("expected the empty value to decode as nil, got %#v", bundle.Recording.Effects[0].Value)
	}

	rr, err := trace.Replay(fresh, &bundle)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	in := strings.NewReader(effectLine(t, 1, "tool:noop", "k", "") + "\n")
	if err := harness.NewServer(in, &out, harness.NewReplayHandler(rr)).Serve(); err != nil {
		t.Fatal(err)
	}
	raw := out.String()
	if !strings.Contains(raw, `"value":""`) || strings.Contains(raw, `"value":null`) {
		t.Fatalf("replay result must carry an empty string value, got %s", raw)
	}

	rr2, err := trace.Replay(fresh, &bundle)
	if err != nil {
		t.Fatal(err)
	}
	replies, err := serveReplies(t, harness.NewReplayHandler(rr2), effectLine(t, 1, "tool:noop", "k", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := replies[0].Result.Value; len(got) != 0 {
		t.Fatalf("replayed value length %d, want 0", len(got))
	}
}
