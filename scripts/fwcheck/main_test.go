package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xbelite2-configurator/internal/firmware"
)

var (
	v5240 = firmware.Version{Major: 5, Minor: 24, Build: 0}
	v5236 = firmware.Version{Major: 5, Minor: 23, Build: 6}
	v5213 = firmware.Version{Major: 5, Minor: 21, Build: 3242}
)

func official(v firmware.Version) (string, []byte) {
	b := firmware.DemoImage(v)
	return firmware.Filename(v, sha256.Sum256(b)), b
}

// cdn serves a PC manifest listing names, a console manifest that always
// fails, and the given files.
func cdn(t *testing.T, names []string, files map[string][]byte) *httptest.Server {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/M/pc.json", func(w http.ResponseWriter, r *http.Request) {
		var f []string
		for _, n := range names {
			f = append(f, fmt.Sprintf(`{"p":%q}`, n))
		}
		fmt.Fprintf(w, `{"p":%q,"d":[{"id":"045E_0B00","f":[%s]}]}`, srv.URL+"/FW/", strings.Join(f, ","))
	})
	mux.HandleFunc("/M/console.json", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusInternalServerError)
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

func sources(srv *httptest.Server) []manifestSource {
	return []manifestSource{{srv.URL + "/M/pc.json", true}, {srv.URL + "/M/console.json", false}}
}

const header = "# header line\n# second\n"

func listFile(t *testing.T, names ...string) string {
	path := filepath.Join(t.TempDir(), "builtin.txt")
	if err := os.WriteFile(path, []byte(header+strings.Join(names, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunAddsNewVersionNewestFirst(t *testing.T) {
	newName, newImg := official(v5240)
	known236, _ := official(v5236)
	known213, _ := official(v5213)
	srv := cdn(t, []string{newName, known236}, map[string][]byte{newName: newImg})
	list := listFile(t, known236, known213)
	var log strings.Builder

	added, err := run(context.Background(), list, sources(srv), srv.Client(), &log)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != v5240 {
		t.Fatalf("added %v", added)
	}
	got, _ := os.ReadFile(list)
	want := header + strings.Join([]string{newName, known236, known213}, "\n") + "\n"
	if string(got) != want {
		t.Errorf("list:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(log.String(), "::warning::") {
		t.Errorf("console manifest failure wasn't a warning: %q", log.String())
	}

	again, err := run(context.Background(), list, sources(srv), srv.Client(), io.Discard)
	if err != nil || len(again) != 0 {
		t.Fatalf("second run added %v (%v)", again, err)
	}
	if after, _ := os.ReadFile(list); string(after) != want {
		t.Error("second run changed the list")
	}
}

func TestRunRejectsCorruptImage(t *testing.T) {
	newName, newImg := official(v5240)
	bad := append([]byte(nil), newImg...)
	bad[5000] ^= 1
	known, _ := official(v5236)
	srv := cdn(t, []string{newName}, map[string][]byte{newName: bad})
	list := listFile(t, known)

	if _, err := run(context.Background(), list, sources(srv), srv.Client(), io.Discard); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("got %v", err)
	}
	if got, _ := os.ReadFile(list); string(got) != header+known+"\n" {
		t.Error("list changed after a failed check")
	}
}

func TestRunFailsWhenPCManifestIsUnreachable(t *testing.T) {
	srv := cdn(t, nil, nil)
	src := []manifestSource{{srv.URL + "/M/console.json", true}}
	known, _ := official(v5236)
	if _, err := run(context.Background(), listFile(t, known), src, srv.Client(), io.Discard); err == nil {
		t.Fatal("an unreadable required manifest didn't fail the run")
	}
}
