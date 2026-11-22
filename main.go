package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"twitch-clipper/clipper"
	"twitch-clipper/kick"
	"twitch-clipper/twitch"

	"github.com/grafov/m3u8"
)

const httpHost = "localhost:8989"
const clipsDir = "/var/www/fi.supa.sh/clips"

// Every DVR segment is held in memory until ffmpeg runs, about 1MB per second
// at source quality.
const maxClipDuration = 180 * time.Second

func resError(w http.ResponseWriter, message string, statusCode int) {
	m := map[string]interface{}{
		"message": message,
		"error":   statusCode,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(m)
}

func playlistDuration(playlist *m3u8.MediaPlaylist) (total time.Duration) {
	for _, seg := range playlist.Segments[:playlist.Count()] {
		total += time.Duration(seg.Duration * float64(time.Second))
	}
	return
}

// extendPlaylist reaches back further than the live playlist's ~30s window
// using the broadcast's archive when the requested duration needs it. On any
// archive failure the clip falls back to whatever the live playlist covers.
func extendPlaylist(live *m3u8.MediaPlaylist, dvrURLs []string, duration time.Duration) (*m3u8.MediaPlaylist, time.Duration) {
	if duration > playlistDuration(live) {
		for _, u := range dvrURLs {
			archive, err := clipper.FetchMediaPlaylist(u)
			if err != nil {
				continue
			}
			merged, skip, err := clipper.Backfill(archive, live, duration)
			if err != nil {
				log.Println("dvr backfill failed", u, err)
				continue
			}
			return merged, skip
		}
		log.Println("no usable dvr playlist")
	}
	return clipper.Trim(live, duration)
}

func main() {
	http.HandleFunc("/clip/", func(w http.ResponseWriter, r *http.Request) {
		channelName := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/clip/"))
		if channelName == "" {
			resError(w, "invalid channel name", 400)
			return
		}

		saveDir := fmt.Sprintf("%s/%s", clipsDir, channelName)

		createdAt := time.Now().UTC()
		clipID := fmt.Sprintf("%v", createdAt.Unix())

		query := r.URL.Query()
		infoPath := fmt.Sprintf("%s/%s.info.json", saveDir, clipID)

		var duration time.Duration
		if d := query.Get("duration"); d != "" {
			seconds, err := strconv.Atoi(d)
			if err != nil || seconds <= 0 {
				resError(w, "invalid duration", 400)
				return
			}
			duration = min(time.Duration(seconds)*time.Second, maxClipDuration)
		}

		var playlistURL string
		var dvrURLs, twitchDVRDirs []string
		var data []byte
		if query.Get("platform") == "kick" {
			clipInfo, err := kick.GetClipInfo(createdAt, channelName)
			if err != nil {
				statusCode := 500
				if errors.Is(err, clipper.ErrStreamNotFound) {
					statusCode = 404
				}

				log.Println("kick clip info failed", err)
				resError(w, "failed to fetch clip info: "+err.Error(), statusCode)
				return
			}

			if clipInfo.Channel.ID == 0 {
				resError(w, "channel not found", 404)
				return
			}

			if !clipInfo.Channel.Livestream.IsLive {
				resError(w, clipper.ErrStreamNotFound.Error(), 404)
				return
			}

			playlistURL = clipInfo.Channel.PlaybackURL

			if duration > 0 {
				dvrURL, err := kick.DVRPlaylistURL(clipInfo)
				if err != nil {
					log.Println("kick dvr lookup failed", err)
				} else {
					dvrURLs = []string{dvrURL}
				}
			}

			clipInfo.Channel.PlaybackURL = "" // remove playback URL from saved info

			log.Printf("clipped kick/%s\n", clipInfo.Channel.Slug)

			data, err = json.Marshal(clipInfo)
			if err != nil {
				log.Println("clip info marshal failed", err)
				return
			}
		} else {
			clipInfo, err := twitch.GetClipInfo(createdAt, channelName, query.Get("creator_id"), query.Get("parent_id"))
			if err != nil {
				log.Println("twitch clip info failed", err)
				resError(w, "failed to fetch clip info: "+err.Error(), 500)
				return
			}

			if clipInfo.Channel == nil {
				resError(w, "channel not found", 404)
				return
			}

			playlistURL = fmt.Sprintf("https://luminous.alienpls.org/live/%s?platform=web&allow_source=true&allow_audio_only=true", channelName)

			var clipLog strings.Builder
			if clipInfo.Creator != nil {
				clipLog.WriteString(fmt.Sprintf("@%s ", clipInfo.Creator.Login))
			}
			clipLog.WriteString(fmt.Sprintf("clipped twitch/%s", clipInfo.Channel.Login))
			if clipInfo.Parent != nil {
				clipLog.WriteString(fmt.Sprintf(" from #%s", clipInfo.Parent.Login))
			}
			log.Println(clipLog.String())

			if duration > 0 {
				twitchDVRDirs, err = clipInfo.DVRDirs()
				if err != nil {
					log.Println("dvr playlist guess failed", err)
				}
			}
			if clipInfo.Channel.Stream != nil {
				clipInfo.Channel.Stream.ArchiveVideo = nil // remove archive URL from saved info
			}

			data, err = json.Marshal(clipInfo)
			if err != nil {
				log.Println("clip info marshal failed", err)
				return
			}
		}

		playlist, variant, err := clipper.FetchPlaylist(playlistURL, 0)
		if err != nil {
			statusCode := 500
			if errors.Is(err, clipper.ErrStreamNotFound) {
				statusCode = 404
			}

			resError(w, err.Error(), statusCode)
			return
		}

		var skip time.Duration
		if duration > 0 {
			if twitchDVRDirs != nil {
				dvrURLs = twitch.DVRPlaylistURLs(twitchDVRDirs, variant)
			}
			playlist, skip = extendPlaylist(playlist, dvrURLs, duration)
		}

		path, err := clipper.MakeClip(saveDir, clipID, channelName, playlist, skip)
		if err != nil {
			resError(w, err.Error(), 500)
			return
		}

		os.WriteFile(infoPath, data, 0644)

		m := map[string]interface{}{
			"path": path,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m)
	})

	log.Println("Server running on " + httpHost)

	log.Fatal(http.ListenAndServe(httpHost, nil))
}
