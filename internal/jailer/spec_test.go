package jailer

import (
	"strings"
	"testing"
)

func TestSpecValidate(t *testing.T) {
	base := Spec{
		ID:         "sbx-abc123",
		ExecFile:   "/usr/bin/firecracker",
		UID:        10000,
		GID:        10000,
		ChrootBase: "/srv/jailer",
	}

	cases := []struct {
		name    string
		mutate  func(*Spec)
		wantErr string // substring; empty = expect nil
	}{
		{name: "valid", mutate: func(*Spec) {}},
		{
			name:    "empty ID",
			mutate:  func(s *Spec) { s.ID = "" },
			wantErr: "invalid ID",
		},
		{
			name:    "ID with slash",
			mutate:  func(s *Spec) { s.ID = "bad/id" },
			wantErr: "invalid ID",
		},
		{
			name:    "ID too long",
			mutate:  func(s *Spec) { s.ID = strings.Repeat("a", 65) },
			wantErr: "invalid ID",
		},
		{
			name:    "ID with underscore (jailer forbids)",
			mutate:  func(s *Spec) { s.ID = "bad_id" },
			wantErr: "invalid ID",
		},
		{
			name:    "empty ExecFile",
			mutate:  func(s *Spec) { s.ExecFile = "" },
			wantErr: "ExecFile required",
		},
		{
			name:    "empty ChrootBase",
			mutate:  func(s *Spec) { s.ChrootBase = "" },
			wantErr: "ChrootBase required",
		},
		{
			name:   "ID exactly 64 chars",
			mutate: func(s *Spec) { s.ID = strings.Repeat("a", 64) },
		},
		{
			name:   "ID with hyphens and digits",
			mutate: func(s *Spec) { s.ID = "snap-l87p5qcl5fkhs" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutate(&s)
			err := s.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
