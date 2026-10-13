// Command fwcheck adds official Elite Series 2 firmware that Microsoft's
// manifests list but internal/firmware/builtin.txt doesn't. Each new image is
// downloaded, checked against the SHA-256 in its name, and parsed before it is
// added. It prints the versions it added, one per line.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"xbelite2-configurator/internal/firmware"
)

// consoleManifest is the Xbox console channel's manifest. It has no aka.ms
// link, so this URL may go stale; reading it is best-effort.
const consoleManifest = "https://xbaccessories-bhf7bsbfgxdpbrg4.b02.azurefd.net/accessories/M/XB_GA_ew92RW1KHEGfgr6ZoY3DyQ.json"

type manifestSource struct {
	url      string
	required bool // failing to read it fails the run; otherwise it's a warning
}

func main() {
	list := flag.String("list", "internal/firmware/builtin.txt", "built-in firmware `file` to update")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	srcs := []manifestSource{{firmware.ManifestURL, true}, {consoleManifest, false}}
	added, err := run(ctx, *list, srcs, &http.Client{Timeout: 2 * time.Minute}, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fwcheck:", err)
		os.Exit(1)
	}
	for _, v := range added {
		fmt.Println(v)
	}
}

func run(ctx context.Context, listPath string, srcs []manifestSource, client *http.Client, logw io.Writer) ([]firmware.Version, error) {
	raw, err := os.ReadFile(listPath)
	if err != nil {
		return nil, err
	}
	names := firmware.ParseBuiltIn(string(raw))
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	cache, err := os.MkdirTemp("", "fwcheck")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(cache)

	var added []firmware.Version
	for _, src := range srcs {
		c := firmware.NewCatalog(cache)
		c.ManifestURL, c.BuiltIn, c.Client = src.url, nil, client
		entries, note := c.List(ctx)
		if note != "" {
			if src.required {
				return nil, fmt.Errorf("%s: %s", src.url, note)
			}
			fmt.Fprintf(logw, "::warning::%s: %s\n", src.url, note)
			continue
		}
		for _, e := range entries {
			if known[e.File] {
				continue
			}
			b, err := c.Fetch(ctx, e, nil) // checks the SHA-256 in the name
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.File, err)
			}
			if _, err := firmware.Parse(b); err != nil {
				return nil, fmt.Errorf("%s: %w", e.File, err)
			}
			fmt.Fprintf(logw, "new firmware %s from %s\n", e.Version, src.url)
			known[e.File] = true
			names = append(names, e.File)
			added = append(added, e.Version)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	return added, writeList(listPath, string(raw), names)
}

// writeList keeps the file's leading comment block and writes names newest first.
func writeList(path, old string, names []string) error {
	var head strings.Builder
	for _, line := range strings.SplitAfter(old, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			break
		}
		head.WriteString(line)
	}
	version := func(n string) firmware.Version { v, _, _ := firmware.ParseFilename(n); return v }
	sort.SliceStable(names, func(i, j int) bool { return version(names[i]).Compare(version(names[j])) > 0 })
	return os.WriteFile(path, []byte(head.String()+strings.Join(names, "\n")+"\n"), 0o644)
}
