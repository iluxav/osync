package bundle

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// tree creates files under a new folder. A name ending in a slash is an
// empty folder; "x!" marks the file executable.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, "!") {
			name, mode = strings.TrimSuffix(name, "!"), 0o755
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// listing returns every file and folder under dir with file content, and a
// trailing "*" on the names of executables.
func listing(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := make(map[string]string)
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			got[rel+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(p)
		if info.Mode()&0o111 != 0 {
			rel += "*"
		}
		got[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func pack(t *testing.T, paths ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, paths); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFilesAndFoldersSurviveTheTrip(t *testing.T) {
	src := tree(t, map[string]string{
		"notes.txt":             "hello\n",
		"run.sh!":               "#!/bin/sh\n",
		"ünïcödé name.txt":      "✓",
		"project/readme.md":     "# project\n",
		"project/src/main.go":   "package main\n",
		"project/empty/":        "",
		"project/src/deep/a/b/": "",
	})
	old := time.Date(2020, 5, 17, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "notes.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(src, "notes.txt"),
		filepath.Join(src, "project"),
		filepath.Join(src, "run.sh"),
		filepath.Join(src, "ünïcödé name.txt"),
	}

	sum, err := Scan(paths)
	if err != nil {
		t.Fatal(err)
	}
	// 4 files of 6+10+3+10+13 bytes; folders project, src, empty, deep, a, b.
	if want := []string{"notes.txt", "project", "run.sh", "ünïcödé name.txt"}; !slices.Equal(sum.Names, want) {
		t.Errorf("names = %q, want %q", sum.Names, want)
	}
	if sum.Bytes != 42 || sum.Entries != 11 {
		t.Errorf("scan found %d bytes in %d entries, want 42 in 11", sum.Bytes, sum.Entries)
	}

	dst := t.TempDir()
	got, err := Read(bytes.NewReader(pack(t, paths...)), dst)
	if err != nil {
		t.Fatal(err)
	}
	if !got.equal(sum) {
		t.Errorf("unpacked %+v, but the scan said %+v", got, sum)
	}
	want := map[string]string{
		"notes.txt":             "hello\n",
		"run.sh*":               "#!/bin/sh\n",
		"ünïcödé name.txt":      "✓",
		"project/":              "",
		"project/readme.md":     "# project\n",
		"project/src/":          "",
		"project/src/main.go":   "package main\n",
		"project/empty/":        "",
		"project/src/deep/":     "",
		"project/src/deep/a/":   "",
		"project/src/deep/a/b/": "",
	}
	if out := listing(t, dst); !mapsEqual(out, want) {
		t.Errorf("unpacked tree = %q\nwant %q", out, want)
	}
	if info, err := os.Stat(filepath.Join(dst, "notes.txt")); err != nil || !info.ModTime().Equal(old) {
		t.Errorf("notes.txt modified %v, %v; want the original time %v", info.ModTime(), err, old)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

func TestFilesWithTheSameNameAreToldApart(t *testing.T) {
	src := tree(t, map[string]string{"a/report.pdf": "first", "b/report.pdf": "second", "c/report.pdf": "third"})
	paths := []string{
		filepath.Join(src, "a", "report.pdf"),
		filepath.Join(src, "b", "report.pdf"),
		filepath.Join(src, "c", "report.pdf"),
	}
	dst := t.TempDir()
	sum, err := Read(bytes.NewReader(pack(t, paths...)), dst)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"report.pdf", "report (2).pdf", "report (3).pdf"}; !slices.Equal(sum.Names, want) {
		t.Errorf("names = %q, want %q", sum.Names, want)
	}
	if got := listing(t, dst); got["report.pdf"] != "first" || got["report (2).pdf"] != "second" || got["report (3).pdf"] != "third" {
		t.Errorf("unpacked %q", got)
	}
}

func TestLinksInsideAFolderAreLeftOutAndACopiedLinkIsFollowed(t *testing.T) {
	src := tree(t, map[string]string{"folder/real.txt": "real", "outside.txt": "outside"})
	if err := os.Symlink(filepath.Join(src, "outside.txt"), filepath.Join(src, "folder", "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "outside.txt"), filepath.Join(src, "shortcut.txt")); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if _, err := Read(bytes.NewReader(pack(t, filepath.Join(src, "folder"), filepath.Join(src, "shortcut.txt"))), dst); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"folder/": "", "folder/real.txt": "real", "shortcut.txt": "outside"}
	if got := listing(t, dst); !mapsEqual(got, want) {
		t.Errorf("unpacked %q, want %q", got, want)
	}
}

func TestWhatCannotBeSentIsRefusedBeforeSending(t *testing.T) {
	src := tree(t, map[string]string{"small.txt": "x"})
	if _, err := Scan([]string{filepath.Join(src, "gone.txt")}); err == nil {
		t.Error("a missing file: want an error")
	}

	big := filepath.Join(src, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file: the size counts, and no disk is used.
	if err := f.Truncate(MaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := Scan([]string{big}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("a file over the limit: err = %v, want one naming the limit", err)
	}
	if _, err := Scan([]string{filepath.Join(src, "small.txt"), big}); err == nil {
		t.Error("files that are over the limit together: want an error")
	}
	if err := Write(io.Discard, []string{big}); err == nil {
		t.Error("Write must refuse what Scan refuses")
	}
}

// archive builds a bundle by hand, the way a broken or hostile peer might.
func archive(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range entries {
		content := "content of " + hdr.Name
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(content))
		}
		hdr.Mode = 0o644
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			io.WriteString(tw, content)
		}
	}
	tw.Close()
	return buf.Bytes()
}

func TestBundlesThatReachOutsideTheirFolderAreRefused(t *testing.T) {
	file := func(name string) tar.Header { return tar.Header{Name: name, Typeflag: tar.TypeReg} }
	hostile := map[string][]tar.Header{
		"a parent folder":            {file("../escaped.txt")},
		"a parent folder further in": {file("ok/../../escaped.txt")},
		"an absolute path":           {file("/tmp/osync-escaped.txt")},
		"a link":                     {{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
		"a file through a link":      {{Name: "dir", Typeflag: tar.TypeSymlink, Linkname: ".."}, file("dir/escaped.txt")},
		"a device":                   {{Name: "null", Typeflag: tar.TypeChar}},
		"the same file twice":        {file("twice.txt"), file("twice.txt")},
		"nothing at all":             {},
	}
	for name, entries := range hostile {
		t.Run(name, func(t *testing.T) {
			outer := t.TempDir()
			dst := filepath.Join(outer, "inbox")
			if err := os.Mkdir(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(bytes.NewReader(archive(t, entries...)), dst); err == nil {
				t.Error("want the bundle refused")
			}
			// Nothing may exist next to the folder that was unpacked into.
			if got := listing(t, outer); len(got) != 0 {
				for name := range got {
					if !strings.HasPrefix(name, "inbox/") {
						t.Errorf("%s was written outside the folder", name)
					}
				}
			}
		})
	}
}

func TestBundleOverTheLimitsIsRefusedByTheReceiver(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// The header alone claims too much; no content follows.
	tw.WriteHeader(&tar.Header{Name: "huge.bin", Typeflag: tar.TypeReg, Size: MaxBytes + 1, Mode: 0o644})
	if _, err := Read(&buf, t.TempDir()); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("err = %v, want the size limit", err)
	}

	buf.Reset()
	tw = tar.NewWriter(&buf)
	for i := range MaxEntries + 1 {
		tw.WriteHeader(&tar.Header{Name: "d" + strings.Repeat("x", i%7) + "/" + string(rune('a'+i%26)) + "/" + time.Duration(i).String(), Typeflag: tar.TypeDir, Mode: 0o755})
	}
	tw.Close()
	if _, err := Read(&buf, t.TempDir()); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("err = %v, want the limit on the number of entries", err)
	}
}

func TestInboxKeepsWholeBundlesOnly(t *testing.T) {
	src := tree(t, map[string]string{"a.txt": "alpha", "dir/b.txt": "beta"})
	paths := []string{filepath.Join(src, "a.txt"), filepath.Join(src, "dir")}
	sum, err := Scan(paths)
	if err != nil {
		t.Fatal(err)
	}
	data := pack(t, paths...)
	inboxDir := filepath.Join(t.TempDir(), "inbox")
	in, err := OpenInbox(inboxDir)
	if err != nil {
		t.Fatal(err)
	}

	// feed writes a bundle in small pieces, as it arrives from the network.
	feed := func(r *Receiver, data []byte) error {
		for len(data) > 0 {
			n := min(len(data), 700)
			if _, err := r.Write(data[:n]); err != nil {
				return err
			}
			data = data[n:]
		}
		return nil
	}

	t.Run("a whole bundle", func(t *testing.T) {
		r, err := in.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if err := feed(r, data); err != nil {
			t.Fatal(err)
		}
		got, err := r.Finish(sum)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || filepath.Base(got[0]) != "a.txt" || filepath.Base(got[1]) != "dir" {
			t.Fatalf("paths = %q", got)
		}
		for _, p := range got {
			if !in.Contains(p) {
				t.Errorf("%s is not inside the inbox", p)
			}
		}
		if b, err := os.ReadFile(filepath.Join(got[1], "b.txt")); err != nil || string(b) != "beta" {
			t.Errorf("dir/b.txt = %q, %v", b, err)
		}
	})

	leftovers := func() int {
		entries, err := os.ReadDir(inboxDir)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := leftovers()

	t.Run("cut short inside a file", func(t *testing.T) {
		r, _ := in.Receive()
		feed(r, data[:700])
		if _, err := r.Finish(sum); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("cut short between two files", func(t *testing.T) {
		// One whole file of the two that were announced.
		r, _ := in.Receive()
		feed(r, pack(t, paths[0]))
		if _, err := r.Finish(sum); err == nil {
			t.Error("want an error: the bundle holds less than was announced")
		}
	})
	t.Run("abandoned", func(t *testing.T) {
		r, _ := in.Receive()
		feed(r, data[:300])
		r.Abort()
	})
	t.Run("unusable from the start", func(t *testing.T) {
		r, _ := in.Receive()
		if err := feed(r, bytes.Repeat([]byte("not a tar archive "), 400)); err == nil {
			t.Error("want Write to fail once the bundle is known to be unusable")
		}
		r.Abort()
	})
	if after := leftovers(); after != before {
		t.Errorf("the inbox holds %d folders, want %d: failed transfers must leave nothing", after, before)
	}

	if in.Contains(src) || in.Contains(inboxDir) || in.Contains(filepath.Join(inboxDir, "..", "elsewhere")) {
		t.Error("Contains is true for a path outside the inbox")
	}
}

func TestInboxKeepsTheNewestAndStartupClearsTheUnfinished(t *testing.T) {
	src := tree(t, map[string]string{"a.txt": "alpha"})
	paths := []string{filepath.Join(src, "a.txt")}
	sum, _ := Scan(paths)
	data := pack(t, paths...)
	dir := t.TempDir()
	in, err := OpenInbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	var (
		kept [][]string
		recv []*Receiver
	)
	for range 4 {
		r, err := in.Receive()
		if err != nil {
			t.Fatal(err)
		}
		r.Write(data)
		got, err := r.Finish(sum)
		if err != nil {
			t.Fatal(err)
		}
		kept, recv = append(kept, got), append(recv, r)
	}
	exists := func() (out []bool) {
		for _, got := range kept {
			_, err := os.Stat(got[0])
			out = append(out, err == nil)
		}
		return out
	}
	recv[3].Keep(2)
	if got, want := exists(), []bool{false, false, true, true}; !slices.Equal(got, want) {
		t.Errorf("bundles left = %v, want only the newest two: %v", got, want)
	}
	// A bundle that took longer than the ones after it is still the one
	// to keep when it is the one being pasted.
	recv[2].Keep(1)
	if got, want := exists(), []bool{false, false, true, false}; !slices.Equal(got, want) {
		t.Errorf("bundles left = %v, want only the one that was kept: %v", got, want)
	}
	again, _ := in.Receive()
	again.Write(data)
	if _, err := again.Finish(sum); err != nil {
		t.Fatal(err)
	}

	// A daemon dies mid-transfer; the next one starts clean.
	r, _ := in.Receive()
	r.Write(data[:100])
	if _, err := OpenInbox(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("the inbox holds %d folders after a restart, want the 2 finished ones", len(entries))
	}
	r.Abort()
}
