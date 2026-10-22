import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { buildServer } from "../vendor/server.js";
import { createState, createUser, getOrCreateDialog, userPressesCallback, userSendsMessage, userStartsBot } from "../vendor/state.js";

const scenarios = ["standard", "exhausted", "drawable", "final", "expired"];
const subscriptions = ["yes", "no", "error"];
const assetTypes = { "app.js": "text/javascript; charset=utf-8", "media.js": "text/javascript; charset=utf-8", "style.css": "text/css; charset=utf-8", "banner.png": "image/png" };
const publicDirectory = fileURLToPath(new URL("../public/", import.meta.url));

function failure(status, message) {
  return Object.assign(new Error(message), { statusCode: status });
}

function required(environment, name) {
  const value = environment[name]?.trim();
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function requestPath(request) {
  return (request.raw.url ?? request.url).split("?", 1)[0];
}

function canonicalPath(path) {
  return path.startsWith("/") && !/[\\%]/.test(path) && !path.includes("//") && !path.split("/").some(segment => segment === "." || segment === "..");
}

function controlPath(path) {
  return path.startsWith("/_control") || path === "/subscriptions" || path.startsWith("/subscriptions/");
}

export function loadConfig(environment) {
  const botURL = new URL(required(environment, "BOT_URL"));
  if (!["http:", "https:"].includes(botURL.protocol) || botURL.username || botURL.password || botURL.search || botURL.hash || botURL.pathname !== "/") {
    throw new Error("BOT_URL must contain only an HTTP origin");
  }
  return {
    botURL: botURL.origin,
    maxToken: required(environment, "MAX_TOKEN"),
    webhookSecret: required(environment, "MAX_WEBHOOK_SECRET"),
    demoToken: required(environment, "DEMO_TOKEN"),
    adminToken: required(environment, "ADMIN_TOKEN"),
  };
}

export function buildSimulator(config) {
  const state = createState({ user_id: 1, first_name: "Промоакции", username: "promo_simulator", is_bot: true }, config.maxToken);
  const app = buildServer(state);
  const users = new Map();
  const runs = new Map();
  const acknowledgements = new Map();

  app.addHook("onRequest", async (request, reply) => {
    const path = requestPath(request);
    if (!canonicalPath(path) || controlPath(path) || controlPath(request.routeOptions.url ?? "")) {
      return reply.code(404).send({ error: "Route not found" });
    }
    if (path.startsWith("/api/")) {
      const origin = request.headers.origin;
      const expected = `${request.protocol}://${request.headers.host}`;
      if ((origin && origin !== expected) || request.headers["sec-fetch-site"] === "cross-site") {
        return reply.code(403).send({ error: "Same-origin requests are required" });
      }
      if (!["GET", "POST"].includes(request.method)) {
        return reply.code(405).send({ error: "Method not allowed" });
      }
    }
    if (path.startsWith("/api/") || path === "/" || path.startsWith("/assets/")) request.headers.authorization = state.token;
  });

  app.addHook("preHandler", async (request, reply) => {
    if (controlPath(request.routeOptions.url ?? "")) return reply.code(404).send({ error: "Route not found" });
    const path = requestPath(request);
    if (path.startsWith("/api/") || path === "/" || path.startsWith("/assets/")) return;
    if (path === "/messages" && request.method === "POST") {
      const id = Number(request.query.user_id);
      if (!Number.isSafeInteger(id) || id < 1) return reply.code(400).send({ error: "A valid user_id is required" });
    }
  });

  app.addHook("onResponse", async (request, reply) => {
    if (request.url.split("?", 1)[0] === "/answers" && request.method === "POST" && reply.statusCode < 300) {
      const callbackID = request.query.callback_id;
      if (typeof callbackID === "string") acknowledgements.set(callbackID, Date.now());
    }
  });

  async function bot(path, { method = "GET", body, token = config.adminToken, campaign } = {}) {
    let response;
    try {
      response = await fetch(`${config.botURL}${path}`, {
        method,
        redirect: "error",
        signal: AbortSignal.timeout(config.timeoutMS ?? 10_000),
        headers: {
          "content-type": "application/json",
          ...(token ? { authorization: `Bearer ${token}` } : {}),
          ...(campaign ? { "x-demo-campaign": campaign, "x-max-bot-api-secret": config.webhookSecret } : {}),
        },
        ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
      });
    } catch {
      throw failure(502, "Bot service is unavailable");
    }
    const text = await response.text();
    let result;
    try { result = JSON.parse(text); } catch { throw failure(502, "Bot service returned an invalid response"); }
    if (!response.ok) {
      throw Object.assign(failure(response.status >= 500 ? 502 : response.status, "Bot request failed"), { upstreamStatus: response.status });
    }
    return result;
  }

  function userFor(id) {
    const user = users.get(Number(id));
    if (!user) throw failure(404, "User not found");
    return user;
  }

  function runFor(campaign) {
    const run = runs.get(campaign);
    if (!run) throw failure(404, "Campaign not found");
    return run;
  }

  async function deliver(binding, update) {
    binding.lastUpdate = structuredClone(update);
    try {
      const result = await bot("/webhooks/max", { method: "POST", body: update, token: null, campaign: binding.campaignID });
      binding.lastWebhook = {
        status: 200,
        timestamp: Date.now(),
        body: result,
        ...(typeof result.duplicate === "boolean" ? { duplicate: result.duplicate } : {}),
        ...(typeof result.event_id === "string" ? { event_id: result.event_id } : {}),
      };
      return { status: "accepted" };
    } catch (error) {
      binding.lastWebhook = { status: error.upstreamStatus ?? 0, timestamp: Date.now(), error: error.message };
      throw error;
    }
  }

  function serialize(binding, operation) {
    const pending = binding.pending.catch(() => {}).then(operation);
    binding.pending = pending;
    return pending;
  }

  function bodySchema(properties = {}, requiredFields = []) {
    return { type: "object", additionalProperties: false, properties, required: requiredFields };
  }

  function route(method, url, body, handler) {
    app.route({
      method, url, bodyLimit: 16_384,
      ...(body ? {
        schema: { body },
        preValidation: async request => {
          if (request.body && Object.keys(request.body).some(key => !(key in body.properties))) throw failure(400, "Invalid request");
        },
      } : {}),
      handler,
    });
  }

  route("POST", "/api/runs", bodySchema({ scenario: { type: "string", enum: scenarios } }, ["scenario"]), async (request, reply) => {
    const run = await bot("/_demo/runs", { method: "POST", body: request.body, token: config.demoToken });
    if (!run || typeof run.campaign_id !== "string" || !Array.isArray(run.user_ids) || run.user_ids.some(id => !Number.isSafeInteger(id) || id < 1)) {
      throw failure(502, "Bot service returned an invalid campaign");
    }
    const campaign = encodeURIComponent(run.campaign_id);
    const [stats, drawList] = await Promise.all([
      bot(`/api/admin/campaigns/${campaign}/stats`),
      bot(`/api/admin/campaigns/${campaign}/draws`),
    ]);
    const partners = stats.promo_remaining;
    if (!Array.isArray(partners) || !partners.some(partner => partner.days?.some(day => day.day_id === run.day_id)) || !Array.isArray(drawList.draws)) {
      throw failure(502, "Bot service returned an invalid campaign configuration");
    }
    const channels = new Set(partners.map(partner => partner.channel_id));
    const drawIDs = new Set(drawList.draws.map(draw => draw.id));
    for (const id of run.user_ids) {
      if (users.has(id)) throw failure(502, "Bot service returned an existing user");
    }
    for (const [index, id] of run.user_ids.entries()) {
      const user = createUser(state, id, `Участник ${index + 1}`);
      getOrCreateDialog(state, id);
      users.set(id, { user, campaignID: run.campaign_id, channels, subscription: "no", pending: Promise.resolve() });
    }
    runs.set(run.campaign_id, { ...run, drawIDs });
    return reply.code(201).send(run);
  });

  route("GET", "/api/users/:id", null, async request => {
    const binding = userFor(request.params.id);
    const dialog = getOrCreateDialog(state, binding.user.user_id);
    return {
      user_id: binding.user.user_id,
      campaign_id: binding.campaignID,
      subscription: binding.subscription,
      messages: (state.messagesByChat.get(dialog.chat_id) ?? []).map(({ source, message }) => ({
        source,
        mid: message.body.mid,
        text: message.body.text,
        attachments: message.body.attachments ?? [],
        timestamp: message.timestamp,
      })),
      ...(binding.lastWebhook ? { last_webhook: binding.lastWebhook } : {}),
      ...(binding.lastUpdate?.update_type === "message_callback" ? { callback_acknowledged: acknowledgements.has(binding.lastUpdate.callback.callback_id) } : {}),
    };
  });

  route("POST", "/api/users/:id/start", bodySchema(), async request => {
    const binding = userFor(request.params.id);
    return serialize(binding, () => deliver(binding, userStartsBot(state, binding.user)));
  });

  route("POST", "/api/users/:id/messages", bodySchema({ text: { type: "string", minLength: 1, maxLength: 4000 } }, ["text"]), async request => {
    const binding = userFor(request.params.id);
    return serialize(binding, () => deliver(binding, userSendsMessage(state, binding.user, request.body.text)));
  });

  route("POST", "/api/users/:id/callbacks", bodySchema({ mid: { type: "string", minLength: 1, maxLength: 256 }, payload: { type: "string", minLength: 1, maxLength: 512 } }, ["mid", "payload"]), async request => {
    const binding = userFor(request.params.id);
    return serialize(binding, async () => {
      const { mid, payload } = request.body;
      const stored = state.messagesByMid.get(mid);
      if (!stored || stored.source !== "bot" || stored.message.recipient.user_id !== binding.user.user_id) throw failure(404, "Message not found");
      const keyboards = stored.message.body.attachments?.filter(attachment => attachment.type === "inline_keyboard") ?? [];
      const hasButton = keyboards.some(keyboard => keyboard.payload?.buttons?.some(row => row.some(button => button.type === "callback" && button.payload === payload)));
      if (!hasButton) throw failure(400, "Callback button not found");
      return deliver(binding, userPressesCallback(state, binding.user, mid, payload));
    });
  });

  route("POST", "/api/users/:id/replay", bodySchema(), async request => {
    const binding = userFor(request.params.id);
    return serialize(binding, () => {
      if (!binding.lastUpdate) throw failure(409, "There is no update to replay");
      return deliver(binding, binding.lastUpdate);
    });
  });

  route("POST", "/api/users/:id/subscription", bodySchema({ status: { type: "string", enum: subscriptions } }, ["status"]), async request => {
    const binding = userFor(request.params.id);
    binding.subscription = request.body.status;
    return { status: binding.subscription };
  });

  app.get("/chats/:chatId/members", async (request, reply) => {
    const chatID = Number(request.params.chatId);
    const userIDs = typeof request.query.user_ids === "string" ? request.query.user_ids.split(",").map(Number) : [];
    if (!Number.isSafeInteger(chatID) || !userIDs.length || userIDs.some(id => !Number.isSafeInteger(id) || id < 1)) {
      return reply.code(400).send({ code: "proto.payload", message: "Valid chat and user_ids are required" });
    }
    const bindings = userIDs.map(id => users.get(id));
    if (bindings.some(binding => !binding || !binding.channels.has(chatID))) {
      return reply.code(404).send({ code: "chat.not.found", message: "Channel not found" });
    }
    if (bindings.some(binding => binding.subscription === "error")) {
      return reply.code(503).send({ code: "service.unavailable", message: "Membership service is unavailable" });
    }
    return { members: bindings.filter(binding => binding.subscription === "yes").map(binding => ({ ...binding.user })) };
  });

  route("GET", "/api/runs/:campaign", null, async request => {
    const run = runFor(request.params.campaign);
    const campaign = encodeURIComponent(run.campaign_id);
    const [drawList, stats] = await Promise.all([
      bot(`/api/admin/campaigns/${campaign}/draws`),
      bot(`/api/admin/campaigns/${campaign}/stats`),
    ]);
    const offerLists = await Promise.all([...run.drawIDs].map(draw => bot(`/api/admin/campaigns/${campaign}/draws/${encodeURIComponent(draw)}/offers`)));
    return { draws: drawList.draws ?? [], stats, offers: offerLists.flatMap(result => result.offers ?? []) };
  });

  for (const action of ["run", "approve"]) {
    route("POST", `/api/runs/:campaign/draws/:draw/${action}`, bodySchema(), async request => {
      const run = runFor(request.params.campaign);
      if (!run.drawIDs.has(request.params.draw)) throw failure(404, "Draw not found");
      return bot(`/api/admin/campaigns/${encodeURIComponent(run.campaign_id)}/draws/${encodeURIComponent(request.params.draw)}/${action}`, { method: "POST", body: {} });
    });
  }

  app.get("/", async (_request, reply) => {
    reply.header("content-security-policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' https:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'");
    reply.header("x-content-type-options", "nosniff");
    reply.header("cache-control", "no-store");
    return reply.type("text/html; charset=utf-8").send(await readFile(resolve(config.publicDirectory ?? publicDirectory, "index.html")));
  });
  app.get("/assets/:file", async (request, reply) => {
    const type = assetTypes[request.params.file];
    if (!type) throw failure(404, "Asset not found");
    reply.header("x-content-type-options", "nosniff");
    reply.header("cache-control", "no-cache");
    return reply.type(type).send(await readFile(resolve(config.publicDirectory ?? publicDirectory, request.params.file)));
  });

  app.setErrorHandler((error, _request, reply) => {
    const status = error.statusCode >= 400 && error.statusCode < 600 ? error.statusCode : 500;
    reply.code(status).send({ error: status === 500 ? "Simulator request failed" : error.validation ? "Invalid request" : error.message });
  });
  return app;
}
