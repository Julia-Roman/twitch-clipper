package clipper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafov/m3u8"
)

type pCache struct {
	Expiry         time.Time
	MasterPlaylist *m3u8.MasterPlaylist
}

var ErrStreamNotFound = errors.New("stream not found")

var playlistCache = map[string]pCache{}

var httpClient = &http.Client{Timeout: time.Minute}

func FetchPlaylist(url string, retries int) (*m3u8.MediaPlaylist, error) {
	if retries > 3 {
		return nil, fmt.Errorf("failed fetching stream segments after %v tries", retries)
	}

	d := playlistCache[url]

	if time.Now().After(d.Expiry) {
		res, err := httpClient.Get(url)
		if err != nil {
			return nil, err
		}

		defer res.Body.Close()

		if res.StatusCode == http.StatusNotFound {
			return nil, ErrStreamNotFound
		} else if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("bad status code (%v)", res.StatusCode)
		}

		p, _, err := m3u8.DecodeFrom(res.Body, false)
		if err != nil {
			return nil, err
		}

		d.MasterPlaylist = p.(*m3u8.MasterPlaylist)
	}

	variants := d.MasterPlaylist.Variants

	if len(variants) == 0 {
		return nil, errors.New("no stream playlist available")
	}

	SortVariantsByResolution(variants)

	res, err := httpClient.Get(variants[0].URI)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	mediaPlaylist, _, err := m3u8.DecodeWith(res.Body, true, customDecoders)
	if err != nil {
		d.Expiry = time.Now()
		playlistCache[url] = d
		return FetchPlaylist(url, retries+1)
	}

	d.Expiry = time.Now().Add(time.Hour)

	playlistCache[url] = d

	return mediaPlaylist.(*m3u8.MediaPlaylist), nil
}

func FetchMediaPlaylist(playlistURL string) (*m3u8.MediaPlaylist, error) {
	base, err := url.Parse(playlistURL)
	if err != nil {
		return nil, err
	}

	res, err := httpClient.Get(playlistURL)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status code (%v)", res.StatusCode)
	}

	p, listType, err := m3u8.DecodeWith(res.Body, true, customDecoders)
	if err != nil {
		return nil, err
	}
	if listType == m3u8.MASTER {
		variants := p.(*m3u8.MasterPlaylist).Variants
		if len(variants) == 0 {
			return nil, errors.New("no stream playlist available")
		}
		SortVariantsByResolution(variants)
		ref, err := url.Parse(variants[0].URI)
		if err != nil {
			return nil, err
		}
		variantURL := base.ResolveReference(ref).String()
		if variantURL == playlistURL {
			return nil, errors.New("master playlist refers to itself")
		}
		return FetchMediaPlaylist(variantURL)
	}

	playlist := p.(*m3u8.MediaPlaylist)
	for _, seg := range playlist.Segments[:playlist.Count()] {
		ref, err := url.Parse(seg.URI)
		if err != nil {
			return nil, err
		}
		seg.URI = base.ResolveReference(ref).String()
	}

	return playlist, nil
}

func FetchFirstMediaPlaylist(urls []string) (*m3u8.MediaPlaylist, error) {
	err := errors.New("no playlist URLs")
	for _, u := range urls {
		var p *m3u8.MediaPlaylist
		p, err = FetchMediaPlaylist(u)
		if err == nil {
			return p, nil
		}
	}
	return nil, err
}

// elapsedTag holds the stream time, in seconds since the broadcast started, of
// a playlist's first segment. Twitch puts it on both live and archive
// playlists, and Kick's IVS live playlists carry it under their own name.
type elapsedTag struct {
	name    string
	seconds float64
	valid   bool
}

func (t elapsedTag) TagName() string { return t.name }

func (t elapsedTag) SegmentTag() bool { return false }

func (t elapsedTag) Decode(line string) (m3u8.CustomTag, error) {
	seconds, err := strconv.ParseFloat(strings.TrimPrefix(line, t.name), 64)
	return elapsedTag{name: t.name, seconds: seconds, valid: err == nil}, nil
}

func (t elapsedTag) Encode() *bytes.Buffer {
	return bytes.NewBufferString(t.String())
}

func (t elapsedTag) String() string {
	return fmt.Sprintf("%s%.3f", t.name, t.seconds)
}

