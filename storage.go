package main

import (
	"os"
	"path/filepath"
)

// Store is the folder blobs live in, next to the database on the same volume.
//
// Deliberately thin: there is no bucket and no CDN account behind this, so the object key is
// just a filename, and the public URL is this app's own /blobs/:key. Serving them is the one
// job this process does that a bucket used to do - see handleBlob, which streams the file
// rather than reading it in.
type Store struct {
	dir  string
	base string
}

func NewStore(c Config) *Store {
	return &Store{dir: filepath.Join(c.DataDir, "blobs"), base: c.BlobBaseURL()}
}

// path keeps every key inside dir: the key comes off a request path in handleBlob, so a
// traversal attempt has to end at a bare filename.
func (s *Store) path(key string) string { return filepath.Join(s.dir, filepath.Base(key)) }

func (s *Store) Put(key string, body []byte) error {
	return os.WriteFile(s.path(key), body, 0o640)
}

func (s *Store) Delete(key string) error {
	err := os.Remove(s.path(key))
	if os.IsNotExist(err) {
		return nil // already gone; a half-finished purge has to stay retryable
	}
	return err
}

// Open hands back the file itself, for streaming. The caller closes it. It is an *os.File
// rather than an io.Reader because http.ServeContent seeks to size the body and to answer
// Range requests.
func (s *Store) Open(key string) (*os.File, error) {
	return os.Open(s.path(key))
}

func (s *Store) URL(key string) string { return s.base + "/" + key }
