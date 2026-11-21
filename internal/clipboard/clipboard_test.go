package clipboard

import (
	"slices"
	"strings"
	"testing"
)

func TestParseURIList(t *testing.T) {
	tests := []struct {
		name string
		list string
		want []string
	}{
		{"one file", "file:///home/me/report.pdf\r\n", []string{"/home/me/report.pdf"}},
		{"several, as a file manager writes them", "file:///a/one.txt\r\nfile:///a/two.txt\r\n", []string{"/a/one.txt", "/a/two.txt"}},
		{"bare newlines and no final one", "file:///a/one.txt\nfile:///a/two.txt", []string{"/a/one.txt", "/a/two.txt"}},
		{"escaped characters", "file:///a/with%20space/%C3%BCn%C3%AF.txt", []string{"/a/with space/ünï.txt"}},
		{"a folder with a trailing slash", "file:///a/folder/", []string{"/a/folder"}},
		{"comments and blank lines", "# copied\r\n\r\nfile:///a/one.txt\r\n", []string{"/a/one.txt"}},
		{"localhost names this machine", "file://localhost/a/one.txt", []string{"/a/one.txt"}},
		{"another machine", "file://nas/share/one.txt", nil},
		{"web links", "https://example.com/a\r\nhttps://example.com/b\r\n", nil},
		{"a file among links", "file:///a/one.txt\r\nhttps://example.com/\r\n", nil},
		{"a path that is not a URI", "/a/one.txt", nil},
		{"a relative file URI", "file:one.txt", nil},
		{"nothing", "", nil},
		{"only a comment", "# nothing here\r\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseURIList(tt.list); !slices.Equal(got, tt.want) {
				t.Errorf("parseURIList(%q) = %q, want %q", tt.list, got, tt.want)
			}
		})
	}
}

func TestURIListSurvivesARoundTrip(t *testing.T) {
	paths := []string{"/home/me/plain.txt", "/home/me/with space & ünïcödé ✓.txt", "/home/me/100% #1?.txt", "/home/me/folder"}
	if got := parseURIList(uriList(paths)); !slices.Equal(got, paths) {
		t.Errorf("round trip = %q, want %q", got, paths)
	}
}

func TestValidImage(t *testing.T) {
	png := "\x89PNG\r\n\x1a\n"
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"a PNG", png + "the rest of the picture", true},
		{"the largest allowed", png + strings.Repeat("x", MaxImageBytes-len(png)), true},
		{"one byte too large", png + strings.Repeat("x", MaxImageBytes-len(png)+1), false},
		{"a JPEG", "\xff\xd8\xff\xe0 JFIF", false},
		{"text", "not a picture", false},
		{"half a signature", "\x89PNG", false},
		{"nothing", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidImage([]byte(tt.data)); got != tt.want {
				t.Errorf("ValidImage(%d bytes) = %v, want %v", len(tt.data), got, tt.want)
			}
		})
	}
}
