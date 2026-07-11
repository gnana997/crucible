package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gnana997/crucible/internal/oci"
	"github.com/gnana997/crucible/sdk/api"
)

// ImageStore is the daemon's view of the OCI image cache
// (internal/oci.Store implements it). An interface keeps the route
// tests hermetic — no real mkfs — and the daemon oblivious to the
// conversion machinery.
type ImageStore interface {
	Pull(ctx context.Context, ref string) (*oci.ImageRecord, error)
	Import(ctx context.Context, r io.Reader, tag string) (*oci.ImageRecord, error)
	List() []*oci.ImageRecord
	Get(ref string) (*oci.ImageRecord, error)
	Delete(ref string) error
}

// maxImportBody bounds a streamed docker-save upload. Large but finite;
// a real archive is layers of a rootfs, so tens of GiB is the ceiling,
// not the norm.
const maxImportBody = 64 << 30

func imageResponseFrom(rec *oci.ImageRecord) api.ImageResponse {
	resp := api.ImageResponse{
		Digest:       rec.Digest,
		SourceRef:    rec.SourceRef,
		SizeBytes:    rec.SizeBytes,
		ContentBytes: rec.ContentBytes,
		Entries:      rec.Entries,
		ConvertMode:  rec.ConvertMode,
		ConvertedAt:  rec.ConvertedAtUnixMs,
	}
	if rec.RunConfig != nil {
		resp.Entrypoint = rec.RunConfig.Entrypoint
		resp.Cmd = rec.RunConfig.Cmd
		resp.ExposedPorts = rec.RunConfig.ExposedPorts
	}
	return resp
}

// imagesEnabled reports whether an image store is wired; when nil, the
// routes answer 501 rather than panicking.
func (s *Server) imagesEnabled(w http.ResponseWriter) bool {
	if s.cfg.Images == nil {
		writeError(w, http.StatusNotImplemented,
			errors.New("image support is not enabled on this daemon (set --image-dir)"))
		return false
	}
	return true
}

func (s *Server) handlePullImage(w http.ResponseWriter, r *http.Request) {
	if !s.imagesEnabled(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req api.PullImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %w", err))
		return
	}
	if req.Ref == "" {
		writeError(w, http.StatusBadRequest, errors.New("ref is required"))
		return
	}
	// Pull + convert can run for minutes; the server write deadline is
	// armed once at request start and never per-write, so clear it as
	// the snapshot/fork handlers do. r.Context() still bounds the work.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	rec, err := s.cfg.Images.Pull(r.Context(), req.Ref)
	if err != nil {
		s.imageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, imageResponseFrom(rec))
}

func (s *Server) handleImportImage(w http.ResponseWriter, r *http.Request) {
	if !s.imagesEnabled(w) {
		return
	}
	// A docker-save archive is a tar stream on the body; the tag (which
	// image inside a multi-image archive) rides a query param.
	tag := r.URL.Query().Get("tag")
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBody)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	rec, err := s.cfg.Images.Import(r.Context(), r.Body, tag)
	if err != nil {
		s.imageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, imageResponseFrom(rec))
}

func (s *Server) handleListImages(w http.ResponseWriter, _ *http.Request) {
	if !s.imagesEnabled(w) {
		return
	}
	recs := s.cfg.Images.List()
	out := make([]api.ImageResponse, 0, len(recs))
	for _, rec := range recs {
		out = append(out, imageResponseFrom(rec))
	}
	writeJSON(w, http.StatusOK, api.ImageListResponse{Images: out})
}

func (s *Server) handleGetImage(w http.ResponseWriter, r *http.Request) {
	if !s.imagesEnabled(w) {
		return
	}
	rec, err := s.cfg.Images.Get(r.PathValue("ref"))
	if err != nil {
		s.imageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, imageResponseFrom(rec))
}

func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	if !s.imagesEnabled(w) {
		return
	}
	if err := s.cfg.Images.Delete(r.PathValue("ref")); err != nil {
		s.imageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// imageError maps store errors onto HTTP statuses: unknown reference →
// 404, ambiguous short reference → 409, everything else (pull/convert
// failures) → 502/500. A pull failure is upstream's fault far more
// often than ours, so it reports 502.
func (s *Server) imageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, oci.ErrImageNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, oci.ErrAmbiguousImage):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusBadGateway, err)
	}
}
