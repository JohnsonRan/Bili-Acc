(() => {
  "use strict";

  const argument = String($argument || "");
  const separator = argument.indexOf("|");
  const configuredServer = separator >= 0 ? argument.slice(0, separator) : "";
  const token = separator >= 0 ? argument.slice(separator + 1) : "";
  const server = configuredServer.replace(/\/$/, "");
  const MEDIA_HOSTS = ["bilivideo.com", "bilivideo.cn", "biliapi.net", "akamaized.net"];
  const URL_KEYS = new Set(["baseUrl", "base_url", "backupUrl", "backup_url", "url", "playurl"]);
  if (!/^https:\/\/[^/]+$/i.test(server)) {
    log("skip reason=invalid_server");
    $done({});
    return;
  }
  if (!token) {
    log("skip reason=missing_token");
    $done({});
    return;
  }
  if (!$response.body) {
    log(`skip reason=empty_body status=${String($response.status || "unknown")}`);
    $done({});
    return;
  }
  const requestURL = String($request.url || "");
  const originalAPI = /^https:\/\/api(?:\.live)?\.bilibili\.com\/(?:x\/player\/(?:wbi\/)?playurl|pgc\/player\/web(?:\/v2)?\/playurl|xlive\/web-room\/v2\/index\/getRoomPlayInfo|room\/v1\/Room\/playUrl)(?:\?|$)/i;
  const proxiedPrefix = `${server}/playurl/${encodeURIComponent(token)}/`;
  const requestKind = originalAPI.test(requestURL) ? "original_api" : requestURL.startsWith(proxiedPrefix) ? "proxied_api" : "unrelated";
  if (requestKind === "unrelated") {
    $done({});
    return;
  }

  let payload;
  try {
    payload = JSON.parse($response.body);
  } catch (error) {
    log(`skip reason=invalid_json status=${String($response.status || "unknown")} source=${requestKind} error=${errorName(error)}`);
    $done({});
    return;
  }

  const groups = collectMediaGroups(payload);
  if (groups.length > 0 && typeof $httpClient !== "undefined" && typeof $httpClient.post === "function") {
    $httpClient.post({url: `${server}/media-groups/${encodeURIComponent(token)}`, headers: {"Content-Type": "application/json"}, body: JSON.stringify({groups: groups.map((group) => ({urls: group.urls}))})}, (error, response, responseBody) => {
      let ids = [];
      if (!error && Number(response && response.status) >= 200 && Number(response && response.status) < 300) {
        try {
          const parsed = JSON.parse(responseBody || "{}");
          if (Array.isArray(parsed.ids) && parsed.ids.length === groups.length) ids = parsed.ids.map(String);
        } catch (_) {}
      }
      finish(ids);
    });
    return;
  }
  finish([]);

  function finish(groupIDs) {
    let rewrittenURLs = 0;
    rewrite(payload, new Map(groups.map((group, index) => [group.owner, {id: groupIDs[index] || "", fields: group.fields}])));
    const headers = {...$response.headers};
    for (const name of ["content-length", "content-encoding", "content-md5", "digest", "etag", "last-modified"]) deleteHeader(headers, name);
    log(`rewrite status=${String($response.status || "unknown")} source=${requestKind} media_urls=${rewrittenURLs} fallback_groups=${groupIDs.length}`);
    $done({body: JSON.stringify(payload), headers});

    function rewrite(value, groupMap) {
      if (!value || typeof value !== "object") return;
      const group = groupMap.get(value);
      const groupedFields = new Set(group ? group.fields : []);
      let candidateIndex = 0;
      for (const [key, child] of Object.entries(value)) {
        if (groupedFields.has(key) && typeof child === "string" && isMediaURL(child)) {
          value[key] = group.id ? proxyGroupURL(group.id, candidateIndex++) : proxyURL(child);
          rewrittenURLs++;
        } else if (groupedFields.has(key) && Array.isArray(child)) {
          value[key] = child.map((item) => {
            if (typeof item !== "string" || !isMediaURL(item)) return item;
            const rewritten = group.id ? proxyGroupURL(group.id, candidateIndex++) : proxyURL(item);
            rewrittenURLs++;
            return rewritten;
          });
        } else if (URL_KEYS.has(key) && typeof child === "string" && isMediaURL(child)) {
          value[key] = proxyURL(child);
          rewrittenURLs++;
        } else if (URL_KEYS.has(key) && Array.isArray(child)) {
          value[key] = child.map((item) => {
            if (typeof item !== "string" || !isMediaURL(item)) return item;
            rewrittenURLs++;
            return proxyURL(item);
          });
        } else if (key === "host" && typeof child === "string" && isMediaURL(child)) {
          value[key] = proxyURL(child, true);
          rewrittenURLs++;
        } else {
          rewrite(child, groupMap);
        }
      }
    }
  }

  function log(message) {
    console.log(`[Bili Acc][response] ${message}`);
  }

  function errorName(error) {
    return error && error.name ? String(error.name) : "Error";
  }

  function collectMediaGroups(value) {
    const groups = [];
    const seen = new WeakSet();
    const visit = (current) => {
      if (!current || typeof current !== "object" || seen.has(current)) return;
      seen.add(current);
      const primaryKey = ["baseUrl", "base_url", "url"].find((key) => typeof current[key] === "string" && isMediaURL(current[key]));
      const backupKey = ["backupUrl", "backup_url"].find((key) => Array.isArray(current[key]) && current[key].some((item) => typeof item === "string" && isMediaURL(item)));
      if (primaryKey || backupKey) {
        const urls = [];
        if (primaryKey) urls.push(current[primaryKey]);
        if (backupKey) urls.push(...current[backupKey].filter((item) => typeof item === "string" && isMediaURL(item)));
        if (urls.length > 1) groups.push({owner: current, fields: [primaryKey, backupKey].filter(Boolean), urls});
      }
      for (const child of Object.values(current)) visit(child);
    };
    visit(value);
    return groups;
  }

  function isMediaURL(value) {
    const match = String(value).match(/^https?:\/\/([^/:?#]+)(?::\d+)?(?:[/?#]|$)/i);
    if (!match) return false;
    const hostname = match[1].toLowerCase();
    return MEDIA_HOSTS.some((suffix) => hostname === suffix || hostname.endsWith(`.${suffix}`));
  }

  function proxyGroupURL(id, preferred) {
    return `${server}/proxy-group/${encodeURIComponent(token)}/${encodeURIComponent(id)}/${preferred}`;
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
