package twitch

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/grafov/m3u8"
)

const clientID = "ue6666qo983tsx6so1t0vnawi233wa"

type gqlPayload struct {
	OperationName string `json:"operationName"`
	Query         string `json:"query"`
	Variables     any    `json:"variables"`
}

type userData struct {
	ID                string `json:"id"`
	Login             string `json:"login"`
	DisplayName       string `json:"displayName"`
	BroadcastSettings *struct {
		Game struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"game"`
		Title string `json:"title"`
	} `json:"broadcastSettings,omitempty"`
	Stream *struct {
		ID           string `json:"id"`
		CreatedAt    string `json:"createdAt"`
		ArchiveVideo *struct {
			SeekPreviewsURL string `json:"seekPreviewsURL"`
		} `json:"archiveVideo,omitempty"`
		IsMature bool   `json:"isMature"`
		Language string `json:"language"`
	} `json:"stream,omitempty"`
}

type usersResponse struct {
	Data struct {
		Channel *userData   `json:"channel"`
		Users   []*userData `json:"users"`
	} `json:"data"`
}

type ClipInfo struct {
	Platform  string    `json:"platform"`
	CreatedAt time.Time `json:"createdAt"`
	Channel   *userData `json:"channel"`
	Creator   *userData `json:"creator"`
	Parent    *userData `json:"parent"`
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func fetchClipUsers(channelName string, userIDs ...string) (response usersResponse, err error) {
	payload, err := json.Marshal(gqlPayload{
		OperationName: "Users",
		Query:         "query Users($channel: String! $users: [ID!]!) { channel: user(login: $channel) { id login displayName broadcastSettings { game { id name } title } stream { id createdAt isMature language archiveVideo { seekPreviewsURL } } } users(ids: $users) { id login displayName } }",
		Variables: map[string]any{
			"channel": channelName,
			"users":   userIDs,
		},
	})
	if err != nil {
		return
	}

	req, _ := http.NewRequest("POST", "https://gql.twitch.tv/gql", bytes.NewBuffer(payload))
	req.Header.Set("Client-Id", clientID)

	res, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()

	err = json.NewDecoder(res.Body).Decode(&response)
	if err != nil {
		return
	}

	return
}

func GetClipInfo(createdAt time.Time, channelName string, creatorID string, parentID string) (info ClipInfo, err error) {
	res, err := fetchClipUsers(channelName, creatorID, parentID)
	if err != nil {
		return
	}

	if len(res.Data.Users) < 2 {
		err = errors.New("upstream error")
		return
	}

	info = ClipInfo{
		Platform:  "twitch",
		CreatedAt: createdAt.Truncate(time.Second),
		Channel:   res.Data.Channel,
		Creator:   res.Data.Users[0],
		Parent:    res.Data.Users[1],
	}
	return
}

// CloudFront distributions Twitch serves archives from, tried when the stream
// has no archive video to read the host from.
var dvrHosts = []string{
	"d3fi1amfgojobc.cloudfront.net",
	"d2nvs31859zcd8.cloudfront.net",
	"ds0h3roq6wcgc.cloudfront.net",
	"d2aba1wr3818hz.cloudfront.net",
	"d3c27h4odz752x.cloudfront.net",
	"dgeft87wbj63p.cloudfront.net",
	"d1m7jfoe9zdc1j.cloudfront.net",
	"d3vd9lfkzbru3h.cloudfront.net",
	"ddacn6pr5v0tl.cloudfront.net",
	"d3aqoihi2n8ty8.cloudfront.net",
}

// DVRDirs guesses where the in-progress archive of the current broadcast
// lives. Archive directories are named
// sha1(login_streamID_startUnix)[:20] + "_" + login_streamID_startUnix.
func (info ClipInfo) DVRDirs() ([]string, error) {
	if info.Channel == nil || info.Channel.Stream == nil || info.Channel.Stream.ID == "" {
		return nil, errors.New("channel is not live")
	}
	stream := info.Channel.Stream

	startedAt, err := time.Parse(time.RFC3339, stream.CreatedAt)
	if err != nil {
		return nil, err
	}

	name := fmt.Sprintf("%s_%s_%d", info.Channel.Login, stream.ID, startedAt.Unix())
	sum := sha1.Sum([]byte(name))
	dir := hex.EncodeToString(sum[:])[:20] + "_" + name

	var dirs []string
	hosts := dvrHosts
	if stream.ArchiveVideo != nil {
		if u, err := url.Parse(stream.ArchiveVideo.SeekPreviewsURL); err == nil && u.Host != "" {
			if archiveDir, _, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/"); ok && archiveDir != dir {
				dirs = append(dirs, fmt.Sprintf("https://%s/%s", u.Host, archiveDir))
			}
			hosts = append([]string{u.Host}, dvrHosts...)
		}
	}

	seen := map[string]bool{}
	for _, host := range hosts {
		if seen[host] {
			continue
		}
		seen[host] = true
		dirs = append(dirs, fmt.Sprintf("https://%s/%s", host, dir))
	}
	return dirs, nil
}

// DVRPlaylistURLs lists the archive playlists in dirs, preferring the
// rendition of the live variant being clipped so the two splice together.
// "chunked" holds the source rendition, which can be in a codec the live
// playlist doesn't offer, and transcodes are named like 1080p60.
func DVRPlaylistURLs(dirs []string, live *m3u8.Variant) []string {
	renditions := []string{"chunked"}
	if live != nil && live.FrameRate > 0 {
		if _, h, ok := strings.Cut(live.Resolution, "x"); ok {
			renditions = append([]string{fmt.Sprintf("%sp%d", h, int(math.Round(live.FrameRate)))}, renditions...)
		}
	}

	var urls []string
	for _, dir := range dirs {
		for _, r := range renditions {
			urls = append(urls, fmt.Sprintf("%s/%s/index-dvr.m3u8", dir, r))
		}
	}
	return urls
}
