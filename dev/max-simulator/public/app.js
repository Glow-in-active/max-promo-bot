import { imageURL } from "./media.js";

const byId = (id) => document.getElementById(id);
const elements = Object.fromEntries([
  "scenario", "scenario-description", "new-run", "user", "subscription", "replay",
  "delivery-status", "connection-dot", "connection-status", "start", "chat-subtitle",
  "messages", "composer", "message", "send", "error", "error-text", "dismiss-error",
  "stat-users", "stat-issued", "stat-entries", "stat-remaining", "draw", "draw-state",
  "draw-run", "draw-approve", "draw-prize", "offers", "offers-empty",
].map((id) => [id, byId(id)]));

const scenarios = {
  standard: "Промокод, проверка подписки и участие в розыгрыше.",
  exhausted: "Промокоды партнёра закончились.",
  drawable: "Участники собраны, дневной розыгрыш готов.",
  final: "Финальный розыгрыш с учётом накопленных шансов.",
  expired: "Проверка выдачи приза резервному участнику.",
};
const drawStates = { open: "Ожидает розыгрыша", drawn: "Ожидает подтверждения", approved: "Результат подтверждён" };
const offerStates = { pending: "Ожидает отправки", notified: "Ожидает получения", claimed: "Приз получен", expired: "Срок истёк", declined: "Участник отказался", cancelled: "Отменено" };
const storageKey = "promo.max-simulator.run";
const numberFormat = new Intl.NumberFormat("ru-RU");
const timeFormat = new Intl.DateTimeFormat("ru-RU", { hour: "2-digit", minute: "2-digit" });

const state = {
  run: null,
  user: null,
  details: null,
  userData: null,
  generation: 0,
  busy: false,
  connected: false,
  pollTimer: null,
  pollController: null,
  messageSignature: null,
  offersSignature: null,
  drawsSignature: null,
};

class APIError extends Error {
  constructor(message, status) {
    super(message);
    this.status = status;
  }
}

async function request(path, options = {}) {
  const controller = new AbortController();
  const timeout = window.setTimeout(() => controller.abort(), 12000);
  const externalSignal = options.signal;
  const cancel = () => controller.abort();
  externalSignal?.addEventListener("abort", cancel, { once: true });
  if (externalSignal?.aborted) controller.abort();
  try {
    const response = await fetch(path, {
      ...options,
      signal: controller.signal,
      headers: { "Content-Type": "application/json", ...options.headers },
    });
    const body = await response.json().catch(() => null);
    if (!response.ok) {
      const detail = typeof body?.error === "string" ? body.error : body?.error?.message || body?.message;
      throw new APIError(detail || `Сервер вернул ошибку ${response.status}.`, response.status);
    }
    return body;
  } catch (error) {
    if (externalSignal?.aborted) throw error;
    if (error instanceof APIError) throw error;
    throw new APIError(error.name === "AbortError" ? "Сервер не ответил вовремя." : "Нет связи с симулятором.", 0);
  } finally {
    window.clearTimeout(timeout);
    externalSignal?.removeEventListener("abort", cancel);
  }
}

function post(path, body = {}) {
  return request(path, { method: "POST", body: JSON.stringify(body) });
}

function showError(error) {
  elements["error-text"].textContent = error.message || "Не получилось выполнить действие.";
  elements.error.hidden = false;
}

function clearError() {
  elements.error.hidden = true;
  elements["error-text"].textContent = "";
}

function setConnection(connected) {
  state.connected = connected;
  elements["connection-status"].textContent = connected ? "Симулятор доступен" : "Нет связи";
  elements["connection-dot"].className = `status-dot ${connected ? "online" : "offline"}`;
}

function saveSelection() {
  try {
    if (state.run) {
      localStorage.setItem(storageKey, JSON.stringify({ run: state.run, user: state.user, scenario: elements.scenario.value }));
    } else {
      localStorage.removeItem(storageKey);
    }
  } catch {}
}

function setBusy(busy) {
  state.busy = busy;
  elements["chat-subtitle"].textContent = busy ? "Выполняем..." : state.user == null ? "Выберите сценарий" : userLabel(state.user);
  elements.messages.setAttribute("aria-busy", String(busy));
  renderControls();
}

function selectedDraw() {
  return state.details?.draws?.find((draw) => draw.id === elements.draw.value);
}

function registrationOpen(draw) {
  return draw?.state === "open" && draw.closes_at && new Date(draw.closes_at).getTime() > Date.now();
}

