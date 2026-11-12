package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
)

const (
	cliPrompt   = "CLI-PLANTED-PROMPT-91b2"
	cliAgent    = `echo "ENVCHECK[$ZYG_PASSPHRASE][$ZYG_PASSPHRASE_FILE][$ZYG_IDENTITY_FILE]"`
	ageHeader   = "age-encryption.org/v1\n"
	secretValue = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec
)

var zygBinary string

func TestMain(m *testing.M) {
	code := 1
	defer func() { os.Exit(code) }()

	dir, err := os.MkdirTemp("", "zyg-cli-test-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck

	zygBinary = filepath.Join(dir, "zyg-test-bin")
	if runtime.GOOS == "windows" {
		zygBinary += ".exe"
	}
	if out, err := exec.CommandContext(context.Background(), "go", "build", "-o", zygBinary, ".").CombinedOutput(); err != nil { //nolint:gosec
		panic("building zyg: " + err.Error() + "\n" + string(out))
	}
	code = m.Run()
}

type zygResult struct {
	out  string
	code int
}

func runZyg(t *testing.T, env []string, args ...string) zygResult {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), zygBinary, args...) //nolint:gosec
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	res := zygResult{out: buf.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.code = exitErr.ExitCode()
	default:
		t.Fatalf("running zyg: %v", err)
	}
	return res
}

func requireShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

