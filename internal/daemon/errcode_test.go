package daemon

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gnana997/crucible/internal/app"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/internal/volume"
	"github.com/gnana997/crucible/sdk/api"
)

func TestErrCode(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{volume.ErrExists, api.CodeNameTaken},
		{app.ErrNameTaken, api.CodeNameTaken},
		{volume.ErrInUse, api.CodeInUse},
		{app.ErrNotRunning, api.CodeNotRunning},
		{app.ErrNotAsleep, api.CodeNotAsleep},
		{volume.ErrInvalidName, api.CodeInvalidName},
		{sandbox.ErrInvalidConfig, api.CodeInvalidConfig},
		{volume.ErrBackupNotFound, api.CodeBackupNotFound},
		{sandbox.ErrSnapshotNotFound, api.CodeSnapshotNotFound},
		{volume.ErrNotFound, api.CodeNotFound},
		{app.ErrNotFound, api.CodeNotFound},
		{sandbox.ErrNotFound, api.CodeNotFound},
		// wrapped sentinels still classify (errors.Is unwraps).
		{fmt.Errorf("volume: %w: data", volume.ErrExists), api.CodeNameTaken},
		// no dedicated code.
		{errors.New("something else"), ""},
	}
	for _, c := range cases {
		if got := errCode(c.err); got != c.want {
			t.Errorf("errCode(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
