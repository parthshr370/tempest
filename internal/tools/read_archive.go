package tools

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// Archive reads stay in memory and bounded: the read tool lists members of
// .zip, .tar, .tar.gz and .tgz files and reads one member with
// archive.ext:member/path. Nothing is extracted to disk.
const (
	maxArchiveBytes       = 64 << 20
	maxArchiveMemberBytes = 16 << 20
	maxArchiveEntries     = 1000
	// maxTarScanBytes bounds how much decompressed tar stream one read
	// walks, so a compression bomb cannot keep the tool busy.
	maxTarScanBytes = 512 << 20
)

// archiveKind returns "zip", "tar" or "tgz" for an archive file name.
func archiveKind(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return "zip"
	case strings.HasSuffix(lower, ".tar"):
		return "tar"
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "tgz"
	}
	return ""
}

// archiveMemberSplit returns the index of the colon that starts a member
// selector (bundle.zip:dir/file.txt), or -1.
func archiveMemberSplit(p string) int {
	lower := strings.ToLower(p)
	best := -1
	for _, ext := range []string{".zip:", ".tar:", ".tgz:", ".tar.gz:"} {
		if index := strings.Index(lower, ext); index >= 0 {
			colon := index + len(ext) - 1
			if best < 0 || colon < best {
				best = colon
			}
		}
	}
	return best
}

// archiveEntry is one member as listed.
type archiveEntry struct {
	name string
	dir  bool
	size int64
}

// archiveMember is the outcome of a member read: its bytes, or a listing
// when the selector names a directory inside the archive.
type archiveMember struct {
	data    []byte
	listing string
}

// listArchive renders the members of an archive, one per line.
func listArchive(kind string, data []byte) (string, error) {
	entries, complete, err := archiveEntries(kind, data, "")
	if err != nil {
		return "", err
	}
	return renderArchiveEntries(entries, complete), nil
}

// readArchiveMember returns member's bytes, or a listing when member is a
// directory inside the archive.
func readArchiveMember(kind string, data []byte, member string) (archiveMember, error) {
	member = strings.TrimPrefix(path.Clean("/"+member), "/")
	if member == "" {
		listing, err := listArchive(kind, data)
		return archiveMember{listing: listing}, err
	}
	var found []byte
	var foundErr error
	matched := false
	visit := func(name string, size int64, open func() (io.Reader, error)) (bool, error) {
		if strings.HasSuffix(name, "/") || normalizeMemberName(name) != member {
			return false, nil
		}
		matched = true
		if size > maxArchiveMemberBytes {
			foundErr = fmt.Errorf("archive member %s is %d bytes, over the %d MiB read limit; use bash (unzip -p or tar -xOf) for a bounded portion", member, size, maxArchiveMemberBytes>>20)
			return true, nil
		}
		reader, err := open()
		if err != nil {
			return true, err
		}
		content, err := io.ReadAll(io.LimitReader(reader, maxArchiveMemberBytes+1))
		if err != nil {
			return true, fmt.Errorf("read archive member %s: %w", member, err)
		}
		if len(content) > maxArchiveMemberBytes {
			foundErr = fmt.Errorf("archive member %s is over the %d MiB read limit; use bash (unzip -p or tar -xOf) for a bounded portion", member, maxArchiveMemberBytes>>20)
			return true, nil
		}
		found = content
		return true, nil
	}
	if err := walkArchive(kind, data, visit); err != nil {
		return archiveMember{}, err
	}
	if foundErr != nil {
		return archiveMember{}, foundErr
	}
	if matched {
		return archiveMember{data: found}, nil
	}
	entries, complete, err := archiveEntries(kind, data, member+"/")
	if err != nil {
		return archiveMember{}, err
	}
	if len(entries) == 0 {
		return archiveMember{}, fmt.Errorf("archive has no member %q; read the archive without a member to list them", member)
	}
	return archiveMember{listing: renderArchiveEntries(entries, complete)}, nil
}

