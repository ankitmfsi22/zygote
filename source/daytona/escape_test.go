package daytona

import (
	"errors"
	"testing"
)

func TestTarget_RefusesEscape(t *testing.T) {
	a := NewWithFileSystem(nil, "/work")

	for _, rel := range []string{"../escaped.txt", "a/../../etc/passwd", "../work2/x"} {
		if _, err := a.target(rel); !errors.Is(err, ErrPathEscape) {
			t.Fatalf("expected ErrPathEscape for %q, got %v", rel, err)
		}
	}

	got, err := a.target("sub/ok.txt")
	if err != nil || got != "/work/sub/ok.txt" {
		t.Fatalf("unexpected target %q, %v", got, err)
	}
}
