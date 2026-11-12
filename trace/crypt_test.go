package trace_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

const plantedPrompt = "PLANTED-PROMPT-7f3a9c"

func sampleRecording(t *testing.T) (*vfs.MemStore, trace.Recording) {
	t.Helper()
	store := vfs.NewMemStore()
	r := trace.Record(store)
	if err := r.World().WriteFile("main.go", []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Step("init")
	if _, err := r.Recorder().Record("model", "ask", func() ([]byte, error) { return []byte(plantedPrompt), nil }); err != nil {
		t.Fatal(err)
	}
	r.Step("done")
	return store, r.Recording()
}

func newIdentity(t *testing.T) (*age.X25519Identity, string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id.txt")
	if err := os.WriteFile(path, []byte("# test identity\n"+id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return id, path
}

func exportSample(t *testing.T, opts ...trace.ExportOption) string {
	t.Helper()
	store, rec := sampleRecording(t)
	path := filepath.Join(t.TempDir(), "run.zip")
	if err := trace.ExportFile(path, store, rec, opts...); err != nil {
		t.Fatalf("ExportFile: %v", err)
	}
	return path
}

func assertImported(t *testing.T, path string, opts ...trace.ImportOption) {
	t.Helper()
	b, err := trace.ImportFile(path, vfs.NewMemStore(), opts...)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(b.Recording.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(b.Recording.Steps))
	}
	if len(b.Recording.Effects) != 1 || string(b.Recording.Effects[0].Value) != plantedPrompt {
		t.Fatalf("effect value not preserved")
	}
}

func rewrite(t *testing.T, path string, mutate func([]byte) []byte) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, mutate(b), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedRoundTripPassphrase(t *testing.T) {
	path := exportSample(t, trace.WithEncryptionPassphrase("correct horse"))
	assertImported(t, path, trace.WithPassphrase("correct horse"))
}

func TestEncryptedRoundTripOneRecipient(t *testing.T) {
	id, idFile := newIdentity(t)
	path := exportSample(t, trace.WithRecipients(id.Recipient().String()))
	assertImported(t, path, trace.WithIdentityFile(idFile))
}

func TestEncryptedRoundTripTwoRecipients(t *testing.T) {
	id1, file1 := newIdentity(t)
	id2, file2 := newIdentity(t)
	path := exportSample(t, trace.WithRecipients(id1.Recipient().String(), id2.Recipient().String()))
	assertImported(t, path, trace.WithIdentityFile(file1))
	assertImported(t, path, trace.WithIdentityFile(file2))
}

func TestEncryptedWithoutKey(t *testing.T) {
	path := exportSample(t, trace.WithEncryptionPassphrase("pw"))
	_, err := trace.ImportFile(path, vfs.NewMemStore())
	if !errors.Is(err, trace.ErrEncrypted) {
		t.Fatalf("expected ErrEncrypted, got %v", err)
	}
	const want = "bundle is encrypted: supply --identity-file or a passphrase (ZYG_PASSPHRASE_FILE)"
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
}

func TestEncryptedWrongKey(t *testing.T) {
	id, _ := newIdentity(t)
	_, otherFile := newIdentity(t)
	recipientPath := exportSample(t, trace.WithRecipients(id.Recipient().String()))
	if _, err := trace.ImportFile(recipientPath, vfs.NewMemStore(), trace.WithIdentityFile(otherFile)); !errors.Is(err, trace.ErrDecrypt) {
		t.Fatalf("expected ErrDecrypt for wrong identity, got %v", err)
	}

	passPath := exportSample(t, trace.WithEncryptionPassphrase("right"))
	if _, err := trace.ImportFile(passPath, vfs.NewMemStore(), trace.WithPassphrase("wrong")); !errors.Is(err, trace.ErrDecrypt) {
		t.Fatalf("expected ErrDecrypt for wrong passphrase, got %v", err)
	}
}

func TestEncryptedCorruptCiphertext(t *testing.T) {
	id, idFile := newIdentity(t)
	recipient := id.Recipient().String()

	expectCorrupt := func(t *testing.T, path string) {
		t.Helper()
		_, err := trace.ImportFile(path, vfs.NewMemStore(), trace.WithIdentityFile(idFile))
		if !errors.Is(err, trace.ErrCorruptEncrypted) {
			t.Fatalf("expected ErrCorruptEncrypted, got %v", err)
		}
	}

	t.Run("truncated", func(t *testing.T) {
		path := exportSample(t, trace.WithRecipients(recipient))
		rewrite(t, path, func(b []byte) []byte { return b[:len(b)-20] })
		expectCorrupt(t, path)
	})

	t.Run("flipped bit in header", func(t *testing.T) {
		path := exportSample(t, trace.WithRecipients(recipient))
		rewrite(t, path, func(b []byte) []byte {
			i := bytes.Index(b, []byte("\n--- "))
			if i < 0 {
				t.Fatal("no header MAC line")
			}
			b[i+6] ^= 0x01
			return b
		})
		expectCorrupt(t, path)
	})

	t.Run("flipped bit in body", func(t *testing.T) {
		path := exportSample(t, trace.WithRecipients(recipient))
		rewrite(t, path, func(b []byte) []byte {
			b[len(b)-5] ^= 0x01
			return b
		})
		expectCorrupt(t, path)
	})

	t.Run("valid age wrapping a non-zip", func(t *testing.T) {
		var buf bytes.Buffer
		w, err := age.Encrypt(&buf, id.Recipient())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("this is not a zip archive")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "bad.zip")
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		expectCorrupt(t, path)
	})
}

func TestEncryptedFileLeaksNothing(t *testing.T) {
	path := exportSample(t, trace.WithEncryptionPassphrase("pw"))
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"PK", "trace.json", plantedPrompt, "objects/"} {
		if bytes.Contains(b, []byte(needle)) {
			t.Fatalf("encrypted bundle contains %q", needle)
		}
	}
	if !bytes.HasPrefix(b, []byte("age-encryption.org/v1\n")) {
		t.Fatal("encrypted bundle does not start with the age header")
	}
}

