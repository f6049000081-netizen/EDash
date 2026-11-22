package kyb

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Store keeps document files. Files arrive already encrypted (see Sealer), so the store never sees them in clear.
type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// S3Config reaches any S3-compatible object store: a cloud bucket, or one hosted in Ethiopia.
type S3Config struct {
	Endpoint  string // host[:port], without scheme
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	Insecure  bool // plain HTTP: local development only
}

type S3 struct {
	c      *minio.Client
	bucket string
}

func NewS3(cfg S3Config) (*S3, error) {
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""), Secure: !cfg.Insecure, Region: cfg.Region})
	if err != nil {
		return nil, err
	}
	return &S3{c: c, bucket: cfg.Bucket}, nil
}

// EnsureBucket creates the bucket if it doesn't exist (for development; production buckets are provisioned).
func (s *S3) EnsureBucket(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil || ok {
		return err
	}
	return s.c.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
}

func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.c.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return err
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(io.LimitReader(obj, MaxSize+1024))
}

func (s *S3) Delete(ctx context.Context, key string) error {
	return s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// Sealer encrypts files with AES-256-GCM before they leave the server, bound to their object key, so a
// leaked bucket reveals nothing and a file can't be swapped for another merchant's. Old keys stay for
// reading while new files use the current one.
type Sealer struct {
	current string
	aeads   map[string]cipher.AEAD
}

// ParseKeys reads "id:base64key[,id:base64key...]"; the first is the current key.
func ParseKeys(spec string) (*Sealer, error) {
	s := &Sealer{aeads: map[string]cipher.AEAD{}}
	for _, part := range strings.Split(spec, ",") {
		id, b64, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok || id == "" {
			return nil, errors.New("kyb: keys look like id:base64key[,id:base64key]")
		}
		key, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("kyb: key %q must be 32 bytes, base64", id)
		}
		block, _ := aes.NewCipher(key)
		aead, _ := cipher.NewGCM(block)
		s.aeads[id] = aead
		if s.current == "" {
			s.current = id
		}
	}
	return s, nil
}

func (s *Sealer) Seal(objectKey string, plain []byte) (keyID string, blob []byte) {
	aead := s.aeads[s.current]
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	return s.current, aead.Seal(nonce, nonce, plain, []byte(objectKey))
}

func (s *Sealer) Open(keyID, objectKey string, blob []byte) ([]byte, error) {
	aead, ok := s.aeads[keyID]
	if !ok {
		return nil, fmt.Errorf("kyb: no key %q", keyID)
	}
	if len(blob) < aead.NonceSize() {
		return nil, errors.New("kyb: stored file is damaged")
	}
	return aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], []byte(objectKey))
}

// FromEnv builds the document service from S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY, S3_REGION,
// S3_INSECURE ("true" for plain HTTP, dev only) and DOCS_KEYS (see ParseKeys). Without S3_ENDPOINT it
// returns nil: documents are switched off.
// S3FromEnv reaches the object store named by the S3_* settings, using the bucket in bucketVar (or
// defaultBucket). It returns nil when S3_ENDPOINT isn't set. Product photos use it with their own bucket.
func S3FromEnv(ctx context.Context, getenv func(string) string, bucketVar, defaultBucket string) (*S3, error) {
	env := func(k string) string { return strings.TrimSpace(getenv(k)) }
	if env("S3_ENDPOINT") == "" {
		return nil, nil
	}
	bucket := env(bucketVar)
	if bucket == "" {
		bucket = defaultBucket
	}
	store, err := NewS3(S3Config{Endpoint: env("S3_ENDPOINT"), Bucket: bucket, AccessKey: env("S3_ACCESS_KEY"),
		SecretKey: env("S3_SECRET_KEY"), Region: env("S3_REGION"), Insecure: env("S3_INSECURE") == "true"})
	if err != nil {
		return nil, err
	}
	if env("S3_INSECURE") == "true" {
		if err := store.EnsureBucket(ctx); err != nil {
			return nil, fmt.Errorf("storage %s: %w", bucket, err)
		}
	}
	return store, nil
}

func FromEnv(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string) (*Service, error) {
	env := func(k string) string { return strings.TrimSpace(getenv(k)) }
	if env("S3_ENDPOINT") == "" {
		return nil, nil
	}
	for _, k := range []string{"S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY", "DOCS_KEYS"} {
		if env(k) == "" {
			return nil, fmt.Errorf("%s is required when S3_ENDPOINT is set", k)
		}
	}
	sealer, err := ParseKeys(env("DOCS_KEYS"))
	if err != nil {
		return nil, err
	}
	store, err := NewS3(S3Config{Endpoint: env("S3_ENDPOINT"), Bucket: env("S3_BUCKET"), AccessKey: env("S3_ACCESS_KEY"),
		SecretKey: env("S3_SECRET_KEY"), Region: env("S3_REGION"), Insecure: env("S3_INSECURE") == "true"})
	if err != nil {
		return nil, err
	}
	if env("S3_INSECURE") == "true" {
		if err := store.EnsureBucket(ctx); err != nil {
			return nil, fmt.Errorf("document storage: %w", err)
		}
	}
	return &Service{Pool: pool, Store: store, Sealer: sealer}, nil
}
