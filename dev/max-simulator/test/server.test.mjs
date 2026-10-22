import assert from "node:assert/strict";
import { createServer, request as httpRequest } from "node:http";
import { once } from "node:events";
import { test } from "node:test";
import { readFile } from "node:fs/promises";
import { request as httpsRequest } from "node:https";
import { resolve } from "node:path";
import { inflateSync } from "node:zlib";
import { buildSimulator, loadConfig } from "../src/server.mjs";
import { bannerPath, bannerURL, buildBannerServer, tlsDirectory } from "../src/banner.mjs";
import { imageURL } from "../public/media.js";

const tokens = {
  maxToken: "local-simulator-token",
  webhookSecret: "local-webhook-secret",
  demoToken: "local-demo-token",
  adminToken: "local-administrator-token",
};

async function harness(t) {
  let counter = 0;
  const calls = [];
  const runs = new Map();
  const events = new Set();
  const webhookFailures = [];
  let base;
  const bot = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const raw = Buffer.concat(chunks).toString();
    const body = raw ? JSON.parse(raw) : null;
    const call = { method: request.method, path: request.url, body, headers: request.headers };
    calls.push(call);
    function send(status, result) {
      response.writeHead(status, { "content-type": "application/json" });
      response.end(JSON.stringify(result));
    }
    if (request.url === "/_demo/runs") {
      assert.equal(request.headers.authorization, `Bearer ${tokens.demoToken}`);
      const number = ++counter;
      const run = {
        id: `run-${number}`, campaign_id: `campaign-${number}`, day_id: `day-${number}`,
        draw_id: `draw-${number}`, final_draw_id: `final-${number}`,
        user_ids: [number * 100 + 1, number * 100 + 2],
        ...(body.scenario === "expired" ? { offer_id: `offer-${number}` } : {}),
        now: "2026-10-22T00:00:00Z",
      };
      runs.set(run.campaign_id, { run, scenario: body.scenario });
      return send(201, run);
    }
    if (request.url === "/webhooks/max") {
      assert.equal(request.headers.authorization, undefined);
      assert.equal(request.headers["x-max-bot-api-secret"], tokens.webhookSecret);
      assert.ok(runs.has(request.headers["x-demo-campaign"]));
      if (webhookFailures.length) return send(webhookFailures.shift(), { error: "unavailable" });
      const eventID = JSON.stringify(body);
      const duplicate = events.has(eventID);
      events.add(eventID);
      if (body.update_type === "message_callback") {
        const result = await fetch(`${base}/answers?callback_id=${body.callback.callback_id}`, {
          method: "POST", headers: { authorization: tokens.maxToken, "content-type": "application/json" }, body: "{}",
        });
        assert.equal(result.status, 200);
      }
      return send(200, { accepted: true, duplicate, event_id: `event-${events.size}` });
    }
    assert.equal(request.headers.authorization, `Bearer ${tokens.adminToken}`);
    const match = request.url.match(/^\/api\/admin\/campaigns\/(campaign-\d+)\/(.+)$/);
    if (!match || !runs.has(match[1])) return send(404, { error: "not found" });
    const { run, scenario } = runs.get(match[1]);
    const state = ["drawable", "final", "expired"].includes(scenario) ? "drawn" : "open";
    const draws = [
      { id: run.draw_id, campaign_id: run.campaign_id, state, kind: "daily" },
      { id: run.final_draw_id, campaign_id: run.campaign_id, state, kind: "final" },
    ];
    if (match[2] === "stats") return send(200, { users: 2, promo_remaining: [{ channel_id: -42 - counter, days: [{ day_id: run.day_id }] }] });
    if (match[2] === "draws") return send(200, { draws });
    if (match[2].endsWith("/offers")) return send(200, { offers: scenario === "expired" ? [{ id: run.offer_id, draw_id: run.draw_id, user_id: run.user_ids[0] }] : [] });
    const action = match[2].match(/^draws\/([^/]+)\/(run|approve)$/);
    if (action) return send(200, { ...draws.find(draw => draw.id === action[1]), state: action[2] === "approve" ? "approved" : "drawn" });
    return send(404, { error: "not found" });
  });
  bot.listen(0, "127.0.0.1");
  await once(bot, "listening");
  const app = buildSimulator({ ...tokens, botURL: `http://127.0.0.1:${bot.address().port}`, timeoutMS: 500 });
  await app.listen({ host: "127.0.0.1", port: 0 });
  base = `http://127.0.0.1:${app.server.address().port}`;
  t.after(async () => { await app.close(); await new Promise(resolve => bot.close(resolve)); });
  async function request(path, { body, headers = {}, method = body === undefined ? "GET" : "POST" } = {}) {
    const response = await fetch(`${base}${path}`, {
      method,
      headers: { ...(body !== undefined ? { "content-type": "application/json" } : {}), ...headers },
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    });
    return { status: response.status, body: await response.json() };
  }
  async function rawRequest(path, { body = {}, headers = {}, method = "POST" } = {}) {
    return new Promise((resolve, reject) => {
      const outgoing = httpRequest({
        hostname: "127.0.0.1", port: app.server.address().port, path, method,
        headers: { "content-type": "application/json", authorization: tokens.maxToken, ...headers },
      }, response => {
        const chunks = [];
        response.on("data", chunk => chunks.push(chunk));
        response.on("error", reject);
        response.on("end", () => resolve({ status: response.statusCode, body: JSON.parse(Buffer.concat(chunks)) }));
      });
      outgoing.on("error", reject);
      outgoing.end(JSON.stringify(body));
    });
  }
  return { request, rawRequest, calls, webhookFailures, base, run: async scenario => {
    const result = await request("/api/runs", { body: { scenario } });
    assert.equal(result.status, 201);
    return result.body;
  } };
}

