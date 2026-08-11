const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function runScript(name, globals) {
  let result;
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, name), "utf8"), {
    URL,
    console,
    ...globals,
    $done(value = {}) {
      result = value;
    },
  });
  return result;
}

test("request script routes playurl and moves credentials to private headers", () => {
  const logs = [];
  const result = runScript("bili-acc-request.js", {
    console: {log: (message) => logs.push(String(message))},
    $argument: "https://bili.example.com|test-token",
    $request: {
      method: "GET",
      url: "https://api.bilibili.com/x/player/wbi/playurl?bvid=BV1&cid=1",
      headers: {
        Cookie: "SESSDATA=secret",
        Referer: "https://www.bilibili.com/video/BV1",
        Host: "api.bilibili.com",
      },
    },
  });

  assert.match(result.url, /^https:\/\/bili\.example\.com\/playurl\/test-token\//);
  assert.match(result.url, /\/x\/player\/wbi\/playurl\?bvid=BV1&cid=1$/);
  const encodedOrigin = new URL(result.url).pathname.split("/")[3];
  assert.equal(Buffer.from(encodedOrigin, "base64url").toString(), "https://api.bilibili.com");
  assert.equal(result.headers.Cookie, undefined);
  assert.equal(result.headers.Host, "bili.example.com");
  assert.equal(result.headers["X-Bili-Cookie"], "SESSDATA=secret");
  assert.equal(result.headers["X-Bili-Referer"], "https://www.bilibili.com/video/BV1");
  assert.deepEqual(logs, ["[Bili Acc][request] rewrite api_host=api.bilibili.com api_path=/x/player/wbi/playurl cookie_present=true"]);
  assert.doesNotMatch(logs[0], /test-token|SESSDATA|bvid=|cid=/);
});

test("media request script routes native app CDN traffic and preserves Range", () => {
  const logs = [];
  const result = runScript("bili-acc-media-request.js", {
    console: {log: (message) => logs.push(String(message))},
    $argument: "https://bili.example.com|test-token",
    $request: {
      method: "GET",
      url: "https://upos-sz.example.bilivideo.com/upgcxcode/video.m4s?deadline=secret",
      headers: {
        Host: "upos-sz.example.bilivideo.com",
        Range: "bytes=1024-2047",
        Cookie: "SESSDATA=secret",
        "User-Agent": "Bilibili iOS",
      },
    },
  });

  assert.match(result.url, /^https:\/\/bili\.example\.com\/proxy\/test-token\//);
  assert.match(result.url, /\/upgcxcode\/video\.m4s\?deadline=secret$/);
  const encodedOrigin = new URL(result.url).pathname.split("/")[3];
  assert.equal(Buffer.from(encodedOrigin, "base64url").toString(), "https://upos-sz.example.bilivideo.com");
  assert.equal(result.headers.Host, "bili.example.com");
  assert.equal(result.headers.Range, "bytes=1024-2047");
  assert.equal(result.headers.Cookie, undefined);
  assert.equal(result.headers["User-Agent"], "Bilibili iOS");
  assert.deepEqual(logs, ["[Bili Acc][media-request] rewrite media_host=upos-sz.example.bilivideo.com method=GET range=true"]);
  assert.doesNotMatch(logs[0], /test-token|SESSDATA|deadline=|video\.m4s/);
});

test("media request script supports optional Akamai hosts and rejects unrelated hosts", () => {
  const akamaiResult = runScript("bili-acc-media-request.js", {
    $argument: "https://bili.example.com|test-token",
    $request: {
      method: "HEAD",
      url: "https://bilibili.example.akamaized.net/live/stream.flv",
      headers: {},
    },
  });
  assert.match(akamaiResult.url, /^https:\/\/bili\.example\.com\/proxy\/test-token\//);

  const unrelatedResult = runScript("bili-acc-media-request.js", {
    $argument: "https://bili.example.com|test-token",
    $request: {method: "GET", url: "https://cdn.example.com/video.m4s", headers: {}},
  });
  assert.equal(Object.keys(unrelatedResult).length, 0);
});

test("response script rewrites media URLs without changing quality metadata", () => {
  const logs = [];
  const body = JSON.stringify({
    code: 0,
    data: {
      quality: 120,
      accept_quality: [127, 120, 80],
      dash: {
        video: [{baseUrl: "https://cdn.bilivideo.com/video.m4s?deadline=1"}],
        audio: [{backupUrl: ["https://audio.bilivideo.com/audio.m4s"]}],
      },
      live: {host: "https://live.bilivideo.com"},
    },
  });
  const result = runScript("bili-acc-response.js", {
    console: {log: (message) => logs.push(String(message))},
    $argument: "https://bili.example.com|test-token",
    $request: {url: "https://bili.example.com/playurl/test-token/origin/path", method: "GET", headers: {}},
    $response: {
      status: 200,
      headers: {
        "Content-Type": "application/json",
        "Content-Length": String(body.length),
        "Content-Encoding": "gzip",
        "Content-MD5": "stale",
        Digest: "stale",
        ETag: "stale",
      },
      body,
    },
  });

  const parsed = JSON.parse(result.body);
  assert.equal(parsed.data.quality, 120);
  assert.deepEqual(parsed.data.accept_quality, [127, 120, 80]);
  assert.match(parsed.data.dash.video[0].baseUrl, /^https:\/\/bili\.example\.com\/proxy\/test-token\//);
  assert.match(parsed.data.dash.audio[0].backupUrl[0], /^https:\/\/bili\.example\.com\/proxy\/test-token\//);
  assert.match(parsed.data.live.host, /^https:\/\/bili\.example\.com\/proxy\/test-token\/[^/]+$/);
  assert.equal(result.headers["Content-Length"], undefined);
  assert.equal(result.headers["Content-Encoding"], undefined);
  assert.equal(result.headers["Content-MD5"], undefined);
  assert.equal(result.headers.Digest, undefined);
  assert.equal(result.headers.ETag, undefined);
  assert.deepEqual(logs, ["[Bili Acc][response] rewrite status=200 source=proxied_api media_urls=3"]);
  assert.doesNotMatch(logs[0], /test-token|deadline=/);
});

test("scripts preserve pipe-containing tokens and ignore unrelated responses", () => {
  const requestResult = runScript("bili-acc-request.js", {
    $argument: "https://bili.example.com|part1|part2",
    $request: {
      method: "GET",
      url: "https://api.live.bilibili.com/room/v1/Room/playUrl?cid=1",
      headers: {},
    },
  });
  assert.match(requestResult.url, /\/playurl\/part1%7Cpart2\//);
  assert.equal(requestResult.headers["X-Bili-Cookie"], "");

  const responseResult = runScript("bili-acc-response.js", {
    $argument: "https://bili.example.com|test-token",
    $request: {url: "https://other.example/playurl/test-token/origin/path", method: "GET", headers: {}},
    $response: {status: 200, headers: {}, body: `{"url":"https://cdn.bilivideo.com/a.m4s"}`},
  });
  assert.equal(Object.keys(responseResult).length, 0);
});

test("scripts pass through when module arguments are incomplete", () => {
  const requestResult = runScript("bili-acc-request.js", {
    $argument: "https://bili.example.com|",
    $request: {method: "GET", url: "https://api.bilibili.com/x/player/playurl", headers: {}},
  });
  const responseResult = runScript("bili-acc-response.js", {
    $argument: "|test-token",
    $request: {url: "https://api.bilibili.com/x/player/playurl", method: "GET", headers: {}},
    $response: {status: 200, headers: {}, body: "{}"},
  });
  assert.equal(Object.keys(requestResult).length, 0);
  assert.equal(Object.keys(responseResult).length, 0);
});

test("request script diagnoses unresolved module placeholders without leaking them", () => {
  const logs = [];
  const result = runScript("bili-acc-request.js", {
    console: {log: (message) => logs.push(String(message))},
    $argument: "{{{server}}}|{{{token}}}",
    $request: {
      method: "GET",
      url: "https://api.bilibili.com/x/player/playurl?cid=secret",
      headers: {Cookie: "SESSDATA=secret"},
    },
  });

  assert.equal(Object.keys(result).length, 0);
  assert.deepEqual(logs, ["[Bili Acc][request] skip reason=invalid_server"]);
  assert.doesNotMatch(logs[0], /\{\{\{|SESSDATA|cid=/);
});

test("module declares parameterized scripts and MITM hosts", () => {
  const moduleText = fs.readFileSync(path.join(__dirname, "bili-acc.sgmodule"), "utf8");
  assert.match(moduleText, /^#!arguments=server:https:\/\/bili\.example\.com,token:12345$/m);
  assert.match(moduleText, /type=http-request/);
  assert.match(moduleText, /type=http-response/);
  const scriptLines = moduleText.split(/\r?\n/).filter((item) => item.includes("type=http-"));
  for (const line of scriptLines) {
    const pattern = line.match(/pattern=(.*),script-path=/)?.[1];
    assert.doesNotThrow(() => new RegExp(pattern));
  }
  const requestPattern = new RegExp(scriptLines[0].match(/pattern=(.*),script-path=/)[1]);
  for (const url of [
    "https://api.bilibili.com/x/player/playurl?cid=1",
    "https://api.bilibili.com/x/player/wbi/playurl?cid=1",
    "https://api.bilibili.com/pgc/player/web/playurl?ep_id=1",
    "https://api.bilibili.com/pgc/player/web/v2/playurl?ep_id=1",
    "https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo?room_id=1",
    "https://api.live.bilibili.com/room/v1/Room/playUrl?cid=1",
  ]) assert.equal(requestPattern.test(url), true, url);
  assert.equal(requestPattern.test("https://api.bilibili.com/x/web-interface/nav"), false);
  assert.match(moduleText, /requires-body=true,max-size=2097152/);
  assert.equal((moduleText.match(/debug=true/g) || []).length, 3);
  assert.match(moduleText, /bili-acc-media-request\.js/);
  const mediaPattern = new RegExp(scriptLines[1].match(/pattern=(.*),script-path=/)[1]);
  for (const url of [
    "https://upos-sz.example.bilivideo.com/video.m4s",
    "https://live.bilivideo.cn/live/stream.flv",
    "https://cdn.biliapi.net/audio.m4s",
  ]) assert.equal(mediaPattern.test(url), true, url);
  assert.equal(mediaPattern.test("https://cdn.akamaized.net/video.m4s"), false);
  assert.equal(mediaPattern.test("https://notbilivideo.com/video.m4s"), false);
  assert.match(moduleText, /hostname = %APPEND% api\.bilibili\.com, api\.live\.bilibili\.com/);
  assert.match(moduleText, /\*\.bilivideo\.com/);
  assert.doesNotMatch(moduleText, /\*\.akamaized\.net/);
});

test("optional Akamai module is isolated from the main module", () => {
  const moduleText = fs.readFileSync(path.join(__dirname, "bili-acc-akamai.sgmodule"), "utf8");
  const scriptLine = moduleText.split(/\r?\n/).find((item) => item.includes("type=http-request"));
  const pattern = new RegExp(scriptLine.match(/pattern=(.*),script-path=/)[1]);
  assert.equal(pattern.test("https://bilibili.example.akamaized.net/live/stream.flv"), true);
  assert.equal(pattern.test("https://cdn.bilivideo.com/video.m4s"), false);
  assert.match(moduleText, /hostname = %APPEND% akamaized\.net, \*\.akamaized\.net/);
  assert.match(moduleText, /debug=true/);
});
