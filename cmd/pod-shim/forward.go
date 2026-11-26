package main

// The stand-in's process forwards each
// of the workload's TCP ports to the same port on the launcher of the
// stand-in's microVM, whose address the controller writes into the stand-in's
// podmode.kubeswift.io/target annotation (read through the downward API, which
// the kubelet refreshes in place). Until there is a target, connections are
// refused rather than held. It exits 0 on SIGTERM.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

const (
	podShimTargetKey = "podmode.kubeswift.io/target"
	podShimPoll      = time.Second
	podShimDial      = 5 * time.Second
	podShimDrain     = 2 * time.Second
)

func podShim(args []string) int {
	fs := flag.NewFlagSet("pod-shim", flag.ContinueOnError)
	file := fs.String("annotations-file", annotationsFile, "downward API file with the pod's annotations")
	portList := fs.String("ports", "", "comma-separated TCP ports to forward")
	logs := fs.Bool("logs", true, "stream the microVM's console to stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	annotationsFile = *file
	ports, err := parsePorts(*portList)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pod-shim:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var target atomic.Value
	target.Store("")
	refresh := func() {
		t, err := readTarget(*file)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "pod-shim: reading", *file+":", err)
			return
		}
		if old := target.Swap(t); old != t {
			fmt.Printf("pod-shim: forwarding %s to %q\n", *portList, t)
		}
	}
	refresh()
	// An exit already recorded when this process started belongs to an
	// earlier run (the kubelet restarted the shim after it): wait for the
	// next one.
	exit0, _ := readAnnotation(annotationsFile, podmode.AnnotationExit)
	if *logs {
		go followLogs(ctx, os.Stdout)
	}

	var wg sync.WaitGroup
	for _, p := range ports {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if err != nil {
			fmt.Fprintln(os.Stderr, "pod-shim:", err)
			return 1
		}
		go func() { <-ctx.Done(); _ = ln.Close() }()
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			servePort(ctx, ln, port, func() string { return target.Load().(string) })
		}(p)
	}
	tick := time.NewTicker(podShimPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return 0
		case <-tick.C:
			refresh()
			if code, ok := workloadExit(exit0); ok {
				fmt.Printf("pod-shim: the workload exited with code %d\n", code)
				// Let the console stream catch up before the container ends.
				time.Sleep(podShimDrain)
				return code
			}
		}
	}
}

func parsePorts(list string) ([]int, error) {
	var out []int
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := strconv.Atoi(s)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("bad port %q", s)
		}
		out = append(out, p)
	}
	return out, nil
}

// workloadExit returns the exit code once the controller recorded an exit
// other than exit0, as a process exit code (0-255; a non-zero code never
// becomes 0).
func workloadExit(exit0 string) (int, bool) {
	v, err := readAnnotation(annotationsFile, podmode.AnnotationExit)
	if err != nil || v == "" || v == exit0 {
		return 0, false
	}
	_, code, ok := podmode.ParseExit(v)
	if !ok {
		return 0, false
	}
	if c := code & 0xff; c != 0 || code == 0 {
		return c, true
	}
	return 1, true
}

// readTarget returns the launcher IP from the target annotation, "" when it
// is not set.
func readTarget(file string) (string, error) {
	t, err := readAnnotation(file, podShimTargetKey)
	if err != nil {
		return "", err
	}
	if t != "" && net.ParseIP(t) == nil {
		return "", fmt.Errorf("%s: %q is not an IP address", podShimTargetKey, t)
	}
	return t, nil
}

func servePort(ctx context.Context, ln net.Listener, port int, target func() string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			fmt.Fprintln(os.Stderr, "pod-shim: accept:", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go forward(ctx, c, target(), port)
	}
}

func forward(ctx context.Context, c net.Conn, target string, port int) {
	defer c.Close()
	if target == "" {
		return
	}
	d := net.Dialer{Timeout: podShimDial}
	u, err := d.DialContext(ctx, "tcp", net.JoinHostPort(target, strconv.Itoa(port)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "pod-shim:", err)
		return
	}
	defer u.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(u, c)
	go pipe(c, u)
	select {
	case <-done:
		<-done
	case <-ctx.Done():
	}
}