function renderControls() {
  const unavailable = !state.run || state.busy;
  elements["new-run"].disabled = state.busy;
  elements["new-run"].textContent = state.busy && !state.run ? "Создаём..." : "Новый сценарий";
  elements.scenario.disabled = state.busy;
  elements.user.disabled = unavailable;
  elements.subscription.disabled = unavailable || !state.userData;
  elements.start.disabled = unavailable;
  elements.message.disabled = unavailable;
  elements.send.disabled = unavailable || !elements.message.value.trim();
  elements.replay.disabled = unavailable || !state.userData?.last_webhook;
  elements.draw.disabled = unavailable || !state.details?.draws?.length;
  const draw = selectedDraw();
  elements["draw-run"].disabled = unavailable || !draw || draw.state !== "open" || registrationOpen(draw);
  elements["draw-approve"].disabled = unavailable || !draw || draw.state !== "drawn";
  elements.messages.querySelectorAll("button").forEach((button) => { button.disabled = unavailable; });
  elements.offers.querySelectorAll("button").forEach((button) => { button.disabled = unavailable; });
}

function userLabel(userID) {
  const index = state.run?.user_ids?.findIndex((id) => String(id) === String(userID)) ?? -1;
  return index < 0 ? `Участник ${userID}` : `Участник ${index + 1}`;
}

function renderUsers() {
  elements.user.replaceChildren();
  for (const userID of state.run?.user_ids || []) {
    const option = document.createElement("option");
    option.value = String(userID);
    option.textContent = userLabel(userID);
    elements.user.append(option);
  }
  elements.user.value = String(state.user ?? "");
  elements["chat-subtitle"].textContent = state.user == null ? "Выберите сценарий" : userLabel(state.user);
}

function emptyChat() {
  const container = document.createElement("div");
  container.className = "chat-empty";
  const icon = document.createElement("span");
  icon.textContent = "✳";
  icon.setAttribute("aria-hidden", "true");
  const heading = document.createElement("h2");
  heading.textContent = "Поговорите с ботом";
  const text = document.createElement("p");
  text.textContent = "Нажмите «Начать», затем выбирайте кнопки в сообщениях.";
  container.append(icon, heading, text);
  return container;
}

function safeLink(value) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" || url.protocol === "http:" ? url.href : null;
  } catch {
    return null;
  }
}

function renderMessage(message) {
  const article = document.createElement("article");
  article.className = `message ${message.source === "user" ? "user" : "bot"}`;
  article.setAttribute("aria-label", message.source === "user" ? "Вы" : "Бот");
  const bubble = document.createElement("div");
  bubble.className = "bubble";
  for (const attachment of message.attachments || []) {
    if (attachment.type !== "image") continue;
    const source = imageURL(attachment.payload?.url, window.location.origin);
    if (!source) continue;
    const image = document.createElement("img");
    image.className = "message-image";
    image.src = source;
    image.alt = "Баннер акции";
    image.referrerPolicy = "no-referrer";
    image.decoding = "async";
    bubble.append(image);
  }
  const text = document.createElement("p");
  text.className = "message-text";
  text.textContent = message.text || "";
  bubble.append(text);
  const timestamp = new Date(message.timestamp);
  if (!Number.isNaN(timestamp.getTime())) {
    const time = document.createElement("time");
    time.className = "message-time";
    time.dateTime = timestamp.toISOString();
    time.textContent = timeFormat.format(timestamp);
    bubble.append(time);
  }
  article.append(bubble);
  for (const attachment of message.attachments || []) {
    if (attachment.type !== "inline_keyboard" || !Array.isArray(attachment.payload?.buttons)) continue;
    const keyboard = document.createElement("div");
    keyboard.className = "keyboard";
    for (const buttons of attachment.payload.buttons) {
      if (!Array.isArray(buttons)) continue;
      const row = document.createElement("div");
      row.className = "keyboard-row";
      for (const definition of buttons) {
        let button;
        if (definition.type === "callback" && typeof definition.payload === "string") {
          button = document.createElement("button");
          button.type = "button";
          button.addEventListener("click", () => userAction("callbacks", { mid: message.mid, payload: definition.payload }));
          button.disabled = state.busy;
        } else if (definition.type === "link") {
          const href = safeLink(definition.url);
          if (!href) continue;
          button = document.createElement("a");
          button.href = href;
          button.target = "_blank";
          button.rel = "noopener noreferrer";
        } else {
          continue;
        }
        button.textContent = definition.text || "Открыть";
        row.append(button);
      }
      if (row.childElementCount) keyboard.append(row);
    }
    if (keyboard.childElementCount) article.append(keyboard);
  }
  return article;
}

