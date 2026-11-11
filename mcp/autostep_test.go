package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mindfire/zygote/harness"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

func stepSummary(rec trace.Recording) []string {
	out := []string{}
	for _, s := range rec.Steps {
		md, _ := json.Marshal(s.Metadata)
		out = append(out, fmt.Sprintf("%d|%s|%s|%d|%s", s.N, s.Name, s.Root, s.Effects, md))
	}
	return out
}

func TestAutoStepRunStartEffectExport(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{AutoStep: true})
	e1 := call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "tool:search", Key: "q", Value: "hit"})
	if e1.Seq != 0 || e1.AutoStep == nil || e1.AutoStep.N != 1 || e1.AutoStep.Name != "auto:tool:search" || e1.AutoStep.Effects != 1 {
		t.Fatalf("unexpected effect output %+v", e1)
	}
	e2 := call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "model", Key: "p", Value: "answer"})
	if e2.Seq != 1 || e2.AutoStep != nil {
		t.Fatalf("model effect must not close a step: %+v", e2)
	}
	exp := call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})
	if exp.Steps != 3 || exp.Effects != 2 {
		t.Fatalf("unexpected export %+v", exp)
	}

	b := importBundle(t, filepath.Join(root, "run.zip"))
	names := []string{}
	for _, s := range b.Recording.Steps {
		names = append(names, s.Name)
	}
	if !reflect.DeepEqual(names, []string{"init", "auto:tool:search", "auto:final"}) {
		t.Fatalf("unexpected steps %v", names)
	}
	if b.Recording.Meta["auto_step"] != "true" {
		t.Fatalf("auto_step meta missing: %v", b.Recording.Meta)
	}
	if md := b.Recording.Steps[1].Metadata; md["auto"] != "true" || md["trigger"] != "tool:search" || md["seq"] != "0" {
		t.Fatalf("unexpected metadata %v", md)
	}
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "again.zip"})
	again := importBundle(t, filepath.Join(root, "again.zip"))
	if len(again.Recording.Steps) != 3 {
		t.Fatalf("second export added steps: %d", len(again.Recording.Steps))
	}
}

func TestAutoStepExplicitRunStepMixed(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{AutoStep: true})
	call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "tool:a", Key: "k1", Value: "v"})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "a.txt", Content: "one"})
	s := call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "manual"})
	if s.N != 2 || s.Name != "manual" {
		t.Fatalf("unexpected explicit step %+v", s)
	}
	call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "tool:b", Key: "k2", Value: "v"})
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})

	b := importBundle(t, filepath.Join(root, "run.zip"))
	names := []string{}
	for _, st := range b.Recording.Steps {
		names = append(names, st.Name)
	}
	if !reflect.DeepEqual(names, []string{"init", "auto:tool:a", "manual", "auto:tool:b"}) {
		t.Fatalf("unexpected steps %v", names)
	}
}

func TestAutoStepOffByDefaultMCP(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	e := call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "tool:a", Key: "k", Value: "v"})
	if e.AutoStep != nil {
		t.Fatalf("unexpected auto step %+v", e.AutoStep)
	}
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})

	b := importBundle(t, filepath.Join(root, "run.zip"))
	if len(b.Recording.Steps) != 1 || b.Recording.Meta["auto_step"] != "" {
		t.Fatalf("default behaviour changed: %+v", b.Recording)
	}
}

func TestAutoStepToolsListStillTen(t *testing.T) {
	cs := newSession(t, t.TempDir(), 0)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 10 {
		t.Fatalf("expected 10 tools, got %d", len(res.Tools))
	}
}

func TestAutoStepHarnessMCPParity(t *testing.T) {
	type effect struct{ op, key, value string }
	effects := []effect{{"tool:search", "q", "hit"}, {"model", "p", "answer"}, {"tool:fetch", "u", "page"}, {"clock", "now", "t"}}

	store := vfs.NewMemStore()
	r := trace.Record(store, trace.WithAutoStep())
	r.Step("init")
	var lines []string
	for i, e := range effects {
		params, err := json.Marshal(harness.EffectParams{Op: e.op, Key: e.key, Value: []byte(e.value)})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf(`{"jsonrpc":"2.0","method":"effect","params":%s,"id":%d}`, params, i+1))
	}
	srv := harness.NewServer(strings.NewReader(strings.Join(lines, "\n")+"\n"), &bytes.Buffer{}, harness.NewRecordHandler(r))
	if err := srv.Serve(); err != nil {
		t.Fatal(err)
	}
	r.Finish()

	root := t.TempDir()
	cs := newSession(t, root, 0)
	call[RunStartOutput](t, cs, "run.start", RunStartInput{AutoStep: true})
	for _, e := range effects {
		call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: e.op, Key: e.key, Value: e.value})
	}
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})
	b := importBundle(t, filepath.Join(root, "run.zip"))

	got, want := stepSummary(b.Recording), stepSummary(r.Recording())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("harness and MCP steps differ:\n mcp     %v\n harness %v", got, want)
	}
	if len(want) != 4 {
		t.Fatalf("expected init, two tool steps and a final, got %v", want)
	}
}
