package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/texttheater/golang-levenshtein/levenshtein"
	"golang.org/x/text/unicode/norm"
)

type Album struct {
	RYMAlbumID      string `json:"rym_album_id"`
	ID              string `json:"Id"` // keep if you also use Jellyfin items
	Name            string `json:"Name"`
	AlbumArtist     string `json:"AlbumArtist"`
	ProductionYear  int    `json:"ProductionYear"`
	Overview        string `json:"Overview"`
	PrimaryImageTag string `json:"PrimaryImageTag"`
}

type NameID struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

// itemsResponse matches Jellyfin's ItemQueryResult for /Users/{userId}/Items
type itemsResponse struct {
	Items            []Album `json:"Items"`
	TotalRecordCount int     `json:"TotalRecordCount"`
}

// Client wraps HTTP behavior and base params.
type Client struct {
	BaseURL   string       // e.g. http://localhost:8096
	Token     string       // Jellyfin API token (user session token or API key)
	HTTP      *http.Client // optional; if nil a sane default is used
	UserAgent string       // optional; a sensible default is used if empty
}

var (
	albumList []Album
)

var pageTpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"add": func(a, b int) int { return a + b },
}).ParseFiles("index.html"))

func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		HTTP: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
		UserAgent: "Jellyfin-Go/1.0 (+https://example.com)",
	}
}

func (c *Client) GetAllAlbums(ctx context.Context) ([]Album, error) {
	const pageSize = 200
	startIndex := 0
	var all []Album

	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}

	for {
		u := base.ResolveReference(&url.URL{Path: "/Items"})
		q := u.Query()
		q.Set("IncludeItemTypes", "MusicAlbum")
		q.Set("Recursive", "true")
		q.Set("SortBy", "SortName")
		q.Set("SortOrder", "Ascending")
		q.Set("StartIndex", fmt.Sprintf("%d", startIndex))
		q.Set("Limit", fmt.Sprintf("%d", pageSize))
		q.Set("Fields", "PrimaryImageTag,AlbumArtist,AlbumArtists,ProductionYear,Overview")
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-MediaBrowser-Token", c.Token)

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("bad status %d", resp.StatusCode)
		}

		var ir itemsResponse
		if err := json.NewDecoder(resp.Body).Decode(&ir); err != nil {
			return nil, err
		}

		all = append(all, ir.Items...)
		startIndex += len(ir.Items)
		if startIndex >= ir.TotalRecordCount || len(ir.Items) == 0 {
			break
		}
	}
	return all, nil
}

