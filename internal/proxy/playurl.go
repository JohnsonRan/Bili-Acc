package proxy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const maxPlayurlResponseSize = 8 << 20

type playurlSummary struct {
	Quality         int
	AcceptQualities string
	VideoQualities  string
	VideoCodecs     string
	MediaGroups     int
}

type playurlMediaReference struct {
	object         map[string]any
	array          []any
	key            string
	index          int
	candidateIndex int
}

type playurlMediaGroup struct {
	urls       []*url.URL
	references []playurlMediaReference
}

func (s *server) rewritePlayurlResponse(body []byte) ([]byte, playurlSummary, error) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, playurlSummary{}, err
	}
	summary := summarizePlayurl(root)
	if s.publicURL == "" {
		return body, summary, nil
	}
	groups := s.collectPlayurlMediaGroups(root)
	if len(groups) == 0 {
		return body, summary, nil
	}
	registrations := make([][]*url.URL, len(groups))
	for index := range groups {
		registrations[index] = groups[index].urls
	}
	ids, err := s.registerMediaGroups(registrations)
	if err != nil {
		return nil, summary, err
	}
	for groupIndex, group := range groups {
		for _, reference := range group.references {
			value := fmt.Sprintf("%s/proxy-group/%s/%s/%d", s.publicURL, url.PathEscape(s.token), ids[groupIndex], reference.candidateIndex)
			if reference.index < 0 {
				reference.object[reference.key] = value
			} else {
				reference.array[reference.index] = value
			}
		}
	}
	summary.MediaGroups = len(groups)
	rewritten, err := json.Marshal(root)
	return rewritten, summary, err
}

func (s *server) collectPlayurlMediaGroups(root any) []playurlMediaGroup {
	groups := make([]playurlMediaGroup, 0)
	walkJSON(root, func(object map[string]any) {
		group := playurlMediaGroup{}
		candidateIndexes := make(map[string]int)
		for _, key := range []string{"baseUrl", "base_url", "url", "backupUrl", "backup_url"} {
			switch value := object[key].(type) {
			case string:
				s.appendPlayurlCandidate(&group, playurlMediaReference{object: object, key: key, index: -1}, value, candidateIndexes)
			case []any:
				for index, item := range value {
					if candidate, ok := item.(string); ok {
						s.appendPlayurlCandidate(&group, playurlMediaReference{array: value, index: index}, candidate, candidateIndexes)
					}
				}
			}
		}
		if len(group.urls) > 1 {
			groups = append(groups, group)
		}
	})
	return groups
}

func (s *server) appendPlayurlCandidate(group *playurlMediaGroup, reference playurlMediaReference, value string, indexes map[string]int) {
	candidate, err := url.Parse(value)
	if err != nil || candidate.User != nil || candidate.Fragment != "" || candidate.Port() != "" || (candidate.Scheme != "http" && candidate.Scheme != "https") || !allowedHost(candidate.Hostname(), s.mediaHosts) {
		return
	}
	index, exists := indexes[value]
	if !exists {
		index = len(group.urls)
		indexes[value] = index
		group.urls = append(group.urls, candidate)
	}
	reference.candidateIndex = index
	group.references = append(group.references, reference)
}

func summarizePlayurl(root any) playurlSummary {
	summary := playurlSummary{}
	qualitySet := make(map[int]bool)
	codecSet := make(map[string]bool)
	walkJSON(root, func(object map[string]any) {
		if summary.Quality == 0 {
			summary.Quality = jsonInt(object["quality"])
		}
		if summary.AcceptQualities == "" {
			if values, ok := object["accept_quality"].([]any); ok {
				summary.AcceptQualities = joinJSONInts(values)
			}
		}
		if id := jsonInt(object["id"]); id > 0 && hasMediaURL(object) {
			qualitySet[id] = true
		}
		if codec, ok := object["codecs"].(string); ok && codec != "" && hasMediaURL(object) {
			codecSet[boundedToken(codec, 32)] = true
		}
	})
	summary.VideoQualities = joinSortedInts(qualitySet)
	codecs := make([]string, 0, len(codecSet))
	for codec := range codecSet {
		codecs = append(codecs, codec)
	}
	sort.Strings(codecs)
	summary.VideoCodecs = strings.Join(codecs, ",")
	return summary
}

func walkJSON(value any, visit func(map[string]any)) {
	switch current := value.(type) {
	case map[string]any:
		visit(current)
		for _, child := range current {
			walkJSON(child, visit)
		}
	case []any:
		for _, child := range current {
			walkJSON(child, visit)
		}
	}
}

func hasMediaURL(object map[string]any) bool {
	for _, key := range []string{"baseUrl", "base_url", "url"} {
		if value, ok := object[key].(string); ok && value != "" {
			return true
		}
	}
	return false
}

func jsonInt(value any) int {
	if number, ok := value.(float64); ok {
		return int(number)
	}
	return 0
}

func joinJSONInts(values []any) string {
	set := make(map[int]bool)
	for _, value := range values {
		if number := jsonInt(value); number > 0 {
			set[number] = true
		}
	}
	return joinSortedInts(set)
}

func joinSortedInts(set map[int]bool) string {
	values := make([]int, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(values)))
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = strconv.Itoa(value)
	}
	return strings.Join(parts, ",")
}
