package wire

import (
	"strings"
	"testing"
)

func TestServiceSpecValidate(t *testing.T) {
	cases := []struct {
		name    string
		spec    ServiceSpec
		wantErr string // substring; empty = valid
	}{
		{
			name:    "empty cmd",
			spec:    ServiceSpec{},
			wantErr: "cmd is required",
		},
		{
			name:    "empty cmd0",
			spec:    ServiceSpec{Cmd: []string{""}},
			wantErr: "cmd[0]",
		},
		{
			name: "minimal valid",
			spec: ServiceSpec{Cmd: []string{"/bin/app"}},
		},
		{
			name:    "negative grace",
			spec:    ServiceSpec{Cmd: []string{"/x"}, StopGraceSec: -1},
			wantErr: "stop_grace_s",
		},
		{
			name:    "unknown policy",
			spec:    ServiceSpec{Cmd: []string{"/x"}, Restart: RestartPolicy{Policy: "sometimes"}},
			wantErr: "unknown restart policy",
		},
		{
			name: "on-failure with retries",
			spec: ServiceSpec{Cmd: []string{"/x"}, Restart: RestartPolicy{Policy: RestartOnFailure, MaxRetries: 5}},
		},
		{
			name:    "max_retries without on-failure",
			spec:    ServiceSpec{Cmd: []string{"/x"}, Restart: RestartPolicy{Policy: RestartAlways, MaxRetries: 5}},
			wantErr: "max_retries",
		},
		{
			name:    "max_retries with never",
			spec:    ServiceSpec{Cmd: []string{"/x"}, Restart: RestartPolicy{MaxRetries: 5}},
			wantErr: "max_retries",
		},
		{
			name:    "negative retries",
			spec:    ServiceSpec{Cmd: []string{"/x"}, Restart: RestartPolicy{Policy: RestartOnFailure, MaxRetries: -1}},
			wantErr: "max_retries",
		},
		{
			name:    "log buffer over cap",
			spec:    ServiceSpec{Cmd: []string{"/x"}, LogBufferBytes: MaxLogBufferBytes + 1},
			wantErr: "log_buffer_bytes",
		},
		{
			name: "always policy valid",
			spec: ServiceSpec{Cmd: []string{"/x"}, Restart: RestartPolicy{Policy: RestartAlways}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestServiceSpecNormalize(t *testing.T) {
	s := ServiceSpec{Cmd: []string{"/bin/app"}}
	s.Normalize()
	if s.StopSignal != DefaultStopSignal {
		t.Errorf("StopSignal = %q, want %q", s.StopSignal, DefaultStopSignal)
	}
	if s.StopGraceSec != DefaultStopGraceSec {
		t.Errorf("StopGraceSec = %d, want %d", s.StopGraceSec, DefaultStopGraceSec)
	}
	if s.Restart.Policy != RestartNever {
		t.Errorf("Restart.Policy = %q, want %q", s.Restart.Policy, RestartNever)
	}
	if s.LogBufferBytes != DefaultLogBufferBytes {
		t.Errorf("LogBufferBytes = %d, want %d", s.LogBufferBytes, DefaultLogBufferBytes)
	}

	// Explicit values survive.
	s2 := ServiceSpec{Cmd: []string{"/x"}, StopSignal: "SIGINT", StopGraceSec: 3, LogBufferBytes: 4096}
	s2.Normalize()
	if s2.StopSignal != "SIGINT" || s2.StopGraceSec != 3 || s2.LogBufferBytes != 4096 {
		t.Errorf("explicit values overwritten: %+v", s2)
	}
}
