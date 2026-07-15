package fsutil

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// AtRest classifies how the storage backing a path protects data at rest. It is
// a best-effort signal for a startup advisory (e.g. "your --work-base isn't
// encrypted"), NOT a security guarantee — an unusual stack resolves to
// AtRestUnknown so a caller stays silent rather than raising a false alarm.
type AtRest int

const (
	// AtRestUnknown means the backing couldn't be classified (network filesystem,
	// overlay, an unreadable sysfs, or a mount stack deeper than we follow).
	AtRestUnknown AtRest = iota
	// AtRestEncrypted means the backing block device is dm-crypt (LUKS), directly
	// or through a device-mapper / LVM stack above it.
	AtRestEncrypted
	// AtRestEphemeral means tmpfs / ramfs: the data never reaches a disk.
	AtRestEphemeral
	// AtRestPlaintext means a real block device with no encryption in its stack.
	AtRestPlaintext
)

// PathAtRest classifies the storage backing path. Linux-only in effect: it reads
// /proc/self/mountinfo to find the mount covering path and inspects the backing
// device through sysfs. Any error → AtRestUnknown.
func PathAtRest(path string) AtRest {
	abs, err := filepath.Abs(path)
	if err != nil {
		return AtRestUnknown
	}
	mi, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return AtRestUnknown
	}
	return classifyAtRest(abs, mi, sysfsDMUUID, sysfsSlaves)
}

// classifyAtRest is the testable core: it takes the mountinfo bytes and the two
// sysfs lookups as seams so a test can drive any device topology without root.
func classifyAtRest(path string, mountinfo []byte, dmUUID func(majMin string) (string, bool), slaves func(majMin string) []string) AtRest {
	majMin, fstype, ok := backingMount(path, mountinfo)
	if !ok {
		return AtRestUnknown
	}
	switch fstype {
	case "tmpfs", "ramfs":
		return AtRestEphemeral
	}
	return deviceAtRest(majMin, dmUUID, slaves, 0)
}

// deviceAtRest walks a device-mapper stack from majMin down to its leaves: a
// CRYPT dm device anywhere makes it encrypted; a plain leaf device makes it
// plaintext. Depth-capped so a pathological topology can't loop.
func deviceAtRest(majMin string, dmUUID func(string) (string, bool), slaves func(string) []string, depth int) AtRest {
	if depth > 16 {
		return AtRestUnknown
	}
	uuid, isDM := dmUUID(majMin)
	if !isDM {
		return AtRestPlaintext // a leaf block device with no dm layer above it
	}
	if strings.HasPrefix(uuid, "CRYPT-") {
		return AtRestEncrypted
	}
	// A non-crypt dm device (LVM, linear, …): encrypted iff an ancestor is.
	sl := slaves(majMin)
	if len(sl) == 0 {
		return AtRestUnknown
	}
	res := AtRestUnknown
	for _, s := range sl {
		switch deviceAtRest(s, dmUUID, slaves, depth+1) {
		case AtRestEncrypted:
			return AtRestEncrypted
		case AtRestPlaintext:
			res = AtRestPlaintext
		}
	}
	return res
}

// backingMount finds the mountinfo entry whose mount point most specifically
// covers path, returning its device major:minor and filesystem type.
func backingMount(path string, mountinfo []byte) (majMin, fstype string, ok bool) {
	best := ""
	sc := bufio.NewScanner(bytes.NewReader(mountinfo))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		// left: id parent maj:min root mountpoint options [optional fields…]
		// right: fstype source superopts
		if len(left) < 5 || len(right) < 1 {
			continue
		}
		mp := left[4]
		if !mountCovers(mp, path) {
			continue
		}
		if len(mp) >= len(best) { // the longest (most specific) mount point wins
			best, majMin, fstype, ok = mp, left[2], right[0], true
		}
	}
	return
}

// mountCovers reports whether mount point mp contains path.
func mountCovers(mp, path string) bool {
	if mp == path || mp == "/" {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(mp, "/")+"/")
}

func sysfsDMUUID(majMin string) (string, bool) {
	b, err := os.ReadFile("/sys/dev/block/" + majMin + "/dm/uuid")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func sysfsSlaves(majMin string) []string {
	dir := "/sys/dev/block/" + majMin + "/slaves"
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "dev"))
		if err != nil {
			continue
		}
		out = append(out, strings.TrimSpace(string(b)))
	}
	return out
}
