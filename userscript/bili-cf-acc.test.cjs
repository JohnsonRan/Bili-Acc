const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function loadScript() {
  const fetchCalls = [];
  let cookieValue = "secret";
  let cookieReads = 0;
  let now = 1_000;
  class FakeXHR {
    open(method, url) {
      this.opened = { method, url };
    }
    setRequestHeader(name, value) {
      (this.headers ||= new Headers()).set(name, value);
    }
    send(body) {
      this.sent = body ?? true;
    }
    get response() {
      return this._response;
    }
    get responseText() {
      return this._responseText;
    }
  }

  class RootRequest extends Request {
    constructor(input, init) {
      super(input, init);
      const hasInit = init !== undefined;
      const hasReferrer = hasInit && "referrer" in Object(init);
      const hasReferrerPolicy = hasInit && "referrerPolicy" in Object(init);
      this._referrer = hasReferrer ? init.referrer : hasInit ? "about:client" : input instanceof RootRequest ? input.referrer : super.referrer;
      this._referrerPolicy = hasReferrerPolicy ? init.referrerPolicy : hasInit ? "" : input instanceof RootRequest ? input.referrerPolicy : super.referrerPolicy;
    }
    get referrer() {
      return this._referrer;
    }
    get referrerPolicy() {
      return this._referrerPolicy;
    }
  }
  class RootResponse extends Response {}
  const root = {
    btoa: (value) => Buffer.from(value, "binary").toString("base64"),
    console,
    document: { cookie: "buvid3=visible" },
    fetch: async (input, init) => {
      fetchCalls.push({ input, init });
      const url = input instanceof Request ? input.url : String(input);
      if (url.includes("/media-groups/")) {
        return new RootResponse('{"ids":["0123456789abcdef0123456789abcdef"]}', {status: 200, headers: {"content-type": "application/json"}});
      }
      return new RootResponse('{"data":{"dash":{"video":[{"baseUrl":"https://cdn.bilivideo.com/a.m4s","backupUrl":["https://backup.bilivideo.com/a.m4s"]}]},"url":"https://cdn.bilivideo.com/a.m4s"}}', {
        headers: { "content-type": "application/json" },
      });
    },
    Headers,
    JSON: { parse: JSON.parse, stringify: JSON.stringify },
    location: { href: "https://www.bilibili.com/video/BV1" },
    Request: RootRequest,
    Response: RootResponse,
    XMLHttpRequest: FakeXHR,
  };
  const context = {
    unsafeWindow: root,
    GM_cookie: {
      list(_details, callback) {
        cookieReads += 1;
        callback([{ name: "SESSDATA", value: cookieValue }]);
      },
    },
    URL,
    Symbol,
    Date: class extends Date {
      static now() {
        return now;
      }
    },
    console,
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, "bili-cf-acc.user.js"), "utf8"), context);
  return {
    root,
    fetchCalls,
    FakeXHR,
    setCookie(value) {
      cookieValue = value;
    },
    advanceTime(ms) {
      now += ms;
    },
    getCookieReads() {
      return cookieReads;
    },
  };
}

