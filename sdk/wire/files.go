package wire

// File transfer contract (the `crucible cp` push path).
//
// Push (host -> guest) is a one-way bulk transfer, so unlike interactive exec
// it does not use the frame protocol. The daemon streams a plain **tar**
// octet-stream to the guest agent:
//
//	PUT /files?path=<dest>      body: a tar archive       -> 200 {FilesPutResult}
//
// `path` is the absolute destination directory inside the guest; the agent
// MkdirAll's it and extracts each tar entry beneath it, rejecting entries whose
// resolved path escapes the destination (absolute paths, `..`, or symlinks that
// point outside). Nothing is buffered whole: the daemon is a streaming proxy
// between the client's request body and the agent's request body.
//
// The pull path (GET /files) mirrors it in the opposite direction.

// FilesPutResult is the JSON summary the agent returns from PUT /files, echoed
// back to the client through the daemon.
type FilesPutResult struct {
	// Files is the number of regular files written.
	Files int `json:"files"`
	// Bytes is the total size of file contents written.
	Bytes int64 `json:"bytes"`
}
