package proxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	metricWindowSeconds = 15 * 60
	maxRecentErrors     = 100
	maxDedupKeys        = 1_024
	maxDedupKeyBytes    = 512
	maxHostsPerBucket   = 32
	maxAggregatedHosts  = 128
	overflowHost        = "other"
	grpcStatusSlots     = 18
)

var latencyBoundsMS = [...]int64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1_000, 2_000, 5_000, 10_000, 30_000}

type Options struct {
	Logger             *slog.Logger
	LogMediaSuccess    bool
	ErrorDedupInterval time.Duration
	ClientIPMode       string
	UpstreamNetwork    string
	MediaIdleTimeout   time.Duration
}

type Application struct {
	server *server
}

func NewApplication(token, publicURL, allowedHosts string, options Options) *Application {
	network := strings.ToLower(strings.TrimSpace(options.UpstreamNetwork))
	if network == "" {
		network = "ipv4"
	}
	s := newServerWithNetwork(token, publicURL, splitHosts(allowedHosts), network)
	if options.Logger != nil {
		s.logger = options.Logger
	}
	s.logMediaSuccess = options.LogMediaSuccess
	s.errorDedupInterval = options.ErrorDedupInterval
	s.clientIPMode = normalizeClientIPMode(options.ClientIPMode)
	s.mediaIdleTimeout = options.MediaIdleTimeout
	return &Application{server: s}
}

func (a *Application) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/diagnostics/", http.StripPrefix("/diagnostics", a.server.diagnosticsHandler()))
	mux.Handle("/", a.server)
	return mux
}

func (a *Application) RunSummaries(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			a.server.logTrafficSummary(now, interval)
		}
	}
}

type requestObservation struct {
	Time                   time.Time
	RequestID              string
	Route                  string
	Method                 string
	TargetHost             string
	Status                 int
	UpstreamStatus         int
	Result                 string
	ErrorStage             string
	Bytes                  int64
	DurationMS             int64
	UpstreamHeaderMS       int64
	UpstreamHeaderObserved bool
	Range                  bool
	StreamResult           string
	QualityParams          string
	ActualQuality          int
	AcceptQualities        string
	VideoQualities         string
	VideoCodecs            string
	MediaGroups            int
	GRPCStatus             string
}

type recentError struct {
	Time           time.Time `json:"time"`
	Route          string    `json:"route"`
	TargetHost     string    `json:"target_host,omitempty"`
	Status         int       `json:"status"`
	UpstreamStatus int       `json:"upstream_status"`
	Result         string    `json:"result"`
	ErrorStage     string    `json:"error_stage,omitempty"`
	GRPCStatus     string    `json:"grpc_status,omitempty"`
}

type hostCounters struct {
	Requests       uint64
	Errors         uint64
	Status403      uint64
	UpstreamStalls uint64
}

type metricBucket struct {
	Second             int64
	Requests           uint64
	Success            uint64
	Failed             uint64
	Cancelled          uint64
	MediaRequests      uint64
	Status403          uint64
	UpstreamStalls     uint64
	CandidateAttempts  uint64
	Fallbacks          uint64
	FallbackRecoveries uint64
	CandidateExhausted uint64
	LivePlaylists      uint64
	PlaylistTrims      uint64
	SegmentsSkipped    uint64
	PlaylistTrimErrors uint64
	Bytes              uint64
	Statuses           [600]uint64
	GRPCStatuses       [grpcStatusSlots]uint64
	Routes             map[string]uint64
	ActualQualities    map[int]uint64
	LatencyBuckets     [len(latencyBoundsMS) + 1]uint64
	Hosts              map[string]hostCounters
}

type metricsStore struct {
	mu           sync.Mutex
	started      time.Time
	buckets      [metricWindowSeconds]metricBucket
	recentErrors []recentError
	active       int64
}

func newMetricsStore(now time.Time) *metricsStore {
	return &metricsStore{
		started:      now,
		recentErrors: make([]recentError, 0, maxRecentErrors),
	}
}

