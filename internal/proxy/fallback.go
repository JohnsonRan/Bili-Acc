package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	mediaGroupTTL          = 30 * time.Minute
	maxMediaGroups         = 512
	maxGroupsPerRequest    = 64
	maxCandidatesPerGroup  = 16
	maxMediaGroupBodySize  = 1 << 20
	maxMediaCandidateURL   = 8192
	mediaGroupIDByteLength = 16
)

type mediaGroup struct {
	URLs      []*url.URL
	ExpiresAt time.Time
	CreatedAt time.Time
}

type mediaGroupRegistration struct {
	Groups []mediaGroupInput `json:"groups"`
}

type mediaGroupInput struct {
	URLs []string `json:"urls"`
}

type mediaGroupRegistrationResponse struct {
	IDs []string `json:"ids"`
}

func (s *server) handleMediaGroupRegistration(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRouteToken(r.URL.Path, "/media-groups/") {
		requestLogFrom(r).errorStage = "authorization"
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMediaGroupBodySize+1))
	if err != nil || len(body) > maxMediaGroupBodySize {
		requestLogFrom(r).errorStage = "request_body"
		http.Error(w, "invalid registration", http.StatusBadRequest)
		return
	}
	var registration mediaGroupRegistration
	if err := json.Unmarshal(body, &registration); err != nil || len(registration.Groups) == 0 || len(registration.Groups) > maxGroupsPerRequest {
		requestLogFrom(r).errorStage = "request_body"
		http.Error(w, "invalid registration", http.StatusBadRequest)
		return
	}

	groups := make([][]*url.URL, len(registration.Groups))
	for index, input := range registration.Groups {
		if len(input.URLs) == 0 || len(input.URLs) > maxCandidatesPerGroup {
			requestLogFrom(r).errorStage = "request_body"
			http.Error(w, "invalid registration", http.StatusBadRequest)
			return
		}
		groups[index] = make([]*url.URL, len(input.URLs))
		for candidateIndex, value := range input.URLs {
			if len(value) == 0 || len(value) > maxMediaCandidateURL {
				requestLogFrom(r).errorStage = "request_body"
				http.Error(w, "invalid registration", http.StatusBadRequest)
				return
			}
			candidate, err := url.Parse(value)
			if err != nil || candidate.User != nil || candidate.Fragment != "" || candidate.Port() != "" || (candidate.Scheme != "http" && candidate.Scheme != "https") || !allowedHost(candidate.Hostname(), s.mediaHosts) {
				requestLogFrom(r).errorStage = "target_validation"
				http.Error(w, "media URL not allowed", http.StatusForbidden)
				return
			}
			groups[index][candidateIndex] = candidate
		}
	}

	ids := make([]string, len(groups))
	now := s.now()
	s.mediaGroupMu.Lock()
	s.pruneMediaGroupsLocked(now)
	for index, candidates := range groups {
		id, err := newMediaGroupID()
		if err != nil {
			s.mediaGroupMu.Unlock()
			requestLogFrom(r).errorStage = "registration"
			http.Error(w, "registration failed", http.StatusInternalServerError)
			return
		}
		ids[index] = id
		s.mediaGroups[id] = mediaGroup{URLs: candidates, CreatedAt: now, ExpiresAt: now.Add(mediaGroupTTL)}
	}
	s.pruneMediaGroupsLocked(now)
	s.mediaGroupMu.Unlock()

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(mediaGroupRegistrationResponse{IDs: ids})
}

func (s *server) handleMediaGroup(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/proxy-group/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		requestLogFrom(r).errorStage = "target_validation"
		http.Error(w, "invalid media group", http.StatusBadRequest)
		return
	}
	token, err := url.PathUnescape(parts[0])
	if err != nil || !secureEqual(token, s.token) {
		requestLogFrom(r).errorStage = "authorization"
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := url.PathUnescape(parts[1])
	if err != nil || len(id) != mediaGroupIDByteLength*2 {
		requestLogFrom(r).errorStage = "target_validation"
		http.Error(w, "invalid media group", http.StatusBadRequest)
		return
	}
	preferred, err := strconv.Atoi(parts[2])
	if err != nil {
		requestLogFrom(r).errorStage = "target_validation"
		http.Error(w, "invalid media group", http.StatusBadRequest)
		return
	}
	candidates, ok := s.mediaGroupCandidates(id, preferred)
	if !ok {
		requestLogFrom(r).errorStage = "target_validation"
		http.Error(w, "media group expired", http.StatusGone)
		return
	}
	s.handleMediaTargets(w, r, candidates)
}

func (s *server) authorizeRouteToken(path, prefix string) bool {
	value := strings.TrimPrefix(path, prefix)
	if value == path || strings.Contains(value, "/") {
		return false
	}
	token, err := url.PathUnescape(value)
	return err == nil && secureEqual(token, s.token)
}

func (s *server) mediaGroupCandidates(id string, preferred int) ([]*url.URL, bool) {
	now := s.now()
	s.mediaGroupMu.Lock()
	defer s.mediaGroupMu.Unlock()
	s.pruneMediaGroupsLocked(now)
	group, ok := s.mediaGroups[id]
	if !ok || preferred < 0 || preferred >= len(group.URLs) {
		return nil, false
	}
	ordered := make([]*url.URL, 0, len(group.URLs))
	ordered = append(ordered, cloneURL(group.URLs[preferred]))
	for index, candidate := range group.URLs {
		if index != preferred {
			ordered = append(ordered, cloneURL(candidate))
		}
	}
	return ordered, true
}

func (s *server) pruneMediaGroupsLocked(now time.Time) {
	for id, group := range s.mediaGroups {
		if !group.ExpiresAt.After(now) {
			delete(s.mediaGroups, id)
		}
	}
	for len(s.mediaGroups) > maxMediaGroups {
		var oldestID string
		var oldest time.Time
		for id, group := range s.mediaGroups {
			if oldestID == "" || group.CreatedAt.Before(oldest) {
				oldestID, oldest = id, group.CreatedAt
			}
		}
		delete(s.mediaGroups, oldestID)
	}
}

func newMediaGroupID() (string, error) {
	value := make([]byte, mediaGroupIDByteLength)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func cloneURL(value *url.URL) *url.URL {
	cloned := *value
	return &cloned
}

func retryableMediaStatus(status int) bool {
	return status == http.StatusForbidden || status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func (s *server) fetchMediaCandidates(ctx context.Context, method string, candidates []*url.URL, headers http.Header) (*http.Response, *url.URL, error) {
	var lastResponse *http.Response
	var lastTarget *url.URL
	var lastErr error
	for index, target := range candidates {
		response, finalURL, err := s.fetchAllowed(ctx, method, target, headers, func(candidate *url.URL) bool {
			return allowedHost(candidate.Hostname(), s.mediaHosts)
		})
		if err != nil {
			lastErr = err
			if ctx.Err() != nil || index == len(candidates)-1 {
				return nil, nil, err
			}
			continue
		}
		lastResponse, lastTarget = response, finalURL
		if !retryableMediaStatus(response.StatusCode) || index == len(candidates)-1 {
			return response, finalURL, nil
		}
		response.Body.Close()
	}
	if lastResponse != nil {
		return lastResponse, lastTarget, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no media candidates")
	}
	return nil, nil, lastErr
}
