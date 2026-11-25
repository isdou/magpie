package sessions

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/steady"
)

// CodexRelocation moves a project's Codex sessions within one Codex home
// (or its selected WSL distro). An empty Token previews without modifying files.
type CodexRelocation struct {
	From  string `json:"from"`
	To    string `json:"to"`
	WSL   string `json:"wsl,omitempty"`
	Token string `json:"token,omitempty"`
}

type CodexRelocationResult struct {
	From     string `json:"from"`
	To       string `json:"to"`
	WSL      string `json:"wsl,omitempty"`
	Sessions int    `json:"sessions"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
	Token    string `json:"token"`
	Backup   string `json:"backup,omitempty"`
}

type codexRelocationPlanFile struct {
	rel  string
	path string
	id   string
	mod  time.Time
	mode fs.FileMode
	next []byte
}

type codexStateUpdate struct {
	dbPath string
	id     string
	from   string
	to     string
}

type codexCatalogUpdate struct {
	dbPath   string
	hostID   string
	threadID string
	from     string
	to       string
}

// RelocateCodexProject moves every Codex rollout and database cwd record for a
// project directory to a new existing directory. Clients must be closed first.
func RelocateCodexProject(in CodexRelocation) (CodexRelocationResult, error) {
	if ownOff() {
		return CodexRelocationResult{}, ErrNotRead
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	root, target, err := codexRelocationRoot(in)
	if err != nil {
		return CodexRelocationResult{}, err
	}
	out, matched, err := relocateCodexProject(root, target, in)
	if err == nil && out.Backup != "" {
		loadCache()
		for _, f := range matched {
			delete(cache, f.path)
			_ = os.Remove(codexWindowPath(f.path))
		}
		if len(matched) > 0 {
			saveCacheWithin(0)
		}
		cacheGeneration++
		directoryCache.Lock()
		directoryCache.entries = map[string]directoryEntry{}
		directoryCache.Unlock()
		resetCalls()
		wslReset()
		forgetListsKept()
		forgetWarmListing()
		forgetPaged()
		forgetKeptStats()
		forgetKeptList()
	}
	return out, err
}

func codexRelocationRoot(in CodexRelocation) (string, string, error) {
	if in.WSL == "" {
		return CodexDir(), in.To, nil
	}
	return wslRelocationRoot(".codex", in.WSL, in.To)
}

func relocateCodexProject(root, target string, in CodexRelocation) (CodexRelocationResult, []codexRelocationPlanFile, error) {
	out := CodexRelocationResult{From: in.From, To: in.To, WSL: in.WSL}
	if err := validateRelocationPaths(in.From, in.To, in.WSL); err != nil {
		return out, nil, err
	}
	if in.From == in.To || (filepath.Separator == '\\' && strings.EqualFold(in.From, in.To)) {
		return out, nil, errors.New("source and target project paths are the same")
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		return out, nil, errors.New("the target project directory does not exist")
	}
	if fi, err := os.Lstat(root); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return out, nil, errors.New("Codex storage directory is missing or is a symbolic link")
	}

	candidates, err := scanCodexRelocationCandidates(root)
	if err != nil {
		return out, nil, err
	}
	matched, threadIDs, err := matchAndTransformCodexRollouts(candidates, in.From, in.To)
	if err != nil {
		return out, nil, err
	}
	if len(threadIDs) == 0 {
		return out, nil, errors.New("no Codex sessions found in this project")
	}
	stateUpdates, catalogUpdates, err := scanCodexDBUpdates(root, threadIDs, in.From, in.To)
	if err != nil {
		return out, nil, err
	}

	out.Sessions = len(threadIDs)
	out.Files = len(matched)
	hash := sha256.New()
	fmt.Fprintf(hash, "%q %q %q %q\n", root, in.From, in.To, in.WSL)
	for _, f := range matched {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			return out, nil, err
		}
		fi, err := os.Lstat(f.path)
		if err != nil {
			return out, nil, err
		}
		out.Bytes += int64(len(raw))
		fmt.Fprintf(hash, "%q %d %d %d\n", f.rel, fi.Size(), fi.ModTime().UnixNano(), fi.Mode())
		hash.Write(raw)
	}
	for _, u := range stateUpdates {
		fmt.Fprintf(hash, "state %q %q %q %q\n", u.dbPath, u.id, u.from, u.to)
	}
	for _, u := range catalogUpdates {
		fmt.Fprintf(hash, "catalog %q %q %q %q %q\n", u.dbPath, u.hostID, u.threadID, u.from, u.to)
	}
	out.Token = fmt.Sprintf("%x", hash.Sum(nil))
	if in.Token == "" {
		return out, nil, nil
	}
	if in.Token != out.Token {
		return out, nil, errors.New("project files changed; preview the relocation again")
	}

	// Acquire writer locks for every matched thread before touching files or DBs.
	for _, id := range threadIDs {
		letGo, err := holdCodexThread(root, id)
		if err != nil {
			return out, nil, err
		}
		defer letGo()
	}

	backupRoot := filepath.Join(root, "magpie-relocations")
	if err := os.MkdirAll(backupRoot, 0o700); err != nil {
		return out, nil, err
	}
	if fi, err := os.Lstat(backupRoot); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return out, nil, errors.New("backup location is not a regular directory")
	}
	backup, err := os.MkdirTemp(backupRoot, "codex-project-")
	if err != nil {
		return out, nil, err
	}
	origDir := filepath.Join(backup, "original")
	stageDir := filepath.Join(backup, "staged")
	if err := os.Mkdir(origDir, 0o700); err != nil {
		return out, nil, err
	}
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		return out, nil, err
	}

	for _, f := range matched {
		origPath := filepath.Join(origDir, f.rel)
		if err := os.MkdirAll(filepath.Dir(origPath), 0o700); err != nil {
			return out, nil, err
		}
		if err := keptCopy(f.path, origPath); err != nil {
			return out, nil, err
		}
		_ = os.Chtimes(origPath, f.mod, f.mod)

		stagePath := filepath.Join(stageDir, f.rel)
		if err := os.MkdirAll(filepath.Dir(stagePath), 0o700); err != nil {
			return out, nil, err
		}
		if err := os.WriteFile(stagePath, f.next, f.mode); err != nil {
			return out, nil, err
		}
		if err := os.Chtimes(stagePath, f.mod, f.mod); err != nil {
			return out, nil, err
		}
	}

	check := in
	check.Token = ""
	fresh, _, err := relocateCodexProject(root, target, check)
	if err != nil {
		return out, nil, err
	}
	if fresh.Token != out.Token {
		return out, nil, errors.New("project files changed during staging; originals were not modified")
	}

	manifest, _ := json.MarshalIndent(map[string]any{
		"projectFrom":  in.From,
		"projectTo":    in.To,
		"wsl":          in.WSL,
		"threads":      threadIDs,
		"original":     origDir,
		"preview":      out,
		"stateUpdates": stateUpdates,
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(backup, "manifest.json"), manifest, 0o600); err != nil {
		return out, nil, err
	}

	if err := publishCodexRelocation(matched, origDir, stageDir, stateUpdates, catalogUpdates, backup); err != nil {
		return out, nil, err
	}
	out.Backup = backup
	return out, matched, nil
}

type codexCandidateFile struct {
	rel  string
	path string
}

func scanCodexRelocationCandidates(root string) ([]codexCandidateFile, error) {
	var out []codexCandidateFile
	for _, folder := range []string{"sessions", "archived_sessions"} {
		dir := filepath.Join(root, folder)
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		err := walkLinked(dir, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if !rolloutName.MatchString(d.Name()) {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			out = append(out, codexCandidateFile{rel: rel, path: p})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

func matchAndTransformCodexRollouts(candidates []codexCandidateFile, from, to string) ([]codexRelocationPlanFile, []string, error) {
	var matched []codexRelocationPlanFile
	seenThreads := map[string]bool{}
	for _, c := range candidates {
		fi, err := os.Lstat(c.path)
		if err != nil {
			return nil, nil, err
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("%s is a symbolic link or special file", c.rel)
		}
		plain, primaryCwd, fileID, paged, err := readCodexRolloutForMatch(c.path)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", c.rel, err)
		}
		if primaryCwd == "" {
			continue
		}
		if _, ok := relocationCwd(primaryCwd, from, to); !ok {
			continue
		}
		if time.Since(fi.ModTime()) < ActiveWindow {
			return nil, nil, ErrActive
		}
		if fileID == "" {
			fileID = codexRolloutID(filepath.Base(c.path))
		}
		rewritten, changed, err := relocateCodexJSONL(plain, from, to, paged)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", filepath.Base(c.path), err)
		}
		if !changed {
			continue
		}
		nextBytes := rewritten
		if packed(c.path) {
			var buf bytes.Buffer
			zw, err := zstd.NewWriter(&buf)
			if err != nil {
				return nil, nil, err
			}
			if _, err := zw.Write(rewritten); err != nil {
				zw.Close()
				return nil, nil, err
			}
			if err := zw.Close(); err != nil {
				return nil, nil, err
			}
			nextBytes = buf.Bytes()
		}
		matched = append(matched, codexRelocationPlanFile{
			rel:  c.rel,
			path: c.path,
			id:   fileID,
			mod:  fi.ModTime(),
			mode: fi.Mode().Perm(),
			next: nextBytes,
		})
		if fileID != "" {
			seenThreads[fileID] = true
		}
	}
	ids := make([]string, 0, len(seenThreads))
	for id := range seenThreads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return matched, ids, nil
}

func readCodexRolloutForMatch(path string) (plain []byte, primaryCwd, id string, paged bool, err error) {
	rc, err := openLines(path)
	if err != nil {
		return nil, "", "", false, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", "", false, err
	}
	br := bufio.NewReader(bytes.NewReader(raw))
	for {
		line, rerr := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if !gjson.ValidBytes(bytes.TrimSpace(line)) {
				return nil, "", "", false, errors.New("invalid transcript JSON; repair it before relocating")
			}
			typ := gjson.GetBytes(line, "type").String()
			if typ == "session_meta" {
				if id == "" {
					id = gjson.GetBytes(line, "payload.id").String()
				}
				if !paged && paginated(line) {
					paged = true
				}
				if primaryCwd == "" {
					primaryCwd = gjson.GetBytes(line, "payload.cwd").String()
				}
			} else if typ == "turn_context" && primaryCwd == "" {
				primaryCwd = gjson.GetBytes(line, "payload.cwd").String()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, "", "", false, rerr
		}
	}
	return raw, primaryCwd, id, paged, nil
}

func relocateCodexJSONL(plain []byte, from, to string, paged bool) ([]byte, bool, error) {
	lines := bytes.SplitAfter(plain, []byte("\n"))
	var out bytes.Buffer
	out.Grow(len(plain))
	changed := false
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 {
			out.Write(line)
			continue
		}
		nextLine, lineChanged := relocateCodexLine(line, from, to)
		if lineChanged {
			if paged {
				fitted, ok := fitLine(line, nextLine, true)
				if !ok {
					return nil, false, errors.New("uses Codex paginated history and the new path is longer than the original; Codex keeps paginated history by byte offset")
				}
				nextLine = fitted
			}
			changed = true
		}
		out.Write(nextLine)
	}
	return out.Bytes(), changed, nil
}

func relocateCodexLine(line []byte, from, to string) ([]byte, bool) {
	typ := gjson.GetBytes(line, "type").String()
	cur := line
	changed := false
	switch typ {
	case "session_meta":
		if next, ok := spliceJSONStringField(cur, "payload.cwd", from, to); ok {
			cur = next
			changed = true
		}
		if next, ok := spliceJSONStringArray(cur, "payload.runtime_workspace_roots", from, to); ok {
			cur = next
			changed = true
		}
	case "turn_context":
		if next, ok := spliceJSONStringField(cur, "payload.cwd", from, to); ok {
			cur = next
			changed = true
		}
	case "event_msg":
		if gjson.GetBytes(cur, "payload.type").String() == "thread_settings_applied" {
			if next, ok := spliceJSONStringField(cur, "payload.thread_settings.cwd", from, to); ok {
				cur = next
				changed = true
			}
		}
	}
	return cur, changed
}

func encodeJSONStringNoEscape(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func spliceJSONStringField(line []byte, path, from, to string) ([]byte, bool) {
	r := gjson.GetBytes(line, path)
	if !r.Exists() || r.Type != gjson.String || r.Index <= 0 {
		return line, false
	}
	next, ok := relocationCwd(r.String(), from, to)
	if !ok {
		return line, false
	}
	enc := encodeJSONStringNoEscape(next)
	out := make([]byte, 0, len(line)-len(r.Raw)+len(enc))
	out = append(out, line[:r.Index]...)
	out = append(out, enc...)
	out = append(out, line[r.Index+len(r.Raw):]...)
	return out, true
}

func spliceJSONStringArray(line []byte, path, from, to string) ([]byte, bool) {
	arr := gjson.GetBytes(line, path)
	if !arr.Exists() || !arr.IsArray() {
		return line, false
	}
	elems := arr.Array()
	cur := line
	changed := false
	for i := len(elems) - 1; i >= 0; i-- {
		el := elems[i]
		if el.Type != gjson.String || el.Index <= 0 {
			continue
		}
		next, ok := relocationCwd(el.String(), from, to)
		if !ok {
			continue
		}
		enc := encodeJSONStringNoEscape(next)
		buf := make([]byte, 0, len(cur)-len(el.Raw)+len(enc))
		buf = append(buf, cur[:el.Index]...)
		buf = append(buf, enc...)
		buf = append(buf, cur[el.Index+len(el.Raw):]...)
		cur = buf
		changed = true
	}
	return cur, changed
}

func scanCodexDBUpdates(root string, threadIDs []string, from, to string) ([]codexStateUpdate, []codexCatalogUpdate, error) {
	wantIDs := map[string]bool{}
	for _, id := range threadIDs {
		wantIDs[id] = true
	}
	var stateUpdates []codexStateUpdate
	for _, p := range codexStateDBs(root) {
		db, err := sql.Open("sqlite", sqliteURL(p, "mode=ro&_pragma=busy_timeout(2000)"))
		if err != nil {
			return nil, nil, err
		}
		var n int
		err = db.QueryRow(`SELECT count(*) FROM pragma_table_info('threads') WHERE name IN ('id','cwd')`).Scan(&n)
		if err != nil || n != 2 {
			db.Close()
			continue
		}
		rows, err := db.Query(`SELECT id, cwd FROM threads`)
		if err != nil {
			db.Close()
			return nil, nil, err
		}
		for rows.Next() {
			var id, cwd string
			if rows.Scan(&id, &cwd) == nil && wantIDs[id] {
				if next, ok := relocationCwd(cwd, from, to); ok {
					stateUpdates = append(stateUpdates, codexStateUpdate{dbPath: p, id: id, from: cwd, to: next})
				}
			}
		}
		rows.Close()
		db.Close()
	}
	sort.Slice(stateUpdates, func(i, j int) bool {
		if stateUpdates[i].dbPath != stateUpdates[j].dbPath {
			return stateUpdates[i].dbPath < stateUpdates[j].dbPath
		}
		return stateUpdates[i].id < stateUpdates[j].id
	})

	var catalogUpdates []codexCatalogUpdate
	for _, p := range codexCatalogDBs(root) {
		db, err := sql.Open("sqlite", sqliteURL(p, "mode=ro&_pragma=busy_timeout(2000)"))
		if err != nil {
			return nil, nil, err
		}
		var n int
		err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('local_thread_catalog', 'local_thread_catalog_hosts')`).Scan(&n)
		if err != nil || n != 2 {
			db.Close()
			continue
		}
		var hasCwd int
		if db.QueryRow(`SELECT count(*) FROM pragma_table_info('local_thread_catalog') WHERE name IN ('host_id','thread_id','cwd')`).Scan(&hasCwd) != nil || hasCwd != 3 {
			db.Close()
			continue
		}
		rows, err := db.Query(`SELECT host_id, thread_id, cwd FROM local_thread_catalog
			WHERE host_id IN (SELECT host_id FROM local_thread_catalog_hosts WHERE host_kind = 'local')`)
		if err != nil {
			db.Close()
			return nil, nil, err
		}
		for rows.Next() {
			var hostID, threadID, cwd string
			if rows.Scan(&hostID, &threadID, &cwd) == nil && wantIDs[threadID] {
				if next, ok := relocationCwd(cwd, from, to); ok {
					catalogUpdates = append(catalogUpdates, codexCatalogUpdate{dbPath: p, hostID: hostID, threadID: threadID, from: cwd, to: next})
				}
			}
		}
		rows.Close()
		db.Close()
	}
	sort.Slice(catalogUpdates, func(i, j int) bool {
		if catalogUpdates[i].dbPath != catalogUpdates[j].dbPath {
			return catalogUpdates[i].dbPath < catalogUpdates[j].dbPath
		}
		if catalogUpdates[i].hostID != catalogUpdates[j].hostID {
			return catalogUpdates[i].hostID < catalogUpdates[j].hostID
		}
		return catalogUpdates[i].threadID < catalogUpdates[j].threadID
	})
	return stateUpdates, catalogUpdates, nil
}