func (m *metricsStore) record(observation requestObservation) {
	observation.TargetHost = diagnosticHost(observation.TargetHost)
	m.mu.Lock()
	defer m.mu.Unlock()
	bucket := m.bucketLocked(observation.Time.Unix())
	bucket.Requests++
	if observation.Route == "media" {
		bucket.MediaRequests++
	}
	bucket.Routes[observation.Route]++
	switch observation.Result {
	case "client_cancelled":
		bucket.Cancelled++
	default:
		if isFailedResult(observation.Result) {
			bucket.Failed++
		} else {
			bucket.Success++
		}
	}
	if observation.Status > 0 && observation.Status < len(bucket.Statuses) {
		bucket.Statuses[observation.Status]++
	}
	if observation.Status == http.StatusForbidden || observation.UpstreamStatus == http.StatusForbidden {
		bucket.Status403++
	}
	if observation.Result == "upstream_stall" {
		bucket.UpstreamStalls++
	}
	if observation.Route == "playurl_grpc" && observation.GRPCStatus != "" {
		bucket.GRPCStatuses[grpcStatusIndex(observation.GRPCStatus)]++
	}
	if observation.Route == "playurl" && observation.ActualQuality > 0 {
		bucket.ActualQualities[observation.ActualQuality]++
	}
	if observation.UpstreamHeaderObserved {
		bucket.LatencyBuckets[latencyBucketIndex(observation.UpstreamHeaderMS)]++
	}
	if observation.TargetHost != "" {
		host := observation.TargetHost
		if _, exists := bucket.Hosts[host]; !exists && host != overflowHost && len(bucket.Hosts) >= maxHostsPerBucket-1 {
			host = overflowHost
		}
		counters := bucket.Hosts[host]
		counters.Requests++
		if isFailedResult(observation.Result) {
			counters.Errors++
		}
		if observation.Status == http.StatusForbidden || observation.UpstreamStatus == http.StatusForbidden {
			counters.Status403++
		}
		if observation.Result == "upstream_stall" {
			counters.UpstreamStalls++
		}
		bucket.Hosts[host] = counters
	}
	if isFailedResult(observation.Result) {
		errorItem := recentError{
			Time:           observation.Time,
			Route:          boundedToken(observation.Route, 32),
			TargetHost:     observation.TargetHost,
			Status:         observation.Status,
			UpstreamStatus: observation.UpstreamStatus,
			Result:         boundedToken(observation.Result, 32),
			ErrorStage:     boundedToken(observation.ErrorStage, 32),
			GRPCStatus:     sanitizeGRPCStatus(observation.GRPCStatus),
		}
		if len(m.recentErrors) == maxRecentErrors {
			copy(m.recentErrors, m.recentErrors[1:])
			m.recentErrors[len(m.recentErrors)-1] = errorItem
		} else {
			m.recentErrors = append(m.recentErrors, errorItem)
		}
	}
}

func (m *metricsStore) recordCandidateSelection(now time.Time, attempts int, recovered, exhausted bool) {
	if attempts <= 0 {
		return
	}
	m.mu.Lock()
	bucket := m.bucketLocked(now.Unix())
	bucket.CandidateAttempts += uint64(attempts)
	if attempts > 1 {
		bucket.Fallbacks++
	}
	if recovered {
		bucket.FallbackRecoveries++
	}
	if exhausted {
		bucket.CandidateExhausted++
	}
	m.mu.Unlock()
}

func (m *metricsStore) recordPlaylist(now time.Time, result playlistTrimResult) {
	if !result.Live {
		return
	}
	m.mu.Lock()
	bucket := m.bucketLocked(now.Unix())
	bucket.LivePlaylists++
	if result.Trimmed {
		bucket.PlaylistTrims++
		bucket.SegmentsSkipped += uint64(result.Skipped)
	}
	if result.Malformed {
		bucket.PlaylistTrimErrors++
	}
	m.mu.Unlock()
}

func (m *metricsStore) addMediaBytes(now time.Time, count int64) {
	if count <= 0 {
		return
	}
	m.mu.Lock()
	bucket := m.bucketLocked(now.Unix())
	bucket.Bytes += uint64(count)
	m.mu.Unlock()
}

func (m *metricsStore) bucketLocked(second int64) *metricBucket {
	index := int(second % metricWindowSeconds)
	if index < 0 {
		index += metricWindowSeconds
	}
	bucket := &m.buckets[index]
	if bucket.Second != second {
		*bucket = metricBucket{Second: second, Hosts: make(map[string]hostCounters), Routes: make(map[string]uint64), ActualQualities: make(map[int]uint64)}
	} else {
		if bucket.Hosts == nil {
			bucket.Hosts = make(map[string]hostCounters)
		}
		if bucket.Routes == nil {
			bucket.Routes = make(map[string]uint64)
		}
		if bucket.ActualQualities == nil {
			bucket.ActualQualities = make(map[int]uint64)
		}
	}
	return bucket
}

