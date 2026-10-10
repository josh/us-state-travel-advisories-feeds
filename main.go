package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const version = "0.3.0"

const (
	feedURLTemplate = "https://josh.github.io/us-state-travel-advisories-feeds/%s.json"
	feedHomePageURL = "https://travel.state.gov/content/travel/en/traveladvisories/traveladvisories.html/"
	feedIconURL     = "https://travel.state.gov/content/dam/tsg-global/tsg_link_img_display.jpg"
)

var sourceURL = "https://travel.state.gov/_res/rss/TAsTWs.xml"

var countryNames = map[string]string{
	"israel-west-bank-and-gaza": "Israel, the West Bank, and Gaza",
}

type feed struct {
	Version     string `json:"version"`
	Title       string `json:"title"`
	HomePageURL string `json:"home_page_url"`
	FeedURL     string `json:"feed_url"`
	Icon        string `json:"icon"`
	Items       []item `json:"items"`
}

type item struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	ContentHTML   string `json:"content_html"`
	DatePublished string `json:"date_published"`
}

type rssItem struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	Description *string `xml:"description"`
	PubDate     string  `xml:"pubDate"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) > 0 && args[0] == "version" {
		fmt.Println(version)
		return 0
	}

	fs := flag.NewFlagSet("us-state-travel-advisories-feeds", flag.ContinueOnError)
	outputDir := fs.String("output-dir", os.Getenv("OUTPUT_DIR"), "directory to write feeds to (env OUTPUT_DIR)")
	combineCountries := fs.String("combine-countries", os.Getenv("COMBINE_COUNTRIES"), "comma-separated slugs for combined.json (env COMBINE_COUNTRIES)")
	var verbose bool
	fs.BoolVar(&verbose, "v", false, "enable debug logging")
	fs.BoolVar(&verbose, "verbose", false, "enable debug logging")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if *outputDir == "" {
		slog.Error("Missing required -output-dir")
		return 2
	}

	if err := generate(ctx, *outputDir, *combineCountries); err != nil {
		slog.Error("exit", "err", err)
		return 1
	}
	return 0
}

func generate(ctx context.Context, outputDir, combineCountries string) error {
	slog.Info("Fetch", "url", sourceURL)
	entries, err := fetch(ctx, sourceURL)
	if err != nil {
		return err
	}

	slugs, items := parseItems(entries)
	if len(items) == 0 {
		return errors.New("no travel advisories found")
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}
	stale, err := filepath.Glob(filepath.Join(outputDir, "*.json"))
	if err != nil {
		return err
	}
	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			return err
		}
	}

	for _, slug := range slugs {
		it := items[slug]
		country, _, _ := strings.Cut(it.Title, " - ")
		country = strings.TrimSpace(strings.TrimSuffix(country, " Travel Advisory"))
		f := newFeed(country, slug, it.URL, []item{it})
		if err := writeFeed(filepath.Join(outputDir, slug+".json"), f); err != nil {
			return err
		}
	}

	if combineCountries != "" {
		combined := []item{}
		seen := map[string]bool{}
		for slug := range strings.SplitSeq(combineCountries, ",") {
			if seen[slug] {
				continue
			}
			seen[slug] = true
			it, ok := items[slug]
			if !ok {
				slog.Warn("Country not found", "slug", slug)
				continue
			}
			combined = append(combined, it)
		}
		slices.SortStableFunc(combined, func(a, b item) int {
			return strings.Compare(b.DatePublished, a.DatePublished)
		})
		f := newFeed("Combined", "combined", feedHomePageURL, combined)
		if err := writeFeed(filepath.Join(outputDir, "combined.json"), f); err != nil {
			return err
		}
	}

	return nil
}

func fetch(ctx context.Context, src string) ([]rssItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return parseRSS(resp.Body)
}

func parseRSS(r io.Reader) ([]rssItem, error) {
	var doc struct {
		Items []rssItem `xml:"channel>item"`
	}
	dec := xml.NewDecoder(r)
	dec.Entity = xml.HTMLEntity
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("malformed feed: %w", err)
	}
	return doc.Items, nil
}

// parseItems returns one item per slug, plus the slugs in first-seen order.
func parseItems(entries []rssItem) ([]string, map[string]item) {
	legacySlugs := map[string]string{}
	for _, e := range entries {
		if e.Title == "" {
			continue
		}
		if slug, ok := legacySlug(strings.TrimSpace(e.Link)); ok {
			legacySlugs[countryName(e.Title)] = slug
		}
	}

	var slugs []string
	items := map[string]item{}
	for _, e := range entries {
		title := strings.TrimSpace(e.Title)
		link := strings.TrimSpace(e.Link)
		if e.Description == nil || title == "" || link == "" {
			continue
		}
		published, ok := parseDate(e.PubDate)
		if !ok {
			continue
		}

		slug, fromLink := legacySlug(link)
		if !fromLink {
			name := countryName(title)
			if s, ok := legacySlugs[name]; ok {
				slug = s
			} else {
				slug = slugify(name)
			}
		}
		if strings.Contains(title, "See Individual Summaries") {
			title = repairTitle(title, slug)
		}

		_, exists := items[slug]
		if !fromLink && exists {
			continue
		}
		if !exists {
			slugs = append(slugs, slug)
		}
		items[slug] = item{
			ID:            fmt.Sprintf("%s-%d", slug, published.Unix()),
			URL:           link,
			Title:         title,
			ContentHTML:   resolveURLs(strings.TrimSpace(*e.Description), sourceURL),
			DatePublished: published.Format("2006-01-02T15:04:05-07:00"),
		}
	}
	return slugs, items
}

var urlAttr = regexp.MustCompile(`(\s(?:href|src)=)(?:"([^"]*)"|'([^']*)')`)