test("start and text updates use the user's campaign and secret", async t => {
  const h = await harness(t);
  const run = await h.run("standard");
  const user = run.user_ids[0];
  assert.deepEqual((await h.request(`/api/users/${user}/start`, { body: {} })).body, { status: "accepted" });
  assert.equal((await h.request(`/api/users/${user}/messages`, { body: { text: "Иван Иванов" } })).status, 200);
  const webhookCalls = h.calls.filter(call => call.path === "/webhooks/max");
  assert.equal(webhookCalls.length, 2);
  assert.equal(webhookCalls[0].body.update_type, "bot_started");
  assert.equal(webhookCalls[1].body.message.sender.user_id, user);
  assert.equal(webhookCalls[1].body.message.body.text, "Иван Иванов");
  assert.equal(webhookCalls[1].headers["x-demo-campaign"], run.campaign_id);
  const snapshot = (await h.request(`/api/users/${user}`)).body;
  assert.equal(snapshot.messages[0].source, "user");
  assert.equal(snapshot.messages[0].text, "Иван Иванов");
  assert.equal(snapshot.last_webhook.body.accepted, true);
  assert.equal(JSON.stringify(snapshot).includes(tokens.demoToken), false);
});

test("MAX messages preserve buttons and callbacks are acknowledged", async t => {
  const h = await harness(t);
  const run = await h.run("standard");
  const [user, anotherUser] = run.user_ids;
  const attachments = [{ type: "inline_keyboard", payload: { buttons: [[{ type: "callback", text: "Получить код", payload: "promo" }]] } }];
  const outgoing = await h.request(`/messages?user_id=${user}`, { headers: { authorization: tokens.maxToken }, body: { text: "Выберите действие", attachments } });
  assert.equal(outgoing.status, 200);
  const mid = outgoing.body.message.body.mid;
  assert.ok(mid);
  assert.deepEqual(outgoing.body.message.body.attachments, attachments);
  const other = await h.request(`/api/users/${anotherUser}/callbacks`, { body: { mid, payload: "promo" } });
  assert.equal(other.status, 404);
  assert.equal((await h.request(`/api/users/${user}/callbacks`, { body: { mid, payload: "unknown" } })).status, 400);
  assert.equal((await h.request(`/api/users/${user}/callbacks`, { body: { mid, payload: "promo" } })).status, 200);
  const snapshot = (await h.request(`/api/users/${user}`)).body;
  assert.equal(snapshot.callback_acknowledged, true);
  assert.equal(snapshot.messages.length, 1);
  assert.equal(snapshot.messages[0].source, "bot");
  assert.deepEqual(snapshot.messages[0].attachments, attachments);
  assert.equal(h.calls.filter(call => call.path === "/webhooks/max").length, 1);
});

