import assert from "node:assert/strict";
import { once } from "node:events";
import http from "node:http";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { createServer, loadConfigFromFile } from "vite";

const root = fileURLToPath(new URL("../", import.meta.url));
const configFile = fileURLToPath(new URL("../vite.config.ts", import.meta.url));
const env = { command: "serve", mode: "test" };

function restoreTarget(value) {
  if (value === undefined) delete process.env.TOKENPIVOT_DEV_API_TARGET;
  else process.env.TOKENPIVOT_DEV_API_TARGET = value;
}

function request(port, path, method = "GET", body = "") {
  return new Promise((resolve, reject) => {
    const req = http.request(
      {
        host: "127.0.0.1",
        port,
        path,
        method,
        headers: {
          Host: `127.0.0.1:${port}`,
          Origin: `http://127.0.0.1:${port}`,
          Cookie: "test-cookie=synthetic",
          "Content-Type": "text/plain",
          Accept: "text/html",
        },
      },
      (res) => {
        let text = "";
        res.setEncoding("utf8");
        res.on("data", (chunk) => {
          text += chunk;
        });
        res.on("end", () =>
          resolve({ status: res.statusCode, headers: res.headers, body: text }),
        );
        res.on("error", reject);
      },
    );
    req.setTimeout(5000, () => req.destroy(new Error("request timeout")));
    req.on("error", reject);
    req.end(body);
  });
}

function closeServer(server) {
  return new Promise((resolve, reject) =>
    server.close((err) => (err ? reject(err) : resolve())),
  );
}

test(
  "real Vite proxy preserves HTTP semantics and exact API boundary",
  { timeout: 30000 },
  async () => {
    const original = process.env.TOKENPIVOT_DEV_API_TARGET;
    const received = [];
    const upstream = http.createServer(async (req, res) => {
      let body = "";
      for await (const chunk of req) body += chunk;
      received.push({
        method: req.method,
        url: req.url,
        headers: req.headers,
        body,
      });
      const status =
        Number(
          new URL(req.url, "http://test.invalid").searchParams.get("status"),
        ) || 200;
      res.writeHead(status, {
        "Content-Type": "application/json",
        "Set-Cookie": [
          "first=synthetic; HttpOnly; Path=/",
          "second=synthetic; SameSite=Lax; Path=/",
        ],
      });
      res.end(JSON.stringify({ status }));
    });
    let vite;
    try {
      upstream.listen(0, "127.0.0.1");
      await once(upstream, "listening");
      const addr = upstream.address();
      assert.ok(addr && typeof addr === "object");
      process.env.TOKENPIVOT_DEV_API_TARGET = `http://127.0.0.1:${addr.port}`;
      vite = await createServer({
        root,
        configFile,
        logLevel: "silent",
        server: { port: 0 },
      });
      await vite.listen();
      const viteAddr = vite.httpServer.address();
      assert.ok(viteAddr && typeof viteAddr === "object");
      for (const [method, path, body, status] of [
        ["GET", "/api", "", 200],
        ["GET", "/api?x=1", "", 200],
        ["POST", "/api/x?x=one%20two", "payload", 200],
        ["GET", "/api/error?status=401", "", 401],
        ["GET", "/api/error?status=404", "", 404],
        ["GET", "/api/error?status=500", "", 500],
      ]) {
        const response = await request(viteAddr.port, path, method, body);
        assert.equal(response.status, status);
        assert.equal(response.headers["content-type"], "application/json");
        assert.equal(response.body, JSON.stringify({ status }));
        assert.deepEqual(response.headers["set-cookie"], [
          "first=synthetic; HttpOnly; Path=/",
          "second=synthetic; SameSite=Lax; Path=/",
        ]);
        const last = received.at(-1);
        assert.equal(last.method, method);
        assert.equal(last.url, path);
        assert.equal(last.body, body);
        assert.equal(last.headers.host, `127.0.0.1:${viteAddr.port}`);
        assert.equal(last.headers.origin, `http://127.0.0.1:${viteAddr.port}`);
        assert.equal(last.headers.cookie, "test-cookie=synthetic");
        assert.equal(last.headers.authorization, undefined);
      }
      const count = received.length;
      await request(viteAddr.port, "/apix");
      await request(viteAddr.port, "/healthz");
      assert.equal(received.length, count);
      await closeServer(upstream);
      const failed = await request(viteAddr.port, "/api/unavailable");
      assert.ok(failed.status >= 400);
      assert.ok(!failed.body.includes('<div id="root">'));
    } finally {
      if (vite) await vite.close();
      if (upstream.listening) await closeServer(upstream);
      restoreTarget(original);
    }
  },
);

test("target validates origins without echoing invalid input", async () => {
  const original = process.env.TOKENPIVOT_DEV_API_TARGET;
  try {
    delete process.env.TOKENPIVOT_DEV_API_TARGET;
    const defaults = await loadConfigFromFile(env, configFile, root, "silent");
    assert.equal(defaults.config.server.host, "127.0.0.1");
    assert.equal(defaults.config.server.port, 5173);
    assert.equal(defaults.config.server.strictPort, true);
    assert.equal(
      defaults.config.server.proxy["^/api(?:/|[?]|$)"].target,
      "http://127.0.0.1:8080",
    );
    for (const value of [
      "",
      "ftp://example.invalid",
      "http://user:synthetic@example.invalid",
      "http://example.invalid/path",
      "http://example.invalid?x=1",
      "http://example.invalid#frag",
      " http://example.invalid",
      "http://example.invalid/../",
      "http://example.invalid?",
      "http://@example.invalid",
    ]) {
      process.env.TOKENPIVOT_DEV_API_TARGET = value;
      await assert.rejects(
        loadConfigFromFile(env, configFile, root, "silent"),
        (err) => {
          assert.equal(
            err.message,
            "TOKENPIVOT_DEV_API_TARGET must be an HTTP(S) origin without credentials, path, query or fragment",
          );
          return true;
        },
      );
    }
    for (const value of ["https://example.invalid", "http://[::1]:8080/"]) {
      process.env.TOKENPIVOT_DEV_API_TARGET = value;
      const loaded = await loadConfigFromFile(env, configFile, root, "silent");
      assert.equal(
        loaded.config.server.proxy["^/api(?:/|[?]|$)"].target,
        new URL(value).origin,
      );
    }
  } finally {
    restoreTarget(original);
  }
});