// resolveURLs makes link targets absolute, since upstream uses site-relative
// hrefs that would break once the feed is served from another host.
func resolveURLs(html, base string) string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return html
	}
	return urlAttr.ReplaceAllStringFunc(html, func(m string) string {
		parts := urlAttr.FindStringSubmatch(m)
		quote, target := `"`, parts[2]
		if strings.HasPrefix(m[len(parts[1]):], "'") {
			quote, target = "'", parts[3]
		}
		ref, err := url.Parse(strings.TrimSpace(target))
		if err != nil {
			return m
		}
		return parts[1] + quote + baseURL.ResolveReference(ref).String() + quote
	})
}

var dateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"Mon, 2 Jan 2006",
	"2 Jan 2006",
}

func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

var legacySlugPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^([a-z-]+)-travel-advisory\.html$`),
	regexp.MustCompile(`^([a-z-]+)-advisory\.html$`),
	regexp.MustCompile(`^([a-z-]+)\.html$`),
}

func legacySlug(link string) (string, bool) {
	last := strings.ToLower(link[strings.LastIndex(link, "/")+1:])
	if last == "worldwide-caution.html" {
		return "worldwide", true
	}
	for _, re := range legacySlugPatterns {
		if m := re.FindStringSubmatch(last); m != nil {
			return m[1], true
		}
	}
	return "", false
}

var levelSeparator = regexp.MustCompile(`(?: Travel Advisory)?\s+-\s+Level `)

func countryName(title string) string {
	title = strings.ReplaceAll(title, " ", " ")
	return strings.TrimSpace(levelSeparator.Split(title, 2)[0])
}

// Approximates NFKD followed by dropping non-ASCII, which the stdlib can't do.
var asciiFold = strings.NewReplacer(
	"À", "A", "Á", "A", "Â", "A", "Ã", "A", "Ä", "A", "Å", "A",
	"Ç", "C", "È", "E", "É", "E", "Ê", "E", "Ë", "E",
	"Ì", "I", "Í", "I", "Î", "I", "Ï", "I", "Ñ", "N",
	"Ò", "O", "Ó", "O", "Ô", "O", "Õ", "O", "Ö", "O",
	"Ù", "U", "Ú", "U", "Û", "U", "Ü", "U", "Ý", "Y",
	"à", "a", "á", "a", "â", "a", "ã", "a", "ä", "a", "å", "a",
	"ç", "c", "è", "e", "é", "e", "ê", "e", "ë", "e",
	"ì", "i", "í", "i", "î", "i", "ï", "i", "ñ", "n",
	"ò", "o", "ó", "o", "ô", "o", "õ", "o", "ö", "o",
	"ù", "u", "ú", "u", "û", "u", "ü", "u", "ý", "y", "ÿ", "y",
	"Ā", "A", "ā", "a", "Ă", "A", "ă", "a", "Ą", "A", "ą", "a",
	"Ć", "C", "ć", "c", "Č", "C", "č", "c", "Ď", "D", "ď", "d",
	"Ē", "E", "ē", "e", "Ė", "E", "ė", "e", "Ę", "E", "ę", "e", "Ě", "E", "ě", "e",
	"Ğ", "G", "ğ", "g", "Ģ", "G", "ģ", "g", "Ī", "I", "ī", "i", "Į", "I", "į", "i", "İ", "I",
	"Ķ", "K", "ķ", "k", "Ĺ", "L", "ĺ", "l", "Ļ", "L", "ļ", "l", "Ľ", "L", "ľ", "l",
	"Ń", "N", "ń", "n", "Ņ", "N", "ņ", "n", "Ň", "N", "ň", "n",
	"Ō", "O", "ō", "o", "Ő", "O", "ő", "o", "Ŕ", "R", "ŕ", "r", "Ř", "R", "ř", "r",
	"Ś", "S", "ś", "s", "Ş", "S", "ş", "s", "Š", "S", "š", "s",
	"Ţ", "T", "ţ", "t", "Ť", "T", "ť", "t",
	"Ū", "U", "ū", "u", "Ů", "U", "ů", "u", "Ű", "U", "ű", "u", "Ų", "U", "ų", "u",
	"Ź", "Z", "ź", "z", "Ż", "Z", "ż", "z", "Ž", "Z", "ž", "z",
)

var nonLetters = regexp.MustCompile(`[^a-z]+`)

func slugify(country string) string {
	s := strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII {
			return -1
		}
		return r
	}, asciiFold.Replace(country))
	s = strings.ToLower(s)
	return strings.Trim(nonLetters.ReplaceAllString(s, "-"), "-")
}

func repairTitle(title, slug string) string {
	country, ok := countryNames[slug]
	if !ok {
		country = titleCase(strings.ReplaceAll(slug, "-", " "))
	}
	_, suffix, found := strings.Cut(title, " - ")
	if !found {
		return country
	}
	return country + " - " + suffix
}

// titleCase matches Python's str.title(): uppercase after any non-letter.
func titleCase(s string) string {
	var b strings.Builder
	prevLetter := false
	for _, r := range s {
		if prevLetter {
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(unicode.ToUpper(r))
		}
		prevLetter = unicode.IsLetter(r)
	}
	return b.String()
}

func newFeed(country, slug, homePageURL string, items []item) feed {
	return feed{
		Version:     "https://jsonfeed.org/version/1.1",
		Title:       "U.S. Department of State - " + country + " Travel Advisories",
		HomePageURL: homePageURL,
		FeedURL:     fmt.Sprintf(feedURLTemplate, slug),
		Icon:        feedIconURL,
		Items:       items,
	}
}

func writeFeed(path string, f feed) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(file)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "    ")
	if err := enc.Encode(f); err != nil {
		_ = file.Close()
		return fmt.Errorf("error writing %s: %w", path, err)
	}
	return file.Close()
}
