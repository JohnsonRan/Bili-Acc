package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"strings"
)

const playViewUniteAdExtraField = 6

// normalizePlayViewUniteRequest removes the opaque client-generated ad payload while
// preserving every other protobuf field byte-for-byte. Bilibili then rebuilds that
// request context on the VPS-originated RPC instead of receiving the captured client
// context embedded by the app.
func normalizePlayViewUniteRequest(body []byte, grpcEncoding string) ([]byte, bool) {
	if len(body) < 5 {
		return body, false
	}
	length := int(binary.BigEndian.Uint32(body[1:5]))
	if length != len(body)-5 {
		return body, false
	}

	if body[0] != 0 && body[0] != 1 {
		return body, false
	}
	payload := body[5:]
	compressed := body[0] == 1
	if compressed {
		if !strings.EqualFold(strings.TrimSpace(grpcEncoding), "gzip") {
			return body, false
		}
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return body, false
		}
		decoded, err := io.ReadAll(io.LimitReader(reader, maxGRPCRequestSize+1))
		closeErr := reader.Close()
		if err != nil || closeErr != nil || len(decoded) > maxGRPCRequestSize {
			return body, false
		}
		payload = decoded
	}

	normalized, ok := removeProtobufField(payload, playViewUniteAdExtraField)
	if !ok || len(normalized) == len(payload) {
		return body, false
	}
	output := make([]byte, 5+len(normalized))
	binary.BigEndian.PutUint32(output[1:5], uint32(len(normalized)))
	copy(output[5:], normalized)
	return output, true
}