func normalize(s string) string {
	// decompose accents, then strip them
	t := norm.NFD.String(strings.ToLower(s))
	var b strings.Builder
	for _, r := range t {
		if unicode.Is(unicode.Mn, r) {
			continue // skip diacritic
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ") // collapse spaces
}

// similarity returns [0..1] based on Levenshtein distance
func similarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	d := levenshtein.DistanceForStrings([]rune(a), []rune(b), levenshtein.DefaultOptions)
	maxLen := len([]rune(a))
	if len([]rune(b)) > maxLen {
		maxLen = len([]rune(b))
	}
	if maxLen == 0 {
		return 1
	}
	return 1 - float64(d)/float64(maxLen)
}

func renderForm(w http.ResponseWriter, albums []Album, errMsg string) {
	var jsonOut string
	if len(albums) > 0 {
		buf, _ := json.MarshalIndent(albums, "", "  ")
		jsonOut = string(buf)
	}

	// Deduplicate albumList against RYM albums
	var filtered []Album
	for _, jfAlbum := range albumList {
		jfTitle := normalize(strings.ToLower(jfAlbum.Name))
		jfArtist := normalize(strings.ToLower(jfAlbum.AlbumArtist))

		duplicate := false
		for _, rymAlbum := range albums {
			rymTitle := normalize(strings.ToLower(rymAlbum.Name))
			rymArtist := normalize(strings.ToLower(rymAlbum.AlbumArtist))

			titleSim := similarity(jfTitle, rymTitle)
			artistSim := similarity(jfArtist, rymArtist)

			if titleSim > 0.75 && artistSim > 0.75 {
				duplicate = true
				break
			}
		}
		if !duplicate {
			filtered = append(filtered, jfAlbum)
		}
	}
	albumList = filtered

	err := pageTpl.ExecuteTemplate(w, "page", map[string]any{
		"Albums": albumList,
		"JSON":   jsonOut,
		"Err":    errMsg,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func ServeRymCSVForm(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			renderForm(w, nil, "")
			return
		case http.MethodPost:
			// Accept either file upload or textarea
			var src io.Reader

			_ = r.ParseMultipartForm(16 << 20) // 16 MB
			if f, hdr, err := r.FormFile("csvfile"); err == nil && hdr != nil {
				defer f.Close()
				var buf bytes.Buffer
				if _, err := io.Copy(&buf, f); err != nil {
					http.Error(w, "failed to read uploaded file: "+err.Error(), http.StatusBadRequest)
					return
				}
				src = &buf
			} else {
				text := r.FormValue("csvtext")
				src = strings.NewReader(text)
			}

			albums, err := parseRymCSV(src)
			if err != nil {
				renderForm(w, nil, "Parse error: "+err.Error())
				return
			}
			renderForm(w, albums, "")
			return
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	})
}

func parseRymCSV(r io.Reader) ([]Album, error) {
	// Ensure UTF-8, strip BOM if present
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	data = stripBOM(data)

	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = -1 // allow variable fields per row
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("empty CSV")
	}

	// Validate header (allow minor whitespace differences)
	hdr := trimAll(rows[0])

	if len(hdr) < 12 {
		return nil, fmt.Errorf("header has %d columns, expected at least %d", len(hdr), 12)
	}

	var out []Album
	for i := 1; i < len(rows); i++ {
		cols := rows[i]
		cols = trimAll(cols)
		i, _ := strconv.Atoi(cols[6])
		alb := Album{
			RYMAlbumID:     cols[0], // from the CSV
			Name:           cols[5],
			ProductionYear: i,
			AlbumArtist:    strings.TrimSpace(cols[1] + " " + cols[2]),
		}

		// Parse release date (YYYY or YYYY-MM-DD)
		/*if t, ok := parseYearOrDate(cols[6]); ok {
			alb.ReleaseDate = t
		}
		*/
		// Build a display name: prefer localized if present
		first := cols[1]
		last := cols[2]
		alb.AlbumArtist = strings.TrimSpace(strings.Join([]string{first, last}, " "))

		out = append(out, alb)
	}

	return out, nil
}

func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

func trimAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, s := range xs {
		out[i] = strings.TrimSpace(s)
	}
	return out
}

func main() {
	ctx := context.Background()
	jf := NewClient("https://jf.skaremyr.se", "96f1167856d947d0822307b911e4ce9b")

	// If you have a user *session* token, you can fetch your userId from /Users/Me.
	// If you're using an API key, supply a specific user's ID instead.
	albums, err := jf.GetAllAlbums(ctx)
	if err != nil {
		panic(err)
	}
	for _, a := range albums {
		albumList = append(albumList, a)
		//fmt.Printf("%s (%d) — %s\n", a.Name, a.ProductionYear, a.AlbumArtist)
	}
	sort.Slice(albumList, func(i, j int) bool {
		ai := strings.ToLower(albumList[i].AlbumArtist)
		aj := strings.ToLower(albumList[j].AlbumArtist)
		if ai == aj {
			return strings.ToLower(albumList[i].Name) < strings.ToLower(albumList[j].Name)
		}
		return ai < aj
	})
	mux := http.NewServeMux()
	ServeRymCSVForm(mux)

	log.Println("listening on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
