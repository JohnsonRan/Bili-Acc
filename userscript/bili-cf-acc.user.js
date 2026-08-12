// ==UserScript==
// @name         Bili CF Acc
// @namespace    bili-cf-acc
// @version      0.4.0
// @description  Route Bilibili playurl APIs and media through your fixed-egress proxy.
// @match        https://www.bilibili.com/*
// @match        https://live.bilibili.com/*
// @run-at       document-start
// @grant        unsafeWindow
// @grant        GM_cookie
// ==/UserScript==

(() => {
  "use strict";

  const SERVER = "https://bili.example.com";
  const TOKEN = "replace-with-the-same-token-as-the-server";
  const MEDIA_HOSTS = ["bilivideo.com", "bilivideo.cn", "biliapi.net", "akamaized.net"];
  const PLAYURL_HOSTS = new Set(["api.bilibili.com", "api.live.bilibili.com"]);
  const PLAYURL_PATHS = [
    /^\/x\/player\/(?:wbi\/)?playurl$/,
    /^\/pgc\/player\/web(?:\/v2)?\/playurl$/,
    /^\/xlive\/web-room\/v2\/index\/getRoomPlayInfo$/,
    /^\/room\/v1\/Room\/playUrl$/,
  ];
  const URL_KEYS = new Set(["baseUrl", "base_url", "backupUrl", "backup_url", "url", "playurl"]);
  const MEDIA_URL_HINT = /(?:bilivideo\.com|bilivideo\.cn|biliapi\.net|akamaized\.net)/i;
  const COOKIE_CACHE_MS = 15_000;
  const root = unsafeWindow;

  const isMediaUrl = (value) => {
    try {
      const host = new URL(value).hostname.toLowerCase();
      return MEDIA_HOSTS.some((suffix) => host === suffix || host.endsWith(`.${suffix}`));
    } catch {
      return false;
    }
  };

  const isPlayurlApi = (value) => {
    try {
      const url = new URL(value, root.location.href);
      return PLAYURL_HOSTS.has(url.hostname.toLowerCase()) && PLAYURL_PATHS.some((pattern) => pattern.test(url.pathname));
    } catch {
      return false;
    }
  };

  const base64url = (value) => root.btoa(value).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  const proxyUrl = (value, route = "proxy", originOnly = false) => {
    const url = new URL(value, root.location.href);
    const path = originOnly ? "" : `${url.pathname}${url.search}`;
    return `${SERVER.replace(/\/$/, "")}/${route}/${encodeURIComponent(TOKEN)}/${base64url(url.origin)}${path}`;
  };

  const collectMediaGroups = (value) => {
    const groups = [];
    const seen = new WeakSet();
    const visit = (current) => {
      if (!current || typeof current !== "object" || seen.has(current)) return;
      seen.add(current);
      const primaryKey = ["baseUrl", "base_url", "url"].find((key) => typeof current[key] === "string" && isMediaUrl(current[key]));
      const backupKey = ["backupUrl", "backup_url"].find((key) => Array.isArray(current[key]) && current[key].some((item) => typeof item === "string" && isMediaUrl(item)));
      if (primaryKey || backupKey) {
        const urls = [];
        if (primaryKey) urls.push(current[primaryKey]);
        if (backupKey) urls.push(...current[backupKey].filter((item) => typeof item === "string" && isMediaUrl(item)));
        if (urls.length > 1) groups.push({owner: current, fields: [primaryKey, backupKey].filter(Boolean), urls});
      }
      for (const child of Object.values(current)) visit(child);
    };
    visit(value);
    return groups;
  };

  const registerMediaGroups = async (groups) => {
    if (groups.length === 0 || !nativeFetch) return [];
    try {
      const response = await nativeFetch(`${SERVER.replace(/\/$/, "")}/media-groups/${encodeURIComponent(TOKEN)}`, {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: root.JSON.stringify({groups: groups.map((group) => ({urls: group.urls}))}),
        credentials: "omit",
      });
      if (!response.ok) return [];
      const parsed = await response.json();
      return Array.isArray(parsed.ids) && parsed.ids.length === groups.length ? parsed.ids.map(String) : [];
    } catch {
      return [];
    }
  };

  const rewrite = (value, registeredGroups = []) => {
    const seen = new WeakSet();
    const groupMap = new Map(registeredGroups.map((group) => [group.owner, group]));
    const visit = (current) => {
      if (!current || typeof current !== "object" || seen.has(current)) return current;
      seen.add(current);

      const group = groupMap.get(current);
      const groupedFields = new Set(group ? group.fields : []);
      let preferred = 0;
      for (const [key, child] of Object.entries(current)) {
        if (groupedFields.has(key) && typeof child === "string" && isMediaUrl(child)) {
          current[key] = `${SERVER.replace(/\/$/, "")}/proxy-group/${encodeURIComponent(TOKEN)}/${encodeURIComponent(group.id)}/${preferred++}`;
        } else if (groupedFields.has(key) && Array.isArray(child)) {
          current[key] = child.map((item) => (typeof item === "string" && isMediaUrl(item)
            ? `${SERVER.replace(/\/$/, "")}/proxy-group/${encodeURIComponent(TOKEN)}/${encodeURIComponent(group.id)}/${preferred++}`
            : item));
        } else if (URL_KEYS.has(key) && typeof child === "string" && isMediaUrl(child)) {
          current[key] = proxyUrl(child);
        } else if (URL_KEYS.has(key) && Array.isArray(child)) {
          current[key] = child.map((item) => (typeof item === "string" && isMediaUrl(item) ? proxyUrl(item) : item));
        } else if (key === "host" && typeof child === "string" && isMediaUrl(child)) {
          current[key] = proxyUrl(child, "proxy", true);
        } else {
          visit(child);
        }
      }
      return current;
    };
    return visit(value);
  };

  const nativeParse = root.JSON.parse;
  const rewriteAsync = async (value) => {
    const groups = collectMediaGroups(value);
    const ids = await registerMediaGroups(groups);
    return rewrite(value, ids.map((id, index) => ({...groups[index], id})));
  };

  const rewriteJSONText = (value) => {
    if (typeof value !== "string" || !MEDIA_URL_HINT.test(value)) return value;
    try {
      return root.JSON.stringify(rewrite(nativeParse(value)));
    } catch {
      return value;
    }
  };

  for (const name of ["__playinfo__", "__NEPTUNE_IS_MY_WAIFU__"]) {
    const descriptor = Object.getOwnPropertyDescriptor(root, name);
    if (descriptor && !descriptor.configurable) continue;
    let value = descriptor && "value" in descriptor ? rewrite(descriptor.value) : undefined;
    Object.defineProperty(root, name, {
      configurable: true,
      enumerable: descriptor?.enumerable ?? true,
      get() {
        return value;
      },
      set(next) {
        value = rewrite(next);
      },
    });
  }

  const readCookies = () => new Promise((resolve) => {
    if (typeof GM_cookie === "undefined") {
      resolve(root.document.cookie);
      return;
    }
    GM_cookie.list({ url: "https://www.bilibili.com/" }, (cookies, error) => {
      if (error) {
        console.warn("Bili CF Acc: cannot read HttpOnly cookies", error);
        resolve(root.document.cookie);
        return;
      }
      resolve(cookies.map(({ name, value }) => `${name}=${value}`).join("; "));
    });
  });

  let cookieValue = "";
  let cookieExpires = 0;
  let cookiePending;
  const getCookies = () => {
    if (Date.now() < cookieExpires) return Promise.resolve(cookieValue);
    if (!cookiePending) {
      cookiePending = readCookies().then((value) => {
        cookieValue = value;
        cookieExpires = Date.now() + COOKIE_CACHE_MS;
        return value;
      }).finally(() => {
        cookiePending = undefined;
      });
    }
    return cookiePending;
  };

  const proxiedFetchResponses = new WeakSet();
  const nativeFetch = root.fetch;
  if (nativeFetch) {
    root.fetch = async function (input, init) {
      const request = input instanceof root.Request ? input : null;
      const target = request ? request.url : String(input);
      const method = String(init?.method || request?.method || "GET").toUpperCase();
      if (method !== "GET" || !isPlayurlApi(target)) return nativeFetch.apply(this, arguments);

      const effectiveRequest = new root.Request(input, init);
      const headers = new root.Headers(effectiveRequest.headers);
      headers.set("X-Bili-Cookie", await getCookies());
      headers.set("X-Bili-Referer", root.location.href);
      const proxiedRequest = new root.Request(proxyUrl(effectiveRequest.url, "playurl"), effectiveRequest);
      const response = await nativeFetch.call(this, new root.Request(proxiedRequest, {
        headers,
        credentials: "omit",
        referrer: effectiveRequest.referrer,
        referrerPolicy: effectiveRequest.referrerPolicy,
      }));
      proxiedFetchResponses.add(response);
      return response;
    };
  }

  const xhrPlayurl = Symbol("biliPlayurl");
  const nativeOpen = root.XMLHttpRequest.prototype.open;
  const nativeSend = root.XMLHttpRequest.prototype.send;
  root.XMLHttpRequest.prototype.open = function (method, target, ...rest) {
    const shouldProxy = String(method).toUpperCase() === "GET" && isPlayurlApi(target);
    this[xhrPlayurl] = shouldProxy;
    return nativeOpen.call(this, method, shouldProxy ? proxyUrl(target, "playurl") : target, ...rest);
  };
  root.XMLHttpRequest.prototype.send = function (body) {
    if (!this[xhrPlayurl]) return nativeSend.call(this, body);
    getCookies().then((cookie) => {
      this.setRequestHeader("X-Bili-Cookie", cookie);
      this.setRequestHeader("X-Bili-Referer", root.location.href);
      nativeSend.call(this, body);
    });
  };

  root.JSON.parse = function (...args) {
    const parsed = nativeParse.apply(this, args);
    return typeof args[0] === "string" && MEDIA_URL_HINT.test(args[0]) ? rewrite(parsed) : parsed;
  };

  const nativeJson = root.Response?.prototype.json;
  if (nativeJson) {
    root.Response.prototype.json = async function (...args) {
      const parsed = await nativeJson.apply(this, args);
      return proxiedFetchResponses.has(this) ? rewriteAsync(parsed) : parsed;
    };
  }
  const nativeText = root.Response?.prototype.text;
  if (nativeText) {
    root.Response.prototype.text = async function (...args) {
      const text = await nativeText.apply(this, args);
      if (!proxiedFetchResponses.has(this)) return text;
      try {
        return root.JSON.stringify(await rewriteAsync(nativeParse(text)));
      } catch {
        return rewriteJSONText(text);
      }
    };
  }

  const responseDescriptor = Object.getOwnPropertyDescriptor(root.XMLHttpRequest.prototype, "response");
  if (responseDescriptor?.get && responseDescriptor.configurable) {
    Object.defineProperty(root.XMLHttpRequest.prototype, "response", {
      ...responseDescriptor,
      get() {
        const response = responseDescriptor.get.call(this);
        if (!this[xhrPlayurl]) return response;
        return typeof response === "string" ? rewriteJSONText(response) : rewrite(response);
      },
    });
  }

  const responseTextDescriptor = Object.getOwnPropertyDescriptor(root.XMLHttpRequest.prototype, "responseText");
  if (responseTextDescriptor?.get && responseTextDescriptor.configurable) {
    Object.defineProperty(root.XMLHttpRequest.prototype, "responseText", {
      ...responseTextDescriptor,
      get() {
        const responseText = responseTextDescriptor.get.call(this);
        return this[xhrPlayurl] ? rewriteJSONText(responseText) : responseText;
      },
    });
  }
})();
