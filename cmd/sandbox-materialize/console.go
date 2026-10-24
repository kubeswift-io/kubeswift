package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// followConsole copies a sandbox's console file to stdout as it grows, so
// the console is read with the pod logs API (`kubectl logs <pod> -c console`)
// rather than an exec into the privileged launcher. It runs as the launcher
// pod's unprivileged `console` container with the console volume mounted
// read-only, and never interprets what it copies: the bytes are the
// workload's, binary or not. It waits for the file to appear (a warm slot's
// workload log appears at checkout) and follows it through truncation.
//
// It is an ordinary container of the launcher pod, started with the launcher
// (an init-ordered sidecar would delay every cold start by a kubelet step),
// so it must end with the launcher and never decide the pod's outcome: it
// stops when the launcher's FIFO (--until-closed) reaches EOF, which the
// kernel causes however the launcher ends, or on SIGTERM, copies what is
// left, and always exits 0.
func followConsole(args []string) int {
	fs := flag.NewFlagSet("follow-console", flag.ContinueOnError)
	file := fs.String("file", "", "console file to follow")
	until := fs.String("until-closed", "", "FIFO the launcher holds open while it lives: stop at its EOF")
	wait := fs.Duration("until-closed-wait", 2*time.Minute, "stop if the FIFO has not appeared by then (a launcher without one)")
	poll := fs.Duration("poll", time.Second, "longest wait between checks when no change is signalled")
	if err := fs.Parse(args); err != nil || *file == "" {
		fmt.Fprintln(os.Stderr, "follow-console: --file is required")
		return 0
	}
	stop := make(chan os.Signal, 2)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	if *until != "" {
		go func() {
			untilClosed(*until, *wait, *poll)
			stop <- syscall.SIGTERM
		}()
	}
	if err := follow(*file, os.Stdout, *poll, stop); err != nil {
		fmt.Fprintf(os.Stderr, "follow-console: %v\n", err)
	}
	return 0
}

// untilClosed returns once every writer of the FIFO at path has closed it,
// or when it has not appeared within wait.
func untilClosed(path string, wait, poll time.Duration) {
	deadline := time.Now().Add(wait)
	for {
		fi, err := os.Stat(path)
		if err == nil && fi.Mode()&os.ModeNamedPipe != 0 {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "follow-console: %s did not appear; stopping\n", path)
			return
		}
		time.Sleep(poll)
	}
	// Opening for reading returns at once: the launcher holds it read-write.
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "follow-console: %v; stopping\n", err)
		return
	}
	defer f.Close()
	buf := make([]byte, 512)
	for {
		if _, err := f.Read(buf); err != nil {
			return // io.EOF: the launcher is gone
		}
	}
}

// follow copies path to w until stop, then copies what is left.
func follow(path string, w io.Writer, poll time.Duration, stop <-chan os.Signal) error {
	notify := newDirWatch(path)
	defer notify.close()
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	var off int64
	buf := make([]byte, 64<<10)
	for {
		if f == nil {
			var err error
			if f, err = os.Open(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if f != nil {
			if fi, err := f.Stat(); err == nil && fi.Size() < off {
				off = 0 // truncated: start over
			}
			for {
				n, err := f.ReadAt(buf, off)
				if n > 0 {
					if _, werr := w.Write(buf[:n]); werr != nil {
						return werr
					}
					off += int64(n)
				}
				if err != nil {
					break // io.EOF, or nothing more for now
				}
			}
		}
		select {
		case <-stop:
			if f != nil {
				_, err := io.Copy(w, io.NewSectionReader(f, off, 1<<62))
				return err
			}
			return nil
		default:
		}
		notify.wait(poll)
	}
}

// dirWatch wakes follow when the file's directory changes (inotify), or after
// the poll interval when inotify is unavailable.
type dirWatch struct{ fd int }

func newDirWatch(path string) *dirWatch {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return &dirWatch{fd: -1}
	}
	dir := filepath.Dir(path)
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_MODIFY|unix.IN_CREATE|unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO|unix.IN_ATTRIB); err != nil {
		unix.Close(fd)
		return &dirWatch{fd: -1}
	}
	return &dirWatch{fd: fd}
}

// wait returns on a change, or after poll: follow checks for a stop between
// waits, so a stop is seen within one poll interval.
func (d *dirWatch) wait(poll time.Duration) {
	if d.fd < 0 {
		time.Sleep(poll)
		return
	}
	fds := []unix.PollFd{{Fd: int32(d.fd), Events: unix.POLLIN}}
	if n, _ := unix.Poll(fds, int(poll.Milliseconds())); n > 0 {
		buf := make([]byte, 4096)
		for {
			if n, err := unix.Read(d.fd, buf); n <= 0 || err != nil {
				break
			}
		}
	}
}

func (d *dirWatch) close() {
	if d.fd >= 0 {
		unix.Close(d.fd)
	}
}
