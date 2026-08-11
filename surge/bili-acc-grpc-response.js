(() => {
  "use strict";

  const AKAMAI_SUFFIX = "akamaized.net";
  const BILI_MEDIA_SUFFIXES = ["bilivideo.com", "bilivideo.cn", "biliapi.net"];
  const SCHEMAS = {
    PlayViewUniteReply: {1: "VodInfo", 10: "FragmentVideo"},
    VodInfo: {5: "Stream"},
    Stream: {2: "DashVideo", 3: "SegmentVideo"},
    SegmentVideo: {1: "ResponseUrl"},
    FragmentVideo: {1: "FragmentVideoInfo"},
    FragmentVideoInfo: {2: "VodInfo"},
  };
  const URL_LAYOUTS = {
    DashVideo: {primary: 1, backup: 2},
    ResponseUrl: {primary: 4, backup: 5},
  };

  const body = $response.body;
  if (!(body instanceof Uint8Array) || body.length === 0) {
    log("skip reason=empty_binary_body");
    $done({});
    return;
  }

  const contentType = getHeader($response.headers || {}, "content-type").split(";", 1)[0].toLowerCase();
  if (contentType !== "application/grpc" && contentType !== "application/grpc+proto") {
    log(`skip reason=unsupported_content_type type=${contentType || "unknown"}`);
    $done({});
    return;
  }

  const result = transformGRPC(body);
  if (!result) {
    log("skip reason=invalid_grpc");
    $done({});
    return;
  }
  if (result.compressed) {
    const encoding = getHeader($response.headers || {}, "grpc-encoding") || "unknown";
    log(`skip reason=compressed_frame encoding=${safeToken(encoding)}`);
    $done({});
    return;
  }

  const endpoint = safeEndpoint(String($request.url || ""));
  log(`rewrite endpoint=${endpoint} frames=${result.frames} akamai_urls=${result.rewritten}`);
  if (result.rewritten === 0) {
    $done({});
    return;
  }

  const headers = {...($response.headers || {})};
  for (const name of ["content-length", "content-md5", "digest", "etag"]) deleteHeader(headers, name);
  $done({body: result.bytes, headers});

  function transformGRPC(bytes) {
    const chunks = [];
    let offset = 0;
    let rewritten = 0;
    let frames = 0;
    while (offset < bytes.length) {
      if (offset + 5 > bytes.length) return null;
      const flags = bytes[offset];
      const length = ((bytes[offset + 1] << 24) | (bytes[offset + 2] << 16) | (bytes[offset + 3] << 8) | bytes[offset + 4]) >>> 0;
      const end = offset + 5 + length;
      if (end > bytes.length) return null;
      if ((flags & 1) !== 0) return {bytes, rewritten: 0, frames: frames + 1, compressed: true};

      const transformed = transformMessage(bytes.slice(offset + 5, end), "PlayViewUniteReply");
      if (!transformed.valid) return null;
      rewritten += transformed.rewritten;
      frames++;
      const header = new Uint8Array(5);
      header[0] = flags;
      const transformedLength = transformed.bytes.length;
      header[1] = (transformedLength >>> 24) & 255;
      header[2] = (transformedLength >>> 16) & 255;
      header[3] = (transformedLength >>> 8) & 255;
      header[4] = transformedLength & 255;
      chunks.push(header, transformed.bytes);
      offset = end;
    }
    return {bytes: concat(chunks), rewritten, frames, compressed: false};
  }

  function transformMessage(bytes, type) {
    const fields = parseFields(bytes);
    if (!fields) return {valid: false, bytes, rewritten: 0};
    let rewritten = 0;
    let changed = false;
    const children = SCHEMAS[type] || {};

    for (const field of fields) {
      const childType = children[field.number];
      if (!childType || field.wireType !== 2) continue;
      const nested = transformMessage(field.payload, childType);
      if (!nested.valid) return {valid: false, bytes, rewritten: 0};
      if (nested.rewritten > 0) {
        field.payload = nested.bytes;
        field.changed = true;
        changed = true;
        rewritten += nested.rewritten;
      }
    }

    const layout = URL_LAYOUTS[type];
    if (layout) {
      const fallbackURLs = fields
        .filter((field) => field.number === layout.backup && field.wireType === 2)
        .map((field) => asciiURL(field.payload))
        .filter((url) => isBiliMediaURL(url));
      const fallback = fallbackURLs[fallbackURLs.length - 1];
      if (fallback) {
        for (const field of fields) {
          if (field.number !== layout.primary || field.wireType !== 2 || !isAkamaiURL(asciiURL(field.payload))) continue;
          field.payload = asciiBytes(fallback);
          field.changed = true;
          changed = true;
          rewritten++;
        }
      }
    }

    if (!changed) return {valid: true, bytes, rewritten: 0};
    return {
      valid: true,
      bytes: concat(fields.map((field) => field.changed
        ? concat([encodeVarint((field.number * 8) + field.wireType), encodeVarint(field.payload.length), field.payload])
        : field.raw)),
      rewritten,
    };
  }

  function parseFields(bytes) {
    const fields = [];
    let offset = 0;
    while (offset < bytes.length) {
      const start = offset;
      const tag = readVarint(bytes, offset);
      if (!tag || tag.value === 0) return null;
      offset = tag.end;
      const number = Math.floor(tag.value / 8);
      const wireType = tag.value & 7;
      if (number === 0 || wireType === 4) return null;

      if (wireType === 2) {
        const length = readVarint(bytes, offset);
        if (!length || length.value > bytes.length - length.end) return null;
        const end = length.end + length.value;
        fields.push({number, wireType, payload: bytes.slice(length.end, end), raw: bytes.slice(start, end), changed: false});
        offset = end;
      } else {
        const end = skipValue(bytes, offset, wireType, number);
        if (end < 0) return null;
        fields.push({number, wireType, raw: bytes.slice(start, end), changed: false});
        offset = end;
      }
    }
    return fields;
  }

  function skipValue(bytes, offset, wireType, fieldNumber) {
    if (wireType === 0) {
      const value = readVarint(bytes, offset);
      return value ? value.end : -1;
    }
    if (wireType === 1) return offset + 8 <= bytes.length ? offset + 8 : -1;
    if (wireType === 5) return offset + 4 <= bytes.length ? offset + 4 : -1;
    if (wireType !== 3) return -1;

    let cursor = offset;
    while (cursor < bytes.length) {
      const tag = readVarint(bytes, cursor);
      if (!tag || tag.value === 0) return -1;
      cursor = tag.end;
      const nestedNumber = Math.floor(tag.value / 8);
      const nestedWireType = tag.value & 7;
      if (nestedWireType === 4) return nestedNumber === fieldNumber ? cursor : -1;
      if (nestedWireType === 2) {
        const length = readVarint(bytes, cursor);
        if (!length || length.value > bytes.length - length.end) return -1;
        cursor = length.end + length.value;
      } else {
        cursor = skipValue(bytes, cursor, nestedWireType, nestedNumber);
        if (cursor < 0) return -1;
      }
    }
    return -1;
  }

  function readVarint(bytes, start) {
    let value = 0;
    let shift = 0;
    for (let offset = start; offset < bytes.length && offset < start + 10; offset++) {
      const byte = bytes[offset];
      if (shift < 53) value += (byte & 127) * (2 ** shift);
      if ((byte & 128) === 0) return {value, end: offset + 1};
      shift += 7;
    }
    return null;
  }

  function encodeVarint(value) {
    const output = [];
    let remaining = value;
    do {
      let byte = remaining % 128;
      remaining = Math.floor(remaining / 128);
      if (remaining > 0) byte |= 128;
      output.push(byte);
    } while (remaining > 0);
    return Uint8Array.from(output);
  }

  function asciiURL(bytes) {
    if (!bytes || bytes.length < 8) return "";
    let value = "";
    for (const byte of bytes) {
      if (byte < 32 || byte > 126) return "";
      value += String.fromCharCode(byte);
    }
    return /^https?:\/\//i.test(value) ? value : "";
  }

  function asciiBytes(value) {
    return Uint8Array.from(String(value), (character) => character.charCodeAt(0));
  }

  function hostnameOf(value) {
    const match = String(value || "").match(/^https?:\/\/([^/:?#]+)/i);
    return match ? match[1].toLowerCase() : "";
  }

  function isAkamaiURL(value) {
    const hostname = hostnameOf(value);
    return hostname === AKAMAI_SUFFIX || hostname.endsWith(`.${AKAMAI_SUFFIX}`);
  }

  function isBiliMediaURL(value) {
    const hostname = hostnameOf(value);
    return BILI_MEDIA_SUFFIXES.some((suffix) => hostname === suffix || hostname.endsWith(`.${suffix}`));
  }

  function concat(chunks) {
    const length = chunks.reduce((total, chunk) => total + chunk.length, 0);
    const output = new Uint8Array(length);
    let offset = 0;
    for (const chunk of chunks) {
      output.set(chunk, offset);
      offset += chunk.length;
    }
    return output;
  }

  function safeEndpoint(value) {
    const match = String(value).match(/^https?:\/\/[^/]+(\/[^?#]*)/i);
    return match ? match[1] : "unknown";
  }

  function safeToken(value) {
    return /^[A-Za-z0-9._-]{1,32}$/.test(value) ? value : "unknown";
  }

  function getHeader(headers, name) {
    const key = Object.keys(headers).find((item) => item.toLowerCase() === name);
    return key ? String(headers[key]) : "";
  }

  function deleteHeader(headers, name) {
    for (const key of Object.keys(headers)) {
      if (key.toLowerCase() === name) delete headers[key];
    }
  }

  function log(message) {
    console.log(`[Bili Acc][grpc-response] ${message}`);
  }
})();
