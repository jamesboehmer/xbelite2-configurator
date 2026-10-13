package firmware

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// ManifestURL is what Xbox Accessories resolves; it redirects to a JSON
	// manifest listing the current image per device. No account needed.
	ManifestURL = "https://aka.ms/xboxaccessoriesfwcdnpcga"
	// DefaultBase is the manifest's firmware base URL, for when it's unreachable.
	DefaultBase = "https://xbaccessories-bhf7bsbfgxdpbrg4.b02.azurefd.net/accessories/FW/"

	deviceID     = "045E_0B00"
	maxImageSize = 4 << 20
)

//go:embed builtin.txt
var builtinTxt string

// BuiltIn lists official images the manifest no longer offers, from
// builtin.txt. The manifest only names the latest; these stay downloadable by
// exact name.
var BuiltIn = ParseBuiltIn(builtinTxt)

// ParseBuiltIn reads the builtin.txt format: one filename per line, with
// blank lines and #-comments ignored.
func ParseBuiltIn(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// Catalog finds official images and downloads them into CacheDir.
type Catalog struct {
	ManifestURL string
	BuiltIn     []string
	CacheDir    string
	Client      *http.Client
	base        string
}

func NewCatalog(cacheDir string) *Catalog {
	return &Catalog{ManifestURL: ManifestURL, BuiltIn: BuiltIn, CacheDir: cacheDir,
		Client: &http.Client{Timeout: 2 * time.Minute}}
}

type manifest struct {
	P string `json:"p"`
	D []struct {
		ID string `json:"id"`
		F  []struct {
			P string `json:"p"`
		} `json:"f"`
	} `json:"d"`
}

// List returns images newest first. note explains a manifest failure; the
// built-in list is still returned then.
func (c *Catalog) List(ctx context.Context) ([]Entry, string) {
	byVersion := map[Version]Entry{}
	add := func(name, source string) {
		// Reject entries with path traversal characters.
		if name != filepath.Base(name) || strings.ContainsAny(name, "/\\") {
			return
		}
		if v, _, ok := ParseFilename(name); ok {
			if _, seen := byVersion[v]; !seen {
				byVersion[v] = Entry{Version: v, File: name, Source: source}
			}
		}
	}
	var note string
	if m, err := c.manifest(ctx); err != nil {
		note = "Couldn't read Microsoft's firmware manifest (" + err.Error() + "); showing the built-in list."
	} else {
		c.base = m.P
		for _, d := range m.D {
			if strings.EqualFold(d.ID, deviceID) {
				for _, f := range d.F {
					add(f.P, "Microsoft manifest")
				}
			}
		}
	}
	for _, name := range c.BuiltIn {
		add(name, "built-in list")
	}
	out := make([]Entry, 0, len(byVersion))
	for _, e := range byVersion {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version.Compare(out[j].Version) > 0 })
	return out, note
}

func (c *Catalog) manifest(ctx context.Context) (*manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ManifestURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var m manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("bad manifest: %w", err)
	}
	if !strings.HasPrefix(m.P, "https://") && !strings.HasPrefix(m.P, "http://") {
		return nil, fmt.Errorf("manifest has no firmware base URL")
	}
	return &m, nil
}

// Fetch returns the image for e, from the cache if a cached copy still
// matches the SHA-256 in its name, otherwise downloaded and cached.
func (c *Catalog) Fetch(ctx context.Context, e Entry, progress func(done, total int64)) ([]byte, error) {
	// Reject entries with path traversal characters (defense in depth).
	if e.File != filepath.Base(e.File) || strings.ContainsAny(e.File, "/\\") {
		return nil, fmt.Errorf("%s is not an official firmware name", e.File)
	}
	_, want, ok := ParseFilename(e.File)
	if !ok {
		return nil, fmt.Errorf("%s is not an official firmware name", e.File)
	}
	path := filepath.Join(c.CacheDir, e.File)
	if b, err := os.ReadFile(path); err == nil && sha256.Sum256(b) == want {
		if progress != nil {
			progress(int64(len(b)), int64(len(b)))
		}
		return b, nil
	}
	base := c.base
	if base == "" {
		base = DefaultBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+e.File, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: HTTP %d", e.Version, resp.StatusCode)
	}
	b, err := io.ReadAll(&progressReader{r: io.LimitReader(resp.Body, maxImageSize), total: resp.ContentLength, fn: progress})
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(b) != want {
		return nil, fmt.Errorf("download of %s is corrupt (SHA-256 mismatch)", e.Version)
	}
	if err := os.MkdirAll(c.CacheDir, 0o755); err == nil {
		tmp := path + ".part"
		if os.WriteFile(tmp, b, 0o644) == nil {
			os.Rename(tmp, path)
		}
	}
	return b, nil
}

type progressReader struct {
	r     io.Reader
	done  int64
	total int64
	fn    func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.fn != nil && n > 0 {
		p.fn(p.done, p.total)
	}
	return n, err
}

// LoadLocal reads a .bin from disk. verified is true when its name carries a
// SHA-256 and the file matches it; a name with a hash that doesn't match is
// an error.
func LoadLocal(path string) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxImageSize+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > maxImageSize {
		return nil, false, fmt.Errorf("%s is too large to be a firmware image", filepath.Base(path))
	}
	if _, want, ok := ParseFilename(path); ok {
		if sha256.Sum256(b) != want {
			return nil, false, fmt.Errorf("%s doesn't match the SHA-256 in its name", filepath.Base(path))
		}
		return b, true, nil
	}
	return b, false, nil
}
