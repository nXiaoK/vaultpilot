package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func previewFixture(t *testing.T) (*testHarness, Source) {
	t.Helper()
	h := newHarness(t, false)
	res := h.call("POST", "/api/sources", map[string]any{"name": "测试目录", "path": h.r.Config.SourceRoots[0], "include_hidden": false, "one_file_system": true}, true)
	var source Source
	if res.Code != 201 || json.Unmarshal(res.Body.Bytes(), &source) != nil {
		t.Fatalf("source setup: %s", res.Body.String())
	}
	return h, source
}
func TestSourcePreviewCursorSearchAndBoundaries(t *testing.T) {
	h, source := previewFixture(t)
	for i := 0; i < 205; i++ {
		if e := os.WriteFile(filepath.Join(source.Path, fmt.Sprintf("file-%03d.txt", i)), []byte("data"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	for _, name := range []string{"中文 文件夹", "空目录"} {
		if e := os.Mkdir(filepath.Join(source.Path, name), 0700); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.WriteFile(filepath.Join(source.Path, ".hidden"), []byte("hidden"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(h.r.Config.StateDir, filepath.Join(source.Path, "outside-link")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("中文 文件夹", filepath.Join(source.Path, "inside-link")); e != nil {
		t.Fatal(e)
	}
	cursor, previous := "", ""
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		res := h.call("GET", "/api/sources/"+source.ID+"/files?limit=100&cursor="+url.QueryEscape(cursor), nil, false)
		var listing struct {
			Items []sourceEntry `json:"items"`
			Total int           `json:"total"`
			Next  string        `json:"next_cursor"`
		}
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &listing) != nil {
			t.Fatalf("list: %s", res.Body.String())
		}
		if listing.Total != 210 || len(listing.Items) > 100 {
			t.Fatalf("incorrect listing: %+v", listing)
		}
		for _, item := range listing.Items {
			if seen[item.Name] || entryKey(item) <= previous {
				t.Fatalf("duplicate or unsorted item: %s", item.Name)
			}
			seen[item.Name] = true
			previous = entryKey(item)
			if item.Kind == "symlink" && item.CanOpen {
				t.Fatal("symlink is browseable")
			}
		}
		cursor = listing.Next
	}
	if len(seen) != 210 || cursor != "" || !seen[".hidden"] {
		t.Fatal("pagination lost entries or hid existing files")
	}
	res := h.call("GET", "/api/sources/"+source.ID+"/files?search=file-020", nil, false)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"total":1`) {
		t.Fatalf("search: %s", res.Body.String())
	}
	for _, path := range []string{"../state", "/etc", "a/../../state", "outside-link", "inside-link"} {
		for _, endpoint := range []string{"files", "content"} {
			res := h.call("GET", "/api/sources/"+source.ID+"/"+endpoint+"?path="+url.QueryEscape(path), nil, false)
			if res.Code == 200 {
				t.Fatalf("unsafe path accepted: %s %s", endpoint, path)
			}
		}
	}
	res = h.call("GET", "/api/sources/"+source.ID+"/files?path="+url.QueryEscape("中文 文件夹"), nil, false)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"items":[]`) {
		t.Fatalf("unicode empty directory: %s", res.Body.String())
	}
	h.cookie = nil
	if res := h.call("GET", "/api/sources/"+source.ID+"/files", nil, false); res.Code != 401 {
		t.Fatal("preview exposed without login")
	}
}
func TestSourceContentPreviewLimitsAndTypes(t *testing.T) {
	h, source := previewFixture(t)
	var imageData bytes.Buffer
	if e := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 2, 2))); e != nil {
		t.Fatal(e)
	}
	text := "中文内容 <script>alert('must render as text')</script>"
	for name, content := range map[string][]byte{"text.txt": []byte(text), "image.png": imageData.Bytes(), "binary.bin": {0, 255, 0}, "large.txt": []byte(strings.Repeat("中", 100000))} {
		if e := os.WriteFile(filepath.Join(source.Path, name), content, 0600); e != nil {
			t.Fatal(e)
		}
	}
	file, e := os.Create(filepath.Join(source.Path, "huge.bin"))
	if e != nil {
		t.Fatal(e)
	}
	if e := file.Truncate(6 << 20); e != nil {
		t.Fatal(e)
	}
	file.Close()
	if e := syscall.Mkfifo(filepath.Join(source.Path, "pipe"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, row := range []struct {
		Name   string
		Status int
		Kind   string
	}{{"text.txt", 200, "text"}, {"image.png", 200, "image"}, {"large.txt", 200, "text"}, {"binary.bin", 415, ""}, {"huge.bin", 413, ""}, {"pipe", 400, ""}} {
		res := h.call("GET", "/api/sources/"+source.ID+"/content?path="+row.Name, nil, false)
		if res.Code != row.Status {
			t.Fatalf("%s: %d %s", row.Name, res.Code, res.Body.String())
		}
		if row.Kind == "" {
			continue
		}
		var result struct {
			Kind      string `json:"kind"`
			Text      string `json:"text"`
			Truncated bool   `json:"truncated"`
			MIME      string `json:"mime"`
		}
		if e := json.Unmarshal(res.Body.Bytes(), &result); e != nil || result.Kind != row.Kind {
			t.Fatalf("%s result: %v", row.Name, e)
		}
		if row.Name == "text.txt" && result.Text != text {
			t.Fatal("text preview changed content")
		}
		if row.Name == "large.txt" && (!result.Truncated || len(result.Text) > 256<<10) {
			t.Fatal("text preview was not bounded")
		}
		if row.Name == "image.png" && result.MIME != "image/png" {
			t.Fatal("image MIME not verified")
		}
	}
}
func TestSourcePreviewRejectsChangedMount(t *testing.T) {
	h, source := previewFixture(t)
	if e := os.Rename(source.Path, source.Path+"-old"); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(source.Path, 0700); e != nil {
		t.Fatal(e)
	}
	res := h.call("GET", "/api/sources/"+source.ID+"/files", nil, false)
	if res.Code != 409 || !strings.Contains(res.Body.String(), "source_mount_changed") {
		t.Fatalf("changed mount exposed: %s", res.Body.String())
	}
}
