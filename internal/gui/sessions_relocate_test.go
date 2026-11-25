package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sessions"
)

func TestSessionsRelocateClaudeRoute(t *testing.T) {
	h := sandboxHome(t)
	root := filepath.Join(h, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	sessions.Reset()
	t.Cleanup(sessions.Reset)
	from, to := filepath.Join(h, "old"), filepath.Join(h, "new")
	name := regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(from, "-")
	source := filepath.Join(root, "projects", name)
	os.MkdirAll(source, 0o700)
	os.Mkdir(to, 0o700)
	const id = "11111111-2222-3333-4444-555555555555"
	b, _ := json.Marshal(map[string]any{
		"type": "user", "cwd": from, "sessionId": id,
		"timestamp": "2026-01-01T00:00:00Z", "message": map[string]string{"role": "user", "content": "hello"},
	})
	os.WriteFile(filepath.Join(source, id+".jsonl"), append(b, '\n'), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(source, id+".jsonl"), old, old)
	os.Chtimes(source, old, old)
	if got := sessions.ListAgent("claude"); len(got) != 1 || got[0].Cwd != from {
		t.Fatalf("before %+v", got)
	}
	mux := http.NewServeMux()
	sessionManageRoutes(mux, folderOnly{})
	post := func(body any) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/sessions/relocate-claude", bytes.NewReader(b)))
		return w
	}
	in := sessions.ClaudeRelocation{From: from, To: to}
	w := post(in)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var out sessions.ClaudeRelocationResult
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Token == "" || out.Backup != "" || out.Sessions != 1 {
		t.Fatalf("preview %+v", out)
	}
	in.Token = "stale"
	if w := post(in); w.Code != http.StatusBadRequest {
		t.Fatalf("stale preview accepted: %s", w.Body)
	}
	in.Token = out.Token
	w = post(in)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Backup == "" {
		t.Fatal("no recovery backup")
	}
	if got := sessions.ListAgent("claude"); len(got) != 1 || got[0].Cwd != to || got[0].ID != id {
		t.Fatalf("stale listing after relocation: %+v", got)
	}
}

func TestSessionsRelocateCodexRoute(t *testing.T) {
	h := sandboxHome(t)
	codex := filepath.Join(h, ".codex")
	t.Setenv("CODEX_HOME", codex)
	sessions.Reset()
	t.Cleanup(sessions.Reset)
	from, to := filepath.Join(h, "old-cx"), filepath.Join(h, "new-cx")
	os.MkdirAll(from, 0o700)
	os.MkdirAll(to, 0o700)
	const id = "019a5555-5555-7555-8555-555555555555"
	day := filepath.Join(codex, "sessions", "2026", "10", "01")
	os.MkdirAll(day, 0o700)
	rollout := filepath.Join(day, "rollout-2026-10-01T10-00-00-"+id+".jsonl")
	meta, _ := json.Marshal(map[string]any{
		"timestamp": "2026-10-01T10:00:00Z", "type": "session_meta",
		"payload": map[string]any{"id": id, "cwd": from},
	})
	os.WriteFile(rollout, append(meta, '\n'), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(rollout, old, old)
	if got := sessions.ListAgent("codex"); len(got) != 1 || got[0].Cwd != from {
		t.Fatalf("before %+v", got)
	}
	mux := http.NewServeMux()
	sessionManageRoutes(mux, folderOnly{})
	post := func(body any) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/sessions/relocate-codex", bytes.NewReader(b)))
		return w
	}
	in := sessions.CodexRelocation{From: from, To: to}
	w := post(in)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var out sessions.CodexRelocationResult
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Token == "" || out.Backup != "" || out.Sessions != 1 {
		t.Fatalf("preview %+v", out)
	}
	in.Token = "stale"
	if w := post(in); w.Code != http.StatusBadRequest {
		t.Fatalf("stale preview accepted: %s", w.Body)
	}
	in.Token = out.Token
	w = post(in)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Backup == "" {
		t.Fatal("no recovery backup")
	}
	if got := sessions.ListAgent("codex"); len(got) != 1 || got[0].Cwd != to || got[0].ID != id {
		t.Fatalf("stale listing after relocation: %+v", got)
	}
}