func publishCodexRelocation(files []codexRelocationPlanFile, origDir, stageDir string, stateUpdates []codexStateUpdate, catalogUpdates []codexCatalogUpdate, backup string) error {
	var replaced []codexRelocationPlanFile
	restoreFiles := func() error {
		var errs []error
		for _, f := range replaced {
			if err := replaceFileFromCopy(filepath.Join(origDir, f.rel), f.path, f.mode, f.mod); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	for _, f := range files {
		if err := replaceFileFromCopy(filepath.Join(stageDir, f.rel), f.path, f.mode, f.mod); err != nil {
			rerr := restoreFiles()
			return fmt.Errorf("relocation failed (backup: %s): %w", backup, errors.Join(err, rerr))
		}
		replaced = append(replaced, f)
	}
	if err := applyCodexStateUpdates(stateUpdates); err != nil {
		rerr := restoreFiles()
		return fmt.Errorf("relocation failed (backup: %s): %w", backup, errors.Join(err, rerr))
	}
	if err := applyCodexCatalogUpdates(catalogUpdates); err != nil {
		// Roll back state DBs back to `from` as well as files.
		var undo []codexStateUpdate
		for _, u := range stateUpdates {
			undo = append(undo, codexStateUpdate{dbPath: u.dbPath, id: u.id, from: u.to, to: u.from})
		}
		_ = applyCodexStateUpdates(undo)
		rerr := restoreFiles()
		return fmt.Errorf("relocation failed (backup: %s): %w", backup, errors.Join(err, rerr))
	}
	return nil
}

func replaceFileFromCopy(src, dst string, mode fs.FileMode, mod time.Time) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".magpie-relocate-*")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := steady.Sync(tmp); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chtimes(tmp.Name(), mod, mod)
	if err := steady.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	done = true
	return nil
}

