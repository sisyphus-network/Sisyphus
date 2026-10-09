// Package s3 reaches a bucket in an object store that speaks Amazon's S3
// dialect, which most do: Ceph's gateway, MinIO, SeaweedFS, Storj's
// gateway, Wasabi, and AWS itself.
package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// Config says which bucket, where, and as whom.
type Config struct {
	// Endpoint is where the object store is, such as
	// https://s3.eu-central-1.wasabisys.com or http://127.0.0.1:9000. One
	// with no scheme is taken to be https.
	Endpoint string
	Bucket   string
	// Prefix, if set, goes before the name of every object, so that a
	// bucket can be shared with other things.
	Prefix string
	// Region is the region the bucket is in, for stores that have them.
	Region string
	// AccessKey and SecretKey are the credentials. They stay on this node.
	AccessKey, SecretKey string
}

// Client is a bucket. It is a storage.Bucket.
type Client struct {
	api    *minio.Client
	bucket string
	prefix string
}

// Open returns the bucket cfg describes, having checked that it is there
// and can be reached with the credentials given.
func Open(ctx context.Context, cfg Config) (*Client, error) {
	endpoint := cfg.Endpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	at, err := url.Parse(endpoint)
	if err != nil || at.Host == "" {
		return nil, fmt.Errorf("object store address %q is not one", cfg.Endpoint)
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("no bucket was named")
	}
	api, err := minio.New(at.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: at.Scheme == "https",
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("object store at %s: %w", cfg.Endpoint, err)
	}
	found, err := api.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("reach bucket %s at %s: %w", cfg.Bucket, cfg.Endpoint, err)
	}
	if !found {
		return nil, fmt.Errorf("there is no bucket %s at %s: make it first, this does not", cfg.Bucket, cfg.Endpoint)
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &Client{api: api, bucket: cfg.Bucket, prefix: prefix}, nil
}

func (c *Client) Put(ctx context.Context, name string, data []byte) error {
	_, err := c.api.PutObject(ctx, c.bucket, c.prefix+name, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return err
}

func (c *Client) Get(ctx context.Context, name string) ([]byte, error) {
	// Opening an object asks for nothing: what is wrong with it is found
	// on reading it.
	object, _ := c.api.GetObject(ctx, c.bucket, c.prefix+name, minio.GetObjectOptions{})
	defer object.Close()
	data, err := io.ReadAll(object)
	return data, absent(err)
}

func (c *Client) Size(ctx context.Context, name string) (int, error) {
	info, err := c.api.StatObject(ctx, c.bucket, c.prefix+name, minio.StatObjectOptions{})
	return int(info.Size), absent(err)
}

func (c *Client) Remove(ctx context.Context, name string) error {
	return c.api.RemoveObject(ctx, c.bucket, c.prefix+name, minio.RemoveObjectOptions{})
}

func (c *Client) List(ctx context.Context, visit func(name string, size int64) bool) error {
	// Stopping the listing early needs its context ended, or it goes on
	// listing to nobody.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	for object := range c.api.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Prefix: c.prefix, Recursive: true}) {
		if object.Err != nil {
			return object.Err
		}
		if !visit(strings.TrimPrefix(object.Key, c.prefix), object.Size) {
			return nil
		}
	}
	return nil
}

// absent turns the object store's "not there" into the one a store knows.
func absent(err error) error {
	if minio.ToErrorResponse(err).StatusCode == http.StatusNotFound {
		return storage.ErrNoObject
	}
	return err
}
