package proxy

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	videoHighestQuality = "127"
	videoFunctionValue  = "4048"
	liveHighestQuality  = "10000"
	legacyLiveQuality   = "4"
	qualityUserAgent    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_7_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Safari/605.1.15"
	maxNavResponseSize  = 1 << 20
	wbiKeyTTL           = time.Hour
)

var mixinKeyEncTable = [...]int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35,
	27, 43, 5, 49, 33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13,
	37, 48, 7, 16, 24, 55, 40, 61, 26, 17, 0, 1, 60, 51, 30, 4,
	22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11, 36, 20, 34, 44, 52,
}

func (s *server) highestQualityTarget(ctx context.Context, target *url.URL, headers http.Header) (*url.URL, bool, error) {
	upgraded := *target
	query := upgraded.Query()

	switch upgraded.Path {
	case "/x/player/playurl", "/x/player/wbi/playurl", "/pgc/player/web/playurl", "/pgc/player/web/v2/playurl":
		query.Set("qn", videoHighestQuality)
		query.Set("fnver", "0")
		query.Set("fnval", videoFunctionValue)
		query.Set("fourk", "1")
		if strings.Contains(upgraded.Path, "/wbi/") || query.Get("w_rid") != "" {
			mixinKey, err := s.getWBIKey(ctx, headers)
			if err != nil {
				return target, false, err
			}
			query = signWBI(query, mixinKey, s.now().Unix())
		}
	case "/xlive/web-room/v2/index/getRoomPlayInfo":
		query.Set("qn", liveHighestQuality)
	case "/room/v1/Room/playUrl":
		query.Set("quality", legacyLiveQuality)
	default:
		return target, false, nil
	}

	upgraded.RawQuery = encodeQuery(query)
	return &upgraded, true, nil
}

func (s *server) getWBIKey(ctx context.Context, headers http.Header) (string, error) {
	for {
		s.wbiMu.Lock()
		if s.wbiMixinKey != "" && s.now().Before(s.wbiKeyExpires) {
			key := s.wbiMixinKey
			s.wbiMu.Unlock()
			return key, nil
		}
		if s.wbiRefresh != nil {
			refresh := s.wbiRefresh
			s.wbiMu.Unlock()
			select {
			case <-refresh:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		refresh := make(chan struct{})
		s.wbiRefresh = refresh
		s.wbiMu.Unlock()

		key, err := s.fetchWBIKey(ctx, headers)
		s.wbiMu.Lock()
		if err == nil {
			s.wbiMixinKey = key
			s.wbiKeyExpires = s.now().Add(wbiKeyTTL)
		}
		close(refresh)
		s.wbiRefresh = nil
		s.wbiMu.Unlock()
		return key, err
	}
}

func (s *server) fetchWBIKey(ctx context.Context, headers http.Header) (string, error) {
	navURL := &url.URL{Scheme: "https", Host: "api.bilibili.com", Path: "/x/web-interface/nav"}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, navURL.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header = copyRequestHeaders(headers, []string{"Accept", "Accept-Language", "Cookie", "Referer", "User-Agent"})
	response, err := s.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", errors.New("WBI key request failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxNavResponseSize+1))
	if err != nil || len(body) > maxNavResponseSize {
		return "", errors.New("invalid WBI key response")
	}
	var nav struct {
		Data struct {
			WBIImage struct {
				ImageURL string `json:"img_url"`
				SubURL   string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &nav); err != nil {
		return "", errors.New("invalid WBI key response")
	}
	imageKey := keyFromURL(nav.Data.WBIImage.ImageURL)
	subKey := keyFromURL(nav.Data.WBIImage.SubURL)
	if !validWBIKey(imageKey) || !validWBIKey(subKey) {
		return "", errors.New("invalid WBI keys")
	}
	return makeMixinKey(imageKey + subKey)
}

func keyFromURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	name := path.Base(parsed.Path)
	return strings.TrimSuffix(name, path.Ext(name))
}

func validWBIKey(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return false
		}
	}
	return true
}

func makeMixinKey(raw string) (string, error) {
	if len(raw) != len(mixinKeyEncTable) {
		return "", errors.New("invalid WBI keys")
	}
	var key strings.Builder
	key.Grow(32)
	for _, index := range mixinKeyEncTable[:32] {
		key.WriteByte(raw[index])
	}
	return key.String(), nil
}

func signWBI(values url.Values, mixinKey string, timestamp int64) url.Values {
	signed := cloneValues(values)
	signed.Del("w_rid")
	signed.Set("wts", strconv.FormatInt(timestamp, 10))
	for name, entries := range signed {
		for index, value := range entries {
			entries[index] = strings.Map(func(char rune) rune {
				if strings.ContainsRune("!'()*", char) {
					return -1
				}
				return char
			}, value)
		}
		signed[name] = entries
	}
	hash := md5.Sum([]byte(encodeQuery(signed) + mixinKey))
	signed.Set("w_rid", hex.EncodeToString(hash[:]))
	return signed
}

func cloneValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for name, entries := range values {
		cloned[name] = append([]string(nil), entries...)
	}
	return cloned
}

func encodeQuery(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var query strings.Builder
	first := true
	for _, key := range keys {
		entries := values[key]
		if len(entries) == 0 {
			entries = []string{""}
		}
		for _, value := range entries {
			if !first {
				query.WriteByte('&')
			}
			first = false
			query.WriteString(escapeQueryComponent(key))
			query.WriteByte('=')
			query.WriteString(escapeQueryComponent(value))
		}
	}
	return query.String()
}

func escapeQueryComponent(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}