func applyCodexStateUpdates(updates []codexStateUpdate) error {
	byDB := map[string][]codexStateUpdate{}
	for _, u := range updates {
		byDB[u.dbPath] = append(byDB[u.dbPath], u)
	}
	for p, us := range byDB {
		db, err := sql.Open("sqlite", sqliteURL(p, "_pragma=busy_timeout(5000)"))
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			db.Close()
			return err
		}
		for _, u := range us {
			if _, err := tx.Exec(`UPDATE threads SET cwd = ? WHERE id = ? AND cwd = ?`, u.to, u.id, u.from); err != nil {
				tx.Rollback()
				db.Close()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			db.Close()
			return err
		}
		db.Close()
	}
	return nil
}

func applyCodexCatalogUpdates(updates []codexCatalogUpdate) error {
	byDB := map[string][]codexCatalogUpdate{}
	for _, u := range updates {
		byDB[u.dbPath] = append(byDB[u.dbPath], u)
	}
	ctx := context.Background()
	for p, us := range byDB {
		db, err := sql.Open("sqlite", sqliteURL(p, "_pragma=busy_timeout(5000)"))
		if err != nil {
			return err
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			db.Close()
			return err
		}
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			conn.Close()
			db.Close()
			return err
		}
		committed := false
		for _, u := range us {
			if _, err := conn.ExecContext(ctx, `UPDATE local_thread_catalog SET cwd = ? WHERE host_id = ? AND thread_id = ? AND cwd = ?`, u.to, u.hostID, u.threadID, u.from); err != nil {
				conn.ExecContext(ctx, `ROLLBACK`)
				conn.Close()
				db.Close()
				return err
			}
		}
		bumpCatalogRevision(ctx, conn)
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			conn.Close()
			db.Close()
			return err
		}
		committed = true
		_ = committed
		conn.Close()
		db.Close()
	}
	return nil
}
