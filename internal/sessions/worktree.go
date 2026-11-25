package sessions

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

const maxGitPointerBytes = 4096

type worktreeKey struct {
	cwd, wsl string
}

var worktreeMemo = struct {
	sync.Mutex
	m map[worktreeKey]string
}{
	m: map[worktreeKey]string{},
}

func forgetWorktrees() {
	worktreeMemo.Lock()
	clear(worktreeMemo.m)
	worktreeMemo.Unlock()
}

// worktreeOf returns the main repository folder when cwd is inside a linked
// git worktree, or "" otherwise.
func worktreeOf(cwd, wsl string) string {
	if cwd == "" {
		return ""
	}
	if wsl != "" && !wslUp(wsl) {
		return ""
	}
	k := worktreeKey{cwd: cwd, wsl: wsl}
	worktreeMemo.Lock()
	v, ok := worktreeMemo.m[k]
	worktreeMemo.Unlock()
	if ok {
		return v
	}
	v = resolveWorktreeOf(cwd, wsl)
	worktreeMemo.Lock()
	if len(worktreeMemo.m) >= 4096 {
		clear(worktreeMemo.m)
	}
	worktreeMemo.m[k] = v
	worktreeMemo.Unlock()
	return v
}

func resolveWorktreeOf(cwd, wsl string) string {
	var mapper *wslPathMap
	if wsl != "" {
		mapper = newWSLPathMap(wsl)
		if mapper == nil {
			return ""
		}
		if main := worktreeByGitFile(cwd, mapper); main != "" {
			return main
		}
		return claudeDeletedWorktreeMain(cwd, mapper)
	}
	if !filepath.IsAbs(cwd) {
		return ""
	}
	if main := worktreeByGitFile(cwd, nil); main != "" {
		return main
	}
	return claudeDeletedWorktreeMain(cwd, nil)
}

func worktreeByGitFile(cwd string, wsl *wslPathMap) string {
	dir := cwd
	if wsl != nil {
		var ok bool
		dir, ok = wsl.toHost(cwd)
		if !ok {
			return ""
		}
	} else {
		dir = filepath.Clean(cwd)
	}

	for d := dir; ; {
		g := filepath.Join(d, ".git")
		fi, err := os.Lstat(g)
		if err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				return ""
			}
			if fi.IsDir() {
				// Inside the main checkout (or a deleted subdirectory of it).
				return ""
			}
			if !fi.Mode().IsRegular() || fi.Size() <= 0 || fi.Size() > maxGitPointerBytes {
				return ""
			}
			gitdir, ok := readGitDirFile(g, d, wsl)
			if !ok {
				return ""
			}
			common, ok := worktreeCommonDir(gitdir, wsl)
			if !ok {
				return ""
			}
			if filepath.Base(common) != ".git" {
				return ""
			}
			mainHost := filepath.Dir(common)
			if mainHost == d {
				return ""
			}
			mfi, err := os.Lstat(mainHost)
			if err != nil || !mfi.IsDir() || mfi.Mode()&os.ModeSymlink != 0 {
				return ""
			}
			if wsl != nil {
				return wsl.toLinux(mainHost)
			}
			return mainHost
		}
		up := filepath.Dir(d)
		if up == d {
			return ""
		}
		d = up
	}
}

func readGitDirFile(gitFile, worktreeDir string, wsl *wslPathMap) (string, bool) {
	b, err := os.ReadFile(gitFile)
	if err != nil || len(b) > maxGitPointerBytes {
		return "", false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	raw, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "", false
	}
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", false
	}
	var host string
	if wsl != nil {
		if strings.HasPrefix(target, "/") {
			var mapped bool
			host, mapped = wsl.toHost(target)
			if !mapped {
				return "", false
			}
		} else {
			host = filepath.Join(worktreeDir, filepath.FromSlash(target))
		}
	} else if filepath.IsAbs(target) {
		host = filepath.Clean(target)
	} else {
		host = filepath.Join(worktreeDir, target)
	}
	fi, err := os.Lstat(host)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	return host, true
}

