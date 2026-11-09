// Package fsutil holds well-known locations and the safe-write primitives:
// every write goes through a backup plus temp-file-and-rename.
package fsutil

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Home returns the user's home directory.
func Home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// Expand resolves a leading ~ to the home directory.
func Expand(p string) string {
	if p == "~" {
		return Home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(Home(), p[2:])
	}
	return p
}

// DataDir is bridge's private data directory (BRIDGE_DATA_DIR overrides it).
func DataDir() string {
	if d := os.Getenv("BRIDGE_DATA_DIR"); d != "" {
		return d
	}
	return filepath.Join(Home(), ".local", "share", "bridge")
}

// ConfigDir holds the global config and canonical rule and skill sources.
func ConfigDir() string { return filepath.Join(Home(), ".config", "bridge") }

// DBPath is the event database location.
func DBPath() string { return filepath.Join(DataDir(), "bridge.db") }

// SocketPath is the daemon's Unix socket. When the data path would exceed the
// sun_path limit it lives in a private, hash-named directory under the temp
// directory.
func SocketPath() string {
	p := filepath.Join(DataDir(), "bridge.sock")
	if len(p) < 100 {
		return p
	}
	return filepath.Join(os.TempDir(), "bridge-"+HashString(p)[:12], "s.sock")
}

func pauseFlag() string { return filepath.Join(DataDir(), "paused") }

// Paused reports whether the user has paused syncing.
func Paused() bool {
	_, err := os.Stat(pauseFlag())
	return err == nil
}

// SetPaused switches the pause state.
func SetPaused(on bool) error {
	if !on {
		if err := os.Remove(pauseFlag()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := EnsurePrivateDir(DataDir()); err != nil {
		return err
	}
	return os.WriteFile(pauseFlag(), nil, 0o600)
}

// BackupDir is the root of day-partitioned backups.
func BackupDir() string { return filepath.Join(DataDir(), "backups") }

// EnsurePrivateDir creates dir with mode 0700.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// HashString returns the hex SHA-256 of s.
func HashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// HashFile returns the hex SHA-256 of a file.
func HashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// AtomicWrite writes data via a temp file in the same directory and a rename,
// so readers see either the old or the new content, never a partial file.
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bridge-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Backup copies path into today's backup directory and returns the copy's
// location. A missing source is not an error and returns "".
func Backup(path string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return BackupData(path, data)
}

// BackupData stores data as a backup of path. Callers use it directly when
// part of the file must not be retained, such as derived memory blocks.
func BackupData(path string, data []byte) (string, error) {
	dir := filepath.Join(BackupDir(), time.Now().Format("2006-01-02"))
	if err := EnsurePrivateDir(BackupDir()); err != nil {
		return "", err
	}
	if err := EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	abs, _ := filepath.Abs(path)
	// The path hash keeps same-named files apart; a flattened full path could
	// exceed the file-name length limit.
	name := fmt.Sprintf("%s.%s.%d", filepath.Base(abs), HashString(abs)[:12], time.Now().UnixNano())
	dst := filepath.Join(dir, name)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := out.Write(data); err != nil {
		return "", err
	}
	return dst, nil
}

// ScrubBackups removes every occurrence of text from existing backups.
func ScrubBackups(text string) error {
	needles := [][]byte{[]byte(text), []byte(strings.ReplaceAll(text, "\n", " "))}
	return filepath.WalkDir(BackupDir(), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		next := b
		for _, n := range needles {
			next = bytes.ReplaceAll(next, n, []byte("[forgotten]"))
		}
		if bytes.Equal(next, b) {
			return nil
		}
		return AtomicWrite(p, next, 0o600)
	})
}

// Failpoint exits abruptly when BRIDGE_FAILPOINT names this point. It exists
// so tests can kill a real process at each step of a multi-resource write.
func Failpoint(name string) {
	if os.Getenv("BRIDGE_FAILPOINT") == name {
		os.Exit(137)
	}
}

// SafeWrite backs up any existing file and then writes atomically, keeping
// the existing permissions when there are any.
func SafeWrite(path string, data []byte, perm os.FileMode) error {
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
		if _, err := Backup(path); err != nil {
			return fmt.Errorf("backup %s: %w", path, err)
		}
	}
	return AtomicWrite(path, data, perm)
}

// NewUUID returns a time-ordered UUIDv7 string.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	ms := uint64(time.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Inode returns the inode number of a file.
func Inode(st os.FileInfo) uint64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Ino)
	}
	return 0
}