func (m *metricsStore) streamStarted() {
	m.mu.Lock()
	m.active++
	m.mu.Unlock()
}

func (m *metricsStore) streamFinished() {
	m.mu.Lock()
	if m.active > 0 {
		m.active--
	}
	m.mu.Unlock()
}

func (m *metricsStore) activeStreams() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

type windowSnapshot struct {
	Requests            int            `json:"requests"`
	Success             int            `json:"success"`
	Failed              int            `json:"failed"`
	SuccessRate         float64        `json:"success_rate"`
	Bytes               int64          `json:"bytes"`
	MediaRequests       int            `json:"media_requests"`
	ClientCancelled     int            `json:"client_cancelled"`
	Status403           int            `json:"status_403"`
	UpstreamStalls      int            `json:"upstream_stalls"`
	CandidateAttempts   int            `json:"candidate_attempts"`
	Fallbacks           int            `json:"fallbacks"`
	FallbackRecoveries  int            `json:"fallback_recoveries"`
	CandidateExhausted  int            `json:"candidate_exhausted"`
	LivePlaylists       int            `json:"live_playlists"`
	PlaylistTrims       int            `json:"playlist_trims"`
	SegmentsSkipped     int            `json:"segments_skipped"`
	PlaylistTrimErrors  int            `json:"playlist_trim_errors"`
	ActualQualities     map[string]int `json:"actual_qualities"`
	Routes              map[string]int `json:"routes"`
	Statuses            map[string]int `json:"statuses"`
	UpstreamHeaderP50MS int64          `json:"upstream_header_p50_ms"`
	UpstreamHeaderP95MS int64          `json:"upstream_header_p95_ms"`
}

type hostSnapshot struct {
	Host           string  `json:"host"`
	Requests       int     `json:"requests"`
	Errors         int     `json:"errors"`
	Status403      int     `json:"status_403"`
	UpstreamStalls int     `json:"upstream_stalls"`
	ErrorRate      float64 `json:"error_rate"`
}

type diagnosticsSnapshot struct {
	GeneratedAt             time.Time                 `json:"generated_at"`
	UptimeSeconds           int64                     `json:"uptime_seconds"`
	ActiveStreams           int64                     `json:"active_streams"`
	ThroughputBPS           int64                     `json:"throughput_bps"`
	MetricsRetentionSeconds int                       `json:"metrics_retention_seconds"`
	Windows                 map[string]windowSnapshot `json:"windows"`
	Hosts                   []hostSnapshot            `json:"hosts"`
	GRPCStatuses            map[string]int            `json:"grpc_statuses"`
	RecentErrors            []recentError             `json:"recent_errors"`
}

type metricAggregate struct {
	Requests           uint64
	Success            uint64
	Failed             uint64
	Cancelled          uint64
	MediaRequests      uint64
	Status403          uint64
	UpstreamStalls     uint64
	CandidateAttempts  uint64
	Fallbacks          uint64
	FallbackRecoveries uint64
	CandidateExhausted uint64
	LivePlaylists      uint64
	PlaylistTrims      uint64
	SegmentsSkipped    uint64
	PlaylistTrimErrors uint64
	Bytes              uint64
	Statuses           [600]uint64
	GRPCStatuses       [grpcStatusSlots]uint64
	Routes             map[string]uint64
	ActualQualities    map[int]uint64
	LatencyBuckets     [len(latencyBoundsMS) + 1]uint64
	Hosts              map[string]hostCounters
}

func (m *metricsStore) snapshot(now time.Time) diagnosticsSnapshot {
	m.mu.Lock()
	started := m.started
	active := m.active
	recentErrors := make([]recentError, len(m.recentErrors))
	copy(recentErrors, m.recentErrors)
	oneMinute := m.aggregateLocked(now, 60, false)
	fiveMinutes := m.aggregateLocked(now, 5*60, false)
	fifteenMinutes := m.aggregateLocked(now, metricWindowSeconds, true)
	throughputBytes := m.bytesLocked(now, 5)
	m.mu.Unlock()

	return diagnosticsSnapshot{
		GeneratedAt:             now.UTC(),
		UptimeSeconds:           max(now.Sub(started).Milliseconds()/1000, 0),
		ActiveStreams:           active,
		ThroughputBPS:           int64(throughputBytes * 8 / 5),
		MetricsRetentionSeconds: metricWindowSeconds,
		Windows: map[string]windowSnapshot{
			"1m":  windowFromAggregate(oneMinute),
			"5m":  windowFromAggregate(fiveMinutes),
			"15m": windowFromAggregate(fifteenMinutes),
		},
		Hosts:        hostsFromAggregate(fifteenMinutes.Hosts),
		GRPCStatuses: grpcStatusesFromAggregate(fifteenMinutes.GRPCStatuses),
		RecentErrors: recentErrors,
	}
}

