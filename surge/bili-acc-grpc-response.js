(() => {
  "use strict";

  const argument = String($argument || "");
  const separator = argument.indexOf("|");
  const configuredServer = separator >= 0 ? argument.slice(0, separator) : "";
  const token = separator >= 0 ? argument.slice(separator + 1) : "";
  const server = configuredServer.replace(/\/$/, "");
  const SCHEMAS = {
    PlayViewUniteReply: {1: "VodInfo", 10: "FragmentVideo"},
    VodInfo: {5: "Stream", 6: "DashItem", 7: "DolbyItem", 9: "LossLessItem"},
    Stream: {2: "DashVideo", 3: "SegmentVideo"},
    SegmentVideo: {1: "ResponseUrl"},
    DolbyItem: {2: "DashItem"},
    LossLessItem: {2: "DashItem"},
    FragmentVideo: {1: "FragmentVideoInfo"},
    FragmentVideoInfo: {2: "VodInfo"},
  };
  const URL_LAYOUTS = {
    DashVideo: {primary: 1, backup: 2},
    DashItem: {primary: 2, backup: 3},
    ResponseUrl: {primary: 4, backup: 5},
  };
  const MEDIA_HOSTS = ["bilivideo.com", "bilivideo.cn", "biliapi.net", "akamaized.net"];

  if (!/^https:\/\/[^/]+$/i.test(server) || !token) {
    log("skip reason=invalid_arguments");
    $done({});
    return;
  }

  const responseHeaders = $response.headers || {};
  const rawTunnelStatus = getHeader(responseHeaders, "x-bili-acc-grpc-status").trim();
  const tunneledResponse = hasHeader(responseHeaders, "x-bili-acc-grpc-status") || /\/playurl-grpc\//.test(String($request.url || ""));
  const parsedTunnelStatus = /^\d{1,2}$/.test(rawTunnelStatus) ? Number(rawTunnelStatus) : -1;
  const tunnelStatus = tunneledResponse && parsedTunnelStatus >= 0 && parsedTunnelStatus <= 16 ? String(parsedTunnelStatus) : tunneledResponse ? "2" : "";
  const headers = normalizedResponseHeaders();
  const body = $response.body;
  if (!(body instanceof Uint8Array) || body.length === 0) {
    log("skip reason=empty_binary_body");
    passThrough();
    return;
  }

  const contentType = getHeader($response.headers || {}, "content-type").split(";", 1)[0].toLowerCase();
  if (contentType !== "application/grpc" && contentType !== "application/grpc+proto") {
    log(`skip reason=unsupported_content_type type=${contentType || "unknown"}`);
    passThrough();
    return;
  }

  const encoding = getHeader($response.headers || {}, "grpc-encoding").split(",", 1)[0].trim().toLowerCase();
  const prepared = prepareGRPC(body, encoding);
  if (!prepared) {
    log("skip reason=invalid_grpc");
    passThrough();
    return;
  }
  if (prepared.compressed) {
    log(`skip reason=compressed_frame encoding=${safeToken(encoding || "unknown")}`);
    passThrough();
    return;
  }
  if (prepared.groups.length > 0 && typeof $httpClient !== "undefined" && typeof $httpClient.post === "function") {
    const registrationURL = `${server}/media-groups/${encodeURIComponent(token)}`;
    $httpClient.post({url: registrationURL, headers: {"Content-Type": "application/json"}, body: JSON.stringify({groups: prepared.groups.map((urls) => ({urls}))})}, (error, response, responseBody) => {
      let ids = [];
      if (!error && Number(response && response.status) >= 200 && Number(response && response.status) < 300) {
        try {
          const parsed = JSON.parse(responseBody || "{}");
          if (Array.isArray(parsed.ids) && parsed.ids.length === prepared.groups.length) ids = parsed.ids.map(String);
        } catch (_) {}
      }
      finishGRPC(prepared, ids);
    });
    return;
  }
  finishGRPC(prepared, []);

  function finishGRPC(prepared, groupIDs) {
    const result = transformPreparedGRPC(prepared, groupIDs);
    if (!result) {
      log("skip reason=invalid_grpc");
      passThrough();
      return;
    }
    const endpoint = safeEndpoint(String($request.url || ""));
    log(`rewrite endpoint=${endpoint} frames=${result.frames} media_urls=${result.rewritten} fallback_groups=${groupIDs.length} decompressed_frames=${result.decompressed}`);
    if (result.rewritten === 0 && result.decompressed === 0) {
      $done(tunnelStatus ? {headers} : {});
      return;
    }
    for (const name of ["content-length", "content-encoding", "content-md5", "digest", "etag"]) deleteHeader(headers, name);
    if (result.decompressed > 0) deleteHeader(headers, "grpc-encoding");
    $done({body: result.bytes, headers});
  }

  function passThrough() {
    $done(tunneledResponse ? {headers} : {});
  }

  function normalizedResponseHeaders() {
    const headers = {...($response.headers || {})};
    deleteHeader(headers, "x-bili-acc-grpc-status");
    if (tunneledResponse) {
      deleteHeader(headers, "grpc-status");
      headers["Grpc-Status"] = tunnelStatus;
    }
    return headers;
  }

  function prepareGRPC(bytes, compression) {
    const frames = [];
    const groups = [];
    let offset = 0;
    let decompressed = 0;
    while (offset < bytes.length) {
      if (offset + 5 > bytes.length) return null;
      const flags = bytes[offset];
      const length = ((bytes[offset + 1] << 24) | (bytes[offset + 2] << 16) | (bytes[offset + 3] << 8) | bytes[offset + 4]) >>> 0;
      const end = offset + 5 + length;
      if (end > bytes.length) return null;
      let payload = bytes.slice(offset + 5, end);
      let outputFlags = flags;
      if ((flags & 1) !== 0) {
        if (compression !== "gzip" || typeof $utils === "undefined" || typeof $utils.ungzip !== "function") {
          return {compressed: true, groups: [], frames: [], decompressed: 0};
        }
        try {
          payload = $utils.ungzip(payload);
        } catch (_) {
          return null;
        }
        if (!(payload instanceof Uint8Array)) return null;
        outputFlags = flags & 254;
        decompressed++;
      }
      const collected = collectGroups(payload, "PlayViewUniteReply", groups);
      if (!collected.valid) return null;
      frames.push({flags: outputFlags, payload});
      offset = end;
    }
    return {compressed: false, groups, frames, decompressed};
  }

  function transformPreparedGRPC(prepared, groupIDs) {
    const chunks = [];
    const groupIndex = {value: 0};
    let rewritten = 0;
    for (const frame of prepared.frames) {
      const transformed = transformMessage(frame.payload, "PlayViewUniteReply", groupIDs, groupIndex);
      if (!transformed.valid) return null;
      rewritten += transformed.rewritten;
      const header = new Uint8Array(5);
      header[0] = frame.flags;
      const transformedLength = transformed.bytes.length;
      header[1] = (transformedLength >>> 24) & 255;
      header[2] = (transformedLength >>> 16) & 255;
      header[3] = (transformedLength >>> 8) & 255;
      header[4] = transformedLength & 255;
      chunks.push(header, transformed.bytes);
    }
    return {bytes: concat(chunks), rewritten, decompressed: prepared.decompressed, frames: prepared.frames.length};
  }

  function collectGroups(bytes, type, groups) {
    const fields = parseFields(bytes);
    if (!fields) return {valid: false};
    const children = SCHEMAS[type] || {};
    for (const field of fields) {
      const childType = children[field.number];
      if (childType && field.wireType === 2 && !collectGroups(field.payload, childType, groups).valid) return {valid: false};
    }
    const layout = URL_LAYOUTS[type];
    if (layout) {
      const urls = fields
        .filter((field) => (field.number === layout.primary || field.number === layout.backup) && field.wireType === 2)
        .map((field) => asciiURL(field.payload))
        .filter(isMediaURL);
      if (urls.length > 1) groups.push(urls);
    }
    return {valid: true};
  }

  function transformMessage(bytes, type, groupIDs, groupIndex) {
    const fields = parseFields(bytes);
    if (!fields) return {valid: false, bytes, rewritten: 0};
    let rewritten = 0;
    let changed = false;
    const children = SCHEMAS[type] || {};

    for (const field of fields) {
      const childType = children[field.number];
      if (!childType || field.wireType !== 2) continue;
      const nested = transformMessage(field.payload, childType, groupIDs, groupIndex);
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
      const mediaFields = fields.filter((field) => (field.number === layout.primary || field.number === layout.backup) && field.wireType === 2 && isMediaURL(asciiURL(field.payload)));
      const groupID = mediaFields.length > 1 ? groupIDs[groupIndex.value] || "" : "";
      if (mediaFields.length > 1) groupIndex.value++;
      for (let index = 0; index < mediaFields.length; index++) {
        const field = mediaFields[index];
        const mediaURL = asciiURL(field.payload);
        field.payload = asciiBytes(groupID ? proxyGroupURL(groupID, index) : proxyURL(mediaURL));
        field.changed = true;
        changed = true;
        rewritten++;
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

  function isMediaURL(value) {
    try {
      const parsed = new URL(String(value));
      const host = parsed.hostname.toLowerCase();
      return (parsed.protocol === "http:" || parsed.protocol === "https:")
        && MEDIA_HOSTS.some((suffix) => host === suffix || host.endsWith(`.${suffix}`));
    } catch (_) {
      return false;
    }
  }

  function proxyGroupURL(id, preferred) {
    return `${server}/proxy-group/${encodeURIComponent(token)}/${encodeURIComponent(id)}/${preferred}`;
  }

  function proxyURL(value) {
    const match = String(value).match(/^(https?:\/\/[^/]+)(\/.*)?$/i);
    if (!match) return value;
    return `${server}/proxy/${encodeURIComponent(token)}/${base64url(match[1])}${match[2] || "/"}`;
  }

  function base64url(value) {
    const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let result = "";
    for (let index = 0; index < value.length; index += 3) {
      const first = value.charCodeAt(index);
      const second = index + 1 < value.length ? value.charCodeAt(index + 1) : 0;
      const third = index + 2 < value.length ? value.charCodeAt(index + 2) : 0;
      const block = (first << 16) | (second << 8) | third;
      result += alphabet[(block >> 18) & 63];
      result += alphabet[(block >> 12) & 63];
      result += index + 1 < value.length ? alphabet[(block >> 6) & 63] : "=";
      result += index + 2 < value.length ? alphabet[block & 63] : "=";
    }
    return result.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
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
    const endpoint = "/bilibili.app.playerunite.v1.Player/PlayViewUnite";
    const match = String(value).match(/^https?:\/\/[^/]+(\/[^?#]*)/i);
    if (!match) return "unknown";
    return match[1].endsWith(endpoint) ? endpoint : "unknown";
  }

  function safeToken(value) {
    return /^[A-Za-z0-9._-]{1,32}$/.test(value) ? value : "unknown";
  }

  function hasHeader(headers, name) {
    return Object.keys(headers).some((item) => item.toLowerCase() === name);
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
