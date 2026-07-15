package fsutil

import "testing"

// A tiny fake device topology: source-path → real majMin (stat), majMin → dm uuid
// (or not a dm device), and majMin → its slave majMins.
type fakeDevs struct {
	dev    map[string]string   // source path → real majMin; absent = stat fails (fallback)
	uuid   map[string]string   // majMin → dm uuid; absent = not a dm device
	slaves map[string][]string // majMin → slave majMins
}

func (f fakeDevs) statDev(src string) (string, bool) { mm, ok := f.dev[src]; return mm, ok }
func (f fakeDevs) dmUUID(mm string) (string, bool)   { u, ok := f.uuid[mm]; return u, ok }
func (f fakeDevs) slave(mm string) []string          { return f.slaves[mm] }

func TestClassifyAtRest(t *testing.T) {
	// mountinfo covering /var/lib/crucible with different backing devices.
	mi := func(majMin, mp, fstype, src string) []byte {
		return []byte("25 1 " + majMin + " / " + mp + " rw,relatime shared:1 - " + fstype + " " + src + " rw\n")
	}

	cases := []struct {
		name  string
		mount []byte
		devs  fakeDevs
		want  AtRest
	}{
		{
			name:  "plain partition",
			mount: mi("259:2", "/var/lib/crucible", "ext4", "/dev/nvme0n1p2"),
			devs:  fakeDevs{}, // 259:2 is not a dm device
			want:  AtRestPlaintext,
		},
		{
			name:  "dm-crypt directly",
			mount: mi("253:0", "/var/lib/crucible", "ext4", "/dev/mapper/cryptdata"),
			devs:  fakeDevs{uuid: map[string]string{"253:0": "CRYPT-LUKS2-abcd-cryptdata"}},
			want:  AtRestEncrypted,
		},
		{
			name:  "tmpfs is ephemeral",
			mount: mi("0:42", "/var/lib/crucible", "tmpfs", "tmpfs"),
			devs:  fakeDevs{},
			want:  AtRestEphemeral,
		},
		{
			name:  "LVM on LUKS → encrypted via a slave",
			mount: mi("253:5", "/var/lib/crucible", "ext4", "/dev/mapper/vg-work"),
			devs: fakeDevs{
				uuid:   map[string]string{"253:5": "LVM-xxxx", "253:1": "CRYPT-LUKS2-yyyy"},
				slaves: map[string][]string{"253:5": {"253:1"}},
			},
			want: AtRestEncrypted,
		},
		{
			name:  "LVM on a plain disk → plaintext",
			mount: mi("253:6", "/var/lib/crucible", "ext4", "/dev/mapper/vg-work"),
			devs: fakeDevs{
				uuid:   map[string]string{"253:6": "LVM-zzzz"}, // 8:2 (the slave) is a plain disk
				slaves: map[string][]string{"253:6": {"8:2"}},
			},
			want: AtRestPlaintext,
		},
		{
			name:  "most specific mount wins",
			mount: append(mi("8:1", "/", "ext4", "/dev/sda1"), mi("253:0", "/var/lib/crucible", "ext4", "/dev/mapper/cryptdata")...),
			devs:  fakeDevs{uuid: map[string]string{"253:0": "CRYPT-LUKS2-abcd"}},
			want:  AtRestEncrypted,
		},
		{
			// btrfs reports a SYNTHETIC anon device (0:xx) in mountinfo; the real
			// backing device must be found via the source path. This is the case
			// the bench caught: a dm-crypt work root was misread as plaintext.
			name:  "btrfs on dm-crypt: source path resolves to CRYPT",
			mount: mi("0:42", "/var/lib/crucible", "btrfs", "/dev/mapper/cryptwork"),
			devs: fakeDevs{
				dev:  map[string]string{"/dev/mapper/cryptwork": "253:0"},
				uuid: map[string]string{"253:0": "CRYPT-LUKS2-work"},
			},
			want: AtRestEncrypted,
		},
		{
			name:  "btrfs on a plain loop: source resolves to a non-dm device",
			mount: mi("0:43", "/var/lib/crucible", "btrfs", "/dev/loop0"),
			devs:  fakeDevs{dev: map[string]string{"/dev/loop0": "7:0"}}, // 7:0 not dm → plaintext
			want:  AtRestPlaintext,
		},
		{
			name:  "no covering mount → unknown",
			mount: mi("8:1", "/other", "ext4", "/dev/sda1"),
			devs:  fakeDevs{},
			want:  AtRestUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAtRest("/var/lib/crucible/run", tc.mount, tc.devs.statDev, tc.devs.dmUUID, tc.devs.slave)
			if got != tc.want {
				t.Fatalf("classifyAtRest = %v, want %v", got, tc.want)
			}
		})
	}
}
