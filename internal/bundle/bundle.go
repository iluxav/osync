// Package bundle packs copied files and folders into one stream for another
// machine, and unpacks such a stream into a folder of its own.
//
// A bundle is a tar archive holding regular files and folders, nothing else.
package bundle

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Limits on one copy. A copy of files goes to every machine whether or not it
// is ever pasted there, so these bound what is reasonable to send unasked.
// Both sides enforce them.
const (
	MaxBytes   = 100 << 20
	MaxEntries = 10_000
)

// Summary says what a bundle holds.
type Summary struct {
	// Names are the copied files and folders as the receiving machine will
	// see them, in the order they were copied.
	Names   []string `json:"names,omitempty"`
	Bytes   int64    `json:"bytes,omitempty"`   // the content of all files
	Entries int      `json:"entries,omitempty"` // files and folders, counting what is inside folders
}

func (s Summary) equal(o Summary) bool {
	return s.Bytes == o.Bytes && s.Entries == o.Entries && slices.Equal(s.Names, o.Names)
}

// add counts one more file or folder, refusing to go over the limits.
func (s *Summary) add(dir bool, size int64) error {
	if s.Entries++; s.Entries > MaxEntries {
		return fmt.Errorf("more than %d files and folders", MaxEntries)
	}
	if dir {
		return nil
	}
	if s.Bytes += size; s.Bytes > MaxBytes || size < 0 {
		return fmt.Errorf("over the %s limit", Size(MaxBytes))
	}
	return nil
}

// Size renders a number of bytes the way a person would say it.
func Size(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%d kB", n>>10)
	default:
		// One decimal, and none when it would be a zero: "2.5 MB", "100 MB".
		return strconv.FormatFloat(math.Round(float64(n)/(1<<20)*10)/10, 'f', -1, 64) + " MB"
	}
}

// Scan reports what a bundle of paths would hold, and fails if they cannot
// be sent: something is missing or unreadable, or the limits are exceeded.
func Scan(paths []string) (Summary, error) {
	sum := Summary{Names: names(paths)}
	err := walk(paths, sum.Names, func(_, _ string, info fs.FileInfo) error {
		return sum.add(info.IsDir(), info.Size())
	})
	return sum, err
}

// names returns what each of paths is called inside a bundle: its base name,
// numbered when two of them share one.
func names(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		base := filepath.Base(filepath.Clean(p))
		name := base
		for n := 2; slices.Contains(out[:i], name); n++ {
			ext := filepath.Ext(base)
			name = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(base, ext), n, ext)
		}
		out[i] = name
	}
	return out
}

// walk calls visit for every file and folder that a bundle of paths holds,
// each folder before what is inside it. name is the slash-separated name
// within the bundle.
func walk(paths, names []string, visit func(name, path string, info fs.FileInfo) error) error {
	for i, top := range paths {
		// A copied link stands for what it points to. Links further down,
		// inside a copied folder, are left out along with devices and
		// sockets: following them could leave the folder or loop.
		top, err := filepath.EvalSymlinks(top)
		if err != nil {
			return err
		}
		err = filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				if p == top {
					return fmt.Errorf("%s is neither a file nor a folder", top)
				}
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(top, p)
			if err != nil {
				return err
			}
			return visit(path.Join(names[i], filepath.ToSlash(rel)), p, info)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Write packs the files and folders at paths into w.
func Write(w io.Writer, paths []string) error {
	tw := tar.NewWriter(w)
	var sum Summary
	err := walk(paths, names(paths), func(name, p string, info fs.FileInfo) error {
		if err := sum.add(info.IsDir(), info.Size()); err != nil {
			return err
		}
		hdr := &tar.Header{Name: name, ModTime: info.ModTime(), Format: tar.FormatPAX}
		if info.IsDir() {
			hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, name+"/", 0o755
			return tw.WriteHeader(hdr)
		}
		// Of the permissions only "may be run" travels; the rest is the
		// receiving machine's business.
		hdr.Typeflag, hdr.Size, hdr.Mode = tar.TypeReg, info.Size(), 0o644
		if info.Mode()&0o111 != 0 {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%s changed while it was being sent", p)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Open returns the bundle of paths as a stream. Reading fails where Write
// would, and Close stops the packing.
func Open(paths []string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(Write(pw, paths)) }()
	return pr
}

// Read unpacks a bundle into dir, an existing empty folder, and reports what
// it held. Nothing is written outside dir whatever the bundle says, and a
// bundle holding anything but files and folders is refused.
func Read(r io.Reader, dir string) (Summary, error) {
	var sum Summary
	// A Root refuses every name that would lead out of dir.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return sum, err
	}
	defer root.Close()

	tops := make(map[string]bool)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sum, fmt.Errorf("reading the bundle: %w", err)
		}
		name := path.Clean(hdr.Name)
		if name == "." || !filepath.IsLocal(filepath.FromSlash(name)) {
			return sum, fmt.Errorf("the bundle names %q, which is not inside it", hdr.Name)
		}
		if top, _, _ := strings.Cut(name, "/"); !tops[top] {
			tops[top] = true
			sum.Names = append(sum.Names, top)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := sum.add(true, 0); err != nil {
				return sum, err
			}
			if err := root.MkdirAll(name, 0o755); err != nil {
				return sum, err
			}
		case tar.TypeReg:
			if err := sum.add(false, hdr.Size); err != nil {
				return sum, err
			}
			if err := unpackFile(root, name, hdr, tr); err != nil {
				return sum, err
			}
		default:
			return sum, fmt.Errorf("the bundle holds %q, which is neither a file nor a folder", hdr.Name)
		}
	}
	if len(sum.Names) == 0 {
		return sum, errors.New("the bundle is empty")
	}
	return sum, nil
}

func unpackFile(root *os.Root, name string, hdr *tar.Header, content io.Reader) error {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	mode := os.FileMode(0o644)
	if hdr.Mode&0o111 != 0 {
		mode = 0o755
	}
	// O_EXCL: a bundle that names the same file twice is broken, not a
	// request to overwrite.
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, content); err != nil {
		f.Close()
		return fmt.Errorf("reading the bundle: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !hdr.ModTime.IsZero() {
		root.Chtimes(name, time.Now(), hdr.ModTime)
	}
	return nil
}
