// +build !windows

package desync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/pkg/errors"
)

// UntarOptions are used to influence the behaviour of untar
type UntarOptions struct {
	NoSameOwner       bool
	NoSamePermissions bool
}

// UnTar implements the untar command, decoding a catar file and writing the
// contained tree to a target directory.
func UnTar(ctx context.Context, r io.Reader, dst string, opts UntarOptions) error {
	dec := NewArchiveDecoder(r)
loop:
	for {
		// See if we're meant to stop
		select {
		case <-ctx.Done():
			return Interrupted{}
		default:
		}
		c, err := dec.Next()
		if err != nil {
			return err
		}
		switch n := c.(type) {
		case NodeDirectory:
			err = makeDir(dst, n, opts)
		case NodeFile:
			err = makeFile(dst, n, opts)
		case NodeDevice:
			err = makeDevice(dst, n, opts)
		case NodeSymlink:
			err = makeSymlink(dst, n, opts)
		case nil:
			break loop
		default:
			err = fmt.Errorf("unsupported type %s", reflect.TypeOf(c))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func makeDir(base string, n NodeDirectory, opts UntarOptions) error {
	dst := filepath.Join(base, n.Name)

	// Let's see if there is a dir with the same name already
	if info, err := os.Stat(dst); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", dst)
		}
	} else {
		// Stat error'ed out, presumably because the dir doesn't exist. Create it.
		if err := os.Mkdir(dst, n.Mode); err != nil {
			return err
		}
	}
	// The dir exists now, fix the UID/GID if needed
	if !opts.NoSameOwner {
		if err := os.Chown(dst, n.UID, n.GID); err != nil {
			return err
		}
	}
	if !opts.NoSamePermissions {
		if err := syscall.Chmod(dst, uint32(n.Mode)); err != nil {
			return err
		}
	}
	return os.Chtimes(dst, n.MTime, n.MTime)
}

func makeFile(base string, n NodeFile, opts UntarOptions) error {
	dst := filepath.Join(base, n.Name)

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, n.Mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = io.Copy(f, n.Data); err != nil {
		return err
	}
	if !opts.NoSameOwner {
		if err = f.Chown(n.UID, n.GID); err != nil {
			return err
		}
	}
	if !opts.NoSamePermissions {
		if err := syscall.Chmod(dst, uint32(n.Mode)); err != nil {
			return err
		}
	}
	return os.Chtimes(dst, n.MTime, n.MTime)
}

func makeSymlink(base string, n NodeSymlink, opts UntarOptions) error {
	dst := filepath.Join(base, n.Name)

	if err := syscall.Unlink(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(n.Target, dst); err != nil {
		return err
	}
	// TODO: On Linux, the permissions of the link don't matter so we don't
	// set them here. But they do matter somewhat on Mac, so should probably
	// add some Mac-specific logic for that here.
	// fchmodat() with flag AT_SYMLINK_NOFOLLOW
	if !opts.NoSameOwner {
		if err := os.Lchown(dst, n.UID, n.GID); err != nil {
			return err
		}
	}
	return nil
}

func makeDevice(base string, n NodeDevice, opts UntarOptions) error {
	dst := filepath.Join(base, n.Name)

	if err := syscall.Unlink(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syscall.Mknod(dst, uint32(n.Mode), int(mkdev(n.Major, n.Minor))); err != nil {
		return errors.Wrapf(err, "mknod %s", dst)
	}
	if !opts.NoSameOwner {
		if err := os.Chown(dst, n.UID, n.GID); err != nil {
			return err
		}
	}
	if !opts.NoSamePermissions {
		if err := syscall.Chmod(dst, uint32(n.Mode)); err != nil {
			return errors.Wrapf(err, "chmod %s", dst)
		}
	}
	return os.Chtimes(dst, n.MTime, n.MTime)
}

func mkdev(major, minor uint64) uint64 {
	dev := (major & 0x00000fff) << 8
	dev |= (major & 0xfffff000) << 32
	dev |= (minor & 0x000000ff) << 0
	dev |= (minor & 0xffffff00) << 12
	return dev
}

// UnTarIndex takes an index file (of a chunked catar), re-assembles the catar
// and decodes it on-the-fly into the target directory 'dst'. Uses n gorountines
// to retrieve and decompress the chunks.
func UnTarIndex(ctx context.Context, dst string, index Index, s Store, n int, opts UntarOptions) error {
	type requestJob struct {
		chunk IndexChunk    // requested chunk
		data  chan ([]byte) // channel for the (decompressed) chunk
	}
	var (
		req      = make(chan requestJob)
		assemble = make(chan chan []byte, n)
	)
	g, ctx := errgroup.WithContext(ctx)

	// Use a pipe as input to untar and write the chunks into that (in the right
	// order of course)
	r, w := io.Pipe()

	// Workers - getting chunks from the store
	for i := 0; i < n; i++ {
		g.Go(func() error {
			for r := range req {
				// Pull the chunk from the store
				chunk, err := s.GetChunk(r.chunk.ID)
				if err != nil {
					close(r.data)
					return err
				}
				b, err := chunk.Uncompressed()
				if err != nil {
					close(r.data)
					return err
				}
				// Might as well verify the chunk size while we're at it
				if r.chunk.Size != uint64(len(b)) {
					close(r.data)
					return fmt.Errorf("unexpected size for chunk %s", r.chunk.ID)
				}
				r.data <- b
				close(r.data)
			}
			return nil
		})
	}

	// Feeder - requesting chunks from the workers and handing a result data channel
	// to the assembler
	g.Go(func() error {
	loop:
		for _, c := range index.Chunks {
			data := make(chan []byte, 1)
			select {
			case <-ctx.Done():
				break loop
			case req <- requestJob{chunk: c, data: data}: // request the chunk
				assemble <- data // and hand over the data channel to the assembler
			}
		}
		close(req)      // tell the workers this is it
		close(assemble) // tell the assembler we're done
		return nil
	})

	// Assember - Read from data channels push the chunks into the pipe that untar reads from
	g.Go(func() error {
		for data := range assemble {
			b := <-data
			if _, err := io.Copy(w, bytes.NewReader(b)); err != nil {
				return err
			}
		}
		w.Close() // No more chunks to come, stop the untar
		return nil
	})

	// Run untar in the main goroutine
	err := UnTar(ctx, r, dst, opts)

	// We now have 2 possible error values. pErr for anything that failed during
	// chunk download and assembly of the catar stream and err for failures during
	// the untar stage. If pErr is set, this would have triggered an error from
	// the untar stage as well (since it cancels the context), so pErr takes
	// precedence here.
	if pErr := g.Wait(); pErr != nil {
		return pErr
	}
	return err
}
