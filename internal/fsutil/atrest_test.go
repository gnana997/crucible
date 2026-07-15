package fsutil

import "testing"

// A tiny fake device topology: majMin → dm uuid (or not a dm device), and
// majMin → its slave majMins.
type fakeDevs struct {
	uuid   map[string]string   // majMin → dm uuid; absent = not a dm device
	slaves map[string][]string // majMin → slave majMins
}

func (f fakeDevs) dmUUID(mm string) (string, bool) { u, ok := f.uuid[mm]; return u, ok }
func (f fakeDevs) slave(mm string) []string        { return f.slaves[mm] }

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
			name:  "no covering mount → unknown",
			mount: mi("8:1", "/other", "ext4", "/dev/sda1"),
			devs:  fakeDevs{},
			want:  AtRestUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAtRest("/var/lib/crucible/run", tc.mount, tc.devs.dmUUID, tc.devs.slave)
			if got != tc.want {
				t.Fatalf("classifyAtRest = %v, want %v", got, tc.want)
			}
		})
	}
}