func worktreeCommonDir(gitdir string, wsl *wslPathMap) (string, bool) {
	cf := filepath.Join(gitdir, "commondir")
	if fi, err := os.Lstat(cf); err == nil {
		if !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 || fi.Size() <= 0 || fi.Size() > maxGitPointerBytes {
			return "", false
		}
		b, err := os.ReadFile(cf)
		if err != nil {
			return "", false
		}
		c := strings.TrimSpace(string(b))
		if c == "" {
			return "", false
		}
		var common string
		if wsl != nil {
			if strings.HasPrefix(c, "/") {
				var ok bool
				common, ok = wsl.toHost(c)
				if !ok {
					return "", false
				}
			} else {
				common = filepath.Join(gitdir, filepath.FromSlash(c))
			}
		} else if filepath.IsAbs(c) {
			common = filepath.Clean(c)
		} else {
			common = filepath.Join(gitdir, c)
		}
		cfi, err := os.Lstat(common)
		if err != nil || !cfi.IsDir() || cfi.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
		return common, true
	}
	if filepath.Base(filepath.Dir(gitdir)) != "worktrees" {
		return "", false
	}
	common := filepath.Dir(filepath.Dir(gitdir))
	cfi, err := os.Lstat(common)
	if err != nil || !cfi.IsDir() || cfi.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	return common, true
}

// claudeDeletedWorktreeMain recovers the main repository when Claude Code ran in
// <mainRepo>/.claude/worktrees/<name> and that worktree was later removed.
func claudeDeletedWorktreeMain(cwd string, wsl *wslPathMap) string {
	slash := filepath.ToSlash(cwd)
	idx := strings.Index(slash, "/.claude/worktrees/")
	if idx <= 0 {
		return ""
	}
	rest := slash[idx+len("/.claude/worktrees/"):]
	name, _, _ := strings.Cut(rest, "/")
	if name == "" || name == "." || name == ".." {
		return ""
	}
	mainRaw := cwd[:idx]
	var mainHost string
	if wsl != nil {
		var ok bool
		mainHost, ok = wsl.toHost(mainRaw)
		if !ok {
			return ""
		}
	} else {
		mainHost = filepath.Clean(mainRaw)
	}
	gitDir := filepath.Join(mainHost, ".git")
	fi, err := os.Lstat(gitDir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	if wsl != nil {
		return path.Clean(filepath.ToSlash(mainRaw))
	}
	return mainHost
}

type wslPathMap struct {
	hostHome  string
	linuxHome string
	hostRoot  string
}

func newWSLPathMap(distro string) *wslPathMap {
	hostHome := wslHomeOf(distro)
	if hostHome == "" && WSLHomes != nil {
		for _, h := range WSLHomes() {
			if h.Distro == distro {
				hostHome = h.Home
				break
			}
		}
	}
	if hostHome == "" {
		return nil
	}
	linuxHome, hostRoot := splitWSLHomeRoot(hostHome)
	return &wslPathMap{
		hostHome:  filepath.Clean(hostHome),
		linuxHome: linuxHome,
		hostRoot:  hostRoot,
	}
}

func splitWSLHomeRoot(hostHome string) (linuxHome, hostRoot string) {
	slash := filepath.ToSlash(filepath.Clean(hostHome))
	vol := filepath.VolumeName(hostHome)
	if vol != "" {
		// On Windows UNC paths (\\wsl.localhost\Ubuntu\home\me), VolumeName is \\wsl.localhost\Ubuntu.
		trimmed := strings.TrimPrefix(slash, filepath.ToSlash(vol))
		if strings.HasPrefix(trimmed, "/") {
			return trimmed, vol
		}
	}
	if i := strings.LastIndex(slash, "/home/"); i >= 0 {
		rest := slash[i+len("/home/"):]
		if rest != "" && !strings.Contains(rest, "/") {
			return "/home/" + rest, filepath.FromSlash(slash[:i])
		}
	}
	if strings.HasSuffix(slash, "/root") {
		return "/root", filepath.FromSlash(strings.TrimSuffix(slash, "/root"))
	}
	return "", ""
}

func (m *wslPathMap) toHost(linuxPath string) (string, bool) {
	p := path.Clean(filepath.ToSlash(linuxPath))
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	if m.linuxHome != "" && (p == m.linuxHome || strings.HasPrefix(p, m.linuxHome+"/")) {
		rel := strings.TrimPrefix(p, m.linuxHome)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return m.hostHome, true
		}
		return filepath.Join(m.hostHome, filepath.FromSlash(rel)), true
	}
	if m.hostRoot != "" {
		return filepath.Join(m.hostRoot, filepath.FromSlash(strings.TrimPrefix(p, "/"))), true
	}
	return "", false
}

func (m *wslPathMap) toLinux(hostPath string) string {
	clean := filepath.Clean(hostPath)
	if m.linuxHome != "" {
		rel, err := filepath.Rel(m.hostHome, clean)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if rel == "." {
				return m.linuxHome
			}
			return path.Join(m.linuxHome, filepath.ToSlash(rel))
		}
	}
	if m.hostRoot != "" {
		rel, err := filepath.Rel(m.hostRoot, clean)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if rel == "." {
				return "/"
			}
			return "/" + filepath.ToSlash(rel)
		}
	}
	return ""
}