function renderUser(data) {
  state.userData = data;
  elements.subscription.value = data.subscription || "yes";
  const webhook = data.last_webhook;
  elements["delivery-status"].textContent = !webhook ? "" : webhook.error ? `Ошибка: ${webhook.error}` : webhook.duplicate ? "Повторное событие распознано" : "Последнее событие принято";
  const signature = JSON.stringify(data.messages || []);
  if (signature !== state.messageSignature) {
    const nearBottom = elements.messages.scrollHeight - elements.messages.scrollTop - elements.messages.clientHeight < 90;
    const initial = state.messageSignature == null;
    state.messageSignature = signature;
    const messages = data.messages || [];
    elements.messages.replaceChildren(...(messages.length ? messages.map(renderMessage) : [emptyChat()]));
    if (initial || nearBottom) elements.messages.scrollTop = elements.messages.scrollHeight;
  }
  renderControls();
}

function drawLabel(draw) {
  if (draw.kind === "final") return "Финальный";
  const daily = state.details.draws.filter((item) => item.kind !== "final");
  return daily.length > 1 ? `Дневной ${daily.findIndex((item) => item.id === draw.id) + 1}` : "Дневной";
}

function renderDraw() {
  const draw = selectedDraw();
  elements["draw-state"].textContent = registrationOpen(draw) ? "Приём заявок открыт" : draw ? drawStates[draw.state] || draw.state : "Розыгрышей пока нет";
  elements["draw-prize"].textContent = draw?.prize || "";
  renderControls();
}

function renderDetails(details) {
  state.details = details;
  const stats = details.stats || {};
  const values = {
    users: stats.users || 0,
    issued: stats.promo_issued || 0,
    entries: (details.draws || []).filter((draw) => draw.kind === "daily").reduce((sum, draw) => sum + Number(stats.entries?.[draw.id] || 0), 0),
    remaining: (stats.promo_remaining || []).reduce((sum, partner) => sum + Number(partner.remaining || 0), 0),
  };
  for (const [key, value] of Object.entries(values)) elements[`stat-${key}`].textContent = numberFormat.format(value);
  const drawsSignature = JSON.stringify((details.draws || []).map((draw) => ({ id: draw.id, kind: draw.kind })));
  if (drawsSignature !== state.drawsSignature) {
    state.drawsSignature = drawsSignature;
    const selected = elements.draw.value;
    elements.draw.replaceChildren();
    for (const draw of details.draws || []) {
      const option = document.createElement("option");
      option.value = draw.id;
      option.textContent = drawLabel(draw);
      elements.draw.append(option);
    }
    const preferred = selected || (elements.scenario.value === "final" ? state.run.final_draw_id : state.run.draw_id);
    if ((details.draws || []).some((draw) => draw.id === preferred)) elements.draw.value = preferred;
  }
  renderDraw();
  const signature = JSON.stringify(details.offers || []);
  if (signature === state.offersSignature) return;
  state.offersSignature = signature;
  elements.offers.replaceChildren();
  elements["offers-empty"].hidden = Boolean(details.offers?.length);
  for (const offer of details.offers || []) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "offer";
    const avatar = document.createElement("span");
    avatar.className = "offer-avatar";
    avatar.textContent = userLabel(offer.user_id).split(" ").at(-1);
    const label = document.createElement("span");
    label.className = "offer-label";
    const name = document.createElement("span");
    name.textContent = userLabel(offer.user_id);
    const status = document.createElement("small");
    status.textContent = offerStates[offer.status] || offer.status;
    label.append(name, status);
    const arrow = document.createElement("span");
    arrow.className = "offer-arrow";
    arrow.textContent = "›";
    arrow.setAttribute("aria-hidden", "true");
    button.append(avatar, label, arrow);
    button.addEventListener("click", () => selectUser(offer.user_id));
    button.disabled = state.busy;
    elements.offers.append(button);
  }
}

function stopPolling() {
  window.clearTimeout(state.pollTimer);
  state.pollController?.abort();
  state.pollTimer = null;
  state.pollController = null;
}

function schedulePoll(delay = 700) {
  window.clearTimeout(state.pollTimer);
  if (state.run && !document.hidden) state.pollTimer = window.setTimeout(refresh, delay);
}

function resetRun() {
  stopPolling();
  state.generation += 1;
  state.run = null;
  state.user = null;
  state.details = null;
  state.userData = null;
  state.messageSignature = null;
  state.offersSignature = null;
  state.drawsSignature = null;
  elements.message.value = "";
  elements.messages.replaceChildren(emptyChat());
  elements.draw.replaceChildren();
  elements["draw-state"].textContent = "Создайте сценарий";
  elements["draw-prize"].textContent = "";
  elements.offers.replaceChildren();
  elements["offers-empty"].hidden = false;
  elements["delivery-status"].textContent = "";
  for (const name of ["users", "issued", "entries", "remaining"]) elements[`stat-${name}`].textContent = "0";
  renderUsers();
  renderControls();
  saveSelection();
}

