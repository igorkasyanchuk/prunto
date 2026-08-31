package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Store is the bucket, or the disk standing in for it.
//
// Deliberately thin: the object key *is* the public URL here, served straight off a CDN
// domain, so nothing in the serving path goes through this process.
type Store interface {
	Put(ctx context.Context, key string, body []byte, contentType string) error
	Delete(ctx context.Context, key string) error
	URL(key string) string
}

func NewStore(c Config) (Store, error) {
	if c.Local() {
		return &diskStore{dir: filepath.Join(c.DataDir, "blobs"), base: c.CDNBaseURL}, nil
	}

	host := c.Endpoint
	secure := true
	if u, err := url.Parse(c.Endpoint); err == nil && u.Host != "" {
		host, secure = u.Host, u.Scheme != "http"
	}
	client, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(c.KeyID, c.AppKey, ""),
		Secure: secure,
		Region: c.Region,
	})
	if err != nil {
		return nil, err
	}
	return &bucketStore{client: client, bucket: c.Bucket, base: c.CDNBaseURL}, nil
}

type bucketStore struct {
	client *minio.Client
	bucket string
	base   string
}

func (s *bucketStore) Put(ctx context.Context, key string, body []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{
			ContentType: contentType,
			// The CDN serves the object directly, so every response header has to be baked in
			// at write time - this process never sees a request for the bytes.
			ContentDisposition: "inline",
			CacheControl:       "public, max-age=31536000, immutable",
		})
	return err
}

func (s *bucketStore) Delete(ctx context.Context, key string) error {
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && resp.Code == "NoSuchKey" {
		return nil // already gone; a half-finished purge has to stay retryable
	}
	return err
}

func (s *bucketStore) URL(key string) string { return s.base + "/" + key }

type diskStore struct {
	dir  string
	base string
}

func (s *diskStore) Put(_ context.Context, key string, body []byte, _ string) error {
	return os.WriteFile(filepath.Join(s.dir, filepath.Base(key)), body, 0o640)
}

func (s *diskStore) Delete(_ context.Context, key string) error {
	err := os.Remove(filepath.Join(s.dir, filepath.Base(key)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *diskStore) URL(key string) string { return s.base + "/" + key }

// A 1x1 transparent PNG, used as the probe object below.
var probePNG = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
	0, 0, 0, 0x0D, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0,
	0x1F, 0x15, 0xC4, 0x89,
	0, 0, 0, 0x0A, 'I', 'D', 'A', 'T', 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
	0x0D, 0x0A, 0x2D, 0xB4,
	0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xAE, 0x42, 0x60, 0x82,
}

// VerifyCDNHeaders round-trips a throwaway object through the configured CDN and checks that
// the headers the whole design leans on are actually being served.
//
// Nothing in this process sits in the serving path, so a bucket fronted by a CDN with no
// nosniff is a silent hole: uploads keep working and the browser is free to second-guess the
// content type of attacker-supplied bytes. Since there is no re-encode here, these headers are
// the control. A wrong header is a misconfiguration and stops the boot; a failed request is
// probably the network and only warns.
func VerifyCDNHeaders(ctx context.Context, store Store) error {
	const key = ".prunto-header-probe.png"

	if err := store.Put(ctx, key, probePNG, "image/png"); err != nil {
		log.Printf("WARNING: could not write the CDN header probe: %v", err)
		return nil
	}
	defer func() {
		if err := store.Delete(context.WithoutCancel(ctx), key); err != nil {
			log.Printf("WARNING: could not remove the CDN header probe %q: %v", key, err)
		}
	}()

	// The object has to be reachable through the CDN before it can be inspected; a fresh
	// bucket rule can take a moment to apply.
	var resp *http.Response
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, store.URL(key), nil)
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
	}
	if err != nil {
		log.Printf("WARNING: could not fetch %s to check its headers: %v", store.URL(key), err)
		return nil
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("WARNING: %s returned %d, so its headers could not be checked", store.URL(key), resp.StatusCode)
		return nil
	}

	var missing []string
	if !strings.EqualFold(resp.Header.Get("X-Content-Type-Options"), "nosniff") {
		missing = append(missing, "X-Content-Type-Options: nosniff")
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		missing = append(missing, "Content-Security-Policy: sandbox")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		missing = append(missing, fmt.Sprintf("Content-Type: image/png (got %q)", ct))
	}
	if len(missing) == 0 {
		log.Printf("CDN headers verified on %s", store.URL(""))
		return nil
	}
	return fmt.Errorf("the CDN at %s is not serving %s.\n"+
		"Uploads are attacker-controlled bytes and this build does not re-encode them, so these\n"+
		"headers are the control that stops a browser treating one as active content. Add them\n"+
		"with a Cloudflare Transform Rule (see COOLIFY.md) and restart",
		store.URL(""), strings.Join(missing, ", "))
}
