// MinIO-backed ObjectStore. Works against any S3-compatible endpoint
// (MinIO / Garage) that a self-hoster already runs (ADR-0008).
//
// Governing: ADR-0008 (Storage & Content Model)
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIOConfig configures the S3-compatible client.
type MinIOConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	UseSSL    bool
}

// MinIO is an ObjectStore backed by an S3-compatible endpoint.
type MinIO struct {
	client *minio.Client
	bucket string
}

// NewMinIO connects a client and ensures the bucket exists.
func NewMinIO(ctx context.Context, cfg MinIOConfig) (*MinIO, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: connect %s: %w", cfg.Endpoint, err)
	}
	ok, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("objectstore: stat bucket %s: %w", cfg.Bucket, err)
	}
	if !ok {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("objectstore: create bucket %s: %w", cfg.Bucket, err)
		}
	}
	return &MinIO{client: client, bucket: cfg.Bucket}, nil
}

func (s *MinIO) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	// size == -1 lets the client stream with an unknown length (multipart).
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("objectstore: put %s: %w", key, err)
	}
	return nil
}

func (s *MinIO) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("objectstore: get %s: %w", key, err)
	}
	// GetObject is lazy; stat now so a missing key surfaces as ErrNotExist.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if isNotFound(err) {
			return nil, fmt.Errorf("objectstore: get %s: %w", key, ErrNotExist)
		}
		return nil, fmt.Errorf("objectstore: get %s: %w", key, err)
	}
	return obj, nil
}

func (s *MinIO) Copy(ctx context.Context, srcKey, dstKey string) error {
	_, err := s.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: s.bucket, Object: dstKey},
		minio.CopySrcOptions{Bucket: s.bucket, Object: srcKey},
	)
	if err != nil {
		return fmt.Errorf("objectstore: copy %s->%s: %w", srcKey, dstKey, err)
	}
	return nil
}

func (s *MinIO) Remove(ctx context.Context, key string) error {
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("objectstore: remove %s: %w", key, err)
	}
	return nil
}

func (s *MinIO) Stat(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("objectstore: stat %s: %w", key, err)
	}
	return true, nil
}

func isNotFound(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.StatusCode == 404
	}
	return false
}
