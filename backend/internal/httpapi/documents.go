package httpapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/kyb"
	"superapp/backend/internal/platform"
)

type documentJSON struct {
	ID          uuid.UUID  `json:"id"`
	Kind        string     `json:"kind"`
	ContentType string     `json:"content_type"`
	Size        int        `json:"size_bytes"`
	Status      string     `json:"status"`
	UploadedAt  time.Time  `json:"uploaded_at"`
	ReviewedAt  *time.Time `json:"reviewed_at"`
	ReviewNote  *string    `json:"review_note"`
}

func toDocumentJSON(d *kyb.Document) documentJSON {
	j := documentJSON{ID: d.ID, Kind: d.Kind, ContentType: d.ContentType, Size: d.Size, Status: d.Status,
		UploadedAt: d.UploadedAt, ReviewedAt: d.ReviewedAt}
	if d.ReviewNote != "" {
		n := d.ReviewNote
		j.ReviewNote = &n
	}
	return j
}

func (s *Server) documentsOff(w http.ResponseWriter) bool {
	if s.KYB != nil {
		return false
	}
	platform.WriteProblem(w, platform.NewProblem(http.StatusServiceUnavailable, "documents_unavailable",
		"Documents can't be sent yet", "Document storage isn't set up on this server."))
	return true
}

func (s *Server) listDocuments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || s.documentsOff(w) {
		return
	}
	docs, err := s.KYB.List(r.Context(), platform.UserFrom(r.Context()), id)
	if err != nil {
		s.failDocuments(w, err)
		return
	}
	required, err := kyb.RequiredTx(r.Context(), s.Pool, id)
	if err != nil {
		s.failDocuments(w, err)
		return
	}
	items := make([]documentJSON, 0, len(docs))
	for i := range docs {
		items = append(items, toDocumentJSON(&docs[i]))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "required": required})
}

// uploadDocument takes the file as the raw request body (image/jpeg, image/png or application/pdf).
func (s *Server) uploadDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || s.documentsOff(w) {
		return
	}
	// A photo over a slow mobile connection takes longer than the server's usual read timeout.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(3 * time.Minute))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, kyb.MaxSize))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		platform.WriteProblem(w, platform.NewProblem(http.StatusRequestEntityTooLarge, "file_too_large", "File too large",
			"The file is over 10 MB. Take the photo again at a lower resolution."))
		return
	}
	if err != nil {
		platform.WriteProblem(w, platform.NewProblem(http.StatusBadRequest, "upload_incomplete", "Upload didn't finish",
			"The connection dropped while sending. Try again."))
		return
	}
	d, err := s.KYB.Upload(r.Context(), platform.UserFrom(r.Context()), id, r.URL.Query().Get("kind"), data)
	if err != nil {
		s.failDocuments(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, toDocumentJSON(d))
}

func (s *Server) failDocuments(w http.ResponseWriter, err error) {
	var v *kyb.ValidationError
	switch {
	case errors.As(err, &v):
		platform.WriteProblem(w, platform.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Check the document", v.Msg))
	case errors.Is(err, kyb.ErrNotFound):
		platform.WriteProblem(w, platform.NewProblem(http.StatusNotFound, "not_found", "Not found", ""))
	case errors.Is(err, kyb.ErrForbidden):
		platform.WriteProblem(w, platform.NewProblem(http.StatusForbidden, "permission_denied", "Not allowed",
			"Only the shop's owner can send and see its documents."))
	default:
		s.fail(w, err)
	}
}
