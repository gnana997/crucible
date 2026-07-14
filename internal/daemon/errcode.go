package daemon

import (
	"errors"

	"github.com/gnana997/crucible/internal/app"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/internal/volume"
	"github.com/gnana997/crucible/sdk/api"
)

// errCode maps a domain sentinel error to a stable, machine-readable code for
// ErrorResponse.Code, so a programmatic client (e.g. a control plane) can tell
// conflict and lifecycle cases apart without matching human-readable messages.
// "" means "no dedicated code" (the client branches on the HTTP status).
func errCode(err error) string {
	switch {
	case errors.Is(err, volume.ErrExists), errors.Is(err, app.ErrNameTaken):
		return api.CodeNameTaken
	case errors.Is(err, volume.ErrInUse):
		return api.CodeInUse
	case errors.Is(err, app.ErrNotRunning):
		return api.CodeNotRunning
	case errors.Is(err, app.ErrNotAsleep):
		return api.CodeNotAsleep
	case errors.Is(err, volume.ErrInvalidName):
		return api.CodeInvalidName
	case errors.Is(err, sandbox.ErrInvalidConfig):
		return api.CodeInvalidConfig
	case errors.Is(err, volume.ErrBackupNotFound):
		return api.CodeBackupNotFound
	case errors.Is(err, sandbox.ErrSnapshotNotFound):
		return api.CodeSnapshotNotFound
	case errors.Is(err, volume.ErrNotFound),
		errors.Is(err, app.ErrNotFound),
		errors.Is(err, sandbox.ErrNotFound):
		return api.CodeNotFound
	}
	return ""
}
