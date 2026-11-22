package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"superapp/backend/internal/commerce"
	"superapp/backend/internal/photos"
	"superapp/backend/internal/platform"
)

// putProductPhoto takes the photo as the raw request body (JPEG, PNG or WebP).
func (s *Server) putProductPhoto(w http.ResponseWriter, r *http.Request) {
	merchant, ok := pathID(w, r)
	if !ok {
		return
	}
	product, err := uuid.Parse(r.PathValue("product"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(3 * time.Minute)) // slow mobile uploads
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, photos.MaxSize))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		platform.WriteProblem(w, platform.NewProblem(http.StatusRequestEntityTooLarge, "file_too_large", "Photo too large",
			"The photo is over 10 MB. Take it again at a lower resolution."))
		return
	}
	if err != nil {
		platform.WriteProblem(w, platform.NewProblem(http.StatusBadRequest, "upload_incomplete", "Upload didn't finish",
			"The connection dropped while sending. Try again."))
		return
	}
	p, err := s.Commerce.SetProductPhoto(r.Context(), platform.UserFrom(r.Context()), merchant, product, data)
	if err != nil {
		s.failPhoto(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, productJSON(*p))
}

func (s *Server) deleteProductPhoto(w http.ResponseWriter, r *http.Request) {
	merchant, ok := pathID(w, r)
	if !ok {
		return
	}
	product, err := uuid.Parse(r.PathValue("product"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.Commerce.RemoveProductPhoto(r.Context(), platform.UserFrom(r.Context()), merchant, product)
	if err != nil {
		s.failPhoto(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, productJSON(*p))
}

func (s *Server) getProductPhoto(w http.ResponseWriter, r *http.Request) {
	product, err := uuid.Parse(r.PathValue("product"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := s.Commerce.ProductPhoto(r.Context(), product, r.PathValue("photo"), r.URL.Query().Get("size") == "thumb")
	if err != nil {
		if errors.Is(err, commerce.ErrNotFound) || errors.Is(err, commerce.ErrPhotosOff) {
			http.NotFound(w, r)
			return
		}
		s.fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'")
	_, _ = w.Write(data)
}

func (s *Server) failPhoto(w http.ResponseWriter, err error) {
	if errors.Is(err, commerce.ErrPhotosOff) {
		platform.WriteProblem(w, platform.NewProblem(http.StatusServiceUnavailable, "photos_unavailable",
			"Photos can't be added yet", "Photo storage isn't set up on this server."))
		return
	}
	s.failCommerce(w, err)
}
