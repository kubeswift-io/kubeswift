package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// objectStore is the minimal S3 surface snapshot-s3 needs. It is an interface so
// the upload/download orchestration is unit-testable with an in-memory fake.
type objectStore interface {
	// stat returns the object's size and the sha256 recorded as user metadata at
	// upload time (empty if the object predates checksum metadata); ok=false
	// (nil err) when the object does not exist.
	stat(ctx context.Context, key string) (size int64, sha256 string, ok bool, err error)
	// put writes the object and records sha256 as user metadata so a later
	// upload can detect same-size-but-different-content (a memory-ranges file is
	// always the same byte size as the guest RAM, so size alone cannot tell a
	// stale object from a current one).
	put(ctx context.Context, key string, r io.Reader, size int64, sha256 string) error
	get(ctx context.Context, key string) (io.ReadCloser, error)
	remove(ctx context.Context, key string) error
	// list returns every object key under prefix (recursive).
	list(ctx context.Context, prefix string) ([]string, error)
}

// objectSHA256MetaKey is the user-metadata key (sent as the x-amz-meta-sha256
// header) that records an uploaded artifact's content hash.
const objectSHA256MetaKey = "sha256"

type minioStore struct {
	client *minio.Client
	bucket string
}

// newMinioStore builds an S3 client. endpoint is host[:port] (no scheme); empty
// means AWS ("s3.amazonaws.com"). Credentials come from the standard AWS
// environment (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN) —
// never from flags or annotations. pathStyle is required by most
// S3-compatible stores (MinIO, Ceph RGW); secure=false (plaintext) is gated by
// the caller.
func newMinioStore(endpoint, region, bucket string, pathStyle, secure bool) (*minioStore, error) {
	host := strings.TrimSpace(endpoint)
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	if host == "" {
		host = "s3.amazonaws.com"
	}
	opts := &minio.Options{
		Creds:  credentials.NewEnvAWS(),
		Secure: secure,
		Region: region,
	}
	if pathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	client, err := minio.New(host, opts)
	if err != nil {
		return nil, fmt.Errorf("init S3 client for %q: %w", host, err)
	}
	return &minioStore{client: client, bucket: bucket}, nil
}

func (s *minioStore) stat(ctx context.Context, key string) (int64, string, bool, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" || minio.ToErrorResponse(err).StatusCode == 404 {
			return 0, "", false, nil
		}
		return 0, "", false, err
	}
	// info.Metadata is the raw http.Header; Get is case-insensitive and handles
	// the x-amz-meta- canonicalization regardless of minio-go's UserMetadata
	// key-casing quirks.
	sha := info.Metadata.Get("X-Amz-Meta-" + objectSHA256MetaKey)
	return info.Size, sha, true, nil
}

func (s *minioStore) put(ctx context.Context, key string, r io.Reader, size int64, sha256 string) error {
	// minio-go streams and auto-multiparts large objects; size enables a
	// single known-length transfer (use -1 for unknown).
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{
		ContentType:  "application/octet-stream",
		UserMetadata: map[string]string{objectSHA256MetaKey: sha256},
	})
	return err
}

func (s *minioStore) get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func (s *minioStore) remove(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func (s *minioStore) list(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}
