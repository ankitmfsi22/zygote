package trace

import (
	"encoding/hex"
	"testing"

	"github.com/mindfire/zygote/pkg/redact"
	"github.com/mindfire/zygote/vfs"
)

func TestRedactBundle(t *testing.T) {
	// Create a fake bundle with a secret in effects and a secret in VFS
	store := vfs.NewMemStore()
	world := vfs.NewWorld(store)
	_ = world.WriteFile("secret.txt", []byte("My token is AKIAIOSFODNN7EXAMPLE !"), 0644)
	r := Record(store)
	// r.world needs the file, wait r.world is a DIFFERENT world!
	// We can just use r.World()
	_ = r.World().WriteFile("secret.txt", []byte("My token is AKIAIOSFODNN7EXAMPLE !"), 0644)
	r.Step("init")

	_, _ = r.Recorder().Record("model", "q1", func() ([]byte, error) {
		return []byte("I leaked AKIAIOSFODNN7EXAMPLE !!"), nil
	})
	r.Step("step1")

	inBundle := &Bundle{
		Recording: r.Recording(),
		Store:     store,
	}

	policy := &redact.Policy{
		Rules: []redact.PolicyRule{
			{ID: "aws-access-token", Action: redact.ActionRedact},
		},
	}

	cleanBundle, err := RedactBundle(inBundle, policy)
	if err != nil {
		t.Fatal(err)
	}

	if len(cleanBundle.Recording.Effects) != 1 {
		t.Fatal("expected 1 effect")
	}

	effValue := string(cleanBundle.Recording.Effects[0].Value)
	if effValue != "I leaked *** !!" {
		t.Fatalf("effect not redacted correctly: %s", effValue)
	}

	// Verify VFS hash changed
	oldRoot := inBundle.Recording.Steps[1].Root
	newRoot := cleanBundle.Recording.Steps[1].Root
	if oldRoot == newRoot {
		t.Fatal("expected root hash to change after redaction")
	}

	// Verify the hash is valid
	newRootHash, _ := hex.DecodeString(newRoot)
	var h vfs.Hash
	copy(h[:], newRootHash)

	_, ok := cleanBundle.Store.Get(h)
	if !ok {
		t.Fatal("new root object not found in store")
	}
}

func TestRedactBundle_PreservesFidelity(t *testing.T) {
	store := vfs.NewMemStore()
	r := Record(store)
	_ = r.World().WriteFile("a.txt", []byte("hello"), 0644)
	r.Step("init")

	rec := r.Recording()
	rec.Fidelity = &Fidelity{Tier: "A", Limitations: []string{"one", "two"}}
	in := &Bundle{Recording: rec, Store: store}

	out, err := RedactBundle(in, &redact.Policy{})
	if err != nil {
		t.Fatal(err)
	}

	f := out.Recording.Fidelity
	if f == nil || f.Tier != "A" || len(f.Limitations) != 2 || f.Limitations[0] != "one" || f.Limitations[1] != "two" {
		t.Fatalf("fidelity not preserved: %+v", f)
	}
	if f == in.Recording.Fidelity {
		t.Fatal("Fidelity pointer is aliased")
	}

	// Mutating the output must not affect the input.
	f.Limitations[0] = "changed"
	if in.Recording.Fidelity.Limitations[0] != "one" {
		t.Fatal("Limitations slice is aliased")
	}
}

func TestRedactBundle_NilFidelity(t *testing.T) {
	store := vfs.NewMemStore()
	r := Record(store)
	r.Step("init")

	out, err := RedactBundle(&Bundle{Recording: r.Recording(), Store: store}, &redact.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Recording.Fidelity != nil {
		t.Fatalf("expected nil Fidelity, got %+v", out.Recording.Fidelity)
	}
}
