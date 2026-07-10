//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/gnana997/crucible/sdk/wire"
)

// dockerDefaultPath is the PATH Docker gives a container process when
// the image sets none — matched so OCI apps resolve their binaries the
// same way they do under `docker run`.
const dockerDefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// resolveLaunch computes the child process's argv (with an absolute
// executable), environment, and — for OCI services (EnvExact) — its
// user credential and working directory. It reads the guest's own
// /etc/passwd + /etc/group, which is the only place the image's users
// exist. For profile services it preserves the historical
// merge-with-agent-environ behavior and sets no credential.
//
// The executable is resolved against the effective PATH here because
// os.StartProcess (the init-mode spawn) does no PATH search, and Docker
// resolves a bare ENTRYPOINT/CMD[0] against the image's PATH.
func resolveLaunch(spec *wire.ServiceSpec) (argv []string, env []string, cred *syscall.Credential, err error) {
	var home string
	if spec.User != "" {
		cred, home, err = resolveUser(spec.User)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolve user %q: %w", spec.User, err)
		}
	}
	env = buildServiceEnv(spec, home)

	// Docker creates WORKDIR if it doesn't exist. Only for OCI services
	// (EnvExact) so profile behavior — where a missing cwd is an error —
	// is unchanged. Done before executable resolution so a relative
	// entrypoint resolves against a present cwd.
	if spec.EnvExact && spec.Cwd != "" {
		if err := os.MkdirAll(spec.Cwd, 0o755); err != nil {
			return nil, nil, nil, fmt.Errorf("create working dir %q: %w", spec.Cwd, err)
		}
	}

	exe, err := lookExecutable(spec.Cmd[0], pathFromEnv(env), spec.Cwd)
	if err != nil {
		return nil, nil, nil, err
	}
	argv = append([]string{exe}, spec.Cmd[1:]...)
	return argv, env, cred, nil
}

// lookExecutable resolves cmd[0] to an absolute path the way Docker
// does: a name containing a slash is a path (absolute as-is, relative
// resolved against cwd); a bare name is searched on PATH. An empty
// PATH falls back to Docker's default so a bare name still resolves in
// a guest whose environment sets none.
func lookExecutable(name, pathEnv, cwd string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty executable")
	}
	if strings.Contains(name, "/") {
		if filepath.IsAbs(name) {
			return name, nil
		}
		if cwd != "" {
			return filepath.Join(cwd, name), nil
		}
		return name, nil // relative, no cwd — exec resolves it against the agent's cwd
	}
	if pathEnv == "" {
		pathEnv = dockerDefaultPath
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		if isExecutableFile(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in PATH", name)
}

// isExecutableFile reports whether path is a regular file with any
// execute bit set. The kernel enforces the real exec permission (as the
// target user) at exec time; this is just the PATH search predicate.
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// pathFromEnv extracts the PATH value from a KEY=VALUE env slice.
func pathFromEnv(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			return v
		}
	}
	return ""
}

// buildServiceEnv composes the child's environment. Profile services
// merge onto the agent's environ (buildEnv). OCI services get exactly
// the spec's env, plus Docker's default PATH when unset and HOME from
// the resolved user when unset — no agent-environ leakage.
func buildServiceEnv(spec *wire.ServiceSpec, home string) []string {
	if !spec.EnvExact {
		return buildEnv(spec.Env)
	}
	m := make(map[string]string, len(spec.Env)+2)
	for k, v := range spec.Env {
		m[k] = v
	}
	if _, ok := m["PATH"]; !ok {
		m["PATH"] = dockerDefaultPath
	}
	if home != "" {
		if _, ok := m["HOME"]; !ok {
			m["HOME"] = home
		}
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// resolveUser resolves an OCI user string to a process credential and
// the user's home directory. Forms: "user", "uid", "user:group",
// "uid:gid", "uid:group", "user:gid".
//
// Semantics follow Docker: a numeric id with no passwd entry is used
// as-is (gid defaults to 0); a name must exist. When a group is given,
// only that group applies (no supplementary groups); otherwise the
// user's supplementary group memberships are carried.
func resolveUser(s string) (*syscall.Credential, string, error) {
	userPart, groupPart, hasGroup := strings.Cut(s, ":")
	if userPart == "" {
		return nil, "", errors.New("empty user")
	}

	var (
		uid, gid uint32
		home     string
		uname    string // set only when a passwd entry was found
	)
	if isNumericID(userPart) {
		n, _ := strconv.ParseUint(userPart, 10, 32)
		uid = uint32(n)
		if u, err := user.LookupId(userPart); err == nil {
			gid = parseID(u.Gid)
			home = u.HomeDir
			uname = u.Username
		}
	} else {
		u, err := user.Lookup(userPart)
		if err != nil {
			return nil, "", err
		}
		uid = parseID(u.Uid)
		gid = parseID(u.Gid)
		home = u.HomeDir
		uname = u.Username
	}

	cred := &syscall.Credential{Uid: uid, Gid: gid}
	switch {
	case hasGroup:
		// Explicit group overrides the primary gid; no supplementary
		// groups (Docker behavior).
		if isNumericID(groupPart) {
			cred.Gid = parseID(groupPart)
		} else {
			g, err := user.LookupGroup(groupPart)
			if err != nil {
				return nil, "", err
			}
			cred.Gid = parseID(g.Gid)
		}
	case uname != "":
		// No explicit group: carry the user's supplementary groups.
		if u, err := user.Lookup(uname); err == nil {
			if gids, err := u.GroupIds(); err == nil {
				for _, gs := range gids {
					if gv := parseID(gs); gv != cred.Gid {
						cred.Groups = append(cred.Groups, gv)
					}
				}
			}
		}
	}
	return cred, home, nil
}

func isNumericID(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func parseID(s string) uint32 {
	n, _ := strconv.ParseUint(s, 10, 32)
	return uint32(n)
}
