package tar

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/influxdata/influxdb/pkg/file"
)

// Stream is a convenience function for creating a tar of a shard dir. It walks over the directory and subdirs,
// possibly writing each file to a tar writer stream.  By default StreamFile is used, which will result in all files
// being written.  A custom writeFunc can be passed so that each file may be written, modified+written, or skipped
// depending on the custom logic.
func Stream(w io.Writer, dir, relativePath string, writeFunc func(f os.FileInfo, shardRelativePath, fullPath string, tw *tar.Writer) error, keepTarOpen bool) error {
	tw := tar.NewWriter(w)
	// The caller may want to make the data we generate to be a part of an existing tar, in which case we do not want
	// to write the trailing zero blocks that Close does but instead just ensure the shard data has been fully flushed.
	// Close does not release any resources so not calling it has no ill effects from that point-of-view.
	if keepTarOpen {
		defer tw.Flush()
	} else {
		defer tw.Close()
	}

	if writeFunc == nil {
		writeFunc = StreamFile
	}

	return filepath.Walk(dir, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip adding an entry for the root dir
		if dir == path && f.IsDir() {
			if keepTarOpen {
				// It is possible the shard is otherwise empty. To avoid shard backup to be considered failed in that
				// case always write an entry for the root directory. The restore command will ignore it (all directories
				// are categorically ignored) but it causes some data to be returned.
				// This is necessary as the tar generated from here is directly added as part of the backup tar and we
				// cannot add end of tar zero bytes to signal some data was returned.
				return writeFunc(f, relativePath, path, tw)
			}
			return nil
		}

		// Figure out the the full relative path including any sub-dirs
		subDir, _ := filepath.Split(path)
		subDir, err = filepath.Rel(dir, subDir)
		if err != nil {
			return err
		}

		return writeFunc(f, filepath.Join(relativePath, subDir), path, tw)
	})
}

// Generates a filtering function for Stream that checks an incoming file, and only writes the file to the stream if
// its mod time is later than since.  Example: to tar only files newer than a certain datetime, use
// tar.Stream(w, dir, relativePath, SinceFilterTarFile(datetime))
func SinceFilterTarFile(since time.Time) func(f os.FileInfo, shardRelativePath, fullPath string, tw *tar.Writer) error {
	return func(f os.FileInfo, shardRelativePath, fullPath string, tw *tar.Writer) error {
		if f.ModTime().After(since) {
			return StreamFile(f, shardRelativePath, fullPath, tw)
		}
		return nil
	}
}

// stream a single file to tw, extending the header name using the shardRelativePath
func StreamFile(f os.FileInfo, shardRelativePath, fullPath string, tw *tar.Writer) error {
	return StreamRenameFile(f, f.Name(), shardRelativePath, fullPath, tw)
}

/// Stream a single file to tw, using tarHeaderFileName instead of the actual filename
// e.g., when we want to write a *.tmp file using the original file's non-tmp name.
func StreamRenameFile(f os.FileInfo, tarHeaderFileName, relativePath, fullPath string, tw *tar.Writer) error {
	h, err := tar.FileInfoHeader(f, f.Name())
	if err != nil {
		return err
	}
	h.Name = filepath.ToSlash(filepath.Join(relativePath, tarHeaderFileName))

	if err := tw.WriteHeader(h); err != nil {
		return err
	}

	if !f.Mode().IsRegular() {
		return nil
	}

	fr, err := os.Open(fullPath)
	if err != nil {
		return err
	}

	defer fr.Close()

	_, err = io.CopyN(tw, fr, h.Size)

	return err
}

// Restore reads a tar archive from r and extracts all of its files into dir,
// using only the base name of each file.
func Restore(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		if err := extractFile(tr, dir); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
	}

	return file.SyncDir(dir)
}

// extractFile copies the next file from tr into dir, using the file's base name.
func extractFile(tr *tar.Reader, dir string) error {
	// Read next archive file.
	hdr, err := tr.Next()
	if err != nil {
		return err
	}

	// The hdr.Name is the relative path of the file from the root data dir.
	// e.g (db/rp/1/xxxxx.tsm or db/rp/1/index/xxxxxx.tsi)
	sections := strings.Split(filepath.FromSlash(hdr.Name), string(filepath.Separator))
	if len(sections) < 3 {
		return fmt.Errorf("invalid archive path: %s", hdr.Name)
	}

	relativePath := filepath.Join(sections[3:]...)

	subDir, _ := filepath.Split(relativePath)
	// If this is a directory entry (usually just `index` for tsi), create it an move on.
	if hdr.Typeflag == tar.TypeDir {
		return os.MkdirAll(filepath.Join(dir, subDir), os.FileMode(hdr.Mode).Perm())
	}

	// Make sure the dir we need to write into exists.  It should, but just double check in
	// case we get a slightly invalid tarball.
	if subDir != "" {
		if err := os.MkdirAll(filepath.Join(dir, subDir), 0755); err != nil {
			return err
		}
	}

	destPath := filepath.Join(dir, relativePath)
	return CreateFileFromTar(destPath, tr, hdr)
}

// CreateFileFromTar copies the contents of current file in the tar into local file via a temp file
func CreateFileFromTar(destPath string, tr *tar.Reader, hdr *tar.Header) error {
	tmp := destPath + ".tmp"

	// Create new file on disk.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, os.FileMode(hdr.Mode).Perm())
	if err != nil {
		return err
	}
	defer f.Close()

	// Copy from archive to the file.
	if _, err := io.CopyN(f, tr, hdr.Size); err != nil {
		return err
	}

	// Sync to disk & close.
	if err := f.Sync(); err != nil {
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	return file.RenameFile(tmp, destPath)
}