func (m *metricsStore) window(now time.Time, duration time.Duration) windowSnapshot {
	seconds := int(duration / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	if seconds > metricWindowSeconds {
		seconds = metricWindowSeconds
	}
	m.mu.Lock()
	aggregate := m.aggregateLocked(now, seconds, false)
	m.mu.Unlock()
	return windowFromAggregate(aggregate)
}

func (m *metricsStore) aggregateLocked(now time.Time, seconds int, includeHosts bool) metricAggregate {
	aggregate := metricAggregate{Routes: make(map[string]uint64), ActualQualities: make(map[int]uint64)}
	if includeHosts {
		aggregate.Hosts = make(map[string]hostCounters)
	}
	cutoff := now.Unix() - int64(seconds) + 1
	for index := range m.buckets {
		bucket := &m.buckets[index]
		if bucket.Second < cutoff || bucket.Second > now.Unix() {
			continue
		}
		aggregate.Requests += bucket.Requests
		aggregate.Success += bucket.Success
		aggregate.Failed += bucket.Failed
		aggregate.Cancelled += bucket.Cancelled
		aggregate.MediaRequests += bucket.MediaRequests
		aggregate.Status403 += bucket.Status403
		aggregate.UpstreamStalls += bucket.UpstreamStalls
		aggregate.CandidateAttempts += bucket.CandidateAttempts
		aggregate.Fallbacks += bucket.Fallbacks
		aggregate.FallbackRecoveries += bucket.FallbackRecoveries
		aggregate.CandidateExhausted += bucket.CandidateExhausted
		aggregate.LivePlaylists += bucket.LivePlaylists
		aggregate.PlaylistTrims += bucket.PlaylistTrims
		aggregate.SegmentsSkipped += bucket.SegmentsSkipped
		aggregate.PlaylistTrimErrors += bucket.PlaylistTrimErrors
		aggregate.Bytes += bucket.Bytes
		for status, count := range bucket.Statuses {
			aggregate.Statuses[status] += count
		}
		for status, count := range bucket.GRPCStatuses {
			aggregate.GRPCStatuses[status] += count
		}
		for route, count := range bucket.Routes {
			aggregate.Routes[route] += count
		}
		for quality, count := range bucket.ActualQualities {
			aggregate.ActualQualities[quality] += count
		}
		for latency, count := range bucket.LatencyBuckets {
			aggregate.LatencyBuckets[latency] += count
		}
		if includeHosts {
			for host, counters := range bucket.Hosts {
				aggregateHost(aggregate.Hosts, host, counters)
			}
		}
	}
	return aggregate
}

func aggregateHost(hosts map[string]hostCounters, host string, counters hostCounters) {
	if _, exists := hosts[host]; !exists && host != overflowHost && len(hosts) >= maxAggregatedHosts-1 {
		host = overflowHost
	}
	combined := hosts[host]
	combined.Requests += counters.Requests
	combined.Errors += counters.Errors
	combined.Status403 += counters.Status403
	combined.UpstreamStalls += counters.UpstreamStalls
	hosts[host] = combined
}

func windowFromAggregate(aggregate metricAggregate) windowSnapshot {
	window := windowSnapshot{
		Requests:           int(aggregate.Requests),
		Success:            int(aggregate.Success),
		Failed:             int(aggregate.Failed),
		Bytes:              int64(aggregate.Bytes),
		MediaRequests:      int(aggregate.MediaRequests),
		ClientCancelled:    int(aggregate.Cancelled),
		Status403:          int(aggregate.Status403),
		UpstreamStalls:     int(aggregate.UpstreamStalls),
		CandidateAttempts:  int(aggregate.CandidateAttempts),
		Fallbacks:          int(aggregate.Fallbacks),
		FallbackRecoveries: int(aggregate.FallbackRecoveries),
		CandidateExhausted: int(aggregate.CandidateExhausted),
		LivePlaylists:      int(aggregate.LivePlaylists),
		PlaylistTrims:      int(aggregate.PlaylistTrims),
		SegmentsSkipped:    int(aggregate.SegmentsSkipped),
		PlaylistTrimErrors: int(aggregate.PlaylistTrimErrors),
		ActualQualities:    make(map[string]int),
		Routes:             make(map[string]int),
		Statuses:           make(map[string]int),
	}
	denominator := aggregate.Success + aggregate.Failed
	if denominator > 0 {
		window.SuccessRate = float64(aggregate.Success) * 100 / float64(denominator)
	}
	for quality, count := range aggregate.ActualQualities {
		window.ActualQualities[strconv.Itoa(quality)] = int(count)
	}
	for route, count := range aggregate.Routes {
		window.Routes[route] = int(count)
	}
	for status, count := range aggregate.Statuses {
		if count > 0 {
			window.Statuses[strconv.Itoa(status)] = int(count)
		}
	}
	window.UpstreamHeaderP50MS = histogramPercentile(aggregate.LatencyBuckets, 50)
	window.UpstreamHeaderP95MS = histogramPercentile(aggregate.LatencyBuckets, 95)
	return window
}

func hostsFromAggregate(byHost map[string]hostCounters) []hostSnapshot {
	hosts := make([]hostSnapshot, 0, len(byHost))
	for host, counters := range byHost {
		item := hostSnapshot{Host: host, Requests: int(counters.Requests), Errors: int(counters.Errors), Status403: int(counters.Status403), UpstreamStalls: int(counters.UpstreamStalls)}
		if counters.Requests > 0 {
			item.ErrorRate = float64(counters.Errors) * 100 / float64(counters.Requests)
		}
		hosts = append(hosts, item)
	}
	sort.Slice(hosts, func(i, j int) bool {
		if hosts[i].UpstreamStalls != hosts[j].UpstreamStalls {
			return hosts[i].UpstreamStalls > hosts[j].UpstreamStalls
		}
		if hosts[i].Errors != hosts[j].Errors {
			return hosts[i].Errors > hosts[j].Errors
		}
		if hosts[i].Requests != hosts[j].Requests {
			return hosts[i].Requests > hosts[j].Requests
		}
		return hosts[i].Host < hosts[j].Host
	})
	if len(hosts) > 20 {
		hosts = hosts[:20]
	}
	return hosts
}

func grpcStatusesFromAggregate(counts [grpcStatusSlots]uint64) map[string]int {
	statuses := make(map[string]int)
	for index, count := range counts {
		if count == 0 {
			continue
		}
		statuses[grpcStatusLabel(index)] = int(count)
	}
	return statuses
}

func latencyBucketIndex(milliseconds int64) int {
	for index, bound := range latencyBoundsMS {
		if milliseconds <= bound {
			return index
		}
	}
	return len(latencyBoundsMS)
}

func histogramPercentile(counts [len(latencyBoundsMS) + 1]uint64, percent int) int64 {
	var total uint64
	for _, count := range counts {
		total += count
	}
	if total == 0 {
		return 0
	}
	rank := (total*uint64(percent) + 99) / 100
	var cumulative uint64
	for index, count := range counts {
		cumulative += count
		if cumulative >= rank {
			if index < len(latencyBoundsMS) {
				return latencyBoundsMS[index]
			}
			return latencyBoundsMS[len(latencyBoundsMS)-1]
		}
	}
	return latencyBoundsMS[len(latencyBoundsMS)-1]
}

func grpcStatusIndex(value string) int {
	status, err := strconv.Atoi(value)
	if err == nil && status >= 0 && status <= 16 {
		return status
	}
	return grpcStatusSlots - 1
}

func grpcStatusLabel(index int) string {
	if index >= 0 && index <= 16 {
		return strconv.Itoa(index)
	}
	return "other"
}

func (m *metricsStore) bytesLocked(now time.Time, seconds int) uint64 {
	cutoff := now.Unix() - int64(seconds) + 1
	var bytes uint64
	for index := range m.buckets {
		bucket := &m.buckets[index]
		if bucket.Second >= cutoff && bucket.Second <= now.Unix() {
			bytes += bucket.Bytes
		}
	}
	return bytes
}

type dedupState struct {
	last       time.Time
	suppressed int
}

func (s *server) completeRequest(r *http.Request, writer *loggingResponseWriter, meta *requestLog) {
	if meta.route == "health" || r.Method == http.MethodOptions {
		return
	}
	streamResult := meta.streamResult
	if streamResult == "" {
		streamResult = "not_started"
	}
	status := writer.status
	if status == 0 && streamResult != "client_cancelled" {
		status = http.StatusOK
	}
	qualityParams := meta.qualityParams
	if meta.route == "playurl" && qualityParams == "" {
		qualityParams = "not_attempted"
	}
	observation := requestObservation{
		Time:                   s.now(),
		RequestID:              meta.requestID,
		Route:                  meta.route,
		Method:                 r.Method,
		TargetHost:             diagnosticHost(meta.targetHost),
		Status:                 status,
		UpstreamStatus:         meta.upstreamStatus,
		ErrorStage:             meta.errorStage,
		Bytes:                  writer.bytes,
		DurationMS:             max(s.now().Sub(meta.started).Milliseconds(), 0),
		UpstreamHeaderMS:       meta.upstreamHeaderMS,
		UpstreamHeaderObserved: meta.upstreamHeaderObserved,
		Range:                  r.Header.Get("Range") != "",
		StreamResult:           streamResult,
		QualityParams:          qualityParams,
		ActualQuality:          meta.actualQuality,
		AcceptQualities:        boundedToken(meta.acceptQualities, 128),
		VideoQualities:         boundedToken(meta.videoQualities, 128),
		VideoCodecs:            boundedToken(meta.videoCodecs, 128),
		MediaGroups:            meta.mediaGroups,
		GRPCStatus:             sanitizeGRPCStatus(meta.grpcStatus),
	}
	observation.Result = classifyResult(observation)
	if observation.UpstreamStatus >= http.StatusBadRequest {
		observation.ErrorStage = "upstream_response"
	} else if observation.Result == "grpc_error" {
		observation.ErrorStage = "grpc_status"
	}
	s.metrics.record(observation)
	s.logRequest(observation, clientIPForLog(r, s.clientIPMode))
}

func classifyResult(observation requestObservation) string {
	if observation.GRPCStatus != "" && observation.GRPCStatus != "0" {
		return "grpc_error"
	}
	if observation.UpstreamStatus >= http.StatusBadRequest {
		return "upstream_rejected"
	}
	switch observation.StreamResult {
	case "client_cancelled":
		return "client_cancelled"
	case "error":
		return "stream_error"
	case "upstream_stall":
		return "upstream_stall"
	}
	if observation.Status >= http.StatusInternalServerError {
		return "upstream_error"
	}
	if observation.Status >= http.StatusBadRequest {
		return "client_rejected"
	}
	if observation.QualityParams == "failed" {
		return "degraded"
	}
	return "ok"
}

func isFailedResult(result string) bool {
	switch result {
	case "ok", "degraded", "client_cancelled":
		return false
	default:
		return true
	}
}

func (s *server) logRequest(observation requestObservation, remote string) {
	if observation.Route == "health" || observation.Method == http.MethodOptions || observation.Result == "client_cancelled" {
		return
	}
	failed := isFailedResult(observation.Result)
	if observation.Route == "media" && !failed && !s.logMediaSuccess {
		return
	}

	attributes := []any{
		"event", "request_complete",
		"request_id", observation.RequestID,
		"route", observation.Route,
		"method", observation.Method,
		"target_host", observation.TargetHost,
		"status", observation.Status,
		"upstream_status", observation.UpstreamStatus,
		"result", observation.Result,
		"error_stage", observation.ErrorStage,
		"bytes", observation.Bytes,
		"duration_ms", observation.DurationMS,
	}
	if observation.UpstreamHeaderObserved {
		attributes = append(attributes, "upstream_header_ms", observation.UpstreamHeaderMS)
	}
	if observation.Route == "media" {
		attributes = append(attributes, "range", observation.Range, "stream_result", observation.StreamResult)
	}
	if observation.Route == "playurl" {
		attributes = append(attributes, "quality_params", observation.QualityParams)
		if observation.ActualQuality > 0 {
			attributes = append(attributes, "actual_quality", observation.ActualQuality)
		}
		if observation.AcceptQualities != "" {
			attributes = append(attributes, "accept_quality", observation.AcceptQualities)
		}
		if observation.VideoQualities != "" {
			attributes = append(attributes, "video_qualities", observation.VideoQualities)
		}
		if observation.VideoCodecs != "" {
			attributes = append(attributes, "video_codecs", observation.VideoCodecs)
		}
		if observation.MediaGroups > 0 {
			attributes = append(attributes, "media_groups", observation.MediaGroups)
		}
	}
	if observation.Route == "playurl_grpc" {
		attributes = append(attributes, "grpc_status", valueOrUnknown(observation.GRPCStatus))
	}
	if remote != "" {
		attributes = append(attributes, "remote", remote)
	}

	if failed {
		emit, suppressed := s.shouldEmitFailure(observation)
		if !emit {
			return
		}
		if suppressed > 0 {
			attributes = append(attributes, "repeats_suppressed", suppressed)
		}
		s.logger.Warn("request complete", attributes...)
		return
	}
	if observation.Result == "degraded" {
		s.logger.Warn("request complete", attributes...)
		return
	}
	s.logger.Info("request complete", attributes...)
}

func (s *server) shouldEmitFailure(observation requestObservation) (bool, int) {
	key := failureDedupKey(observation)
	now := observation.Time
	s.dedupMu.Lock()
	defer s.dedupMu.Unlock()
	state, exists := s.dedup[key]
	if !exists {
		if len(s.dedup) >= maxDedupKeys {
			var oldestKey string
			var oldest time.Time
			for candidate, candidateState := range s.dedup {
				if oldestKey == "" || candidateState.last.Before(oldest) {
					oldestKey = candidate
					oldest = candidateState.last
				}
			}
			delete(s.dedup, oldestKey)
		}
		s.dedup[key] = &dedupState{last: now}
		return true, 0
	}
	if s.errorDedupInterval <= 0 || now.Sub(state.last) >= s.errorDedupInterval {
		suppressed := state.suppressed
		state.last = now
		state.suppressed = 0
		return true, suppressed
	}
	state.suppressed++
	return false, 0
}

func failureDedupKey(observation requestObservation) string {
	key := strings.Join([]string{
		boundedToken(observation.Route, 32),
		diagnosticHost(observation.TargetHost),
		strconv.Itoa(observation.Status),
		strconv.Itoa(observation.UpstreamStatus),
		boundedToken(observation.Result, 32),
		boundedToken(observation.ErrorStage, 32),
		sanitizeGRPCStatus(observation.GRPCStatus),
	}, "|")
	if len(key) <= maxDedupKeyBytes {
		return key
	}
	hash := sha256.Sum256([]byte(key))
	return fmt.Sprintf("sha256:%x", hash)
}

func (s *server) logTrafficSummary(now time.Time, interval time.Duration) {
	windowDuration := interval
	if windowDuration > metricWindowSeconds*time.Second {
		windowDuration = metricWindowSeconds * time.Second
	}
	window := s.metrics.window(now, windowDuration)
	if window.Requests == 0 {
		return
	}
	s.logger.Info("traffic summary",
		"event", "traffic_summary",
		"window", windowDuration.String(),
		"requests", window.Requests,
		"success", window.Success,
		"failed", window.Failed,
		"media_requests", window.MediaRequests,
		"bytes", window.Bytes,
		"active_streams", s.metrics.activeStreams(),
		"client_cancelled", window.ClientCancelled,
		"status_403", window.Status403,
		"upstream_stalls", window.UpstreamStalls,
		"fallback_recoveries", window.FallbackRecoveries,
		"candidate_exhausted", window.CandidateExhausted,
		"upstream_header_p95_ms", window.UpstreamHeaderP95MS,
	)
}

func normalizeClientIPMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "full", "off":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "masked"
	}
}

func clientIPForLog(r *http.Request, mode string) string {
	if mode == "off" {
		return ""
	}
	ip := net.ParseIP(clientIP(r))
	if ip == nil {
		return "unknown"
	}
	if mode == "full" {
		return ip.String()
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return fmt.Sprintf("%d.%d.%d.x", ipv4[0], ipv4[1], ipv4[2])
	}
	masked := ip.Mask(net.CIDRMask(64, 128))
	return masked.String() + "/64"
}

func valueOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func sanitizeGRPCStatus(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	status, err := strconv.Atoi(value)
	if err != nil || status < 0 || status > 999 {
		return "unknown"
	}
	return strconv.Itoa(status)
}

func boundedToken(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}

func nextRequestID(sequence *atomic.Uint64) string {
	return fmt.Sprintf("%08x", sequence.Add(1))
}
