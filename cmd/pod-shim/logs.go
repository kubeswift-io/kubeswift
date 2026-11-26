package main

// followLogs makes `kubectl logs <stand-in>` show the microVM: it streams the
// guest's console from the relay to stdout, reconnecting from the last line it
// printed (kubelet timestamps, so a reconnect neither repeats nor skips).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

const logsRetry = 3 * time.Second

func followLogs(ctx context.Context, out io.Writer) {
	var last time.Time
	reported := ""
	for ctx.Err() == nil {
		err := streamLogs(ctx, out, &last)
		if err != nil && !errors.Is(err, errNoRelay) && ctx.Err() == nil && err.Error() != reported {
			fmt.Fprintln(os.Stderr, "pod-shim: microVM logs:", err)
			reported = err.Error()
		}
		select {
		case <-ctx.Done():
		case <-time.After(logsRetry):
		}
	}
}

// streamLogs prints the console lines after *last until the stream ends.
func streamLogs(ctx context.Context, out io.Writer, last *time.Time) error {
	rel, tlsCfg, err := readRelay()
	if err != nil {
		return err
	}
	token, err := readToken()
	if err != nil {
		return err
	}
	tr := &http.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, rel.Addr)
		},
		ResponseHeaderTimeout: 30 * time.Second,
	}
	defer tr.CloseIdleConnections()
	u := url.URL{Scheme: "https", Host: rel.ServerName, Path: podmode.RelayLogsPath}
	if !last.IsZero() {
		u.RawQuery = url.Values{"sinceTime": {last.UTC().Format(time.RFC3339)}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return copyLogLines(resp.Body, out, last)
}

// copyLogLines writes each "<RFC3339Nano> <line>" newer than *last without
// its timestamp, and moves *last.
func copyLogLines(r io.Reader, out io.Writer, last *time.Time) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			ts, rest, ok := strings.Cut(line, " ")
			t, perr := time.Parse(time.RFC3339Nano, ts)
			switch {
			case !ok || perr != nil:
				_, _ = io.WriteString(out, line)
			case t.After(*last):
				*last = t
				_, _ = io.WriteString(out, rest)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
