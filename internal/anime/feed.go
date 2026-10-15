package anime

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const MaxFeedBytes = 2 << 20

type FeedItem struct {
	GUID, Title, URL string
	Episode          int
}
type rssItem struct {
	Title     string `xml:"title"`
	GUID      string `xml:"guid"`
	ID        string `xml:"id"`
	Link      string `xml:"link"`
	Enclosure struct {
		URL string `xml:"url,attr"`
	} `xml:"enclosure"`
}

// ParseFeed streams RSS items; item count and input bytes both have limits.
// DTDs/entities are never fetched by encoding/xml.
func ParseFeed(reader io.Reader, max int) ([]FeedItem, error) {
	if max < 1 || max > 1000 {
		max = 200
	}
	limited := &io.LimitedReader{R: reader, N: MaxFeedBytes + 1}
	d := xml.NewDecoder(limited)
	out := []FeedItem{}
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("anime: invalid RSS: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "item" {
			continue
		}
		var item rssItem
		if err := d.DecodeElement(&item, &start); err != nil {
			return nil, err
		}
		uri := strings.TrimSpace(item.Enclosure.URL)
		if uri == "" {
			uri = strings.TrimSpace(item.Link)
		}
		if !validDownloadURL(uri) {
			continue
		}
		guid := strings.TrimSpace(item.GUID)
		if guid == "" {
			guid = uri
		}
		if len(guid) > 4096 || len(uri) > 8192 || len(item.Title) > 2048 {
			continue
		}
		out = append(out, FeedItem{GUID: guid, Title: strings.TrimSpace(item.Title), URL: uri, Episode: ParseEpisode(item.Title)})
		if len(out) >= max {
			break
		}
	}
	if limited.N <= 0 {
		return nil, errors.New("anime: RSS exceeds 2 MiB")
	}
	return out, nil
}

var episodePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bS\d{1,2}E(\d{1,3})\b`),
	regexp.MustCompile(`(?:第\s*)(\d{1,3})(?:\s*[话話集])`),
	regexp.MustCompile(`(?:\s-\s*|\[|【|\s)(\d{1,3})(?:v\d)?(?:\s*(?:\[|【|\]|】|\(|\.|$))`),
}
var seasonPattern = regexp.MustCompile(`(?i)\bS(\d{1,2})E\d`)
var batchPattern = regexp.MustCompile(`(?i)\b(batch|complete)\b|合集|全集|\[\d{1,3}\s*[-~]\s*\d{1,3}\]`)

func ParseEpisode(title string) int {
	if batchPattern.MatchString(title) {
		return 0
	}
	for _, re := range episodePatterns {
		if m := re.FindStringSubmatch(title); len(m) > 1 {
			n, _ := strconv.Atoi(m[1])
			if n > 0 && n < 1000 {
				return n
			}
		}
	}
	return 0
}
func validateRules(include, exclude string) error {
	for _, rule := range []string{include, exclude} {
		if len(rule) > 512 {
			return inputError("筛选规则超过 512 字节")
		}
		if rule != "" {
			if _, err := regexp.Compile("(?i)" + rule); err != nil {
				return inputError("正则表达式无效：" + err.Error())
			}
		}
	}
	return nil
}
func matches(sub Subscription, title string) bool {
	include := sub.Include
	if include == "" {
		include = regexp.QuoteMeta(sub.Title)
	}
	ok, _ := regexp.MatchString("(?i)"+include, title)
	if !ok {
		return false
	}
	if sub.Exclude != "" {
		skip, _ := regexp.MatchString("(?i)"+sub.Exclude, title)
		if skip {
			return false
		}
	}
	// Never file another season's explicitly labelled release under this season.
	if m := seasonPattern.FindStringSubmatch(title); len(m) > 1 {
		n, _ := strconv.Atoi(m[1])
		if n != sub.Season {
			return false
		}
	}
	return true
}
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return inputError("需要 HTTP(S) 地址，不能嵌入账号密码或片段")
	}
	return nil
}
func validDownloadURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme == "magnet" {
		return strings.HasPrefix(u.Query().Get("xt"), "urn:btih:")
	}
	return validateHTTPURL(raw) == nil
}
