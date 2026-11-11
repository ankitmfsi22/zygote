package trace_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/mindfire/zygote/pkg/redact"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

const leakedKey = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec

func redactNothing() *redact.Policy {
	return &redact.Policy{}
}

func redactAWS() *redact.Policy {
	return &redact.Policy{Rules: []redact.PolicyRule{{ID: "aws-access-token", Action: redact.ActionRedact}}}
}

func digestAWS() *redact.Policy {
	return &redact.Policy{Rules: []redact.PolicyRule{{ID: "aws-access-token", Action: redact.ActionDigestOnly}}}
}

func largeWithSecret() []byte {
	return append(append(filler(trace.DefaultSpillThreshold+100, 'x'), leakedKey...), filler(200, 'y')...)
}

func assertNoSecret(t *testing.T, files map[string][]byte, secret string) {
	t.Helper()
	for name, data := range files {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("secret leaked into %s", name)
		}
	}
	m := manifestOf(t, files)
	for i, e := range m.Effects {
		value := e.Value
		if e.ValueRef != "" {
			value = files["objects/"+e.ValueRef]
		}
		if bytes.Contains(value, []byte(secret)) {
			t.Fatalf("secret leaked through effect %d", i)
		}
	}
}

func TestRedactLargeValueNeverLeaks(t *testing.T) {
	rec, store := recordRun(t, effectSpec{"model", "p", largeWithSecret(), nil})

	clean, err := trace.RedactBundle(&trace.Bundle{Recording: rec, Store: store}, redactAWS())
	if err != nil {
		t.Fatal(err)
	}
	e := clean.Recording.Effects[0]
	if e.ValueRef != "" || e.ValueSize != 0 || len(e.Value) == 0 {
		t.Fatalf("RedactBundle must emit inline values: %+v", e.ValueRef)
	}
	if bytes.Contains(e.Value, []byte(leakedKey)) || !bytes.Contains(e.Value, []byte("***")) {
		t.Fatal("secret not redacted in the value")
	}
	if !clean.Recording.HasRawPrompts {
		t.Fatal("a raw model value must set has_raw_prompts")
	}

	files := readZip(t, exportTo(t, "clean.zip", clean.Recording, clean.Store))
	assertNoSecret(t, files, leakedKey)
	if m := manifestOf(t, files); m.Effects[0].ValueRef == "" || !m.HasRawPrompts {
		t.Fatalf("redacted large value must be spilled after redaction: %+v", m.Effects[0])
	}
}

func TestRedactHydratesValueRef(t *testing.T) {
	rec, store := recordRun(t, effectSpec{"model", "p", largeWithSecret(), nil})
	raw := importFrom(t, exportTo(t, "raw.zip", rec, store))
	if raw.Recording.Effects[0].ValueRef == "" || len(raw.Recording.Effects[0].Value) != 0 {
		t.Fatal("fixture must carry the value by reference")
	}

	if _, err := trace.RedactBundle(&raw, redactNothing()); err == nil || !strings.Contains(err.Error(), "fail-closed") {
		t.Fatalf("a secret inside a referenced value must block export, got %v", err)
	}

	clean, err := trace.RedactBundle(&raw, redactAWS())
	if err != nil {
		t.Fatal(err)
	}
	if clean.Recording.Effects[0].ValueRef != "" || bytes.Contains(clean.Recording.Effects[0].Value, []byte(leakedKey)) {
		t.Fatal("referenced value was not hydrated and redacted")
	}
	assertNoSecret(t, readZip(t, exportTo(t, "clean.zip", clean.Recording, clean.Store)), leakedKey)
}

func TestRedactDigestOnlyLargeValue(t *testing.T) {
	value := largeWithSecret()
	rec, store := recordRun(t,
		effectSpec{"model", "p", value, []int{1000, len(value) - 1000}},
		effectSpec{"tool:x", "q", value, nil},
	)

	for name, bundle := range map[string]*trace.Bundle{
		"inline": {Recording: rec, Store: store},
		"ref":    ptr(importFrom(t, exportTo(t, "raw.zip", rec, store))),
	} {
		clean, err := trace.RedactBundle(bundle, digestAWS())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := hashOf(t, value)
		for i, e := range clean.Recording.Effects {
			if string(e.Value) != want || e.Chunks != nil || e.ValueRef != "" {
				t.Fatalf("%s effect %d: expected digest only without chunks, got %d bytes %v", name, i, len(e.Value), e.Chunks)
			}
		}
		if clean.Recording.HasRawPrompts {
			t.Fatalf("%s: digest-only model value is not a raw prompt", name)
		}
		assertNoSecret(t, readZip(t, exportTo(t, "clean.zip", clean.Recording, clean.Store)), leakedKey)
	}
}

func ptr(b trace.Bundle) *trace.Bundle {
	return &b
}