test("membership states are isolated by user and campaign channel", async t => {
  const h = await harness(t);
  const run = await h.run("standard");
  const [user, anotherUser] = run.user_ids;
  const native = path => h.request(path, { headers: { authorization: tokens.maxToken } });
  assert.deepEqual((await native(`/chats/-43/members?user_ids=${user}`)).body.members, []);
  assert.equal((await h.request(`/api/users/${user}/subscription`, { body: { status: "yes" } })).status, 200);
  assert.equal((await native(`/chats/-43/members?user_ids=${user}`)).body.members[0].user_id, user);
  assert.deepEqual((await native(`/chats/-43/members?user_ids=${anotherUser}`)).body.members, []);
  await h.request(`/api/users/${user}/subscription`, { body: { status: "error" } });
  assert.equal((await native(`/chats/-43/members?user_ids=${user}`)).status, 503);
  assert.equal((await native(`/chats/-999/members?user_ids=${anotherUser}`)).status, 404);
  assert.equal((await native(`/chats/-43/members?user_ids=1.5`)).status, 400);
  assert.equal((await h.request(`/chats/-43/members?user_ids=${user}`)).status, 401);
});

test("replay preserves the original update and retries a failed webhook", async t => {
  const h = await harness(t);
  const run = await h.run("standard");
  const user = run.user_ids[0];
  assert.equal((await h.request(`/api/users/${user}/replay`, { body: {} })).status, 409);
  h.webhookFailures.push(503);
  assert.equal((await h.request(`/api/users/${user}/messages`, { body: { text: "/start" } })).status, 502);
  assert.equal((await h.request(`/api/users/${user}`)).body.last_webhook.status, 503);
  assert.equal((await h.request(`/api/users/${user}/replay`, { body: {} })).status, 200);
  assert.equal((await h.request(`/api/users/${user}/replay`, { body: {} })).status, 200);
  const snapshots = h.calls.filter(call => call.path === "/webhooks/max").map(call => call.body);
  assert.deepEqual(snapshots[0], snapshots[1]);
  assert.deepEqual(snapshots[1], snapshots[2]);
  const snapshot = (await h.request(`/api/users/${user}`)).body;
  assert.equal(snapshot.messages.length, 1);
  assert.equal(snapshot.last_webhook.duplicate, true);
});

test("draws and offers are scoped to simulator campaigns", async t => {
  const h = await harness(t);
  for (const scenario of ["drawable", "final", "expired"]) {
    const run = await h.run(scenario);
    const snapshot = await h.request(`/api/runs/${run.campaign_id}`);
    assert.equal(snapshot.status, 200);
    assert.equal(snapshot.body.draws.length, 2);
    if (scenario === "expired") assert.equal(snapshot.body.offers[0].user_id, run.user_ids[0]);
    assert.equal((await h.request(`/api/runs/${run.campaign_id}/draws/${run.final_draw_id}/run`, { body: {} })).body.state, "drawn");
    assert.equal((await h.request(`/api/runs/${run.campaign_id}/draws/${run.draw_id}/approve`, { body: {} })).body.state, "approved");
    assert.equal((await h.request(`/api/runs/${run.campaign_id}/draws/unrelated/approve`, { body: {} })).status, 404);
  }
  assert.equal((await h.request("/api/runs/unrelated")).status, 404);
});