test("routes playurl fetch through server with login cookie", async () => {
  const { root, fetchCalls } = loadScript();
  const response = await root.fetch("https://api.bilibili.com/x/player/playurl?bvid=BV1&cid=1");
  assert.match(fetchCalls[0].input.url, /^https:\/\/bili\.example\.com\/playurl\/replace-with-the-same-token-as-the-server\//);
  assert.equal(fetchCalls[0].input.headers.get("X-Bili-Cookie"), "SESSDATA=secret");
  const parsed = await response.json();
  assert.match(parsed.data.url, /^https:\/\/bili\.example\.com\/proxy\//);
  assert.match(parsed.data.url, /replace-with-the-same-token-as-the-server/);
  assert.match(parsed.data.dash.video[0].baseUrl, /\/proxy-group\/replace-with-the-same-token-as-the-server\/0123456789abcdef0123456789abcdef\/0$/);
  assert.match(parsed.data.dash.video[0].backupUrl[0], /\/proxy-group\/replace-with-the-same-token-as-the-server\/0123456789abcdef0123456789abcdef\/1$/);
  assert.equal(fetchCalls.length, 2);
});

test("preserves Request options and leaves unrelated fetch responses alone", async () => {
  const { root, fetchCalls } = loadScript();
  const controller = new AbortController();
  const request = new root.Request("https://api.bilibili.com/x/player/playurl?cid=1", {
    cache: "no-store",
    redirect: "manual",
    referrer: "https://www.bilibili.com/video/BV1",
    referrerPolicy: "no-referrer",
    signal: controller.signal,
  });
  await root.fetch(request);
  const proxied = fetchCalls[0].input;
  assert.equal(proxied.cache, "no-store");
  assert.equal(proxied.redirect, "manual");
  assert.equal(proxied.referrer, request.referrer);
  assert.equal(proxied.referrerPolicy, "no-referrer");
  controller.abort();
  assert.equal(proxied.signal.aborted, true);

  const response = await root.fetch("https://api.bilibili.com/x/web-interface/nav");
  const parsed = await response.json();
  assert.equal(parsed.data.url, "https://cdn.bilivideo.com/a.m4s");
});

test("refreshes cookies after the short cache window", async () => {
  const { root, fetchCalls, setCookie, advanceTime, getCookieReads } = loadScript();
  await root.fetch("https://api.bilibili.com/x/player/playurl?cid=1");
  setCookie("renewed");
  await root.fetch("https://api.bilibili.com/x/player/playurl?cid=2");
  assert.equal(getCookieReads(), 1);
  assert.equal(fetchCalls[1].input.headers.get("X-Bili-Cookie"), "SESSDATA=secret");

  advanceTime(15_001);
  await root.fetch("https://api.bilibili.com/x/player/playurl?cid=3");
  assert.equal(getCookieReads(), 2);
  assert.equal(fetchCalls[2].input.headers.get("X-Bili-Cookie"), "SESSDATA=renewed");
});

test("routes playurl XHR and rewrites media URLs", async () => {
  const { root, FakeXHR } = loadScript();
  const xhr = new FakeXHR();
  xhr.open("GET", "https://api.bilibili.com/x/player/wbi/playurl?bvid=BV1&cid=1");
  xhr.send();
  await new Promise((resolve) => setImmediate(resolve));
  assert.match(xhr.opened.url, /^https:\/\/bili\.example\.com\/playurl\/replace-with-the-same-token-as-the-server\//);
  assert.equal(xhr.headers.get("X-Bili-Cookie"), "SESSDATA=secret");

  const parsed = root.JSON.parse('{"data":{"dash":{"video":[{"baseUrl":"https://cdn.bilivideo.com/a.m4s"}]}}}');
  assert.match(parsed.data.dash.video[0].baseUrl, /^https:\/\/bili\.example\.com\/proxy\//);
});

test("rewrites initial page playinfo before the first quality switch", () => {
  const { root } = loadScript();
  root.__playinfo__ = {
    data: {dash: {video: [{baseUrl: "https://cdn.bilivideo.com/initial.m4s"}]}},
  };
  assert.match(root.__playinfo__.data.dash.video[0].baseUrl, /^https:\/\/bili\.example\.com\/proxy\//);

  root.__NEPTUNE_IS_MY_WAIFU__ = {
    roomInfoRes: {data: {playurl: "https://live.bilivideo.com/live/index.m3u8"}},
  };
  assert.match(root.__NEPTUNE_IS_MY_WAIFU__.roomInfoRes.data.playurl, /^https:\/\/bili\.example\.com\/proxy\//);
});

test("rewrites proxied fetch text and XHR responseText consumers", async () => {
  const { root, FakeXHR } = loadScript();
  const response = await root.fetch("https://api.bilibili.com/x/player/playurl?cid=1");
  const text = await response.text();
  assert.match(text, /https:\/\/bili\.example\.com\/proxy\//);

  const xhr = new FakeXHR();
  xhr.open("GET", "https://api.bilibili.com/x/player/playurl?cid=1");
  xhr._responseText = '{"data":{"url":"https://cdn.bilivideo.com/initial.m4s"}}';
  assert.match(xhr.responseText, /https:\/\/bili\.example\.com\/proxy\//);
});
