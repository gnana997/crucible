package volume

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSize = 8 << 20 // 8 MiB — small + fast mkfs

// newMgr builds a Manager over dir, chowning to the current user (no root
// needed). Skips if mkfs.ext4 is unavailable.
func newMgr(t *testing.T, dir string) *Manager {
	t.Helper()
	m, err := NewManager(dir, testSize, "testhost", os.Getuid(), os.Getgid())
	if err != nil {
		if strings.Contains(err.Error(), "mkfs.ext4 not found") {
			t.Skip("mkfs.ext4 not available")
		}
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestAttachProvisionsOnceAndPersists(t *testing.T) {
	m := newMgr(t, t.TempDir())

	path, err := m.Attach("data", "sbx1")
	if err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	if filepath.Base(path) != "data.img" {
		t.Fatalf("backing path = %q, want .../data.img", path)
	}
	fi, _ := os.Stat(path)
	firstMod := fi.ModTime()

	m.Release("data")
	path2, err := m.Attach("data", "sbx2")
	if err != nil {
		t.Fatalf("second Attach: %v", err)
	}
	if path2 != path {
		t.Fatalf("re-attach path = %q, want %q", path2, path)
	}
	fi2, _ := os.Stat(path2)
	if !fi2.ModTime().Equal(firstMod) {
		t.Fatalf("backing file reformatted on re-attach — provision not idempotent")
	}
}

func TestSyncFsyncsBackingFile(t *testing.T) {
	m := newMgr(t, t.TempDir())

	path, err := m.Attach("data", "sbx1") // provisions the backing file
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// Dirty the backing file, then Sync it: fsync must succeed (durability at
	// snapshot-sleep depends on this).
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open backing file: %v", err)
	}
	if _, err := f.WriteAt([]byte("crucible"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = f.Close()

	if err := m.Sync("data"); err != nil {
		t.Fatalf("Sync provisioned volume: %v", err)
	}

	// Unknown and invalid names are surfaced, not silently ignored.
	if err := m.Sync("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Sync unknown = %v, want ErrNotFound", err)
	}
	if err := m.Sync("Bad Name"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("Sync bad name = %v, want ErrInvalidName", err)
	}
}

func TestAttachGuardIsSingleWriter(t *testing.T) {
	m := newMgr(t, t.TempDir())
	if _, err := m.Attach("v", "sbx1"); err != nil {
		t.Fatalf("Attach sbx1: %v", err)
	}
	if _, err := m.Attach("v", "sbx2"); !errors.Is(err, ErrInUse) {
		t.Fatalf("Attach sbx2 err = %v, want ErrInUse", err)
	}
	if _, err := m.Attach("v", "sbx1"); err != nil {
		t.Fatalf("re-Attach sbx1: %v", err)
	}
	m.Release("v")
	if _, err := m.Attach("v", "sbx2"); err != nil {
		t.Fatalf("Attach sbx2 after release: %v", err)
	}
}

func TestAttachRejectsBadName(t *testing.T) {
	m := newMgr(t, t.TempDir())
	for _, bad := range []string{"", "UPPER", "has space", "../escape", "a/b"} {
		if _, err := m.Attach(bad, "sbx"); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("Attach(%q) err = %v, want ErrInvalidName", bad, err)
		}
	}
}

func TestCreateListAndDuplicate(t *testing.T) {
	m := newMgr(t, t.TempDir())
	rec, err := m.Create("big", 16<<20, CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.SizeBytes != 16<<20 || rec.HostID != "testhost" || !rec.Formatted {
		t.Fatalf("record = %+v, want size 16MiB, host testhost, formatted", rec)
	}
	infos, err := m.List()
	if err != nil || len(infos) != 1 || infos[0].Name != "big" {
		t.Fatalf("List = %+v, err %v; want one volume 'big'", infos, err)
	}
	if _, err := m.Create("big", 0, CreateOpts{}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create err = %v, want ErrExists", err)
	}
}

func TestRemoveRefusesAttachedThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	m := newMgr(t, dir)
	if _, err := m.Attach("v", "sbx1"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := m.Remove("v"); !errors.Is(err, ErrInUse) {
		t.Fatalf("Remove while attached err = %v, want ErrInUse", err)
	}
	m.Release("v")
	if err := m.Remove("v"); err != nil {
		t.Fatalf("Remove after release: %v", err)
	}
	if _, err := m.Get("v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Remove err = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "v.img")); !os.IsNotExist(err) {
		t.Fatalf("backing file still present after Remove")
	}
	if err := m.Remove("v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove nonexistent err = %v, want ErrNotFound", err)
	}
}

func TestRecordsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	m1 := newMgr(t, dir)
	if _, err := m1.Create("keep", 16<<20, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = m1.Close()

	m2, err := NewManager(dir, testSize, "testhost", os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = m2.Close() }()
	got, err := m2.Get("keep")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.SizeBytes != 16<<20 {
		t.Fatalf("reopened size = %d, want 16MiB (record not durable)", got.SizeBytes)
	}
}

func TestBackfillAdoptsBareImg(t *testing.T) {
	dir := t.TempDir()
	// A bare backing file with no record (simulates a pre-store volume).
	if err := os.WriteFile(filepath.Join(dir, "legacy.img"), make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("write bare img: %v", err)
	}
	m := newMgr(t, dir)
	got, err := m.Get("legacy")
	if err != nil {
		t.Fatalf("backfilled volume not found: %v", err)
	}
	if got.Name != "legacy" {
		t.Fatalf("backfill name = %q", got.Name)
	}
}

func TestDiskBytesAccounting(t *testing.T) {
	m := newMgr(t, t.TempDir())
	if got := m.DiskBytes(); got != 0 {
		t.Fatalf("DiskBytes with no volumes = %d, want 0", got)
	}
	if _, err := m.Create("data", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := m.DiskBytes()
	if got <= 0 {
		t.Fatalf("DiskBytes = %d, want > 0 after Create", got)
	}
	// The backing file is sparse: a fresh ext4 image occupies its metadata,
	// not the full provisioned size.
	if got >= testSize {
		t.Fatalf("DiskBytes = %d, want below the %d provisioned size (sparse)", got, int64(testSize))
	}

	if got := m.BackupDiskBytes(); got != 0 {
		t.Fatalf("BackupDiskBytes with no backups = %d, want 0", got)
	}
	b, err := m.Backup("data")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got := m.BackupDiskBytes(); got <= 0 {
		t.Fatalf("BackupDiskBytes = %d, want > 0 after Backup", got)
	}
	if err := m.DeleteBackup(b.ID); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if got := m.BackupDiskBytes(); got != 0 {
		t.Fatalf("BackupDiskBytes after delete = %d, want 0", got)
	}
}

func TestOpenBackupStreamsTheFile(t *testing.T) {
	dir := t.TempDir()
	m := newMgr(t, dir)
	if _, err := m.Create("data", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := m.Backup("data")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	f, rec, size, err := m.OpenBackup(b.ID)
	if err != nil {
		t.Fatalf("OpenBackup: %v", err)
	}
	defer func() { _ = f.Close() }()
	if rec.ID != b.ID {
		t.Errorf("OpenBackup record id = %q, want %q", rec.ID, b.ID)
	}
	// The reported size is the on-disk file, and the bytes match the backup file.
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if int64(len(got)) != size {
		t.Errorf("read %d bytes, OpenBackup reported %d", len(got), size)
	}
	onDisk, _ := os.ReadFile(b.Path)
	if !bytes.Equal(got, onDisk) {
		t.Error("streamed bytes differ from the backup file")
	}

	if _, _, _, err := m.OpenBackup("nope-does-not-exist"); !errors.Is(err, ErrBackupNotFound) {
		t.Errorf("OpenBackup(missing) err = %v, want ErrBackupNotFound", err)
	}
}

// TestBackupExportImportRestoreRoundTrip is the off-host loop in-process: back
// up a volume, stream it out (gzip), delete the original volume + backup
// (simulate host loss), stream it back in, restore to a new volume, and prove
// the bytes survived — with the imported record taking this host's id.
func TestBackupExportImportRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := newMgr(t, dir)
	if _, err := m.Create("data", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A recognizable byte in the backing file so we can prove data survived.
	orig, _ := os.ReadFile(filepath.Join(dir, "data.img"))

	b, err := m.Backup("data")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Export → gzip into a buffer (what the CP would ship off-host).
	f, _, _, err := m.OpenBackup(b.ID)
	if err != nil {
		t.Fatalf("OpenBackup: %v", err)
	}
	var shipped bytes.Buffer
	gz := gzip.NewWriter(&shipped)
	if _, err := io.Copy(gz, f); err != nil {
		t.Fatalf("gzip copy: %v", err)
	}
	_ = gz.Close()
	_ = f.Close()

	// Simulate host loss: drop the backup record + the source volume.
	if err := m.DeleteBackup(b.ID); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if err := m.Remove("data"); err != nil {
		t.Fatalf("Remove volume: %v", err)
	}

	// Import the shipped bytes back (gzip), then restore to a NEW volume.
	imp, err := m.ImportBackup(ImportMeta{SourceVolume: "data", Compressed: true}, &shipped)
	if err != nil {
		t.Fatalf("ImportBackup: %v", err)
	}
	if imp.HostID != "testhost" {
		t.Errorf("imported HostID = %q, want this host (testhost)", imp.HostID)
	}
	if imp.SourceVolume != "data" || imp.Consistency != "filesystem" {
		t.Errorf("imported record unexpected: %+v", imp)
	}
	if imp.ID == b.ID {
		t.Error("import should assign a fresh id, not reuse the origin's")
	}

	rv, err := m.RestoreTo(imp.ID, "data-restored")
	if err != nil {
		t.Fatalf("RestoreTo: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, rv.Name+".img"))
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatal("restored volume differs from the original after export→import→restore")
	}
}

func TestBackupCreatesListsAndDeletes(t *testing.T) {
	dir := t.TempDir()
	m := newMgr(t, dir)
	if _, err := m.Create("data", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := m.Backup("data")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if b.SourceVolume != "data" || b.SizeBytes != testSize || b.Consistency != "filesystem" {
		t.Fatalf("unexpected record: %+v", b)
	}
	// default backup dir is <dir>/backups/<vol>/…
	if !strings.HasPrefix(b.Path, filepath.Join(dir, "backups", "data")) {
		t.Fatalf("backup path %q not under default backup dir", b.Path)
	}
	// the backup file is byte-identical to the source backing file.
	src, _ := os.ReadFile(filepath.Join(dir, "data.img"))
	dst, err := os.ReadFile(b.Path)
	if err != nil {
		t.Fatalf("read backup file: %v", err)
	}
	if !bytes.Equal(src, dst) {
		t.Fatal("backup content differs from source")
	}
	// listing (all + filtered) returns it; a bogus filter returns none.
	if all, err := m.ListBackups(""); err != nil || len(all) != 1 || all[0].ID != b.ID {
		t.Fatalf("ListBackups(all) = %v, %v", all, err)
	}
	if got, _ := m.ListBackups("data"); len(got) != 1 {
		t.Fatalf("ListBackups(data) len = %d, want 1", len(got))
	}
	if got, _ := m.ListBackups("other"); len(got) != 0 {
		t.Fatalf("ListBackups(other) len = %d, want 0", len(got))
	}
	// GetBackup round-trips; delete removes both the record and the file.
	if _, err := m.GetBackup(b.ID); err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if err := m.DeleteBackup(b.ID); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if _, err := m.GetBackup(b.ID); !errors.Is(err, ErrBackupNotFound) {
		t.Fatalf("GetBackup after delete = %v, want ErrBackupNotFound", err)
	}
	if _, err := os.Stat(b.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("backup file not removed on delete")
	}
}

func TestBackupUnknownVolume(t *testing.T) {
	m := newMgr(t, t.TempDir())
	if _, err := m.Backup("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Backup(unknown) = %v, want ErrNotFound", err)
	}
	if err := m.DeleteBackup("nope-123"); !errors.Is(err, ErrBackupNotFound) {
		t.Fatalf("DeleteBackup(unknown) = %v, want ErrBackupNotFound", err)
	}
}

func TestSetBackupDirOverrides(t *testing.T) {
	dir, alt := t.TempDir(), t.TempDir()
	m := newMgr(t, dir)
	m.SetBackupDir(alt)
	if _, err := m.Create("v", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := m.Backup("v")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !strings.HasPrefix(b.Path, alt) {
		t.Fatalf("backup path %q not under override dir %q", b.Path, alt)
	}
}

func TestRestoreToNewVolume(t *testing.T) {
	dir := t.TempDir()
	m := newMgr(t, dir)
	if _, err := m.Create("src", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := m.Backup("src")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	rec, err := m.RestoreTo(b.ID, "restored")
	if err != nil {
		t.Fatalf("RestoreTo: %v", err)
	}
	if rec.Name != "restored" || rec.SizeBytes != testSize || !rec.Formatted {
		t.Fatalf("restored record: %+v", rec)
	}
	// the restored backing file is byte-identical to the backup, and it lists.
	got, _ := os.ReadFile(filepath.Join(dir, "restored.img"))
	want, _ := os.ReadFile(b.Path)
	if !bytes.Equal(got, want) {
		t.Fatal("restored content differs from backup")
	}
	if _, err := m.Get("restored"); err != nil {
		t.Fatalf("Get(restored): %v", err)
	}
	// never overwrites; unknown backup is ErrBackupNotFound.
	if _, err := m.RestoreTo(b.ID, "restored"); !errors.Is(err, ErrExists) {
		t.Fatalf("RestoreTo existing = %v, want ErrExists", err)
	}
	if _, err := m.RestoreTo("nope-1", "x"); !errors.Is(err, ErrBackupNotFound) {
		t.Fatalf("RestoreTo unknown backup = %v, want ErrBackupNotFound", err)
	}
}

func TestCloneVolume(t *testing.T) {
	dir := t.TempDir()
	m := newMgr(t, dir)
	if _, err := m.Create("orig", testSize, CreateOpts{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec, err := m.Clone("orig", "copy")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if rec.Name != "copy" || rec.SizeBytes != testSize {
		t.Fatalf("clone record: %+v", rec)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "copy.img"))
	want, _ := os.ReadFile(filepath.Join(dir, "orig.img"))
	if !bytes.Equal(got, want) {
		t.Fatal("clone content differs from source")
	}
	if _, err := m.Get("copy"); err != nil {
		t.Fatalf("Get(copy): %v", err)
	}
	// never overwrites; unknown source is ErrNotFound.
	if _, err := m.Clone("orig", "copy"); !errors.Is(err, ErrExists) {
		t.Fatalf("Clone existing = %v, want ErrExists", err)
	}
	if _, err := m.Clone("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Clone unknown src = %v, want ErrNotFound", err)
	}
}

func TestNewManagerRequiresDir(t *testing.T) {
	if _, err := NewManager("", 0, "", 0, 0); err == nil {
		t.Fatal("NewManager(\"\") should error")
	}
}