test("browser controls reject foreign origins, unexpected fields and upstream controls", async t => {
  const h = await harness(t);
  assert.equal((await h.request("/api/runs", { body: { scenario: "standard" }, headers: { origin: "https://example.org" } })).status, 403);
  assert.equal((await h.request("/api/runs", { body: { scenario: "standard" }, headers: { "sec-fetch-site": "cross-site" } })).status, 403);
  assert.equal((await h.request("/api/runs", { body: { scenario: "standard", url: "http://example.org" } })).status, 400);
  assert.equal((await h.request("/_control/users", { body: { user_id: 99, first_name: "User" } })).status, 404);
  assert.equal((await h.request("/subscriptions", { body: { url: "http://example.org" }, headers: { authorization: tokens.maxToken } })).status, 404);
  assert.equal((await h.request("/messages?user_id=42", { body: { text: "hello" } })).status, 401);
  assert.equal(h.calls.length, 0);
});

test("upstream controls stay inaccessible through alternate request targets", async t => {
  const h = await harness(t);
  const run = await h.run("standard");
  const user = run.user_ids[0];
  await h.request(`/messages?user_id=${user}`, { headers: { authorization: tokens.maxToken }, body: { text: "Keep this message" } });
  const targets = [
    "/_control/reset", "/%5fcontrol/reset", "/_%63ontrol/reset", "/%5Fcontrol/reset",
    "/_control%2freset", "/_control%2Freset", "/%255fcontrol/reset", "/%25255fcontrol/reset",
    "//_control/reset", "/safe/../_control/reset", "/safe/%2e%2e/_control/reset", "/_control\\reset",
    "/subscriptions", "/%73ubscriptions", "/subscript%69ons", "/%2573ubscriptions", "/subscriptions/",
    "/subscriptions%3Fignored", "/subscriptions%00", "/subscriptions%252F", "/%", "/%2", "/%gg",
  ];
  for (const path of targets) {
    const result = await h.rawRequest(path, { body: { url: "http://127.0.0.1/webhook" } });
    assert.ok([400, 404].includes(result.status), `${path}: HTTP ${result.status}`);
  }
  for (const method of ["GET", "DELETE"]) {
    assert.equal((await h.rawRequest("/%73ubscriptions?url=http%3A%2F%2Fexample.org", { method })).status, 404);
  }
  const snapshot = (await h.request(`/api/users/${user}`)).body;
  assert.equal(snapshot.messages.length, 1);
  assert.equal(snapshot.messages[0].text, "Keep this message");
  assert.equal((await h.rawRequest("/%61pi/runs", { body: { scenario: "standard" }, headers: { origin: "https://example.org" } })).status, 404);
  assert.equal((await h.request("/me")).status, 401);
  assert.equal((await h.request("/me", { headers: { authorization: tokens.maxToken } })).status, 200);
});

