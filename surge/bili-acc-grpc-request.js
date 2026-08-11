(() => {
  "use strict";

  const headers = {...($request.headers || {})};
  deleteHeader(headers, "grpc-accept-encoding");
  headers["grpc-accept-encoding"] = "identity";
  console.log("[Bili Acc][grpc-request] compression=identity");
  $done({headers});

  function deleteHeader(headers, name) {
    for (const key of Object.keys(headers)) {
      if (key.toLowerCase() === name) delete headers[key];
    }
  }
})();
