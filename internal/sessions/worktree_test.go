package sessions

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func initFakeRepo(t *testing.T, repo string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ".git", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func initLinkedWorktree(t *testing.T, repo, wtName, wtDir string, relative bool) {
	t.Helper()
	initFakeRepo(t, repo)
	gitdir := filepath.Join(repo, ".git", "worktrees", wtName)
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := gitdir
	if relative {
		rel, err := filepath.Rel(wtDir, gitdir)
		if err != nil {
			t.Fatal(err)
		}
		target = filepath.ToSlash(rel)
	}
	if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte("gitdir: "+target+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeOfGitWorktreeAndSubdir(t *testing.T) {
	setup(t)
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "code", "magpie")
	initFakeRepo(t, repo)
	if got := worktreeOf(repo, ""); got != "" {
		t.Fatalf("main checkout worktreeOf = %q, want empty", got)
	}
	subMain := filepath.Join(repo, "internal", "sessions")
	if err := os.MkdirAll(subMain, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := worktreeOf(subMain, ""); got != "" {
		t.Fatalf("main checkout subdir worktreeOf = %q, want empty", got)
	}

	bbWT := filepath.Join(tmp, "home", ".bb-machines", "devbox", "plugins", "environment-git-worktree", "host-data", "worktrees", "thr_abc-0", "magpie")
	initLinkedWorktree(t, repo, "thr_abc-0", bbWT, false)
	bbSub := filepath.Join(bbWT, "pkg", "ui")
	if err := os.MkdirAll(bbSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := worktreeOf(bbWT, ""); got != repo {
		t.Fatalf("bb worktree = %q, want %q", got, repo)
	}
	if got := worktreeOf(bbSub, ""); got != repo {
		t.Fatalf("bb worktree subdir = %q, want %q", got, repo)
	}

	relWT := filepath.Join(tmp, "code", "magpie-rel")
	initLinkedWorktree(t, repo, "rel-wt", relWT, true)
	if got := worktreeOf(relWT, ""); got != repo {
		t.Fatalf("relative gitdir worktree = %q, want %q", got, repo)
	}
}

func TestWorktreeOfDeletedFallbacks(t *testing.T) {
	setup(t)
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "code", "app")
	initFakeRepo(t, repo)

	// Deleted Claude Code worktree inside <repo>/.claude/worktrees/<name>
	// falls back to <repo> while <repo>/.git still exists.
	deletedClaude := filepath.Join(repo, ".claude", "worktrees", "agent-gone")
	if got := worktreeOf(deletedClaude, ""); got != repo {
		t.Fatalf("deleted claude worktree = %q, want %q", got, repo)
	}
	// A deleted plain subdirectory inside <repo> is still the main checkout, not a worktree.
	if got := worktreeOf(filepath.Join(repo, "deleted-pkg"), ""); got != "" {
		t.Fatalf("deleted normal subdir = %q, want empty", got)
	}

	// Deleted bb worktree has no .git pointer left on disk, so it stays ""
	// and groups as a bb draft in the UI rather than pretending to resolve.
	deletedBB := filepath.Join(tmp, "home", ".bb-machines", "srv", "plugins", "environment-git-worktree", "host-data", "worktrees", "thr_dead-0", "app")
	if got := worktreeOf(deletedBB, ""); got != "" {
		t.Fatalf("deleted bb worktree = %q, want empty", got)
	}
}

func TestWorktreeOfGuards(t *testing.T) {
	setup(t)
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "code", "app")
	initFakeRepo(t, repo)

	// Bare repo worktree has common dir not named .git: no main checkout.
	bare := filepath.Join(tmp, "bare.git")
	bareWT := filepath.Join(tmp, "bare-wt")
	bareGitDir := filepath.Join(bare, "worktrees", "feat")
	if err := os.MkdirAll(bareGitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bareWT, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bareWT, ".git"), []byte("gitdir: "+bareGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bareGitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := worktreeOf(bareWT, ""); got != "" {
		t.Fatalf("bare repo worktree = %q, want empty", got)
	}

	// Symlink .git file is skipped.
	symWT := filepath.Join(tmp, "sym-wt")
	targetGit := filepath.Join(tmp, "real-git-ptr")
	realGD := filepath.Join(repo, ".git", "worktrees", "sym")
	if err := os.MkdirAll(realGD, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(symWT, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetGit, []byte("gitdir: "+realGD+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetGit, filepath.Join(symWT, ".git")); err == nil {
		if got := worktreeOf(symWT, ""); got != "" {
			t.Fatalf("symlink .git = %q, want empty", got)
		}
	}
}

func TestWorktreeOfWSLAndListIntegration(t *testing.T) {
	home, _ := wslDistro(t)
	linuxRepo := "/home/me/code/proj"
	hostRepo := filepath.Join(home, "code", "proj")
	linuxWT := "/home/me/.bb-machines/srv1/plugins/environment-git-worktree/host-data/worktrees/thr_123-0/proj"
	hostWT := filepath.Join(home, ".bb-machines", "srv1", "plugins", "environment-git-worktree", "host-data", "worktrees", "thr_123-0", "proj")
	initFakeRepo(t, hostRepo)
	gitdir := filepath.Join(hostRepo, ".git", "worktrees", "thr_123-0")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hostWT, 0o755); err != nil {
		t.Fatal(err)
	}
	// Git inside WSL writes Linux paths into .git's gitdir pointer.
	if err := os.WriteFile(filepath.Join(hostWT, ".git"), []byte("gitdir: "+linuxRepo+"/.git/worktrees/thr_123-0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := worktreeOf(linuxWT, "Ubuntu"); got != linuxRepo {
		t.Fatalf("WSL worktreeOf = %q, want %q", got, linuxRepo)
	}

	// Verify List and ListAgent both populate WorktreeOf on local sessions too.
	claudeDir, _ := setup(t)
	tmp := t.TempDir()
	localRepo := filepath.Join(tmp, "repo")
	localWT := filepath.Join(tmp, "home", ".codex", "worktrees", "aa11", "repo")
	initLinkedWorktree(t, localRepo, "wt-1", localWT, false)
	projDir := filepath.Join(claudeDir, "projects", "-tmp-wt-1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	line := `{"type":"user","cwd":` + `"` + filepath.ToSlash(localWT) + `"` + `,"sessionId":"22222222-2222-4222-8222-222222222222","timestamp":"` + now + `","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(projDir, "22222222-2222-4222-8222-222222222222.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	Reset()
	found := false
	for _, s := range List(0) {
		if s.ID == "22222222-2222-4222-8222-222222222222" {
			found = true
			if s.WorktreeOf != filepath.Clean(filepath.ToSlash(localRepo)) && s.WorktreeOf != localRepo {
				t.Fatalf("List WorktreeOf = %q, want %q", s.WorktreeOf, localRepo)
			}
		}
	}
	if !found {
		t.Fatal("test claude worktree session missing from List")
	}
	managedFound := false
	for _, m := range ListAgent("claude") {
		if m.ID == "22222222-2222-4222-8222-222222222222" {
			managedFound = true
			if m.WorktreeOf != localRepo {
				t.Fatalf("ListAgent WorktreeOf = %q, want %q", m.WorktreeOf, localRepo)
			}
		}
	}
	if !managedFound {
		t.Fatal("test claude worktree session missing from ListAgent")
	}
}
