package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

// annotationsFile is the downward API file; a variable for tests.
var (
	annotationsFile = podmode.ShimAnnotationsFile
	tokenFile       = podmode.ShimTokenFile
)

// readAnnotation returns one annotation from a downward API annotations file
// (lines of key="Go-quoted value"), "" when it is not set.
func readAnnotation(file, key string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok || k != key {
			continue
		}
		s, err := strconv.Unquote(v)
		if err != nil {
			return "", fmt.Errorf("%s: %w", k, err)
		}
		return s, nil
	}
	return "", sc.Err()
}

// errNoRelay: the controller has not written the relay yet.
var errNoRelay = errors.New("the microVM is not linked to this Pod yet")

// readRelay returns the relay address and the TLS config to reach it.
func readRelay() (podmode.Relay, *tls.Config, error) {
	var r podmode.Relay
	s, err := readAnnotation(annotationsFile, podmode.AnnotationRelay)
	if errors.Is(err, os.ErrNotExist) || err == nil && s == "" {
		return r, nil, errNoRelay
	}
	if err != nil {
		return r, nil, err
	}
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return r, nil, fmt.Errorf("%s: %w", podmode.AnnotationRelay, err)
	}
	pool := x509.NewCertPool()
	if r.Addr == "" || r.ServerName == "" || !pool.AppendCertsFromPEM([]byte(r.CA)) {
		return r, nil, fmt.Errorf("%s: incomplete", podmode.AnnotationRelay)
	}
	return r, &tls.Config{RootCAs: pool, ServerName: r.ServerName, MinVersion: tls.VersionTLS12}, nil
}

// readToken returns the stand-in's relay token, read per use (the kubelet
// rotates it).
func readToken() (string, error) {
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
