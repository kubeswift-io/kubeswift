package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Writes and mount points the platform puts inside the sandbox root (secret
// files, artifact and output mount points, the resolver config, the
// pseudo-filesystems). The root holds an untrusted image, so a plain
// filepath.Join(root, p) would follow the image's symlinks on the agent's
// side of the chroot: an image with /hostile -> / would have a secret
// written to /hostile/token land in the bridge's own root. These helpers
// resolve p as the workload sees it (resolveInRoot) and act through an
// os.Root, so the result is always inside the sandbox root.
//
// The returned host paths are used for mount(2), which takes a path. They are
// safe because nothing runs in the sandbox root while they are used: the cold
// init sets up before it starts the workload, and a warm checkout before it
// runs one.

// mkdirInRoot creates the directory p (clean, absolute) and its parents in
// root and returns its path from outside root. It refuses a p that resolves
// to the root itself: everything it creates is a mount point.
func mkdirInRoot(root, p string, perm os.FileMode) (string, error) {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return "", fmt.Errorf("%q is not a clean absolute path", p)
	}
	r, err := openRoot(root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	rel, err := resolveInRoot(r, p, true)
	if err != nil {
		return "", err
	}
	if rel == "." { // e.g. an image with /proc -> /: mounting there would hide the rootfs
		return "", errors.New("resolves to the sandbox root")
	}
	if err := r.MkdirAll(rel, perm); err != nil {
		return "", err
	}
	fi, err := r.Stat(rel)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", p)
	}
	return filepath.Join(r.Name(), rel), nil
}

// writeFileInRoot writes data to the file p (clean, absolute) in root with
// mode, creating its directories. A symlink at p itself is replaced, not
// followed, so the file is the one chownPaths (which does not follow the last
// element either) gives to the workload user. Anything at p other than a
// regular file or a symlink is refused, so a FIFO cannot block the write.
func writeFileInRoot(root, p string, data io.Reader, mode os.FileMode) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
		return fmt.Errorf("%q is not a clean absolute path", p)
	}
	r, err := openRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	rel, err := resolveInRoot(r, p, false)
	if err != nil {
		return err
	}
	if rel == "." {
		return errors.New("refusing to write the sandbox root")
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := r.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if fi, err := r.Lstat(rel); err == nil {
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			if err := r.Remove(rel); err != nil {
				return err
			}
		case !fi.Mode().IsRegular():
			return fmt.Errorf("%s exists and is not a regular file", p)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Chmod(mode); err != nil { // the open's perm is masked by the umask
		return errors.Join(err, f.Close())
	}
	return f.Close()
}