func TestRedactPathDigestOnlyLargeModelValue(t *testing.T) {
	value := filler(trace.DefaultSpillThreshold+10, 'p')
	rec, store := recordRun(t, effectSpec{"model", "p", value, []int{10, len(value) - 10}})
	policy := &redact.Policy{Rules: []redact.PolicyRule{{Path: "effect-0", Action: redact.ActionDigestOnly}}}

	clean, err := trace.RedactBundle(&trace.Bundle{Recording: rec, Store: store}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Recording.HasRawPrompts {
		t.Fatal("digest-only model value must not set has_raw_prompts")
	}
	files := readZip(t, exportTo(t, "clean.zip", clean.Recording, clean.Store))
	for name, data := range files {
		if bytes.Contains(data, filler(64, 'p')) {
			t.Fatalf("raw value leaked into %s", name)
		}
	}
	if manifestOf(t, files).HasRawPrompts {
		t.Fatal("manifest declares raw prompts for a digest-only bundle")
	}
}

func TestHasRawPromptsForLargeValues(t *testing.T) {
	value := filler(trace.DefaultSpillThreshold+10, 'm')

	rec, store := recordRun(t, effectSpec{"model", "p", value, nil})
	clean, err := trace.RedactBundle(&trace.Bundle{Recording: rec, Store: store}, redactAWS())
	if err != nil || !clean.Recording.HasRawPrompts {
		t.Fatalf("large model value must set has_raw_prompts: %v", err)
	}

	rec, store = recordRun(t, effectSpec{"tool:x", "p", value, nil})
	clean, err = trace.RedactBundle(&trace.Bundle{Recording: rec, Store: store}, redactAWS())
	if err != nil || clean.Recording.HasRawPrompts {
		t.Fatalf("a tool value is not a raw prompt: %v", err)
	}

	rec, store = recordRun(t, effectSpec{"model", "p", value, nil})
	raw := importFrom(t, exportTo(t, "raw.zip", rec, store))
	clean, err = trace.RedactBundle(&raw, redactAWS())
	if err != nil || !clean.Recording.HasRawPrompts {
		t.Fatalf("has_raw_prompts must be derived from the hydrated value: %v", err)
	}
}

func TestRedactSecretSpanningChunkBoundary(t *testing.T) {
	first := []byte("prefix AKIAIOSF")
	second := []byte("ODNN7EXAMPLE suffix")
	value := append(append([]byte(nil), first...), second...)
	chunks := []int{len(first), len(second)}
	if bytes.Contains(first, []byte(leakedKey[:12])) && bytes.Contains(second, []byte(leakedKey[:12])) {
		t.Fatal("fixture must split the secret")
	}
	rec, store := recordRun(t, effectSpec{"model", "p", value, chunks})

	clean, err := trace.RedactBundle(&trace.Bundle{Recording: rec, Store: store}, redactAWS())
	if err != nil {
		t.Fatal(err)
	}
	e := clean.Recording.Effects[0]
	if string(e.Value) != "prefix *** suffix" {
		t.Fatalf("secret straddling chunks survived: %q", e.Value)
	}
	if e.Chunks != nil {
		t.Fatalf("chunks must be dropped after the value changed: %v", e.Chunks)
	}
	assertNoSecret(t, readZip(t, exportTo(t, "clean.zip", clean.Recording, clean.Store)), leakedKey)

	if _, err := trace.RedactBundle(&trace.Bundle{Recording: rec, Store: store}, redactNothing()); err == nil {
		t.Fatal("block policy must stop a secret straddling chunks")
	}
}

func TestRedactKeepsChunksWhenValueUnchanged(t *testing.T) {
	value := []byte("nothing secret here")
	rec, store := recordRun(t, effectSpec{"model", "p", value, []int{7, 7, 5}})

	clean, err := trace.RedactBundle(&trace.Bundle{Recording: rec, Store: store}, redactAWS())
	if err != nil {
		t.Fatal(err)
	}
	if got := clean.Recording.Effects[0].Chunks; !reflect.DeepEqual(got, []int{7, 7, 5}) {
		t.Fatalf("chunks must survive an unchanged value: %v", got)
	}

	big := filler(trace.DefaultSpillThreshold+5, 'k')
	rec, store = recordRun(t, effectSpec{"model", "p", big, []int{100, len(big) - 100}})
	clean, err = trace.RedactBundle(&trace.Bundle{Recording: rec, Store: store}, redactAWS())
	if err != nil {
		t.Fatal(err)
	}
	m := manifestOf(t, readZip(t, exportTo(t, "clean.zip", clean.Recording, clean.Store)))
	if m.Effects[0].ValueRef == "" || !reflect.DeepEqual(m.Effects[0].Chunks, []int{100, len(big) - 100}) {
		t.Fatalf("chunks lost across redaction and spill: %+v", m.Effects[0])
	}
}

func TestRedactFailsClosedOnUnresolvableValue(t *testing.T) {
	rec, store := recordRun(t, effectSpec{"model", "p", filler(100, 'a'), nil})
	raw := importFrom(t, exportTo(t, "raw.zip", rec, store, trace.WithSpillThreshold(10)))

	empty := trace.Bundle{Recording: raw.Recording, Store: vfs.NewMemStore()}
	if _, err := trace.RedactBundle(&empty, redactAWS()); err == nil {
		t.Fatal("redaction must fail when a referenced value cannot be loaded")
	}

	raw.Recording.Effects[0].ValueRef = strings.ToUpper(raw.Recording.Effects[0].ValueRef)
	if _, err := trace.RedactBundle(&raw, redactAWS()); err == nil {
		t.Fatal("redaction must reject a malformed reference")
	}
}
