package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteCreatesFileWithMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Write(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "hello\n" {
		t.Errorf("contents = %q, want %q", got, "hello\n")
	}

	if runtime.GOOS == "windows" {
		return // Windows does not model Unix permission bits.
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// These files hold credentials; a mode drift is a disclosure bug.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600", perm)
	}
}

func TestWriteReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := Write(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("Write() over an existing file: %v", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("contents = %q, want %q", got, "new")
	}
}

func TestWriteLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "config.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A stray temp file would be a credential written with the right mode but
	// the wrong name, invisible to `dk config path`.
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only config.json", names)
	}
}

func TestWriteFailsWhenDirectoryIsMissing(t *testing.T) {
	// Callers own directory creation, so a missing parent must surface rather
	// than being silently created with a default mode.
	path := filepath.Join(t.TempDir(), "nope", "config.json")
	if err := Write(path, []byte("x"), 0o600); err == nil {
		t.Error("Write() to a missing directory returned nil, want an error")
	}
}
