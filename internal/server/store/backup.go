package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Backup writes a consistent single-file copy of the database at dbPath to
// out, using VACUUM INTO. It is safe while the server is running: it reads
// in its own transaction and never blocks ingest for long.
func Backup(ctx context.Context, dbPath, out string) error {
	if _, err := os.Stat(dbPath); err != nil {
		return err
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("backup: %s already exists", out)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?"+url.Values{"_pragma": {"busy_timeout(5000)"}}.Encode())
	if err != nil {
		return err
	}
	defer db.Close()

	tmp := out + ".tmp"
	os.Remove(tmp)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, out)
}

const backupPrefix, backupSuffix = "probe-", ".db"

// DailyBackup writes today's backup into dir unless one exists, then keeps
// only the newest keep files.
func (s *Store) DailyBackup(ctx context.Context, dir string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := filepath.Join(dir, backupPrefix+s.now().In(s.loc).Format("20060102")+backupSuffix)
	made := ""
	if _, err := os.Stat(name); os.IsNotExist(err) {
		if err := Backup(ctx, s.path, name); err != nil {
			return "", err
		}
		made = name
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return made, err
	}
	var old []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(n, backupPrefix) && strings.HasSuffix(n, backupSuffix) {
			old = append(old, n)
		}
	}
	sort.Strings(old) // date in the name sorts chronologically
	for len(old) > keep {
		if err := os.Remove(filepath.Join(dir, old[0])); err != nil {
			return made, err
		}
		old = old[1:]
	}
	return made, nil
}
