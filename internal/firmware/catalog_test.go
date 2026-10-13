package firmware

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cdn serves a manifest behind a redirect (as aka.ms does) and image files.
func cdn(t *testing.T, manifest string, files map[string][]byte) *httptest.Server {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/aka", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/M/PC_GA_x.json", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/M/PC_GA_x.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.ReplaceAll(manifest, "BASE", srv.URL+"/FW/"))
	})
	mux.HandleFunc("/FW/", func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/FW/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testCatalog(t *testing.T, srv *httptest.Server) *Catalog {
	c := NewCatalog(t.TempDir())
	c.ManifestURL = srv.URL + "/aka"
	c.Client = srv.Client()
	c.BuiltIn = []string{Filename(Version{5, 21, 3242, 0}, sha256.Sum256(DemoImage(Version{5, 21, 3242, 0})))}
	return c
}

func manifestFor(name string) string {
	return `{"g":"pc_ga","v":"1.0.0.23","p":"BASE","d":[
	  {"__n":"merl","id":"045E_0B12","f":[{"__t":"beta","p":"MERL_045E_0B12_v_5_23_6_0_h_jC-y7nRWRtQCHQCyi2rv6xtQpgikM6JG0r6lYE3A5pM=.bin"}]},
	  {"__n":"delp","id":"045E_0B00","f":[{"__t":"beta","p":"` + name + `"}],"req":[{"v":"5.21.3242.0"}]}]}`
}

func TestCatalogListMergesManifestAndBuiltIn(t *testing.T) {
	img := DemoImage(v5236)
	name := Filename(v5236, sha256.Sum256(img))
	c := testCatalog(t, cdn(t, manifestFor(name), map[string][]byte{name: img}))
	entries, note := c.List(context.Background())
	if note != "" || len(entries) != 2 {
		t.Fatalf("entries %+v note %q", entries, note)
	}
	if entries[0].Version != v5236 || entries[0].Source != "Microsoft manifest" ||
		entries[1].Version != (Version{5, 21, 3242, 0}) || entries[1].Source != "built-in list" {
		t.Errorf("entries %+v", entries)
	}
}

func TestCatalogManifestUnreachable(t *testing.T) {
	srv := cdn(t, "", nil)
	c := testCatalog(t, srv)
	srv.Close()
	entries, note := c.List(context.Background())
	if len(entries) != 1 || entries[0].Source != "built-in list" || !strings.Contains(note, "manifest") {
		t.Fatalf("entries %+v note %q", entries, note)
	}
}

func TestCatalogManifestGarbage(t *testing.T) {
	c := testCatalog(t, cdn(t, "<html>not json</html>", nil))
	entries, note := c.List(context.Background())
	if len(entries) != 1 || note == "" {
		t.Fatalf("entries %+v note %q", entries, note)
	}
}

func TestCatalogFetchVerifiesAndCaches(t *testing.T) {
	img := DemoImage(v5236)
	name := Filename(v5236, sha256.Sum256(img))
	srv := cdn(t, manifestFor(name), map[string][]byte{name: img})
	c := testCatalog(t, srv)
	entries, _ := c.List(context.Background())
	var last int64
	b, err := c.Fetch(context.Background(), entries[0], func(done, total int64) { last = done })
	if err != nil || len(b) != len(img) || last != int64(len(img)) {
		t.Fatalf("fetch: %v, %d bytes, progress %d", err, len(b), last)
	}
	srv.Close()
	if _, err := c.Fetch(context.Background(), entries[0], nil); err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
}

func TestCatalogFetchRejectsCorruptDownload(t *testing.T) {
	img := DemoImage(v5236)
	name := Filename(v5236, sha256.Sum256(img))
	bad := append([]byte(nil), img...)
	bad[1000] ^= 1
	c := testCatalog(t, cdn(t, manifestFor(name), map[string][]byte{name: bad}))
	entries, _ := c.List(context.Background())
	if _, err := c.Fetch(context.Background(), entries[0], nil); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.CacheDir, name)); !os.IsNotExist(err) {
		t.Error("corrupt download was cached")
	}
}

func TestCatalogFetchIgnoresCorruptCache(t *testing.T) {
	img := DemoImage(v5236)
	name := Filename(v5236, sha256.Sum256(img))
	c := testCatalog(t, cdn(t, manifestFor(name), map[string][]byte{name: img}))
	if err := os.WriteFile(filepath.Join(c.CacheDir, name), img[:5000], 0o644); err != nil {
		t.Fatal(err)
	}
	entries, _ := c.List(context.Background())
	b, err := c.Fetch(context.Background(), entries[0], nil)
	if err != nil || len(b) != len(img) {
		t.Fatalf("got %d bytes, %v", len(b), err)
	}
}

func TestLoadLocal(t *testing.T) {
	dir := t.TempDir()
	img := DemoImage(v5236)
	official := filepath.Join(dir, Filename(v5236, sha256.Sum256(img)))
	os.WriteFile(official, img, 0o644)
	if _, verified, err := LoadLocal(official); err != nil || !verified {
		t.Errorf("official: %v %v", verified, err)
	}
	plain := filepath.Join(dir, "firmware.bin")
	os.WriteFile(plain, img, 0o644)
	if _, verified, err := LoadLocal(plain); err != nil || verified {
		t.Errorf("plain: %v %v", verified, err)
	}
	wrong := filepath.Join(dir, Filename(v5236, [32]byte{1}))
	os.WriteFile(wrong, img, 0o644)
	if _, _, err := LoadLocal(wrong); err == nil {
		t.Error("hash mismatch accepted")
	}
}
