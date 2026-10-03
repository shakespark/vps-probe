package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := Replace(path, []byte("x")); err == nil {
		t.Fatal("replaced a file that does not exist")
	}
	if err := Write(path, []byte("one"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Replace(path, []byte("two")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if string(b) != "two" || st.Mode().Perm() != 0o640 {
		t.Fatalf("content %q, mode %v", b, st.Mode().Perm())
	}
	if err := Write(path, []byte("three"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v after Write", st.Mode().Perm())
	}
	// No temporary files are left behind.
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("%d files in the directory", len(entries))
	}
	if err := Write(filepath.Join(dir, "missing", "f"), nil, 0o600); err == nil {
		t.Fatal("wrote into a directory that does not exist")
	}
}
