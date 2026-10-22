package export

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

var zeroTime = time.Unix(0, 0)

// Media types of an exported output.
const (
	ManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	EmptyMediaType    = "application/vnd.oci.empty.v1+json"
	LayerMediaType    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// Spec is one export, built by the controller (never from the guest).
type Spec struct {
	// Archive is the packaged output in the spool.
	Archive      string            `json:"archive"`
	Repository   string            `json:"repository"`
	Tag          string            `json:"tag"`
	ArtifactType string            `json:"artifactType"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	MaxBytes     int64             `json:"maxBytes"`
	MaxEntries   int64             `json:"maxEntries"`
}

// Result is what the Job reports (its termination message).
type Result struct {
	Reference     string `json:"reference,omitempty"`
	Digest        string `json:"digest,omitempty"`
	Size          int64  `json:"size,omitempty"`
	ContentDigest string `json:"contentDigest,omitempty"`
	ContentBytes  int64  `json:"contentBytes,omitempty"`
	Entries       int64  `json:"entries,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Message       string `json:"message,omitempty"`
}

// Push verifies the archive and pushes it as a one-layer OCI artifact:
// layer, then an empty config, then the manifest by tag. The manifest is
// written only once both blobs are in the registry. The same archive and spec
// give the same manifest digest (deterministic gzip, no timestamps).
func Push(ctx context.Context, spec Spec, auth authn.Authenticator, rt http.RoundTripper) (Result, error) {
	f, err := os.Open(spec.Archive)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, refuse(ReasonArchiveAbsent, "the packaged output is not on this node")
		}
		return Result{}, err
	}
	sum, err := VerifyArchive(f, spec.MaxBytes, spec.MaxEntries)
	f.Close()
	if err != nil {
		return Result{}, err
	}
	layer, err := newGzipLayer(spec.Archive)
	if err != nil {
		return Result{}, err
	}
	repo, err := name.NewRepository(spec.Repository, name.StrictValidation)
	if err != nil {
		return Result{}, refuse(ReasonInvalidEntry, "repository %q: %v", spec.Repository, err)
	}
	opts := []remote.Option{remote.WithAuth(auth), remote.WithContext(ctx), remote.WithTransport(httpsOnly{rt})}
	cfg := static.NewLayer([]byte("{}"), types.MediaType(EmptyMediaType))
	for _, l := range []v1.Layer{layer, cfg} {
		if err := remote.WriteLayer(repo, l, opts...); err != nil {
			return Result{}, classify(err)
		}
	}
	cfgDigest, _ := cfg.Digest()
	m := manifest{
		SchemaVersion: 2,
		MediaType:     ManifestMediaType,
		ArtifactType:  spec.ArtifactType,
		Config:        descriptor{MediaType: EmptyMediaType, Digest: cfgDigest.String(), Size: 2},
		Layers:        []descriptor{{MediaType: LayerMediaType, Digest: layer.digest.String(), Size: layer.size}},
		Annotations:   spec.Annotations,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return Result{}, err
	}
	tag := repo.Tag(spec.Tag)
	if err := remote.Put(tag, rawManifest(raw), opts...); err != nil {
		return Result{}, classify(err)
	}
	d := sha256.Sum256(raw)
	digest := fmt.Sprintf("sha256:%x", d)
	return Result{
		Reference:     repo.String() + "@" + digest,
		Digest:        digest,
		Size:          layer.size,
		ContentDigest: sum.Digest,
		ContentBytes:  sum.ContentBytes,
		Entries:       sum.Entries,
	}, nil
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type rawManifest []byte

func (r rawManifest) RawManifest() ([]byte, error)        { return r, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return ManifestMediaType, nil }

// gzipLayer is the archive gzip-compressed on the fly, the same way every
// time (default level, empty header), so its digest is computed once and the
// upload produces exactly those bytes.
type gzipLayer struct {
	path   string
	digest v1.Hash
	diffID v1.Hash
	size   int64
}

func newGzipLayer(p string) (*gzipLayer, error) {
	l := &gzipLayer{path: p}
	rc, err := l.Compressed()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return nil, err
	}
	l.size = n
	l.digest = v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%x", h.Sum(nil))}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dh := sha256.New()
	if _, err := io.Copy(dh, f); err != nil {
		return nil, err
	}
	l.diffID = v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%x", dh.Sum(nil))}
	return l, nil
}

func (l *gzipLayer) Digest() (v1.Hash, error)             { return l.digest, nil }
func (l *gzipLayer) DiffID() (v1.Hash, error)             { return l.diffID, nil }
func (l *gzipLayer) Size() (int64, error)                 { return l.size, nil }
func (l *gzipLayer) MediaType() (types.MediaType, error)  { return LayerMediaType, nil }
func (l *gzipLayer) Uncompressed() (io.ReadCloser, error) { return os.Open(l.path) }

func (l *gzipLayer) Compressed() (io.ReadCloser, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		zw, _ := gzip.NewWriterLevel(pw, gzip.DefaultCompression)
		zw.Header = gzip.Header{OS: 255}
		_, err := io.Copy(zw, f)
		if cerr := zw.Close(); err == nil {
			err = cerr
		}
		f.Close()
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// httpsOnly refuses any request that is not HTTPS, including the token
// endpoint a registry points to: the platform credential never travels in
// clear text, whatever the registry or its name suggest.
type httpsOnly struct{ rt http.RoundTripper }

func (h httpsOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, fmt.Errorf("refusing a %s request to %s: export is HTTPS only", r.URL.Scheme, r.URL.Host)
	}
	return h.rt.RoundTrip(r)
}

// classify turns a registry error into a definitive failure where retrying
// cannot help (refused credentials); anything else is returned as is and the
// Job retries it.
func classify(err error) error {
	var te *transport.Error
	if errors.As(err, &te) && (te.StatusCode == http.StatusUnauthorized || te.StatusCode == http.StatusForbidden) {
		return refuse(ReasonUnauthorized, "the registry refused the export credential (%d)", te.StatusCode)
	}
	return err
}
