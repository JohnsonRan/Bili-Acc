const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const zlib = require("node:zlib");

function runScript(name, globals) {
  let result;
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, name), "utf8"), {
    URL,
    Uint8Array,
    console,
    $argument: name === "bili-acc-grpc-response.js" ? "https://bili.example.com|test-token" : undefined,
    ...globals,
    $done(value = {}) {
      result = value;
    },
  });
  return result;
}

function encodeVarint(value) {
  const bytes = [];
  let remaining = value;
  do {
    let byte = remaining % 128;
    remaining = Math.floor(remaining / 128);
    if (remaining > 0) byte |= 128;
    bytes.push(byte);
  } while (remaining > 0);
  return Buffer.from(bytes);
}

function protobufBytesField(number, value) {
  const bytes = Buffer.from(value);
  return Buffer.concat([encodeVarint((number << 3) | 2), encodeVarint(bytes.length), bytes]);
}

function protobufGroup(number, payload) {
  return Buffer.concat([encodeVarint((number << 3) | 3), Buffer.from(payload), encodeVarint((number << 3) | 4)]);
}

function grpcFrame(payload, compressed = false) {
  const header = Buffer.alloc(5);
  header[0] = compressed ? 1 : 0;
  header.writeUInt32BE(payload.length, 1);
  return Uint8Array.from(Buffer.concat([header, payload]));
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

test("routine Surge pass-through conditions do not log", () => {
  const logs = [];
  const console = {log: (message) => logs.push(String(message))};
  const argument = "https://bili.example.com|test-token";

  const requestResult = runScript("bili-acc-request.js", {
    console,
    $argument: argument,
    $request: {method: "POST", url: "https://api.bilibili.com/x/player/playurl", headers: {}},
  });
  const mediaMethodResult = runScript("bili-acc-media-request.js", {
    console,
    $argument: argument,
    $request: {method: "POST", url: "https://cdn.bilivideo.com/video.m4s", headers: {}},
  });
  const mediaHostResult = runScript("bili-acc-media-request.js", {
    console,
    $argument: argument,
    $request: {method: "GET", url: "https://video.example.akamaized.net/video.m4s", headers: {}},
  });
  const responseResult = runScript("bili-acc-response.js", {
    console,
    $argument: argument,
    $request: {url: "https://other.example/playurl/test-token/origin/path", method: "GET", headers: {}},
    $response: {status: 200, headers: {}, body: "{}"},
  });

  for (const result of [requestResult, mediaMethodResult, mediaHostResult, responseResult]) {
    assert.equal(Object.keys(result).length, 0);
  }
  assert.deepEqual(logs, []);
});

test("gRPC request routes playurl through the fixed-egress server", () => {
  const logs = [];
  const body = Uint8Array.from([0, 0, 0, 0, 2, 8, 1]);
  const result = runScript("bili-acc-grpc-request.js", {
    console: {log: (message) => logs.push(String(message))},
    $argument: "https://bili.example.com|test-token",
    $request: {
      method: "POST",
      url: "https://grpc.biliapi.net/bilibili.app.playerunite.v1.Player/PlayViewUnite",
      headers: {Host: "grpc.biliapi.net", "Content-Type": "application/grpc+proto", "grpc-accept-encoding": "gzip,identity", "User-Agent": "Bilibili iOS"},
      body,
    },
  });
  assert.match(result.url, /^https:\/\/bili\.example\.com\/playurl-grpc\/test-token\//);
  assert.match(result.url, /\/bilibili\.app\.playerunite\.v1\.Player\/PlayViewUnite$/);
  const encodedOrigin = new URL(result.url).pathname.split("/")[3];
  assert.equal(Buffer.from(encodedOrigin, "base64url").toString(), "https://grpc.biliapi.net");
  assert.equal(result.headers.Host, "bili.example.com");
  assert.equal(result.headers["Content-Type"], "application/x-bili-acc-grpc");
  assert.equal(result.headers["X-Bili-Acc-Grpc-Content-Type"], "application/grpc+proto");
  assert.equal(result.headers["grpc-accept-encoding"], "identity");
  assert.equal(result.headers["User-Agent"], "Bilibili iOS");
  assert.deepEqual(Buffer.from(result.body), Buffer.from(body));
  assert.deepEqual(logs, ["[Bili Acc][grpc-request] rewrite api_host=grpc.biliapi.net compression=identity tunnel=http"]);
  assert.doesNotMatch(logs[0], /test-token|PlayViewUnite/);
});

test("gRPC response replaces Akamai URLs with Bilibili backup URLs", () => {
  const akamai = "https://video.example.akamaized.net/upgcxcode/video.m4s?deadline=secret";
  const backup = "https://upos-sz-mirrorali.bilivideo.com/upgcxcode/video.m4s?deadline=secret";
  const dashVideo = Buffer.concat([protobufBytesField(1, akamai), protobufBytesField(2, backup)]);
  const stream = protobufBytesField(2, dashVideo);
  const vodInfo = protobufBytesField(5, stream);
  const reply = protobufBytesField(1, vodInfo);
  const logs = [];
  const result = runScript("bili-acc-grpc-response.js", {
    console: {log: (message) => logs.push(String(message))},
    $request: {
      url: "https://bili.example.com/playurl-grpc/test-token/aHR0cHM6Ly9ncnBjLmJpbGlhcGkubmV0/bilibili.app.playerunite.v1.Player/PlayViewUnite",
      method: "POST",
      headers: {},
    },
    $response: {
      status: 200,
      headers: {"Content-Type": "application/grpc", "Content-Length": "123", "X-Bili-Acc-Grpc-Status": "0"},
      body: grpcFrame(reply),
    },
  });

  const rewritten = Buffer.from(result.body).toString("latin1");
  assert.doesNotMatch(rewritten, /video\.example\.akamaized\.net/);
  assert.match(rewritten, /https:\/\/bili\.example\.com\/proxy\/test-token\/aHR0cHM6Ly92aWRlby5leGFtcGxlLmFrYW1haXplZC5uZXQ\/upgcxcode\/video\.m4s/);
  assert.match(rewritten, /upos-sz-mirrorali\.bilivideo\.com/);
  assert.equal(result.headers["Content-Length"], undefined);
  assert.equal(result.headers["X-Bili-Acc-Grpc-Status"], undefined);
  assert.equal(result.headers["Grpc-Status"], "0");
  assert.deepEqual(logs, ["[Bili Acc][grpc-response] rewrite endpoint=/bilibili.app.playerunite.v1.Player/PlayViewUnite frames=1 akamai_urls=1 decompressed_frames=0"]);
  assert.doesNotMatch(logs[0], /test-token|aHR0|deadline=|video\.m4s/);
});

test("gRPC response proxies Akamai primary and preserves backup URLs", () => {
  const akamai = "https://video.example.akamaized.net/main.m4s";
  const preferred = "https://upos-sz-mirrorali.bilivideo.com/preferred.m4s";
  const later = "https://upos-sz-mirrorcosov.bilivideo.com/later.m4s";
  const dashVideo = Buffer.concat([
    protobufBytesField(1, akamai),
    protobufBytesField(2, preferred),
    protobufBytesField(2, later),
  ]);
  const reply = protobufBytesField(1, protobufBytesField(5, protobufBytesField(2, dashVideo)));
  const result = runScript("bili-acc-grpc-response.js", {
    $request: {url: "https://grpc.biliapi.net/bilibili.app.playerunite.v1.Player/PlayViewUnite", headers: {}},
    $response: {headers: {"Content-Type": "application/grpc"}, body: grpcFrame(reply)},
  });
  const rewritten = Buffer.from(result.body).toString("latin1");
  assert.match(rewritten, /https:\/\/bili\.example\.com\/proxy\/test-token\/aHR0cHM6Ly92aWRlby5leGFtcGxlLmFrYW1haXplZC5uZXQ\/main\.m4s/);
  assert.equal(rewritten.match(/upos-sz-mirrorali\.bilivideo\.com\/preferred\.m4s/g)?.length, 1);
  assert.equal(rewritten.match(/upos-sz-mirrorcosov\.bilivideo\.com\/later\.m4s/g)?.length, 1);
});

test("gRPC response restores tunneled status even without URL rewrites", () => {
  const dashVideo = protobufBytesField(1, "https://cdn.bilivideo.com/main.m4s");
  const reply = protobufBytesField(1, protobufBytesField(5, protobufBytesField(2, dashVideo)));
  const result = runScript("bili-acc-grpc-response.js", {
    $request: {url: "https://grpc.biliapi.net/bilibili.app.playerunite.v1.Player/PlayViewUnite", headers: {}},
    $response: {headers: {"Content-Type": "application/grpc", "Grpc-Status": "0", "X-Bili-Acc-Grpc-Status": "14"}, body: grpcFrame(reply)},
  });
  assert.equal(result.body, undefined);
  assert.equal(result.headers["X-Bili-Acc-Grpc-Status"], undefined);
  assert.equal(result.headers["Grpc-Status"], "14");
});

test("gRPC response restores tunneled status on every pass-through path", () => {
  const cases = [
    {name: "empty", headers: {"Content-Type": "application/grpc", "X-Bili-Acc-Grpc-Status": "14"}, body: undefined, status: "14"},
    {name: "malformed", headers: {"Content-Type": "application/grpc", "X-Bili-Acc-Grpc-Status": "14"}, body: Uint8Array.from([0, 0, 0]), status: "14"},
    {name: "compressed", headers: {"Content-Type": "application/grpc", "Grpc-Status": "0", "grpc-encoding": "br", "X-Bili-Acc-Grpc-Status": "14"}, body: grpcFrame(Buffer.from([8, 1]), true), status: "14"},
    {name: "invalid-status", headers: {"Content-Type": "application/grpc", "Grpc-Status": "0", "X-Bili-Acc-Grpc-Status": "unknown"}, body: undefined, status: "2"},
    {name: "missing-status", headers: {"Content-Type": "application/grpc", "Grpc-Status": "0"}, body: undefined, status: "2", url: "https://bili.example.com/playurl-grpc/token/origin/bilibili.app.playerunite.v1.Player/PlayViewUnite"},
    {name: "empty-status", headers: {"Content-Type": "application/grpc", "Grpc-Status": "0", "X-Bili-Acc-Grpc-Status": "   "}, body: undefined, status: "2", url: "https://bili.example.com/playurl-grpc/token/origin/bilibili.app.playerunite.v1.Player/PlayViewUnite"},
  ];
  for (const item of cases) {
    const result = runScript("bili-acc-grpc-response.js", {
      console: {log() {}},
      $request: {url: item.url || "https://grpc.biliapi.net/bilibili.app.playerunite.v1.Player/PlayViewUnite", headers: {}},
      $response: {headers: item.headers, body: item.body},
    });
    assert.equal(result.headers["X-Bili-Acc-Grpc-Status"], undefined, item.name);
    assert.equal(result.headers["Grpc-Status"], item.status, item.name);
    assert.equal(result.body, undefined, item.name);
  }
});

test("gRPC response preserves opaque bytes and protobuf groups", () => {
  const akamai = "https://video.example.akamaized.net/main.m4s";
  const backup = "https://cdn.bilivideo.com/backup.m4s";
  const opaquePayload = Buffer.concat([protobufBytesField(1, akamai), protobufBytesField(2, backup)]);
  const opaqueField = protobufBytesField(99, opaquePayload);
  const dashVideo = Buffer.concat([protobufBytesField(1, akamai), protobufBytesField(2, backup), opaqueField]);
  const stream = protobufBytesField(2, dashVideo);
  const vodInfo = protobufBytesField(5, stream);
  const unknownGroup = protobufGroup(99, protobufBytesField(1, "group-data"));
  const reply = Buffer.concat([protobufBytesField(1, vodInfo), unknownGroup]);
  const result = runScript("bili-acc-grpc-response.js", {
    $request: {url: "https://grpc.biliapi.net/bilibili.app.playerunite.v1.Player/PlayViewUnite", headers: {}},
    $response: {headers: {"Content-Type": "application/grpc"}, body: grpcFrame(reply)},
  });
  const rewritten = Buffer.from(result.body);
  assert.equal(rewritten.includes(opaqueField), true);
  assert.equal(rewritten.includes(unknownGroup), true);
});

test("gRPC response decompresses gzip frames before rewriting", () => {
  const akamai = "https://video.example.akamaized.net/main.m4s";
  const backup = "https://cdn.bilivideo.com/backup.m4s";
  const dashVideo = Buffer.concat([protobufBytesField(1, akamai), protobufBytesField(2, backup)]);
  const reply = protobufBytesField(1, protobufBytesField(5, protobufBytesField(2, dashVideo)));
  const compressed = zlib.gzipSync(reply);
  const logs = [];
  const result = runScript("bili-acc-grpc-response.js", {
    console: {log: (message) => logs.push(String(message))},
    $utils: {ungzip: (value) => Uint8Array.from(zlib.gunzipSync(Buffer.from(value)))},
    $request: {url: "https://grpc.biliapi.net/bilibili.app.playerunite.v1.Player/PlayViewUnite", headers: {}},
    $response: {headers: {"Content-Type": "application/grpc", "Content-Encoding": "gzip", "grpc-encoding": "gzip"}, body: grpcFrame(compressed, true)},
  });
  const rewritten = Buffer.from(result.body);
  assert.equal(rewritten[0], 0);
  assert.doesNotMatch(rewritten.toString("latin1"), /akamaized\.net/);
  assert.match(rewritten.toString("latin1"), /cdn\.bilivideo\.com/);
  assert.equal(result.headers["grpc-encoding"], undefined);
  assert.equal(result.headers["Content-Encoding"], undefined);
  assert.deepEqual(logs, ["[Bili Acc][grpc-response] rewrite endpoint=/bilibili.app.playerunite.v1.Player/PlayViewUnite frames=1 akamai_urls=1 decompressed_frames=1"]);
});

test("gRPC response skips unsupported compressed frames", () => {
  const logs = [];
  const payload = protobufBytesField(1, "https://video.example.akamaized.net/main.m4s");
  const result = runScript("bili-acc-grpc-response.js", {
    console: {log: (message) => logs.push(String(message))},
    $request: {url: "https://grpc.biliapi.net/bilibili.app.playerunite.v1.Player/PlayViewUnite", headers: {}},
    $response: {headers: {"Content-Type": "application/grpc", "grpc-encoding": "br"}, body: grpcFrame(payload, true)},
  });
  assert.equal(Object.keys(result).length, 0);
  assert.deepEqual(logs, ["[Bili Acc][grpc-response] skip reason=compressed_frame encoding=br"]);
});

test("media request script rejects Akamai and unrelated hosts", () => {
  for (const url of ["https://video.example.akamaized.net/video.m4s", "https://cdn.example.com/video.m4s"]) {
    const result = runScript("bili-acc-media-request.js", {
      $argument: "https://bili.example.com|test-token",
      $request: {method: "GET", url, headers: {}},
    });
    assert.equal(Object.keys(result).length, 0, url);
  }
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

test("module declares native gRPC rewriting and scoped MITM hosts", () => {
  const moduleText = fs.readFileSync(path.join(__dirname, "bili-acc.sgmodule"), "utf8");
  assert.match(moduleText, /^#!arguments=server:https:\/\/bili\.example\.com,token:12345$/m);
  assert.match(moduleText, /bili-acc-grpc-request\.js/);
  assert.match(moduleText, /bili-acc-grpc-response\.js/);
  assert.doesNotMatch(moduleText, /bili-acc-auth-request\.js/);
  const scriptLines = moduleText.split(/\r?\n/).filter((item) => item.includes("type=http-"));
  for (const line of scriptLines) {
    const pattern = line.match(/pattern=(.*),script-path=/)?.[1];
    assert.doesNotThrow(() => new RegExp(pattern));
    assert.match(line, /script-path=https:\/\/raw\.githubusercontent\.com\/JohnsonRan\/Bili-Acc\/main\/surge\/[^,]+\.js\?v=\d{8}-\d+,/);
  }
  const requestLine = scriptLines.find((line) => line.includes("bili-acc-request.js"));
  const requestPattern = new RegExp(requestLine.match(/pattern=(.*),script-path=/)[1]);
  for (const url of [
    "https://api.bilibili.com/x/player/playurl?cid=1",
    "https://api.bilibili.com/x/player/wbi/playurl?cid=1",
    "https://api.bilibili.com/pgc/player/web/playurl?ep_id=1",
    "https://api.bilibili.com/pgc/player/web/v2/playurl?ep_id=1",
    "https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo?room_id=1",
    "https://api.live.bilibili.com/room/v1/Room/playUrl?cid=1",
  ]) assert.equal(requestPattern.test(url), true, url);
  assert.equal(requestPattern.test("https://api.bilibili.com/x/web-interface/nav"), false);

  const mediaLine = scriptLines.find((line) => line.includes("bili-acc-media-request.js"));
  const mediaPattern = new RegExp(mediaLine.match(/pattern=(.*),script-path=/)[1]);
  for (const url of [
    "https://upos-sz.example.bilivideo.com/video.m4s",
    "https://live.bilivideo.cn/live/stream.flv",
    "https://cdn.biliapi.net/audio.m4s",
  ]) assert.equal(mediaPattern.test(url), true, url);
  assert.equal(mediaPattern.test("https://cdn.akamaized.net/video.m4s"), false);
  assert.equal(mediaPattern.test("https://notbilivideo.com/video.m4s"), false);

  const grpcLine = scriptLines.find((line) => line.includes("bili-acc-grpc-response.js"));
  const grpcPattern = new RegExp(grpcLine.match(/pattern=(.*),script-path=/)[1]);
  assert.equal(grpcPattern.test("https://grpc.biliapi.net/bilibili.app.playerunite.v1.Player/PlayViewUnite"), true);
  assert.equal(grpcPattern.test("https://app.bilibili.com/bilibili.app.playerunite.v1.Player/PlayViewUnite"), true);
  assert.equal(grpcPattern.test("https://bili.example.com/playurl-grpc/test-token/aHR0cHM6Ly9ncnBjLmJpbGlhcGkubmV0/bilibili.app.playerunite.v1.Player/PlayViewUnite"), true);
  assert.equal(grpcPattern.test("https://app.bilibili.com/bilibili.pgc.gateway.player.v2.PlayURL/PlayView"), false);
  assert.match(grpcLine, /binary-body-mode=true/);
  assert.match(grpcLine, /engine=webview/);
  assert.match(grpcLine, /argument="\{\{\{server\}\}\}\|\{\{\{token\}\}\}"/);

  const grpcRequestLine = scriptLines.find((line) => line.includes("bili-acc-grpc-request.js"));
  assert.match(grpcRequestLine, /type=http-request/);
  assert.match(grpcRequestLine, /argument="\{\{\{server\}\}\}\|\{\{\{token\}\}\}"/);
  assert.match(grpcRequestLine, /requires-body=true/);
  assert.match(grpcRequestLine, /binary-body-mode=true/);
  assert.match(grpcRequestLine, /max-size=2097152/);
  const requestScriptLines = scriptLines.filter((line) => line.includes("type=http-request"));
  for (const host of ["grpc.biliapi.net", "app.bilibili.com", "app.biliapi.net"]) {
    const url = `https://${host}/bilibili.app.playerunite.v1.Player/PlayViewUnite`;
    const firstMatch = requestScriptLines.find((line) => new RegExp(line.match(/pattern=(.*),script-path=/)[1]).test(url));
    assert.equal(firstMatch, grpcRequestLine, url);
  }
  assert.equal((moduleText.match(/debug=true/g) || []).length, 5);
  assert.match(moduleText, /hostname = %APPEND% api\.bilibili\.com/);
  assert.match(moduleText, /grpc\.biliapi\.net/);
  assert.match(moduleText, /\*\.bilivideo\.com/);
  assert.doesNotMatch(moduleText, /akamaized\.net/);
  assert.equal(fs.existsSync(path.join(__dirname, "bili-acc-akamai.sgmodule")), false);
});
