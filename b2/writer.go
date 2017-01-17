// Copyright 2016, Google
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package b2

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/golang/glog"

	"golang.org/x/net/context"
)

type chunk struct {
	id  int
	buf writeBuffer
}

// Writer writes data into Backblaze.  It automatically switches to the large
// file API if the file exceeds ChunkSize bytes.  Due to that and other
// Backblaze API details, there is a large buffer.
//
// Changes to public Writer attributes must be made before the first call to
// Write.
type Writer struct {
	// ConcurrentUploads is number of different threads sending data concurrently
	// to Backblaze for large files.  This can increase performance greatly, as
	// each thread will hit a different endpoint.  However, there is a ChunkSize
	// buffer for each thread.  Values less than 1 are equivalent to 1.
	ConcurrentUploads int

	// Resume an upload.  If true, and the upload is a large file, and a file of
	// the same name was started but not finished, then assume that we are
	// resuming that file, and don't upload duplicate chunks.
	Resume bool

	// ChunkSize is the size, in bytes, of each individual part, when writing
	// large files, and also when determining whether to upload a file normally
	// or when to split it into parts.  The default is 100M (1e8) (which is also
	// the minimum).  Values less than 100M are not an error, but will fail.  The
	// maximum is 5GB (5e9).
	ChunkSize int

	contentType string
	info        map[string]string

	csize  int
	ctx    context.Context
	cancel context.CancelFunc
	ready  chan chunk
	wg     sync.WaitGroup
	start  sync.Once
	once   sync.Once
	done   sync.Once
	file   beLargeFileInterface
	seen   map[int]string

	o    *Object
	name string

	cidx int
	w    writeBuffer

	emux sync.RWMutex
	err  error
}

func (w *Writer) setErr(err error) {
	if err == nil {
		return
	}
	w.emux.Lock()
	defer w.emux.Unlock()
	if w.err == nil {
		glog.Errorf("error writing %s: %v", w.name, err)
		w.err = err
		w.cancel()
	}
}

func (w *Writer) getErr() error {
	w.emux.RLock()
	defer w.emux.RUnlock()
	return w.err
}

var gid int32

func (w *Writer) thread() {
	go func() {
		id := atomic.AddInt32(&gid, 1)
		fc, err := w.file.getUploadPartURL(w.ctx)
		if err != nil {
			w.setErr(err)
			return
		}
		w.wg.Add(1)
		defer w.wg.Done()
		for {
			chunk, ok := <-w.ready
			if !ok {
				return
			}
			if sha, ok := w.seen[chunk.id]; ok {
				if sha != chunk.buf.Hash() {
					w.setErr(errors.New("resumable upload was requested, but chunks don't match!"))
					return
				}
				glog.V(2).Infof("skipping chunk %d", chunk.id)
				continue
			}
			glog.V(2).Infof("thread %d handling chunk %d", id, chunk.id)
			r, err := chunk.buf.Reader()
			if err != nil {
				w.setErr(err)
				return
			}
			sleep := time.Millisecond * 15
		redo:
			n, err := fc.uploadPart(w.ctx, r, chunk.buf.Hash(), chunk.buf.Len(), chunk.id)
			if n != chunk.buf.Len() || err != nil {
				if w.o.b.r.reupload(err) {
					time.Sleep(sleep)
					sleep *= 2
					if sleep > time.Second*15 {
						sleep = time.Second * 15
					}
					glog.Infof("b2 writer: wrote %d of %d: error: %v; retrying", n, chunk.buf.Len(), err)
					f, err := w.file.getUploadPartURL(w.ctx)
					if err != nil {
						w.setErr(err)
						return
					}
					fc = f
					goto redo
				}
				w.setErr(err)
				return
			}
			glog.V(2).Infof("chunk %d handled", chunk.id)
		}
	}()
}

