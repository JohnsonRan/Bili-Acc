(() => {
  "use strict";

  const argument = String($argument || "");
  const separator = argument.indexOf("|");
  const configuredServer = separator >= 0 ? argument.slice(0, separator) : "";
  const token = separator >= 0 ? argument.slice(separator + 1) : "";
  const server = configuredServer.replace(/\/$/, "");
  const MEDIA_HOSTS = ["bilivideo.com", "bilivideo.cn", "biliapi.net", "akamaized.net"];
  const URL_KEYS = new Set(["baseUrl", "base_url", "backupUrl", "backup_url", "url", "playurl"]);
  if (!/^https:\/\/[^/]+$/i.test(server) || !token || !$response.body) {
    $done({});
    return;
  }
  const requestURL = String($request.url || "");
  const originalAPI = /^https:\/\/api(?:\.live)?\.bilibili\.com\/(?:x\/player\/(?:wbi\/)?playurl|pgc\/player\/web(?:\/v2)?\/playurl|xlive\/web-room\/v2\/index\/getRoomPlayInfo|room\/v1\/Room\/playUrl)(?:\?|$)/i;
  const proxiedPrefix = `${server}/playurl/${encodeURIComponent(token)}/`;
  if (!originalAPI.test(requestURL) && !requestURL.startsWith(proxiedPrefix)) {
    $done({});
    return;
  }

  let payload;
  try {
    payload = JSON.parse($response.body);
  } catch (_) {
    $done({});
    return;
  }

  rewrite(payload);
  const headers = {...$response.headers};
  for (const name of ["content-length", "content-encoding", "content-md5", "digest", "etag", "last-modified"]) {
    deleteHeader(headers, name);
  }
  $done({body: JSON.stringify(payload), headers});

  function rewrite(value) {
    if (!value || typeof value !== "object") return;
    for (const [key, child] of Object.entries(value)) {
      if (URL_KEYS.has(key) && typeof child === "string" && isMediaURL(child)) {
        value[key] = proxyURL(child);
      } else if (URL_KEYS.has(key) && Array.isArray(child)) {
        value[key] = child.map((item) => typeof item === "string" && isMediaURL(item) ? proxyURL(item) : item);
      } else if (key === "host" && typeof child === "string" && isMediaURL(child)) {
        value[key] = proxyURL(child, true);
      } else {
        rewrite(child);
      }
    }
  }

  function isMediaURL(value) {
    const match = String(value).match(/^https?:\/\/([^/:?#]+)(?::\d+)?(?:[/?#]|$)/i);
    if (!match) return false;
    const hostname = match[1].toLowerCase();
    return MEDIA_HOSTS.some((suffix) => hostname === suffix || hostname.endsWith(`.${suffix}`));
  }

  function proxyURL(value, originOnly = false) {
    const match = String(value).match(/^(https?:\/\/[^/]+)(\/.*)?$/i);
    if (!match) return value;
    const origin = match[1];
    const path = originOnly ? "" : match[2] || "/";
    return `${server}/proxy/${encodeURIComponent(token)}/${base64url(origin)}${path}`;
  }

  function deleteHeader(headers, name) {
    for (const key of Object.keys(headers)) {
      if (key.toLowerCase() === name) delete headers[key];
    }
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
})();
