package sessions

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func setupCodexRelocationDBs(t *testing.T, codex, id, cwd string) (stateDB, catalogDB string) {
	t.Helper()
	stateDB = filepath.Join(codex, "state_5.sqlite")
	sdb, err := sql.Open("sqlite", stateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	if _, err := sdb.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL, model_provider TEXT NOT NULL, cwd TEXT NOT NULL, title TEXT NOT NULL, archived INTEGER NOT NULL DEFAULT 0);
		INSERT INTO threads VALUES (?, 'x', 'openai', ?, 'fix bug', 0), ('other', 'y', 'openai', '/other/repo', 'elsewhere', 0)`, id, cwd); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(codex, "sqlite"), 0o755); err != nil {
		t.Fatal(err)
	}
	catalogDB = filepath.Join(codex, "sqlite", "codex.db")
	cdb, err := sql.Open("sqlite", catalogDB)
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	if _, err := cdb.Exec(`
		CREATE TABLE local_thread_catalog_hosts (host_id TEXT PRIMARY KEY, host_kind TEXT NOT NULL);
		CREATE TABLE local_thread_catalog_metadata (id INTEGER PRIMARY KEY, catalog_revision INTEGER NOT NULL);
		CREATE TABLE local_thread_catalog (host_id TEXT NOT NULL, thread_id TEXT NOT NULL, cwd TEXT NOT NULL);
		INSERT INTO local_thread_catalog_hosts VALUES ('h1', 'local');
		INSERT INTO local_thread_catalog_metadata VALUES (1, 10);
		INSERT INTO local_thread_catalog VALUES ('h1', ?, ?), ('h1', 'other', '/other/repo')`, id, cwd); err != nil {
		t.Fatal(err)
	}
	return stateDB, catalogDB
}

func TestRelocateCodexProjectLegacyAndZstAndDB(t *testing.T) {
	_, codex := setup(t)
	tmp := t.TempDir()
	from := filepath.Join(tmp, "old-app")
	to := filepath.Join(tmp, "moved-project-app")
	if err := os.MkdirAll(from, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}

	const legacyID = "019a1111-1111-7111-8111-111111111111"
	const packedID = "019a2222-2222-7222-8222-222222222222"
	const otherID = "019a3333-3333-7333-8333-333333333333"
	oldTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)

	day := filepath.Join(codex, "sessions", "2026", "10", "01")
	arch := filepath.Join(codex, "archived_sessions")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(arch, 0o755); err != nil {
		t.Fatal(err)
	}

	legacyPath := filepath.Join(day, "rollout-2026-10-01T10-00-00-"+legacyID+".jsonl")
	fromQ := strconv.Quote(from)
	subQ := strconv.Quote(filepath.Join(from, "sub"))
	toQ := strconv.Quote(to)
	legacyBody := `{"timestamp":"2026-10-01T10:00:00Z","type":"session_meta","payload":{"id":"` + legacyID + `","cwd":` + fromQ + `,"runtime_workspace_roots":[` + fromQ + `]}}` + "\n" +
		`{"timestamp":"2026-10-01T10:00:01Z","type":"turn_context","payload":{"turn_id":"t1","cwd":` + subQ + `,"model":"gpt-5.5"}}` + "\n" +
		`{"timestamp":"2026-10-01T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Keep historical text ` + fromQ[1:len(fromQ)-1] + `"}]}}` + "\n"
	if err := os.WriteFile(legacyPath, []byte(legacyBody), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(legacyPath, oldTime, oldTime)

	packedPath := filepath.Join(arch, "rollout-2026-09-01T10-00-00-"+packedID+".jsonl.zst")
	packedPlain := `{"timestamp":"2026-09-01T10:00:00Z","type":"session_meta","payload":{"id":"` + packedID + `","cwd":` + fromQ + `}}` + "\n"
	zw, _ := zstd.NewWriter(nil)
	if err := os.WriteFile(packedPath, zw.EncodeAll([]byte(packedPlain), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(packedPath, oldTime, oldTime)

	otherPath := filepath.Join(day, "rollout-2026-10-01T10-05-00-"+otherID+".jsonl")
	if err := os.WriteFile(otherPath, []byte(`{"timestamp":"2026-10-01T10:05:00Z","type":"session_meta","payload":{"id":"`+otherID+`","cwd":"/other/repo"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(otherPath, oldTime, oldTime)

	stateDB, catalogDB := setupCodexRelocationDBs(t, codex, legacyID, from)
	Reset()

	in := CodexRelocation{From: from, To: to}
	prev, err := RelocateCodexProject(in)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.Sessions != 2 || prev.Files != 2 || prev.Token == "" || prev.Backup != "" {
		t.Fatalf("unexpected preview: %+v", prev)
	}

	// Bad/stale confirmation token is rejected without touching files.
	if _, err := RelocateCodexProject(CodexRelocation{From: in.From, To: in.To, Token: "stale"}); err == nil {
		t.Fatal("stale token accepted")
	}

	in.Token = prev.Token
	res, err := RelocateCodexProject(in)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if res.Backup == "" || res.Sessions != 2 || res.Files != 2 {
		t.Fatalf("unexpected confirm result: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(res.Backup, "manifest.json")); err != nil {
		t.Fatalf("missing manifest: %v", err)
	}

	// Rollouts updated; historical message text preserved; unrelated rollout untouched.
	gotLegacy, _ := os.ReadFile(legacyPath)
	toSubQ := strconv.Quote(filepath.Join(to, "sub"))
	if !strings.Contains(string(gotLegacy), `"cwd":`+toQ) ||
		!strings.Contains(string(gotLegacy), `"runtime_workspace_roots":[`+toQ+`]`) ||
		!strings.Contains(string(gotLegacy), `"cwd":`+toSubQ) ||
		!strings.Contains(string(gotLegacy), `Keep historical text `+fromQ[1:len(fromQ)-1]) {
		t.Fatalf("legacy rollout after move:\n%s", gotLegacy)
	}

	rc, err := openLines(packedPath)
	if err != nil {
		t.Fatal(err)
	}
	var packedBuf bytes.Buffer
	_, _ = packedBuf.ReadFrom(rc)
	rc.Close()
	if !strings.Contains(packedBuf.String(), `"cwd":`+toQ) {
		t.Fatalf("packed rollout after move:\n%s", packedBuf.String())
	}

	gotOther, _ := os.ReadFile(otherPath)
	if !strings.Contains(string(gotOther), `"/other/repo"`) {
		t.Fatalf("unrelated rollout modified: %s", gotOther)
	}

	// SQLite state_5 and codex.db catalog updated; unrelated rows untouched.
	sdb, _ := sql.Open("sqlite", stateDB)
	defer sdb.Close()
	var scwd, ocwd string
	_ = sdb.QueryRow(`SELECT cwd FROM threads WHERE id = ?`, legacyID).Scan(&scwd)
	_ = sdb.QueryRow(`SELECT cwd FROM threads WHERE id = 'other'`).Scan(&ocwd)
	if scwd != in.To || ocwd != "/other/repo" {
		t.Fatalf("state db cwds = (%q, %q)", scwd, ocwd)
	}
	cdb, _ := sql.Open("sqlite", catalogDB)
	defer cdb.Close()
	var ccwd string
	var rev int
	_ = cdb.QueryRow(`SELECT cwd FROM local_thread_catalog WHERE thread_id = ?`, legacyID).Scan(&ccwd)
	_ = cdb.QueryRow(`SELECT catalog_revision FROM local_thread_catalog_metadata WHERE id = 1`).Scan(&rev)
	if ccwd != in.To || rev != 11 {
		t.Fatalf("catalog db cwd=%q rev=%d", ccwd, rev)
	}

	// Listing cache invalidated cleanly.
	if got := CodexThreadCwd(legacyID); got != in.To {
		t.Fatalf("CodexThreadCwd after relocate = %q, want %q", got, in.To)
	}
	for _, s := range ListAgent("codex") {
		if s.ID == legacyID && s.Cwd != in.To {
			t.Fatalf("ListAgent Cwd = %q, want %q", s.Cwd, in.To)
		}
	}
}

func TestRelocateCodexPaginatedByteConstraintAndGuards(t *testing.T) {
	_, codex := setup(t)
	tmp := t.TempDir()
	from := filepath.Join(tmp, "old-long-name")
	shorter := filepath.Join(tmp, "app")
	longer := filepath.Join(tmp, "much-longer-destination-name")
	for _, d := range []string{from, shorter, longer} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	const pagedID = "019a4444-4444-7444-8444-444444444444"
	day := filepath.Join(codex, "sessions", "2026", "10", "02")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(day, "rollout-2026-10-02T10-00-00-"+pagedID+".jsonl")
	line1 := `{"timestamp":"2026-10-02T10:00:00Z","ordinal":0,"type":"session_meta","payload":{"id":"` + pagedID + `","cwd":` + strconv.Quote(from) + `,"history_mode":"paginated"}}` + "\n"
	line2 := `{"timestamp":"2026-10-02T10:00:01Z","ordinal":1,"type":"turn_context","payload":{"turn_id":"t1","cwd":` + strconv.Quote(from) + `}}` + "\n"
	if err := os.WriteFile(p, []byte(line1+line2), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	_ = os.Chtimes(p, oldTime, oldTime)
	Reset()

	// Longer destination is refused for paginated rollouts because Codex keeps byte offsets.
	if _, err := RelocateCodexProject(CodexRelocation{From: from, To: longer}); err == nil {
		t.Fatal("expected paginated longer destination to be refused")
	}

	// Shorter destination succeeds while preserving exact byte length per line.
	prev, err := RelocateCodexProject(CodexRelocation{From: from, To: shorter})
	if err != nil {
		t.Fatalf("paginated shorter preview: %v", err)
	}
	if _, err := RelocateCodexProject(CodexRelocation{From: from, To: shorter, Token: prev.Token}); err != nil {
		t.Fatalf("paginated shorter confirm: %v", err)
	}
	after, _ := os.ReadFile(p)
	if len(after) != len(line1+line2) {
		t.Fatalf("paginated file byte length changed: got %d, want %d", len(after), len(line1+line2))
	}
	afterLines := strings.Split(strings.TrimSuffix(string(after), "\n"), "\n")
	if len(afterLines[0]) != len(strings.TrimSuffix(line1, "\n")) || len(afterLines[1]) != len(strings.TrimSuffix(line2, "\n")) {
		t.Fatalf("paginated per-line byte length changed")
	}

	// Active window guard: recently modified session is refused with ErrActive.
	_ = os.Chtimes(p, time.Now(), time.Now())
	if _, err := RelocateCodexProject(CodexRelocation{From: shorter, To: from}); err != ErrActive {
		t.Fatalf("expected ErrActive for recent file, got %v", err)
	}
}
