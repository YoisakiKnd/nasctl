package album

import "fmt"

type Collection struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	MediaCount int64  `json:"media_count"`
	MountOK    bool   `json:"mount_ok"`
	MountNote  string `json:"mount_note"`
	FirstSeen  int64  `json:"first_seen"`
	LastScan   int64  `json:"last_scan"`
}
type Collections struct {
	Items  []Collection `json:"items"`
	Paused bool         `json:"paused"`
}
type Status struct {
	IndexReady  *bool    `json:"index_ready,omitempty"`
	Version     string   `json:"version"`
	Paused      bool     `json:"paused"`
	PauseReason string   `json:"pause_reason"`
	Warnings    []string `json:"warnings"`
}
type GPS struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}
type Item struct {
	ID              int64   `json:"id"`
	Collection      string  `json:"collection"`
	Name            string  `json:"name"`
	RelDir          string  `json:"rel_dir"`
	Kind            string  `json:"kind"`
	MIME            string  `json:"mime"`
	Bytes           int64   `json:"bytes"`
	TakenAt         *string `json:"taken_at"`
	TakenAtUnix     *int64  `json:"taken_at_unix"`
	TakenSrc        string  `json:"taken_src"`
	TakenLabel      string  `json:"taken_label"`
	TakenIsFallback bool    `json:"taken_is_fallback"`
	TakenTZ         string  `json:"taken_tz"`
	TZOffsetMin     *int    `json:"tz_offset_min"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	Orientation     int     `json:"orientation"`
	Make            string  `json:"make"`
	Model           string  `json:"model"`
	GPS             *GPS    `json:"gps"`
	DurationMS      *int64  `json:"duration_ms"`
	ProbeState      string  `json:"probe_state"`
	ThumbState      string  `json:"thumb_state"`
	ThumbURL        string  `json:"thumb_url"`
	RawURL          string  `json:"raw_url"`
	Playable        bool    `json:"playable"`
	Placeholder     bool    `json:"placeholder"`
	Missing         bool    `json:"missing"`
	Note            string  `json:"note"`
}

func (it *Item) normalize(prefix string) error {
	if it.ID <= 0 || it.ID > 9007199254740991 || len(it.Name) > 1024 || len(it.RelDir) > 4096 {
		return unavailable("相册返回了无效的媒体记录")
	}
	it.ThumbURL = fmt.Sprintf("%s%d/thumb", prefix, it.ID)
	it.RawURL = fmt.Sprintf("%s%d/raw", prefix, it.ID)
	return nil
}

type Listing struct {
	Items         []Item   `json:"items"`
	NextCursor    *string  `json:"next_cursor"`
	TotalEstimate int64    `json:"total_estimate"`
	Paused        bool     `json:"paused"`
	Degraded      bool     `json:"degraded"`
	Warnings      []string `json:"warnings"`
}
type Details struct {
	Item     Item              `json:"item"`
	Exif     map[string]string `json:"exif"`
	Paths    map[string]string `json:"paths"`
	Warnings []string          `json:"warnings"`
}
