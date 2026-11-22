package commerce

import (
	"context"
	"errors"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/photos"
)

// ErrPhotosOff: no photo storage is set up on this server.
var ErrPhotosOff = errors.New("commerce: product photos are off")

var photoID = regexp.MustCompile(`^[0-9a-f]{24}$`)

func photoKey(product uuid.UUID, id string, thumb bool) string {
	if thumb {
		return "products/" + product.String() + "/" + id + "_thumb.jpg"
	}
	return "products/" + product.String() + "/" + id + ".jpg"
}

// photoErr explains a refused upload in words the shop understands.
func photoErr(err error) error {
	switch {
	case errors.Is(err, photos.ErrNotPhoto):
		return &ValidationError{"Choose a photo (JPEG, PNG or WebP)."}
	case errors.Is(err, photos.ErrTooLarge):
		return &ValidationError{"That photo is too large. Use one under 10 MB and 40 megapixels."}
	case errors.Is(err, photos.ErrTooSmall):
		return &ValidationError{"That photo is too small. Use one at least 200 pixels on each side."}
	}
	return err
}

// SetProductPhoto replaces a product's photo (owners and managers). The upload is drawn again as JPEGs,
// so nothing from the original file, such as its location data, is kept.
func (s *Service) SetProductPhoto(ctx context.Context, user, merchant, product uuid.UUID, data []byte) (*Product, error) {
	if s.Photos == nil {
		return nil, ErrPhotosOff
	}
	if err := s.requireRole(ctx, s.Pool, user, merchant, manageRoles); err != nil {
		return nil, err
	}
	if _, err := s.product(ctx, merchant, product); err != nil {
		return nil, err
	}
	pics, err := photos.Process(data)
	if err != nil {
		return nil, photoErr(err)
	}
	id := photos.NewID()
	if err := s.Photos.Put(ctx, photoKey(product, id, false), pics.Full); err != nil {
		return nil, err
	}
	if err := s.Photos.Put(ctx, photoKey(product, id, true), pics.Thumb); err != nil {
		s.dropPhoto(ctx, product, id)
		return nil, err
	}
	old, err := s.swapPhoto(ctx, user, merchant, product, id)
	if err != nil {
		s.dropPhoto(ctx, product, id)
		return nil, err
	}
	s.dropPhoto(ctx, product, old)
	return s.product(ctx, merchant, product)
}

// RemoveProductPhoto takes a product's photo away (owners and managers).
func (s *Service) RemoveProductPhoto(ctx context.Context, user, merchant, product uuid.UUID) (*Product, error) {
	old, err := s.swapPhoto(ctx, user, merchant, product, "")
	if err != nil {
		return nil, err
	}
	if s.Photos != nil {
		s.dropPhoto(ctx, product, old)
	}
	return s.product(ctx, merchant, product)
}

// swapPhoto records the product's new photo (empty: none) and returns the one it replaced.
func (s *Service) swapPhoto(ctx context.Context, user, merchant, product uuid.UUID, id string) (old string, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := s.requireRole(ctx, tx, user, merchant, manageRoles); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT COALESCE(images[1], '') FROM commerce.products WHERE id = $1 AND merchant_id = $2 FOR UPDATE`,
			product, merchant).Scan(&old)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		images := []string{}
		if id != "" {
			images = []string{id}
		}
		_, err = tx.Exec(ctx, `UPDATE commerce.products SET images = $2, updated_at = now() WHERE id = $1`, product, images)
		return err
	})
	return old, err
}

// dropPhoto deletes a stored photo; a failure only leaves an unused file behind.
func (s *Service) dropPhoto(ctx context.Context, product uuid.UUID, id string) {
	if id == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	_ = s.Photos.Delete(ctx, photoKey(product, id, false))
	_ = s.Photos.Delete(ctx, photoKey(product, id, true))
}

// ProductPhoto returns a product's current photo as JPEG. Old photo IDs and other files aren't served.
func (s *Service) ProductPhoto(ctx context.Context, product uuid.UUID, id string, thumb bool) ([]byte, error) {
	if s.Photos == nil {
		return nil, ErrPhotosOff
	}
	if !photoID.MatchString(id) {
		return nil, ErrNotFound
	}
	var current bool
	err := s.Pool.QueryRow(ctx, `SELECT $2 = ANY(images) FROM commerce.products WHERE id = $1 AND status <> 'archived'`,
		product, id).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !current) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.Photos.Get(ctx, photoKey(product, id, thumb))
}
