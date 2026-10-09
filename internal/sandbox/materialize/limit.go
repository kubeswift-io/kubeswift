package materialize

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Size cap for a read-only artifact (Options.MaxBytes). An OCI layout stores
// blobs as pushed, so its size is known from the manifests before anything is
// downloaded. An unpacked tree is extracted from compressed layers, whose
// uncompressed size is not known in advance (a decompression bomb is small
// on the wire), so its extraction stream is counted and stopped at the cap.

// DefaultArtifactMaxBytes is the cap when none is given: 10 GiB.
const DefaultArtifactMaxBytes int64 = 10 << 30

// ErrTooLarge is returned when an artifact exceeds Options.MaxBytes.
var ErrTooLarge = errors.New("artifact exceeds the size limit")

// layoutSize sums the blobs a layout of desc would hold: every manifest,
// config and layer, through nested indexes.
func layoutSize(desc *remote.Descriptor) (int64, error) {
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return 0, err
		}
		return indexSize(idx, desc.Size, 0)
	}
	img, err := desc.Image()
	if err != nil {
		return 0, err
	}
	return imageSize(img, desc.Size)
}

func imageSize(img v1.Image, manifestSize int64) (int64, error) {
	m, err := img.Manifest()
	if err != nil {
		return 0, err
	}
	n := manifestSize + m.Config.Size
	for _, l := range m.Layers {
		n += l.Size
	}
	return n, nil
}

func indexSize(idx v1.ImageIndex, manifestSize int64, depth int) (int64, error) {
	if depth > 4 {
		return 0, fmt.Errorf("index nested more than 4 deep")
	}
	m, err := idx.IndexManifest()
	if err != nil {
		return 0, err
	}
	n := manifestSize
	for _, d := range m.Manifests {
		switch {
		case d.MediaType.IsIndex():
			child, err := idx.ImageIndex(d.Digest)
			if err != nil {
				return 0, err
			}
			s, err := indexSize(child, d.Size, depth+1)
			if err != nil {
				return 0, err
			}
			n += s
		case d.MediaType.IsImage():
			child, err := idx.Image(d.Digest)
			if err != nil {
				return 0, err
			}
			s, err := imageSize(child, d.Size)
			if err != nil {
				return 0, err
			}
			n += s
		default:
			n += d.Size
		}
	}
	return n, nil
}

// cappedReader fails a read once more than max bytes have passed; exactly
// max is allowed.
type cappedReader struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1] // one byte past the cap is enough to know
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		c.exceeded = true
		return 0, ErrTooLarge
	}
	return n, err
}

// DefaultArtifactMaxEntries caps the files, directories and links an unpacked
// artifact may hold: a byte cap alone admits millions of empty files.
const DefaultArtifactMaxEntries = 1 << 20

// ErrTooManyEntries is returned when an unpacked artifact holds more than
// Options.MaxEntries entries.
var ErrTooManyEntries = errors.New("artifact has too many entries")

// entryLimiter passes a tar stream through, reading its headers as they go
// by, and fails the stream at the entry past max.
type entryLimiter struct {
	tee      io.Reader
	pw       *io.PipeWriter
	exceeded atomic.Bool
	done     chan struct{}
}

func newEntryLimiter(r io.Reader, max int) *entryLimiter {
	pr, pw := io.Pipe()
	l := &entryLimiter{tee: io.TeeReader(r, pw), pw: pw, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		tr := tar.NewReader(pr)
		for n := 0; ; n++ {
			if _, err := tr.Next(); err != nil {
				break
			}
			if n >= max {
				l.exceeded.Store(true)
				pr.CloseWithError(ErrTooManyEntries)
				return
			}
		}
		_, _ = io.Copy(io.Discard, pr) // the end-of-archive padding
	}()
	return l
}

func (l *entryLimiter) Read(p []byte) (int, error) {
	n, err := l.tee.Read(p)
	if err == io.EOF {
		l.pw.Close()
		<-l.done
	}
	return n, err
}
