package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agentenv"
	"github.com/yetone/magpie/internal/sessions"
)

func TestRelocateCLIArguments(t *testing.T) {
	for _, sub := range []string{"relocate-claude", "relocate-codex"} {
		for _, args := range [][]string{
			nil, {"--from"}, {"--from", "/old"}, {"--to", "/new"},
			{"--from", "/old", "--to", "/new", "--confirm", ""},
			{"--unknown", "x"},
		} {
			var out bytes.Buffer
			err := relocateProjectTo(&out, args, sub, func(_, _, _ string) (any, error) { return nil, nil })
			if err == nil || !strings.Contains(err.Error(), "usage: magpie sessions "+sub) || out.Len() != 0 {
				t.Fatalf("%s %v: %v, %s", sub, args, err, out.String())
			}
		}
	}
}

func TestRelocateCodexCLIExecution(t *testing.T) {
	tmp := t.TempDir()
	for _, env := range agentenv.Vars {
		t.Setenv(env, "")
	}
	codex := filepath.Join(tmp, "codex")
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	from, to := filepath.Join(tmp, "from-proj"), filepath.Join(tmp, "to-proj")
	if err := os.MkdirAll(from, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "019a6666-6666-7666-8666-666666666666"
	day := filepath.Join(codex, "sessions", "2026", "10", "03")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(day, "rollout-2026-10-03T10-00-00-"+id+".jsonl")
	line, _ := json.Marshal(map[string]any{
		"timestamp": "2026-10-03T10:00:00Z", "type": "session_meta",
		"payload": map[string]any{"id": id, "cwd": from},
	})
	if err := os.WriteFile(p, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(p, old, old)
	sessions.Reset()
	t.Cleanup(sessions.Reset)

	var prevOut bytes.Buffer
	err := relocateProjectTo(&prevOut, []string{"--from", from, "--to", to}, "relocate-codex", func(f, d, tok string) (any, error) {
		return sessions.RelocateCodexProject(sessions.CodexRelocation{From: f, To: d, Token: tok})
	})
	if err != nil {
		t.Fatalf("preview cli: %v", err)
	}
	var prev sessions.CodexRelocationResult
	if err := json.Unmarshal(prevOut.Bytes(), &prev); err != nil || prev.Token == "" || prev.Backup != "" || prev.Sessions != 1 {
		t.Fatalf("preview json: %v %s", err, prevOut.String())
	}

	var confOut bytes.Buffer
	err = relocateProjectTo(&confOut, []string{"--from", from, "--to", to, "--confirm", prev.Token}, "relocate-codex", func(f, d, tok string) (any, error) {
		return sessions.RelocateCodexProject(sessions.CodexRelocation{From: f, To: d, Token: tok})
	})
	if err != nil {
		t.Fatalf("confirm cli: %v", err)
	}
	var conf sessions.CodexRelocationResult
	if err := json.Unmarshal(confOut.Bytes(), &conf); err != nil || conf.Backup == "" || conf.To != to {
		t.Fatalf("confirm json: %v %s", err, confOut.String())
	}
}