async function refresh() {
  if (!state.run || state.busy || state.pollController) return schedulePoll();
  const generation = state.generation;
  const controller = new AbortController();
  state.pollController = controller;
  try {
    const [user, details] = await Promise.all([
      request(`/api/users/${encodeURIComponent(state.user)}`, { signal: controller.signal }),
      request(`/api/runs/${encodeURIComponent(state.run.campaign_id)}`, { signal: controller.signal }),
    ]);
    if (generation !== state.generation || controller.signal.aborted) return;
    const reconnected = !state.connected;
    renderUser(user);
    renderDetails(details);
    setConnection(true);
    if (reconnected) clearError();
  } catch (error) {
    if (controller.signal.aborted || generation !== state.generation) return;
    setConnection(false);
    if (error.status === 404) {
      resetRun();
      showError(new Error("Сценарий завершён. Создайте новый."));
    } else {
      showError(error);
    }
  } finally {
    if (state.pollController === controller) state.pollController = null;
    if (generation === state.generation) schedulePoll(state.connected ? 700 : 2500);
  }
}

async function perform(action) {
  if (state.busy) return;
  stopPolling();
  setBusy(true);
  clearError();
  try {
    await action();
    setConnection(true);
  } catch (error) {
    if (!error.status) setConnection(false);
    showError(error);
  } finally {
    setBusy(false);
    schedulePoll(50);
  }
}

async function newRun() {
  await perform(async () => {
    const scenario = elements.scenario.value;
    const run = await post("/api/runs", { scenario });
    if (!run?.campaign_id || !run.user_ids?.length) throw new Error("Сервер не вернул участников сценария.");
    resetRun();
    state.run = run;
    state.user = run.user_ids[0];
    renderUsers();
    saveSelection();
  });
}

async function selectUser(userID) {
  if (state.busy || String(state.user) === String(userID)) return;
  if (!state.run?.user_ids.some((id) => String(id) === String(userID))) return;
  stopPolling();
  state.generation += 1;
  state.user = state.run.user_ids.find((id) => String(id) === String(userID));
  state.userData = null;
  state.messageSignature = null;
  elements.message.value = "";
  elements.messages.replaceChildren(emptyChat());
  elements["delivery-status"].textContent = "";
  renderUsers();
  renderControls();
  clearError();
  saveSelection();
  await refresh();
}

async function userAction(action, body = {}) {
  if (!state.run || state.user == null) return;
  await perform(() => post(`/api/users/${encodeURIComponent(state.user)}/${action}`, body));
}

async function drawAction(action) {
  const draw = selectedDraw();
  if (!state.run || !draw) return;
  await perform(() => post(`/api/runs/${encodeURIComponent(state.run.campaign_id)}/draws/${encodeURIComponent(draw.id)}/${action}`));
}

elements["new-run"].addEventListener("click", newRun);
elements.scenario.addEventListener("change", () => { elements["scenario-description"].textContent = scenarios[elements.scenario.value]; });
elements.user.addEventListener("change", () => selectUser(elements.user.value));
elements.subscription.addEventListener("change", () => userAction("subscription", { status: elements.subscription.value }));
elements.start.addEventListener("click", () => userAction("start"));
elements.replay.addEventListener("click", () => userAction("replay"));
elements["dismiss-error"].addEventListener("click", clearError);
elements.message.addEventListener("input", renderControls);
elements.draw.addEventListener("change", renderDraw);
elements["draw-run"].addEventListener("click", () => drawAction("run"));
elements["draw-approve"].addEventListener("click", () => drawAction("approve"));
elements.composer.addEventListener("submit", async (event) => {
  event.preventDefault();
  const text = elements.message.value.trim();
  if (!text || state.busy || !state.run) return;
  await perform(async () => {
    await post(`/api/users/${encodeURIComponent(state.user)}/messages`, { text });
    elements.message.value = "";
  });
  elements.message.focus();
});
document.addEventListener("visibilitychange", () => {
  if (document.hidden) stopPolling();
  else schedulePoll(0);
});
window.addEventListener("pagehide", stopPolling);

async function initialize() {
  let stored;
  try { stored = JSON.parse(localStorage.getItem(storageKey)); } catch { stored = null; }
  if (Object.hasOwn(scenarios, stored?.scenario)) elements.scenario.value = stored.scenario;
  elements["scenario-description"].textContent = scenarios[elements.scenario.value];
  if (!stored?.run?.campaign_id || !Array.isArray(stored.run.user_ids) || !stored.run.user_ids.length) {
    await newRun();
    return;
  }
  state.run = stored.run;
  state.user = state.run.user_ids.find((id) => String(id) === String(stored.user)) ?? state.run.user_ids[0];
  renderUsers();
  renderControls();
  await refresh();
}

initialize();
