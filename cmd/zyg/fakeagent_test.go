package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	b64Ok  = "b2s="
	b64Err = "ZXJy"
)

type zygSplit struct {
	stdout string
	stderr string
	code   int
}

func runZygSplit(t testing.TB, env []string, args ...string) zygSplit {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), zygBinary, args...) //nolint:gosec
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := zygSplit{stdout: stdout.String(), stderr: stderr.String()}
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

// fakeAgent writes a shell agent speaking the harness protocol and returns its command line.
// Each op is one of: effect|op|key|b64value, big|op|key, step|name, failif|b64, exit|n.
func fakeAgent(t testing.TB, ops ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("rpc() { printf '%s\\n' \"$1\"; IFS= read -r _line; }\n")
	id := 1
	b.WriteString(`rpc '{"jsonrpc":"2.0","id":1,"method":"handshake","params":{"version":1}}'` + "\n")
	for _, op := range ops {
		parts := strings.Split(op, "|")
		id++
		switch parts[0] {
		case "effect":
			fmt.Fprintf(&b, "rpc '{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"effect\",\"params\":{\"op\":\"%s\",\"key\":\"%s\",\"value\":\"%s\"}}'\n", id, parts[1], parts[2], parts[3])
		case "big":
			b.WriteString("big=$(head -c 40000 /dev/zero | base64 | tr -d '\\n')\n")
			fmt.Fprintf(&b, "rpc \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":%d,\\\"method\\\":\\\"effect\\\",\\\"params\\\":{\\\"op\\\":\\\"%s\\\",\\\"key\\\":\\\"%s\\\",\\\"value\\\":\\\"$big\\\"}}\"\n", id, parts[1], parts[2])
		case "step":
			fmt.Fprintf(&b, "rpc '{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"step\",\"params\":{\"name\":\"%s\"}}'\n", id, parts[1])
		case "failif":
			fmt.Fprintf(&b, "if [ \"$ZYG_TEST_FAIL\" = 1 ]; then case \"$_line\" in *%s*) exit 4;; esac; fi\n", parts[1])
		case "exit":
			fmt.Fprintf(&b, "exit %s\n", parts[1])
		default:
			t.Fatalf("unknown fake agent op %q", op)
		}
	}
	path := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return "sh '" + filepath.ToSlash(path) + "'"
}

func recordFake(t testing.TB, env []string, agent string, extra ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "run.zip")
	args := append([]string{"record", "--agent", agent, "--dir", workDir(t, nil), "-o", out}, extra...)
	if res := runZyg(t, env, args...); res.code != 0 {
		t.Fatalf("record exit %d: %s", res.code, res.out)
	}
	return out
}

func copyFile(t testing.TB, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil { //nolint:gosec
		t.Fatal(err)
	}
}

func makeCorpus(t testing.TB, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		copyFile(t, src, filepath.Join(dir, name))
	}
	return dir
}

func writeGarbage(t testing.TB, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("this is not a zip archive"), 0o600); err != nil {
		t.Fatal(err)
	}
}