var customDecoders = []m3u8.CustomDecoder{
	elapsedTag{name: "#EXT-X-TWITCH-ELAPSED-SECS:"},
	elapsedTag{name: "#EXT-X-NET-LIVE-VIDEO-ELAPSED-SECS:"},
}

func elapsedSeconds(p *m3u8.MediaPlaylist) (float64, bool) {
	for _, d := range customDecoders {
		if t, ok := p.Custom[d.TagName()].(elapsedTag); ok && t.valid {
			return t.seconds, true
		}
	}
	return 0, false
}

// segmentStarts puts each segment on a timeline shared by both playlists of a
// broadcast: stream time if the playlist says where it starts, otherwise
// program date-time.
func segmentStarts(p *m3u8.MediaPlaylist, segs []*m3u8.MediaSegment, useStreamTime bool) ([]float64, error) {
	starts := make([]float64, len(segs))
	if useStreamTime {
		t, _ := elapsedSeconds(p)
		for i, seg := range segs {
			starts[i] = t
			t += seg.Duration
		}
		return starts, nil
	}

	for i, seg := range segs {
		if seg.ProgramDateTime.IsZero() {
			return nil, errors.New("playlist has neither stream time nor program date-time")
		}
		starts[i] = float64(seg.ProgramDateTime.UnixMicro()) / 1e6
	}
	return starts, nil
}

// Backfill prepends segments from archive, an older and longer playlist of the
// same broadcast, to live so the result reaches back at least duration. Their
// segment lengths and URIs have nothing in common, so they're spliced on
// stream time, or on program date-time when either lacks it. Stream time is
// preferred because some Twitch archives have no program date-time at all.
func Backfill(archive, live *m3u8.MediaPlaylist, duration time.Duration) (*m3u8.MediaPlaylist, time.Duration, error) {
	archiveSegs := archive.Segments[:archive.Count()]
	liveSegs := live.Segments[:live.Count()]
	if len(archiveSegs) == 0 || len(liveSegs) == 0 {
		return nil, 0, errors.New("empty playlist")
	}

	_, archiveHasStreamTime := elapsedSeconds(archive)
	_, liveHasStreamTime := elapsedSeconds(live)
	useStreamTime := archiveHasStreamTime && liveHasStreamTime

	archiveStarts, err := segmentStarts(archive, archiveSegs, useStreamTime)
	if err != nil {
		return nil, 0, fmt.Errorf("archive: %w", err)
	}
	liveStarts, err := segmentStarts(live, liveSegs, useStreamTime)
	if err != nil {
		return nil, 0, fmt.Errorf("live: %w", err)
	}

	last := len(archiveSegs) - 1
	archiveEnd := archiveStarts[last] + archiveSegs[last].Duration

	const tolerance = 0.1
	if liveStarts[0] > archiveEnd+tolerance {
		return nil, 0, fmt.Errorf("archive ends %.3fs before live playlist starts", liveStarts[0]-archiveEnd)
	}

	segs := append([]*m3u8.MediaSegment{}, archiveSegs...)
	for i, seg := range liveSegs {
		if liveStarts[i] >= archiveEnd-tolerance {
			segs = append(segs, seg)
		}
	}

	merged, err := m3u8.NewMediaPlaylist(0, uint(len(segs)))
	if err != nil {
		return nil, 0, err
	}
	for _, seg := range segs {
		if err := merged.AppendSegment(seg); err != nil {
			return nil, 0, err
		}
	}
	merged.Map = live.Map

	trimmed, skip := Trim(merged, duration)
	return trimmed, skip, nil
}

// Trim drops leading segments that aren't needed to cover duration and returns
// how far into the remaining segments the clip should start.
func Trim(playlist *m3u8.MediaPlaylist, duration time.Duration) (*m3u8.MediaPlaylist, time.Duration) {
	segs := playlist.Segments[:playlist.Count()]

	var total time.Duration
	start := len(segs)
	for start > 0 && total < duration {
		start--
		total += time.Duration(segs[start].Duration * float64(time.Second))
	}

	trimmed, _ := m3u8.NewMediaPlaylist(0, uint(max(len(segs)-start, 1)))
	for _, seg := range segs[start:] {
		trimmed.AppendSegment(seg)
	}
	trimmed.Map = playlist.Map

	return trimmed, max(total-duration, 0)
}