// archiveEntries lists members under prefix, at most maxArchiveEntries;
// complete is false when more exist.
func archiveEntries(kind string, data []byte, prefix string) ([]archiveEntry, bool, error) {
	var entries []archiveEntry
	complete := true
	visit := func(name string, size int64, _ func() (io.Reader, error)) (bool, error) {
		dir := strings.HasSuffix(name, "/")
		normalized := normalizeMemberName(name)
		if normalized == "" || !strings.HasPrefix(normalized+"/", prefix) || normalized+"/" == prefix {
			return false, nil
		}
		if len(entries) >= maxArchiveEntries {
			complete = false
			return true, nil
		}
		if dir {
			normalized += "/"
		}
		entries = append(entries, archiveEntry{name: normalized, dir: dir, size: size})
		return false, nil
	}
	if err := walkArchive(kind, data, visit); err != nil {
		return nil, false, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, complete, nil
}

func renderArchiveEntries(entries []archiveEntry, complete bool) string {
	var b strings.Builder
	if len(entries) == 0 {
		b.WriteString("(empty archive)\n")
	}
	for _, entry := range entries {
		if entry.dir {
			fmt.Fprintf(&b, "%s\n", entry.name)
		} else {
			fmt.Fprintf(&b, "%s (%d bytes)\n", entry.name, entry.size)
		}
	}
	if !complete {
		fmt.Fprintf(&b, "[listing stops at %d members; use bash (unzip -l or tar -tf) for the rest]\n", maxArchiveEntries)
	}
	return b.String()
}

// normalizeMemberName drops leading "./" and "/" and a trailing "/".
func normalizeMemberName(name string) string {
	return strings.Trim(path.Clean("/"+name), "/")
}

// walkArchive calls visit for every member in archive order until visit
// reports done. open is valid only during the call.
func walkArchive(kind string, data []byte, visit func(name string, size int64, open func() (io.Reader, error)) (bool, error)) error {
	switch kind {
	case "zip":
		reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return fmt.Errorf("not a readable zip archive: %w", err)
		}
		for _, file := range reader.File {
			var opened io.ReadCloser
			done, err := visit(file.Name, int64(file.UncompressedSize64), func() (io.Reader, error) {
				var openErr error
				opened, openErr = file.Open()
				if openErr != nil {
					return nil, fmt.Errorf("open archive member %s: %w", file.Name, openErr)
				}
				return opened, nil
			})
			if opened != nil {
				err = errors.Join(err, opened.Close())
			}
			if err != nil || done {
				return err
			}
		}
		return nil
	case "tar", "tgz":
		var stream io.Reader = bytes.NewReader(data)
		if kind == "tgz" {
			gz, err := gzip.NewReader(stream)
			if err != nil {
				return fmt.Errorf("not a readable gzip stream: %w", err)
			}
			defer gz.Close()
			stream = gz
		}
		budget := &io.LimitedReader{R: stream, N: maxTarScanBytes}
		reader := tar.NewReader(budget)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				if budget.N <= 0 {
					return fmt.Errorf("stopped after scanning %d MiB of the tar stream; use bash (tar -tf) for this archive", maxTarScanBytes>>20)
				}
				return nil
			}
			if err != nil {
				if budget.N <= 0 {
					return fmt.Errorf("stopped after scanning %d MiB of the tar stream; use bash (tar -tf) for this archive", maxTarScanBytes>>20)
				}
				return fmt.Errorf("not a readable tar archive: %w", err)
			}
			name := header.Name
			switch header.Typeflag {
			case tar.TypeDir:
				name = strings.TrimSuffix(name, "/") + "/"
			case tar.TypeReg:
			default:
				continue // links, devices and metadata entries carry no readable content
			}
			done, err := visit(name, header.Size, func() (io.Reader, error) { return reader, nil })
			if err != nil || done {
				return err
			}
		}
	}
	return fmt.Errorf("unsupported archive kind %q", kind)
}
