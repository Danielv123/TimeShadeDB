package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Meta is what a target stores besides the body: the HTTP headers the object is served
// with.
type Meta struct {
	ContentType     string
	ContentEncoding string // "" (identity), "gzip" or "zstd"
	CacheControl    string
}

// Target stores published objects under keys such as "tile/<ds>/0/1/2/7.bin".
type Target interface {
	// ID names the target (a directory or an endpoint, bucket and prefix) in the state
	// file, so a state is never applied to another target. It never holds a secret.
	ID() string
	// Put stores the object, replacing any object with the same key. Errors are
	// *PutError when the target answered, else network or file errors (retried).
	Put(ctx context.Context, key string, body []byte, m Meta) error
}

// Checker is implemented by targets that can tell cheaply whether an object is still
// there (the directory target): an object the state lists as uploaded but missing is
// uploaded again.
type Checker interface {
	Has(key string, size int64) bool
}

// PutError is a target's refusal of one object.
type PutError struct {
	Key    string
	Status int
	Body   string
}

func (e *PutError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("put %s: status %d: %s", e.Key, e.Status, e.Body)
	}
	return fmt.Sprintf("put %s: status %d", e.Key, e.Status)
}

// putClass sorts a Put error: retry (network, 5xx, 429), auth (401, 403: stop the
// round) or rejected (other 4xx: skip the object).
type putClass int

const (
	putRetry putClass = iota
	putAuth
	putRejected
)

func classify(err error) putClass {
	var pe *PutError
	if !errors.As(err, &pe) {
		return putRetry
	}
	switch {
	case pe.Status == 401 || pe.Status == 403:
		return putAuth
	case pe.Status >= 400 && pe.Status < 500 && pe.Status != 408 && pe.Status != 429:
		return putRejected
	}
	return putRetry
}

// validKey accepts keys made of non-empty segments of [A-Za-z0-9._~-], none of them
// "." or "..", separated by single slashes. Every key the planner makes passes; the
// check keeps a hostile catalog (an HTTP source) from writing outside the target.
func validKey(key string) bool {
	if key == "" || len(key) > 512 {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '~' || c == '-') {
				return false
			}
		}
	}
	return true
}

// DirTarget writes objects as files under Root (the static export directory). Bodies
// are written as they would be uploaded, so with a compressed encoding the files hold
// compressed bytes and the web server must send Content-Encoding (NewStaticHandler
// does, by sniffing). Files are replaced by rename, so readers never see a torn file.
// Files are not fsynced one by one: after an operating system crash, the Has check
// finds lost files only if they are missing, so publish again with a reset state if
// the export may hold torn files.
type DirTarget struct {
	Root string

	mu   sync.Mutex
	dirs map[string]bool
}

// NewDirTarget returns a directory target; the directory is created when missing.
func NewDirTarget(root string) (*DirTarget, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &DirTarget{Root: abs, dirs: map[string]bool{}}, nil
}

// ID implements Target.
func (d *DirTarget) ID() string { return "dir:" + filepath.ToSlash(d.Root) }

func (d *DirTarget) path(key string) string {
	return filepath.Join(d.Root, filepath.FromSlash(key))
}

// Has implements Checker.
func (d *DirTarget) Has(key string, size int64) bool {
	fi, err := os.Stat(d.path(key))
	return err == nil && fi.Mode().IsRegular() && fi.Size() == size
}

// Put implements Target.
func (d *DirTarget) Put(ctx context.Context, key string, body []byte, _ Meta) error {
	if !validKey(key) {
		return &PutError{Key: key, Status: 400, Body: "bad key"}
	}
	p := d.path(key)
	dir := filepath.Dir(p)
	d.mu.Lock()
	known := d.dirs[dir]
	d.mu.Unlock()
	if !known {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		d.mu.Lock()
		d.dirs[dir] = true
		d.mu.Unlock()
	}
	f, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(body)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// On Windows a rename over a file that a web server has open fails for a moment.
	for i := 0; ; i++ {
		err = os.Rename(tmp, p)
		if err == nil || i == 20 || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(i+1) * 5 * time.Millisecond)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