func MakeClip(saveDir string, clipID string, channelName string, playlist *m3u8.MediaPlaylist, skip time.Duration) (string, error) {
	segmentCount := playlist.Count()

	format := "mp4"
	clipPath := fmt.Sprintf("%s/%s.%s", saveDir, clipID, format)

	var mapBuf []byte
	if playlist.Map != nil && playlist.Map.URI != "" {
		res, err := httpClient.Get(playlist.Map.URI)
		if err != nil {
			return "", err
		}
		mapBuf, err = io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return "", err
		}
	}

	buffer := make([][]byte, segmentCount)
	var wg sync.WaitGroup
	wg.Add(int(segmentCount))

	var futile bool
	ch := make(chan error, segmentCount)
	for i, seg := range playlist.Segments[:segmentCount] {
		go func(i int, url string) {
			defer wg.Done()

			res, err := httpClient.Get(url)
			if err != nil {
				if !futile {
					ch <- err
				}
				return
			}

			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				if !futile {
					ch <- fmt.Errorf("bad status code for segment %v (%v)", i, res.StatusCode)
				}
				return
			}
			buf, err := io.ReadAll(res.Body)
			if !futile {
				if err != nil {
					ch <- err
					return
				}
				buffer[i] = buf
			}
		}(i, seg.URI)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	for err := range ch {
		if err != nil {
			futile = true
			return "", err
		}
	}

	if skip > 0 && len(buffer) > 0 {
		skip = keyframeBefore(append(append([]byte{}, mapBuf...), buffer[0]...), skip)
	}

	os.MkdirAll(saveDir, os.ModePerm)

	args := []string{"-hide_banner", "-i", "-"}
	if skip > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", skip.Seconds()))
	}
	args = append(args,
		"-c:v", "copy", "-c:a", "copy", "-c:s", "copy",
		"-f", format, clipPath)
	cmd := exec.Command("ffmpeg", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}

	go func() {
		if mapBuf != nil {
			stdin.Write(mapBuf)
		}
		for _, d := range buffer {
			stdin.Write(d)
		}
		stdin.Close()
	}()

	err = cmd.Run()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s/%s.%s", channelName, clipID, format), err
}

// keyframeBefore moves skip back to the last video keyframe in segment. A
// stream-copy cut drops video until the next keyframe while audio starts right
// away, so cutting anywhere else leaves the clip opening on audio alone. The
// keyframe's DTS is used because ffmpeg compares the cut against DTS, which
// with B-frames sits slightly before the PTS.
func keyframeBefore(segment []byte, skip time.Duration) time.Duration {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "format=start_time:packet=dts_time,flags",
		"-of", "json",
		"-i", "-")
	cmd.Stdin = bytes.NewReader(segment)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}

	var probe struct {
		Format struct {
			StartTime string `json:"start_time"`
		} `json:"format"`
		Packets []struct {
			DTSTime string `json:"dts_time"`
			Flags   string `json:"flags"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return 0
	}

	start, err := strconv.ParseFloat(probe.Format.StartTime, 64)
	if err != nil {
		return 0
	}

	var best time.Duration
	for _, p := range probe.Packets {
		if !strings.HasPrefix(p.Flags, "K") {
			continue
		}
		dts, err := strconv.ParseFloat(p.DTSTime, 64)
		if err != nil {
			continue
		}
		offset := time.Duration((dts - start) * float64(time.Second))
		if offset > skip {
			break
		}
		best = max(best, offset)
	}
	return best
}

func SortVariantsByResolution(variants []*m3u8.Variant) {
	parseRes := func(res string) (w, h int) {
		parts := strings.Split(res, "x")
		if len(parts) != 2 {
			return 0, 0
		}
		w, _ = strconv.Atoi(parts[0])
		h, _ = strconv.Atoi(parts[1])
		return
	}
	sort.SliceStable(variants, func(i, j int) bool {
		w1, h1 := parseRes(variants[i].Resolution)
		w2, h2 := parseRes(variants[j].Resolution)
		if w1 != w2 {
			return w1 > w2
		}
		return h1 > h2
	})
}