test("banner attachment preserves the local HTTPS image and serves its actual PNG bytes", async t => {
  const h = await harness(t);
  const run = await h.run("standard");
  const user = run.user_ids[0];
  const sent = await h.request(`/messages?user_id=${user}`, {
    headers: { authorization: tokens.maxToken },
    body: { text: "Welcome", attachments: [{ type: "image", payload: { url: bannerURL } }] },
  });
  assert.equal(sent.status, 200);
  const snapshot = (await h.request(`/api/users/${user}`)).body;
  const attachment = snapshot.messages[0].attachments[0];
  assert.equal(attachment.type, "image");
  assert.equal(attachment.payload.url, bannerURL);
  assert.equal(imageURL(attachment.payload.url, h.base), bannerPath);

  const browserAsset = await fetch(h.base + bannerPath);
  assert.equal(browserAsset.status, 200);
  assert.equal(browserAsset.headers.get("content-type"), "image/png");
  assert.equal(browserAsset.headers.get("x-content-type-options"), "nosniff");
  const image = Buffer.from(await browserAsset.arrayBuffer());
  assert.deepEqual(image.subarray(0, 8), Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]));
  assert.equal(image.readUInt32BE(16), 1200);
  assert.equal(image.readUInt32BE(20), 480);
  const compressed = [];
  for (let position = 8; position < image.length;) {
    const length = image.readUInt32BE(position);
    if (image.toString("ascii", position + 4, position + 8) === "IDAT") compressed.push(image.subarray(position + 8, position + 8 + length));
    position += length + 12;
  }
  assert.ok(inflateSync(Buffer.concat(compressed)).length > 1200 * 480 * 3);

  const banner = await buildBannerServer();
  await new Promise(resolve => banner.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise((resolve, reject) => banner.close(error => error ? reject(error) : resolve())));
  const ca = await readFile(resolve(tlsDirectory, "cert.pem"));
  async function secureAsset(path, options = {}) {
    return new Promise((resolve, reject) => {
      const request = httpsRequest({ hostname: "127.0.0.1", port: banner.address().port, servername: "simulator", path, ca, ...options }, response => {
        const chunks = [];
        response.on("data", chunk => chunks.push(chunk));
        response.on("error", reject);
        response.on("end", () => resolve({ status: response.statusCode, headers: response.headers, body: Buffer.concat(chunks) }));
      });
      request.on("error", reject);
      request.end();
    });
  }
  const secure = await secureAsset(bannerPath);
  assert.equal(secure.status, 200);
  assert.equal(secure.headers["content-type"], "image/png");
  assert.deepEqual(secure.body, image);
  const head = await secureAsset(bannerPath, { method: "HEAD" });
  assert.equal(head.status, 200);
  assert.equal(Number(head.headers["content-length"]), image.length);
  assert.equal(head.body.length, 0);
  assert.equal((await secureAsset("/_control/reset", { method: "POST" })).status, 404);
  assert.equal((await secureAsset(bannerPath, { method: "POST" })).status, 405);
  await assert.rejects(secureAsset(bannerPath, { ca: undefined }), { code: "DEPTH_ZERO_SELF_SIGNED_CERT" });
  await assert.rejects(secureAsset(bannerPath, { servername: "unrelated" }), { code: "ERR_TLS_CERT_ALTNAME_INVALID" });

  const page = await fetch(h.base);
  assert.match(page.headers.get("content-security-policy"), /img-src 'self' https:/);
  assert.match(await page.text(), /src="\/assets\/app\.js" type="module"/);
  const module = await fetch(h.base + "/assets/media.js");
  assert.equal(module.status, 200);
  assert.match(module.headers.get("content-type"), /^text\/javascript/);
  assert.equal((await h.rawRequest("/assets/../src/banner.mjs", { method: "GET" })).status, 404);
  assert.equal((await fetch(h.base + "/assets/key.pem")).status, 404);
});

test("image URLs reject executable schemes, credentials and foreign HTTP origins", () => {
  const origin = "http://127.0.0.1:4010";
  assert.equal(imageURL(bannerURL, origin), bannerPath);
  assert.equal(imageURL(origin + "/_content/up1-abc", origin), origin + "/_content/up1-abc");
  assert.equal(imageURL("https://cdn.example.org/banner.png?signature=abc", origin), "https://cdn.example.org/banner.png?signature=abc");
  for (const value of [undefined, null, {}, "", "javascript:alert(1)", "data:image/png;base64,AA==", "http://example.org/image.png", "https://user:secret@example.org/image.png", "https://example.org/banner.png#fragment", origin + "/api/runs", origin + "/_content/up1?key=1"]) {
    assert.equal(imageURL(value, origin), null, String(value));
  }
  assert.notEqual(imageURL(bannerURL + "?unexpected=1", origin), bannerPath);
  assert.notEqual(imageURL("https://simulator:4011/assets/%62anner.png", origin), bannerPath);
});

test("environment requires fixed service origins and all credentials", () => {
  const environment = { BOT_URL: "http://bot:8080", MAX_TOKEN: "max", MAX_WEBHOOK_SECRET: "secret", DEMO_TOKEN: "demo", ADMIN_TOKEN: "admin" };
  assert.equal(loadConfig(environment).botURL, "http://bot:8080");
  for (const name of Object.keys(environment)) assert.throws(() => loadConfig({ ...environment, [name]: " " }));
  for (const value of ["ftp://bot", "http://user:password@bot", "http://bot/path", "http://bot?query=1", "http://bot#fragment"]) {
    assert.throws(() => loadConfig({ ...environment, BOT_URL: value }));
  }
});