func workDir(t *testing.T, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"prompt.txt": cliPrompt}
	for k, v := range extra {
		files[k] = v
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeIdentity(t *testing.T) (*age.X25519Identity, string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id.txt")
	if err := os.WriteFile(path, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return id, path
}

func writePassphraseFile(t *testing.T, pass string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pass.txt")
	if err := os.WriteFile(path, []byte(pass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func recordBundle(t *testing.T, env []string, extra ...string) (string, zygResult) {
	t.Helper()
	dir := workDir(t, nil)
	out := filepath.Join(t.TempDir(), "run.zip")
	args := append([]string{"record", "--agent", cliAgent, "--dir", dir, "-o", out}, extra...)
	return out, runZyg(t, env, args...)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRecordEncryptThenRead(t *testing.T) {
	requireShell(t)
	id, idFile := writeIdentity(t)
	_, wrongFile := writeIdentity(t)

	bundle, res := recordBundle(t, nil, "--encrypt", "--recipient", id.Recipient().String())
	if res.code != 0 {
		t.Fatalf("record --encrypt exit %d: %s", res.code, res.out)
	}
	raw, err := os.ReadFile(bundle) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), ageHeader) {
		t.Fatal("bundle is not age-encrypted")
	}
	for _, needle := range []string{"PK", "trace.json", cliPrompt} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Fatalf("encrypted bundle contains %q", needle)
		}
	}

	commands := map[string][]string{
		"steps":   {"steps", bundle},
		"inspect": {"inspect", bundle, "--meta"},
		"replay":  {"replay", bundle, "--agent", "true"},
	}
	for name, args := range commands {
		t.Run(name+" without key", func(t *testing.T) {
			res := runZyg(t, nil, args...)
			if res.code != 2 {
				t.Fatalf("exit %d, want 2: %s", res.code, res.out)
			}
			if !strings.Contains(res.out, "bundle is encrypted: supply --identity-file or a passphrase (ZYG_PASSPHRASE_FILE)") {
				t.Fatalf("missing ErrEncrypted message: %s", res.out)
			}
		})
		t.Run(name+" with wrong key", func(t *testing.T) {
			res := runZyg(t, nil, append(args, "--identity-file", wrongFile)...)
			if res.code != 2 {
				t.Fatalf("exit %d, want 2: %s", res.code, res.out)
			}
			if !strings.Contains(res.out, "cannot decrypt bundle") {
				t.Fatalf("missing ErrDecrypt message: %s", res.out)
			}
		})
	}

	t.Run("steps with key", func(t *testing.T) {
		res := runZyg(t, nil, "steps", bundle, "--identity-file", idFile)
		if res.code != 0 || !strings.Contains(res.out, "init") {
			t.Fatalf("exit %d: %s", res.code, res.out)
		}
	})
	t.Run("steps with key from env", func(t *testing.T) {
		res := runZyg(t, []string{"ZYG_IDENTITY_FILE=" + idFile}, "steps", bundle)
		if res.code != 0 {
			t.Fatalf("exit %d: %s", res.code, res.out)
		}
	})
	t.Run("inspect with key", func(t *testing.T) {
		res := runZyg(t, nil, "inspect", bundle, "--meta", "--identity-file", idFile)
		if res.code != 0 {
			t.Fatalf("exit %d: %s", res.code, res.out)
		}
	})
	t.Run("replay with key", func(t *testing.T) {
		res := runZyg(t, nil, "replay", bundle, "--agent", "true", "--identity-file", idFile)
		if res.code == 2 || strings.Contains(res.out, "encrypted") {
			t.Fatalf("replay failed to open bundle (exit %d): %s", res.code, res.out)
		}
	})
}

func TestRecordEncryptPassphrase(t *testing.T) {
	requireShell(t)
	passFile := writePassphraseFile(t, "hunter2 hunter2")
	bundle, res := recordBundle(t, []string{"ZYG_PASSPHRASE_FILE=" + passFile}, "--encrypt", "--passphrase")
	if res.code != 0 {
		t.Fatalf("record exit %d: %s", res.code, res.out)
	}

	if res := runZyg(t, nil, "steps", bundle); res.code != 2 {
		t.Fatalf("no key: exit %d, want 2: %s", res.code, res.out)
	}
	if res := runZyg(t, []string{"ZYG_PASSPHRASE=wrong"}, "steps", bundle); res.code != 2 || !strings.Contains(res.out, "cannot decrypt bundle") {
		t.Fatalf("wrong passphrase: exit %d: %s", res.code, res.out)
	}
	if res := runZyg(t, []string{"ZYG_PASSPHRASE_FILE=" + passFile}, "steps", bundle); res.code != 0 {
		t.Fatalf("passphrase file: exit %d: %s", res.code, res.out)
	}
	if res := runZyg(t, []string{"ZYG_PASSPHRASE=hunter2 hunter2"}, "steps", bundle); res.code != 0 {
		t.Fatalf("passphrase env: exit %d: %s", res.code, res.out)
	}
}

func TestEncryptWithoutKeyIsAnError(t *testing.T) {
	requireShell(t)
	dir := workDir(t, nil)
	marker := filepath.Join(t.TempDir(), "agent-ran")
	out := filepath.Join(t.TempDir(), "run.zip")

	res := runZyg(t, nil, "record", "--agent", "touch '"+filepath.ToSlash(marker)+"'", "--dir", dir, "-o", out, "--encrypt")
	if res.code == 0 {
		t.Fatalf("record --encrypt without a key succeeded: %s", res.out)
	}
	if fileExists(out) {
		t.Fatal("output bundle was written without a key")
	}
	if fileExists(marker) {
		t.Fatal("agent ran although the encryption options were invalid")
	}

	res = runZyg(t, []string{"ZYG_PASSPHRASE=ignored-without-flag"}, "record", "--agent", cliAgent, "--dir", dir, "-o", out, "--encrypt")
	if res.code == 0 || fileExists(out) {
		t.Fatalf("env passphrase alone must not enable encryption: exit %d: %s", res.code, res.out)
	}

	res = runZyg(t, nil, "record", "--agent", cliAgent, "--dir", dir, "-o", out, "--recipient", "age1abc")
	if res.code == 0 || fileExists(out) {
		t.Fatalf("--recipient without --encrypt must fail: exit %d: %s", res.code, res.out)
	}

	res = runZyg(t, nil, "record", "--agent", cliAgent, "--dir", dir, "-o", out, "--encrypt", "--passphrase=hunter2")
	if res.code == 0 || fileExists(out) {
		t.Fatalf("a passphrase value on argv must be rejected: exit %d: %s", res.code, res.out)
	}
}

func TestRecordHidesKeyVariablesFromAgent(t *testing.T) {
	requireShell(t)
	id, _ := writeIdentity(t)
	_, res := recordBundle(t,
		[]string{"ZYG_PASSPHRASE=s3cret-pass", "ZYG_PASSPHRASE_FILE=/nonexistent/pass", "ZYG_IDENTITY_FILE=/nonexistent/id"},
		"--encrypt", "--recipient", id.Recipient().String())
	if res.code != 0 {
		t.Fatalf("record exit %d: %s", res.code, res.out)
	}
	if !strings.Contains(res.out, "ENVCHECK[][][]") {
		t.Fatalf("agent saw key variables: %s", res.out)
	}
	if strings.Contains(res.out, "s3cret-pass") {
		t.Fatalf("passphrase leaked to agent output: %s", res.out)
	}
}

func TestPlantedSecretBlocksEncryptedExport(t *testing.T) {
	requireShell(t)
	id, _ := writeIdentity(t)
	dir := workDir(t, map[string]string{"config.env": "AWS_ACCESS_KEY_ID=" + secretValue + "\n"})
	out := filepath.Join(t.TempDir(), "run.zip")

	res := runZyg(t, nil, "record", "--agent", cliAgent, "--dir", dir, "-o", out, "--encrypt", "--recipient", id.Recipient().String())
	if res.code == 0 {
		t.Fatalf("record with a planted secret succeeded: %s", res.out)
	}
	if !strings.Contains(res.out, "fail-closed") {
		t.Fatalf("expected a fail-closed redaction error: %s", res.out)
	}
	if fileExists(out) {
		t.Fatal("bundle written although redaction failed")
	}
	entries, err := os.ReadDir(filepath.Dir(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("leftover files in output directory: %d", len(entries))
	}
}

func TestPlainBundleReadsWithKeySupplied(t *testing.T) {
	requireShell(t)
	_, idFile := writeIdentity(t)
	bundle, res := recordBundle(t, nil)
	if res.code != 0 {
		t.Fatalf("record exit %d: %s", res.code, res.out)
	}
	raw, err := os.ReadFile(bundle) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("PK\x03\x04")) {
		t.Fatal("plain bundle is not a zip")
	}
	if res := runZyg(t, nil, "steps", bundle); res.code != 0 {
		t.Fatalf("plain steps exit %d: %s", res.code, res.out)
	}
	if res := runZyg(t, []string{"ZYG_PASSPHRASE=unused"}, "steps", bundle, "--identity-file", idFile); res.code != 0 {
		t.Fatalf("plain steps with key exit %d: %s", res.code, res.out)
	}
}

func TestRedactEncryption(t *testing.T) {
	requireShell(t)
	id, idFile := writeIdentity(t)
	bundle, res := recordBundle(t, nil, "--encrypt", "--recipient", id.Recipient().String())
	if res.code != 0 {
		t.Fatalf("record exit %d: %s", res.code, res.out)
	}

	plainOut := filepath.Join(t.TempDir(), "clean.zip")
	res = runZyg(t, nil, "redact", bundle, "--policy", filepath.Join(t.TempDir(), "none.yaml"), "-o", plainOut, "--identity-file", idFile)
	if res.code != 0 {
		t.Fatalf("redact exit %d: %s", res.code, res.out)
	}
	if !strings.Contains(res.out, "Warning: input bundle was encrypted") {
		t.Fatalf("expected an unencrypted-output warning: %s", res.out)
	}
	raw, err := os.ReadFile(plainOut) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("PK\x03\x04")) {
		t.Fatal("redact output should be plain without --encrypt")
	}

	encOut := filepath.Join(t.TempDir(), "clean-enc.zip")
	res = runZyg(t, nil, "redact", bundle, "--policy", filepath.Join(t.TempDir(), "none.yaml"), "-o", encOut, "--identity-file", idFile, "--encrypt", "--recipient", id.Recipient().String())
	if res.code != 0 {
		t.Fatalf("redact --encrypt exit %d: %s", res.code, res.out)
	}
	if strings.Contains(res.out, "Warning") {
		t.Fatalf("unexpected warning: %s", res.out)
	}
	raw, err = os.ReadFile(encOut) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), ageHeader) {
		t.Fatal("redact --encrypt output is not encrypted")
	}
}
