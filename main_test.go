package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>travel.state.gov: Travel Advisories</title>
    <item>
      <title>France - Level 2: Exercise Increased Caution</title>
      <link>https://travel.state.gov/content/tsg_aem/us/en/home/international-travel/travel-advisories/destination.fra.html</link>
      <pubDate>Tue, 15 Sep 2026</pubDate>
      <description><![CDATA[<p>See <a href='/en/terrorism.html'>terrorism</a> & more.</p>]]></description>
    </item>
    <item>
      <title>Côte d’Ivoire - Level 2: Exercise Increased Caution</title>
      <link>https://travel.state.gov/content/tsg_aem/us/en/home/international-travel/travel-advisories/destination.civ.html</link>
      <pubDate>Mon, 01 Jun 2026</pubDate>
      <description><![CDATA[<p>Côte d’Ivoire</p>]]></description>
    </item>
    <item>
      <title>Vanuatu Travel Advisory - Level 1: Exercise Normal Precautions</title>
      <link>https://travel.state.gov/content/travel/en/traveladvisories/traveladvisories/vanuatu-travel-advisory.html</link>
      <pubDate>Fri, 01 May 2026</pubDate>
      <description><![CDATA[<p>Vanuatu</p>]]></description>
    </item>
    <item>
      <title>No Date - Level 1: Exercise Normal Precautions</title>
      <link>https://travel.state.gov/destination.xxx.html</link>
      <description>skipped</description>
    </item>
  </channel>
</rss>`

func TestRun(t *testing.T) {
	srv := serveRSS(t, testRSS)

	dir := t.TempDir()
	stale := filepath.Join(dir, "stale.json")
	if err := os.WriteFile(stale, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	code := run(t.Context(), []string{"-output-dir", dir, "-combine-countries", "vanuatu,france,missing,france"})
	if code != 0 {
		t.Fatalf("run() returned %d, want 0", code)
	}

	got, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		got[i] = filepath.Base(got[i])
	}
	want := []string{"combined.json", "cote-divoire.json", "france.json", "vanuatu.json"}
	if !slices.Equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}

	f := readFeed(t, filepath.Join(dir, "france.json"))
	if f.Title != "U.S. Department of State - France Travel Advisories" {
		t.Errorf("title = %q", f.Title)
	}
	if f.FeedURL != "https://josh.github.io/us-state-travel-advisories-feeds/france.json" {
		t.Errorf("feed_url = %q", f.FeedURL)
	}
	it := f.Items[0]
	if it.ID != "france-1789430400" {
		t.Errorf("id = %q, want %q", it.ID, "france-1789430400")
	}
	if it.DatePublished != "2026-09-15T00:00:00+00:00" {
		t.Errorf("date_published = %q", it.DatePublished)
	}
	wantHTML := `<p>See <a href='` + srv.URL + `/en/terrorism.html'>terrorism</a> & more.</p>`
	if it.ContentHTML != wantHTML {
		t.Errorf("content_html = %q, want %q", it.ContentHTML, wantHTML)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "cote-divoire.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Côte d’Ivoire Travel Advisories") || !strings.HasSuffix(string(raw), "}\n") {
		t.Errorf("cote-divoire.json not written as unescaped UTF-8 with trailing newline:\n%s", raw)
	}

	combined := readFeed(t, filepath.Join(dir, "combined.json"))
	var ids []string
	for _, it := range combined.Items {
		ids = append(ids, it.ID)
	}
	if want := []string{"france-1789430400", "vanuatu-1777593600"}; !slices.Equal(ids, want) {
		t.Errorf("combined ids = %v, want %v", ids, want)
	}
}

func TestRunEmptyCombined(t *testing.T) {
	serveRSS(t, testRSS)

	dir := t.TempDir()
	if code := run(t.Context(), []string{"-output-dir", dir, "-combine-countries", "nowhere"}); code != 0 {
		t.Fatalf("run() returned %d, want 0", code)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "combined.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"items": []`) {
		t.Errorf("combined.json items not an empty array:\n%s", raw)
	}
}

func TestRunErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed", "<rss><channel><item>"},
		{"empty", `<rss><channel></channel></rss>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			serveRSS(t, c.body)

			if code := run(t.Context(), []string{"-output-dir", t.TempDir()}); code != 1 {
				t.Errorf("run() returned %d, want 1", code)
			}
		})
	}
}

func TestParseItemsDedup(t *testing.T) {
	desc := "x"
	entries := []rssItem{
		{Title: "Foo - Level 1: A", Link: "https://example.com/destination.foo.html", PubDate: "Mon, 01 Jun 2026", Description: &desc},
		{Title: "Foo - Level 2: B", Link: "https://example.com/destination.foo2.html", PubDate: "Mon, 01 Jun 2026", Description: &desc},
		{Title: "Bar - Level 1: A", Link: "https://example.com/destination.bar.html", PubDate: "Mon, 01 Jun 2026", Description: &desc},
		{Title: "Foo - Level 3: C", Link: "https://example.com/foo-travel-advisory.html", PubDate: "Mon, 01 Jun 2026", Description: &desc},
	}
	slugs, items := parseItems(entries)
	if want := []string{"foo", "bar"}; !slices.Equal(slugs, want) {
		t.Errorf("slugs = %v, want %v", slugs, want)
	}
	if got := items["foo"].Title; got != "Foo - Level 3: C" {
		t.Errorf("foo title = %q, want legacy link entry to win", got)
	}
}

func TestLegacySlug(t *testing.T) {
	cases := []struct {
		url  string
		slug string
		ok   bool
	}{
		{"https://x/worldwide-caution.html", "worldwide", true},
		{"https://x/Vanuatu-Travel-Advisory.html", "vanuatu", true},
		{"https://x/afghanistan-advisory.html", "afghanistan", true},
		{"https://x/west-bank.html", "west-bank", true},
		{"https://x/destination.fra.html", "", false},
		{"https://x/page2.html", "", false},
	}
	for _, c := range cases {
		slug, ok := legacySlug(c.url)
		if slug != c.slug || ok != c.ok {
			t.Errorf("legacySlug(%q) = %q, %v, want %q, %v", c.url, slug, ok, c.slug, c.ok)
		}
	}
}

func TestCountryName(t *testing.T) {
	cases := []struct {
		title string
		name  string
	}{
		{"France - Level 2: Exercise Increased Caution", "France"},
		{"Switzerland  - Level 1: Exercise Normal Precautions", "Switzerland"},
		{"Vanuatu Travel Advisory - Level 1: Exercise Normal Precautions", "Vanuatu"},
		{"Mexico - Level 2: See State Summaries", "Mexico"},
		{"Worldwide Caution", "Worldwide Caution"},
	}
	for _, c := range cases {
		if got := countryName(c.title); got != c.name {
			t.Errorf("countryName(%q) = %q, want %q", c.title, got, c.name)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct {
		country string
		slug    string
	}{
		{"Curaçao", "curacao"},
		{"São Tomé and Príncipe", "sao-tome-and-principe"},
		{"Côte d’Ivoire", "cote-divoire"},
		{"Bosnia & Herzegovina", "bosnia-herzegovina"},
		{"Area 51", "area"},
	}
	for _, c := range cases {
		if got := slugify(c.country); got != c.slug {
			t.Errorf("slugify(%q) = %q, want %q", c.country, got, c.slug)
		}
	}
}

func TestRepairTitle(t *testing.T) {
	cases := []struct {
		title string
		slug  string
		want  string
	}{
		{"Level 3 - See Individual Summaries", "israel-west-bank-and-gaza", "Israel, the West Bank, and Gaza - See Individual Summaries"},
		{"X - See Individual Summaries", "cote-d'ivoire", "Cote D'Ivoire - See Individual Summaries"},
		{"See Individual Summaries", "mexico", "Mexico"},
	}
	for _, c := range cases {
		if got := repairTitle(c.title, c.slug); got != c.want {
			t.Errorf("repairTitle(%q, %q) = %q, want %q", c.title, c.slug, got, c.want)
		}
	}
}

func TestParseDate(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Tue, 15 Sep 2026", "2026-09-15T00:00:00+00:00"},
		{"Sat, 10 Oct 2026 05:18:02 GMT", "2026-10-10T05:18:02+00:00"},
		{"Sat, 10 Oct 2026 05:18:02 -0700", "2026-10-10T12:18:02+00:00"},
	}
	for _, c := range cases {
		got, ok := parseDate(c.in)
		if !ok {
			t.Errorf("parseDate(%q) failed", c.in)
			continue
		}
		if s := got.Format("2006-01-02T15:04:05-07:00"); s != c.want {
			t.Errorf("parseDate(%q) = %q, want %q", c.in, s, c.want)
		}
	}
	if _, ok := parseDate("yesterday"); ok {
		t.Error("parseDate(\"yesterday\") succeeded, want failure")
	}
}

func TestResolveURLs(t *testing.T) {
	const base = "https://travel.state.gov/_res/rss/TAsTWs.xml"
	cases := []struct {
		in   string
		want string
	}{
		{`<a href='/en/x.html'>x</a>`, `<a href='https://travel.state.gov/en/x.html'>x</a>`},
		{`<a title="t" href="https://example.com/">x</a>`, `<a title="t" href="https://example.com/">x</a>`},
		{`<img src="img.png">`, `<img src="https://travel.state.gov/_res/rss/img.png">`},
		{`<p>href='/no'</p>`, `<p>href='/no'</p>`},
	}
	for _, c := range cases {
		if got := resolveURLs(c.in, base); got != c.want {
			t.Errorf("resolveURLs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// serveRSS points sourceURL at a test server returning body.
func serveRSS(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	orig := sourceURL
	sourceURL = srv.URL + "/_res/rss/TAsTWs.xml"
	t.Cleanup(func() { sourceURL = orig })
	return srv
}

func readFeed(t *testing.T, path string) feed {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f feed
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}