func TestEncryptWithoutKeyFails(t *testing.T) {
	store, rec := sampleRecording(t)
	id, _ := newIdentity(t)

	cases := map[string]struct {
		opts []trace.ExportOption
		want error
	}{
		"no recipients":    {[]trace.ExportOption{trace.WithRecipients()}, trace.ErrNoEncryptionKey},
		"empty passphrase": {[]trace.ExportOption{trace.WithEncryptionPassphrase("")}, trace.ErrNoEncryptionKey},
		"both":             {[]trace.ExportOption{trace.WithRecipients(id.Recipient().String()), trace.WithEncryptionPassphrase("pw")}, trace.ErrAmbiguousEncryption},
		"bad recipient":    {[]trace.ExportOption{trace.WithRecipients("age1notakey")}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "run.zip")
			err := trace.ExportFile(path, store, rec, tc.opts...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("expected no output files, found %d", len(entries))
			}
		})
	}
}

func TestFailedEncryptLeavesNoFile(t *testing.T) {
	_, rec := sampleRecording(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "run.zip")

	err := trace.ExportFile(path, vfs.NewMemStore(), rec, trace.WithEncryptionPassphrase("pw"))
	if err == nil {
		t.Fatal("expected export from a store missing objects to fail")
	}

	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected empty output directory, found %s", strings.Join(names, ", "))
	}
}

func TestEncryptedFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	path := exportSample(t, trace.WithEncryptionPassphrase("pw"))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %o, want 600", perm)
	}
}

func TestPlainBundleUnchangedByKeys(t *testing.T) {
	path := exportSample(t)
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("PK\x03\x04")) {
		t.Fatal("plain bundle is not a zip")
	}
	_, idFile := newIdentity(t)
	assertImported(t, path)
	assertImported(t, path, trace.WithIdentityFile(idFile), trace.WithPassphrase("unused"))
}

func TestIsEncrypted(t *testing.T) {
	plain := exportSample(t)
	enc := exportSample(t, trace.WithEncryptionPassphrase("pw"))
	for path, want := range map[string]bool{plain: false, enc: true} {
		got, err := trace.IsEncrypted(path)
		if err != nil || got != want {
			t.Fatalf("IsEncrypted(%s) = %v, %v; want %v", path, got, err, want)
		}
	}
}
