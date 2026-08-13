package proxy

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const livePlaylistSegments = 3

var playlistURI = regexp.MustCompile(`URI="([^"]+)"`)

type playlistTrimResult struct {
	Live      bool
	Trimmed   bool
	Skipped   int
	Malformed bool
}

func (s *server) rewritePlaylist(body string, baseURL *url.URL, publicBase string) (string, error) {
	rewrite := func(value string) (string, error) {
		target, err := baseURL.Parse(value)
		if err != nil || !allowedHost(target.Hostname(), s.mediaHosts) {
			return "", errors.New("playlist host not allowed")
		}
		origin := target.Scheme + "://" + target.Host
		encoded := base64.RawURLEncoding.EncodeToString([]byte(origin))
		return fmt.Sprintf("%s/proxy/%s/%s%s", publicBase, url.PathEscape(s.token), encoded, target.RequestURI()), nil
	}

	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			value, err := rewrite(trimmed)
			if err != nil {
				return "", err
			}
			lines[i] = value
			continue
		}
		var rewriteErr error
		lines[i] = playlistURI.ReplaceAllStringFunc(line, func(match string) string {
			parts := playlistURI.FindStringSubmatch(match)
			value, err := rewrite(parts[1])
			if err != nil {
				rewriteErr = err
				return match
			}
			return `URI="` + value + `"`
		})
		if rewriteErr != nil {
			return "", rewriteErr
		}
	}
	return strings.Join(lines, "\n"), nil
}

func trimLivePlaylist(body string, keep int) (string, playlistTrimResult) {
	result := playlistTrimResult{}
	if keep < 1 {
		return body, result
	}
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "#EXT-X-ENDLIST" || trimmed == "#EXT-X-PLAYLIST-TYPE:EVENT" || strings.HasPrefix(trimmed, "#EXT-X-MAP:") || strings.HasPrefix(trimmed, "#EXT-X-BYTERANGE:") {
			return body, result
		}
	}

	type segment struct{ start, extinf, uri int }
	segments := make([]segment, 0)
	previousURI := -1
	for index, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "#EXTINF:") {
			continue
		}
		uri := -1
		for candidate := index + 1; candidate < len(lines); candidate++ {
			trimmed := strings.TrimSpace(lines[candidate])
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			uri = candidate
			break
		}
		if uri < 0 {
			result.Live = true
			result.Malformed = true
			return body, result
		}
		start := previousURI + 1
		if previousURI < 0 {
			start = firstSegmentTag(lines, index)
		}
		segments = append(segments, segment{start: start, extinf: index, uri: uri})
		previousURI = uri
	}
	if len(segments) == 0 {
		return body, result
	}
	result.Live = true
	if len(segments) <= keep {
		return body, result
	}

	skipped := len(segments) - keep
	firstStart := segments[0].start
	removedEnd := segments[skipped-1].uri + 1
	retainedStart := segments[skipped].start
	prefix := append([]string(nil), lines[:firstStart]...)
	prefix = carryActivePlaylistTags(prefix, lines[firstStart:removedEnd])
	prefix = advancePlaylistSequence(prefix, "#EXT-X-MEDIA-SEQUENCE:", skipped)

	discontinuities := 0
	for _, line := range lines[firstStart:removedEnd] {
		if strings.TrimSpace(line) == "#EXT-X-DISCONTINUITY" {
			discontinuities++
		}
	}
	if discontinuities > 0 {
		prefix = advancePlaylistSequence(prefix, "#EXT-X-DISCONTINUITY-SEQUENCE:", discontinuities)
	}

	output := append(prefix, lines[retainedStart:]...)
	result.Trimmed = true
	result.Skipped = skipped
	return strings.Join(output, "\n"), result
}

func firstSegmentTag(lines []string, extinf int) int {
	start := extinf
	for start > 0 {
		trimmed := strings.TrimSpace(lines[start-1])
		if trimmed == "" || trimmed == "#EXT-X-DISCONTINUITY" || strings.HasPrefix(trimmed, "#EXT-X-KEY:") || strings.HasPrefix(trimmed, "#EXT-X-PROGRAM-DATE-TIME:") || strings.HasPrefix(trimmed, "#EXT-X-DATERANGE:") || strings.HasPrefix(trimmed, "#EXT-X-GAP") {
			start--
			continue
		}
		break
	}
	return start
}

func carryActivePlaylistTags(prefix, removed []string) []string {
	for _, tag := range []string{"#EXT-X-KEY:", "#EXT-X-MAP:"} {
		latest := ""
		for _, line := range prefix {
			if strings.HasPrefix(strings.TrimSpace(line), tag) {
				latest = line
			}
		}
		for _, line := range removed {
			if strings.HasPrefix(strings.TrimSpace(line), tag) {
				latest = line
			}
		}
		filtered := prefix[:0]
		for _, line := range prefix {
			if !strings.HasPrefix(strings.TrimSpace(line), tag) {
				filtered = append(filtered, line)
			}
		}
		prefix = filtered
		if latest != "" {
			prefix = append(prefix, latest)
		}
	}
	return prefix
}

func advancePlaylistSequence(lines []string, tag string, delta int) []string {
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, tag) {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(trimmed, tag)), 10, 64)
		if err == nil {
			lines[index] = tag + strconv.FormatInt(value+int64(delta), 10)
		}
		return lines
	}
	insert := 1
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		insert = 0
	}
	lines = append(lines, "")
	copy(lines[insert+1:], lines[insert:])
	lines[insert] = tag + strconv.Itoa(delta)
	return lines
}

func isPlaylist(contentType, path string) bool {
	return strings.Contains(strings.ToLower(contentType), "mpegurl") || strings.HasSuffix(strings.ToLower(path), ".m3u8")
}
