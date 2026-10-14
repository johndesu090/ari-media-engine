// Package storage abstracts blob storage for recordings: local disk or any
// S3-compatible object store (MinIO, AWS S3).
package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config selects and configures the storage backend.
type Config struct {
	Driver      string
	LocalRoot   string
	PublicBase  string
	S3Endpoint  string
	S3Bucket    string
	S3Region    string
	S3AccessKey string
	S3SecretKey string
	S3UseSSL    bool
}

// Object is a stored blob (a recording).
type Object struct {
	Key        string    `json:"key"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
	URL        string    `json:"url,omitempty"`
}

// Store is a blob storage backend.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
	URL(key string) string
	Close() error
}

// New builds a Store from cfg.
func New(cfg Config) (Store, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Driver)) {
	case "", "local":
		return NewLocalStore(cfg.LocalRoot, cfg.PublicBase)
	case "s3":
		if cfg.S3Bucket == "" {
			return nil, fmt.Errorf("storage: s3_bucket is required")
		}
		return NewS3Store(cfg)
	default:
		return nil, fmt.Errorf("storage: unsupported driver %q", cfg.Driver)
	}
}

// LocalStore stores blobs on the local filesystem under Root.
type LocalStore struct {
	root       string
	publicBase string
}

// NewLocalStore creates the root directory and returns a LocalStore.
func NewLocalStore(root, publicBase string) (*LocalStore, error) {
	if root == "" {
		root = "./data/recordings"
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("storage: create root: %w", err)
	}
	return &LocalStore{root: root, publicBase: strings.TrimRight(publicBase, "/")}, nil
}

func (s *LocalStore) path(key string) string {
	return filepath.Join(s.root, filepath.FromSlash(strings.TrimPrefix(path.Clean("/"+key), "/")))
}

// Put writes r to root/key.
func (s *LocalStore) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	full := s.path(key)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.Create(full)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

// List walks the root and returns every blob (optionally filtered by prefix).
func (s *LocalStore) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	err := filepath.Walk(s.root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return nil
		}
		out = append(out, Object{Key: key, Size: info.Size(), ModifiedAt: info.ModTime().UTC(), URL: s.URL(key)})
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// Delete removes root/key.
func (s *LocalStore) Delete(_ context.Context, key string) error {
	return os.Remove(s.path(key))
}

// URL returns the public URL for key, or "" when no public base is configured.
func (s *LocalStore) URL(key string) string {
	if s.publicBase == "" {
		return ""
	}
	return s.publicBase + "/" + strings.TrimLeft(key, "/")
}

// Close is a no-op.
func (s *LocalStore) Close() error { return nil }

// S3Store stores blobs in an S3-compatible bucket.
type S3Store struct {
	client   *minio.Client
	bucket   string
	endpoint string
	useSSL   bool
}

// NewS3Store builds an S3Store. The endpoint may include a scheme.
func NewS3Store(cfg Config) (*S3Store, error) {
	endpoint := cfg.S3Endpoint
	useSSL := cfg.S3UseSSL
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		useSSL, endpoint = true, strings.TrimPrefix(endpoint, "https://")
	case strings.HasPrefix(endpoint, "http://"):
		useSSL, endpoint = false, strings.TrimPrefix(endpoint, "http://")
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: useSSL,
		Region: cfg.S3Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: s3 client: %w", err)
	}
	return &S3Store{client: client, bucket: cfg.S3Bucket, endpoint: endpoint, useSSL: useSSL}, nil
}

func (s *S3Store) ensureBucket(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
}

// Put uploads r to the bucket under key.
func (s *S3Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := s.ensureBucket(ctx); err != nil {
		return err
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

// List returns the objects under prefix.
func (s *S3Store) List(ctx context.Context, prefix string) ([]Object, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var out []Object
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		out = append(out, Object{Key: obj.Key, Size: obj.Size, ModifiedAt: obj.LastModified.UTC(), URL: s.URL(obj.Key)})
	}
	return out, nil
}

// Delete removes key from the bucket.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// URL returns a direct object URL (bucket policy permitting).
func (s *S3Store) URL(key string) string {
	scheme := "http"
	if s.useSSL {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s/%s", scheme, s.endpoint, s.bucket, strings.TrimLeft(key, "/"))
}

// Close is a no-op.
func (s *S3Store) Close() error { return nil }
