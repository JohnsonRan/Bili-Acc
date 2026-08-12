package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
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
	candidateHealthTTL     = 30 * time.Minute
	candidateFailureBase   = 30 * time.Second
	candidateFailureMax    = 5 * time.Minute
	maxCandidateHealth     = 256
)

type candidateHealth struct {
	Successes      uint64
	Failures       uint64
	Consecutive    uint32
	HeaderEWMA     time.Duration
	LastObserved   time.Time
	PenalizedUntil time.Time
}

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
	preferredCandidate := group.URLs[preferred]
	ordered := make([]*url.URL, 0, len(group.URLs))
	if !likelyIPBoundCOSCandidate(preferredCandidate) {
		ordered = append(ordered, cloneURL(preferredCandidate))
	}
	for index, candidate := range group.URLs {
		if index != preferred && !likelyIPBoundCOSCandidate(candidate) {
			ordered = append(ordered, cloneURL(candidate))
		}
	}
	if likelyIPBoundCOSCandidate(preferredCandidate) {
		ordered = append(ordered, cloneURL(preferredCandidate))
	}
	for index, candidate := range group.URLs {
		if index != preferred && likelyIPBoundCOSCandidate(candidate) {
			ordered = append(ordered, cloneURL(candidate))
		}
	}
	return s.rankMediaCandidates(ordered, now), true
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

func likelyIPBoundCOSCandidate(candidate *url.URL) bool {
	if candidate == nil || !strings.Contains(strings.ToLower(candidate.Hostname()), "mirrorcosov") {
		return false
	}
	query := candidate.Query()
	return strings.EqualFold(query.Get("gen"), "playurlv3") && query.Get("oi") != ""
}

func retryableMediaStatus(status int) bool {
	return status == http.StatusForbidden || status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func (s *server) fetchMediaCandidates(ctx context.Context, method string, candidates []*url.URL, headers http.Header) (*http.Response, *url.URL, error) {
	var lastErr error
	attempts := 0
	for index, target := range candidates {
		attempts++
		started := s.now()
		response, finalURL, err := s.fetchAllowed(ctx, method, target, headers, func(candidate *url.URL) bool {
			return allowedHost(candidate.Hostname(), s.mediaHosts)
		})
		latency := s.now().Sub(started)
		if err != nil {
			s.recordCandidateFailure(target, 0)
			lastErr = err
			if ctx.Err() != nil || index == len(candidates)-1 {
				s.metrics.recordCandidateSelection(s.now(), attempts, false, index == len(candidates)-1)
				return nil, nil, err
			}
			continue
		}
		if !retryableMediaStatus(response.StatusCode) || index == len(candidates)-1 {
			if response.StatusCode >= 200 && response.StatusCode < 400 {
				s.recordCandidateSuccess(finalURL, latency)
			} else if retryableMediaStatus(response.StatusCode) && index < len(candidates)-1 {
				s.recordCandidateFailure(finalURL, response.StatusCode)
			}
			s.metrics.recordCandidateSelection(s.now(), attempts, attempts > 1 && response.StatusCode >= 200 && response.StatusCode < 400, attempts == len(candidates) && retryableMediaStatus(response.StatusCode))
			return response, finalURL, nil
		}
		s.recordCandidateFailure(finalURL, response.StatusCode)
		response.Body.Close()
	}
	if lastErr == nil {
		lastErr = errors.New("no media candidates")
	}
	s.metrics.recordCandidateSelection(s.now(), attempts, false, true)
	return nil, nil, lastErr
}

func (s *server) rankMediaCandidates(candidates []*url.URL, now time.Time) []*url.URL {
	type rankedCandidate struct {
		url      *url.URL
		index    int
		health   candidateHealth
		observed bool
		risky    bool
	}
	ranked := make([]rankedCandidate, len(candidates))
	s.candidateMu.Lock()
	s.pruneCandidateHealthLocked(now)
	for index, candidate := range candidates {
		health, ok := s.candidateHealth[candidateHealthKey(candidate)]
		ranked[index] = rankedCandidate{url: candidate, index: index, health: health, observed: ok, risky: likelyIPBoundCOSCandidate(candidate)}
	}
	s.candidateMu.Unlock()
	sort.SliceStable(ranked, func(i, j int) bool {
		left, right := ranked[i], ranked[j]
		if left.risky != right.risky {
			return !left.risky
		}
		leftPenalized := left.health.PenalizedUntil.After(now)
		rightPenalized := right.health.PenalizedUntil.After(now)
		if leftPenalized != rightPenalized {
			return !leftPenalized
		}
		leftReliable := left.observed && left.health.Successes > 0
		rightReliable := right.observed && right.health.Successes > 0
		if leftReliable != rightReliable {
			return leftReliable
		}
		if leftReliable && rightReliable && left.health.HeaderEWMA != right.health.HeaderEWMA {
			return left.health.HeaderEWMA < right.health.HeaderEWMA
		}
		return left.index < right.index
	})
	ordered := make([]*url.URL, len(ranked))
	for index, candidate := range ranked {
		ordered[index] = candidate.url
	}
	return ordered
}

func (s *server) recordCandidateSuccess(target *url.URL, latency time.Duration) {
	now := s.now()
	key := candidateHealthKey(target)
	if key == "" {
		return
	}
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	health := s.candidateHealth[key]
	health.Successes++
	health.Consecutive = 0
	health.PenalizedUntil = time.Time{}
	health.LastObserved = now
	if health.HeaderEWMA == 0 {
		health.HeaderEWMA = latency
	} else {
		health.HeaderEWMA = (health.HeaderEWMA*3 + latency) / 4
	}
	s.candidateHealth[key] = health
	s.pruneCandidateHealthLocked(now)
}

func (s *server) recordCandidateFailure(target *url.URL, status int) {
	now := s.now()
	key := candidateHealthKey(target)
	if key == "" {
		return
	}
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	health := s.candidateHealth[key]
	health.Failures++
	if health.Consecutive < 16 {
		health.Consecutive++
	}
	penalty := candidateFailureBase << min(int(health.Consecutive-1), 4)
	if status == http.StatusForbidden {
		penalty *= 2
	}
	if penalty > candidateFailureMax {
		penalty = candidateFailureMax
	}
	health.PenalizedUntil = now.Add(penalty)
	health.LastObserved = now
	s.candidateHealth[key] = health
	s.pruneCandidateHealthLocked(now)
}

func (s *server) pruneCandidateHealthLocked(now time.Time) {
	for key, health := range s.candidateHealth {
		if now.Sub(health.LastObserved) > candidateHealthTTL {
			delete(s.candidateHealth, key)
		}
	}
	for len(s.candidateHealth) > maxCandidateHealth {
		var oldestKey string
		var oldest time.Time
		for key, health := range s.candidateHealth {
			if oldestKey == "" || health.LastObserved.Before(oldest) {
				oldestKey, oldest = key, health.LastObserved
			}
		}
		delete(s.candidateHealth, oldestKey)
	}
}

func candidateHealthKey(target *url.URL) string {
	if target == nil {
		return ""
	}
	// Keep query signatures out of logs and bound the in-memory key. The full
	// URL is used so a failed signature does not poison another candidate on the
	// same CDN host.
	hash := sha256.Sum256([]byte(target.String()))
	return diagnosticHost(target.Hostname()) + ":" + hex.EncodeToString(hash[:8])
}
