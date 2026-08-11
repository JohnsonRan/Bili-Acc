(() => {
  "use strict";

  const argument = String($argument || "");
  const separator = argument.indexOf("|");
  const configuredServer = separator >= 0 ? argument.slice(0, separator) : "";
  const token = separator >= 0 ? argument.slice(separator + 1) : "";
  const server = configuredServer.replace(/\/$/, "");
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
  if (String($request.method || "").toUpperCase() !== "POST") {
    $done({});
    return;
  }

  const target = String($request.url || "");
  const match = target.match(/^(https?:\/\/[^/]+)(\/.*)?$/i);
  const serverMatch = server.match(/^https?:\/\/([^/]+)$/i);
  if (!match || !serverMatch) {
    log("skip reason=invalid_request_url");
    $done({});
    return;
  }

  const headers = {...($request.headers || {})};
  deleteHeader(headers, "host");
  deleteHeader(headers, "grpc-accept-encoding");
  headers.Host = serverMatch[1];
  headers["grpc-accept-encoding"] = "identity";

  const origin = match[1];
  const path = match[2] || "/";
  const url = `${server}/playurl-grpc/${encodeURIComponent(token)}/${base64url(origin)}${path}`;
  log(`rewrite api_host=${hostOf(origin)} compression=identity`);
  $done({url, headers});

  function log(message) {
    console.log(`[Bili Acc][grpc-request] ${message}`);
  }

  function hostOf(origin) {
    const match = String(origin).match(/^https?:\/\/([^/:?#]+)/i);
    return match ? match[1].toLowerCase() : "unknown";
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