// Write satisfies the io.Writer interface.
func (w *Writer) Write(p []byte) (int, error) {
	if err := w.getErr(); err != nil {
		return 0, err
	}
	w.start.Do(func() {
		w.csize = w.ChunkSize
		if w.csize == 0 {
			w.csize = 1e8
		}
		w.w = newMemoryBuffer()
	})
	left := w.csize - w.w.Len()
	if len(p) < left {
		return w.w.Write(p)
	}
	i, err := w.w.Write(p[:left])
	if err != nil {
		w.setErr(err)
		return i, err
	}
	if err := w.sendChunk(); err != nil {
		w.setErr(err)
		return i, w.getErr()
	}
	k, err := w.Write(p[left:])
	if err != nil {
		w.setErr(err)
	}
	return i + k, err
}

func (w *Writer) simpleWriteFile() error {
	ue, err := w.o.b.b.getUploadURL(w.ctx)
	if err != nil {
		return err
	}
	sha1 := w.w.Hash()
	ctype := w.contentType
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	r, err := w.w.Reader()
	if err != nil {
		return err
	}
redo:
	f, err := ue.uploadFile(w.ctx, r, int(w.w.Len()), w.name, ctype, sha1, w.info)
	if err != nil {
		if w.o.b.r.reupload(err) {
			glog.Infof("b2 writer: %v; retrying", err)
			u, err := w.o.b.b.getUploadURL(w.ctx)
			if err != nil {
				return err
			}
			ue = u
			goto redo
		}
		return err
	}
	w.o.f = f
	return nil
}

func (w *Writer) getLargeFile() (beLargeFileInterface, error) {
	if !w.Resume {
		ctype := w.contentType
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		return w.o.b.b.startLargeFile(w.ctx, w.name, ctype, w.info)
	}
	next := 1
	seen := make(map[int]string)
	var size int64
	var fi beFileInterface
	for {
		cur := &Cursor{Name: w.name}
		objs, _, err := w.o.b.ListObjects(w.ctx, 1, cur)
		if err != nil {
			return nil, err
		}
		if len(objs) < 1 || objs[0].name != w.name {
			w.Resume = false
			return w.getLargeFile()
		}
		fi = objs[0].f
		parts, n, err := fi.listParts(w.ctx, next, 100)
		if err != nil {
			return nil, err
		}
		next = n
		for _, p := range parts {
			seen[p.number()] = p.sha1()
			size += p.size()
		}
		if len(parts) == 0 {
			break
		}
		if next == 0 {
			break
		}
	}
	w.seen = make(map[int]string) // copy the map
	for id, sha := range seen {
		w.seen[id] = sha
	}
	return fi.compileParts(size, seen), nil
}

func (w *Writer) sendChunk() error {
	var err error
	w.once.Do(func() {
		lf, e := w.getLargeFile()
		if e != nil {
			err = e
			return
		}
		w.file = lf
		w.ready = make(chan chunk)
		if w.ConcurrentUploads < 1 {
			w.ConcurrentUploads = 1
		}
		for i := 0; i < w.ConcurrentUploads; i++ {
			w.thread()
		}
	})
	if err != nil {
		return err
	}
	select {
	case w.ready <- chunk{
		id:  w.cidx + 1,
		buf: w.w,
	}:
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
	w.cidx++
	w.w = newMemoryBuffer()
	return nil
}

// Close satisfies the io.Closer interface.  It is critical to check the return
// value of Close on all writers.
func (w *Writer) Close() error {
	w.done.Do(func() {
		if w.cidx == 0 {
			w.setErr(w.simpleWriteFile())
			return
		}
		if w.w.Len() > 0 {
			if err := w.sendChunk(); err != nil {
				w.setErr(err)
				return
			}
		}
		close(w.ready)
		w.wg.Wait()
		f, err := w.file.finishLargeFile(w.ctx)
		if err != nil {
			w.setErr(err)
			return
		}
		w.o.f = f
	})
	return w.getErr()
}

// WithAttrs sets the writable attributes of the resulting file to given
// values.  WithAttrs must be called before the first call to Write.
func (w *Writer) WithAttrs(attrs *Attrs) *Writer {
	w.contentType = attrs.ContentType
	w.info = make(map[string]string)
	for k, v := range attrs.Info {
		w.info[k] = v
	}
	if len(w.info) < 10 && !attrs.LastModified.IsZero() {
		w.info["src_last_modified_millis"] = fmt.Sprintf("%d", attrs.LastModified.UnixNano()/1e6)
	}
	return w
}
