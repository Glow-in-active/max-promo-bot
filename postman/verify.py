#!/usr/bin/env python3
"""Exercise real HTTP boundaries against a running isolated demo service."""
import json
import os
import re
import time
import urllib.error
import urllib.request
import uuid

if not __debug__:
    raise SystemExit("HTTP verification requires assertions. Run without -O or PYTHONOPTIMIZE.")

BASE_URL = os.environ.get("BASE_URL", "http://localhost:8080").rstrip("/")
ADMIN_TOKEN = os.environ.get("ADMIN_TOKEN", "local-administrator-token")
DEMO_TOKEN = os.environ.get("DEMO_TOKEN", "local-demo-token")
WEBHOOK_SECRET = os.environ.get("MAX_WEBHOOK_SECRET") or os.environ.get("WEBHOOK_SECRET") or "local-webhook-secret"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def request(method, path, body=None, token=None, headers=None, expected=200):
    data = None if body is None else json.dumps(body).encode()
    actual_headers = {"Content-Type": "application/json"}
    if token:
        actual_headers["Authorization"] = "Bearer " + token
    actual_headers.update(headers or {})
    req = urllib.request.Request(BASE_URL + path, data=data, method=method, headers=actual_headers)
    try:
        response = HTTP.open(req, timeout=10)
    except urllib.error.HTTPError as error:
        response = error
    payload = response.read().decode()
    assert response.status == expected, (method, path, response.status, payload)
    if response.headers.get_content_type() == "application/json":
        return json.loads(payload)
    return payload


def create(scenario):
    return request("POST", "/_demo/runs", {"scenario": scenario}, DEMO_TOKEN, expected=201)


def all_messages(run):
    return request("GET", "/_demo/runs/" + run["campaign_id"] + "/messages", token=DEMO_TOKEN)["messages"] or []


def wait_message(run, predicate, previous=None):
    previous = previous or set()
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        messages = all_messages(run)
        for message in messages:
            if message["id"] not in previous and message.get("state") == "delivered" and predicate(message):
                return message
        time.sleep(0.1)
    raise AssertionError("Expected outgoing message was not delivered within 15 seconds")


def callback(run, action, user=None, callback_id=None):
    user = user or run["user_ids"][0]
    return request("POST", "/webhooks/max", {
        "update_type": "message_callback", "timestamp": int(time.time() * 1000),
        "message": {"recipient": {"chat_type": "dialog"}},
        "callback": {"callback_id": callback_id or str(uuid.uuid4()), "user": {"user_id": user, "is_bot": False}, "payload": action},
    }, headers={"X-Max-Bot-Api-Secret": WEBHOOK_SECRET, "X-Demo-Campaign": run["campaign_id"]})


def action_reply(run, action, expected_text, user=None):
    previous = {message["id"] for message in all_messages(run)}
    callback(run, action, user)
    return wait_message(run, lambda message: message["user_id"] == (user or run["user_ids"][0]) and message["text"].startswith(expected_text), previous)


def admin(run, suffix, method="GET"):
    return request(method, "/api/admin/campaigns/" + run["campaign_id"] + suffix, token=ADMIN_TOKEN)


def main():
    request("GET", "/healthz")
    request("GET", "/readyz")
    request("POST", "/_demo/runs", {"scenario": "standard"}, expected=401)
    request("POST", "/webhooks/max", {"update_type": "bot_started"}, expected=401)
    run = create("standard")
    request("GET", "/api/admin/campaigns/" + run["campaign_id"] + "/stats", expected=401)
    welcome = action_reply(run, "start", "Добро пожаловать")
    assert welcome.get("banner") and len(welcome.get("buttons", [])) == 5
    first = action_reply(run, "promo", "Ваш бонус от")
    code = re.search(r"Промокод: (\S+)", first["text"])
    assert code, first["text"]
    second = action_reply(run, "promo", "Ваш бонус от")
    assert code.group(1) in second["text"]
    event_id = str(uuid.uuid4())
    replay_body = {"update_type": "message_created", "timestamp": int(time.time()*1000), "message": {"sender": {"user_id": run["user_ids"][0]}, "recipient": {"chat_type": "dialog"}, "body": {"mid": event_id, "text": "/help"}}}
    replay_headers = {"X-Max-Bot-Api-Secret": WEBHOOK_SECRET, "X-Demo-Campaign": run["campaign_id"]}
    previous = {message["id"] for message in all_messages(run)}
    assert request("POST", "/webhooks/max", replay_body, headers=replay_headers)["duplicate"] is False
    assert request("POST", "/webhooks/max", replay_body, headers=replay_headers)["duplicate"] is True
    wait_message(run, lambda message: message["user_id"] == run["user_ids"][0] and message["text"].startswith("Получите промокод партнёра"), previous)
    action_reply(run, "participate", "Чтобы участвовать в розыгрыше")
    registered = action_reply(run, "check", "Вы в лайнапе")
    assert "Заявка зарегистрирована" in registered["text"]
    chances = action_reply(run, "chances", "Ваш фестивальный пропуск")
    assert "Накоплено шансов: 1 из 10" in chances["text"]
    action_reply(run, "check", "Вы уже участвуете")
    stats = admin(run, "/stats")
    assert stats["promo_issued"] == 1 and stats["promo_requests"] == 2
    assert stats["entries"][run["draw_id"]] == 1 and stats["entries"][run["final_draw_id"]] == 1
    stock = stats["promo_remaining"]
    assert isinstance(stock, list) and stock
    assert len({partner["channel_id"] for partner in stock}) == len(stock)
    for partner in stock:
        assert partner["partner"] and partner["remaining"] == sum(day["remaining"] for day in partner["days"])
    current_day = next(day for partner in stock for day in partner["days"] if day["day_id"] == run["day_id"])
    assert current_day["remaining"] == 19
    second_user = run["user_ids"][1]
    for status, expected_text in [("no", "Пока не вижу подписку"), ("error", "Не удалось проверить подписку")]:
        request("POST", "/_demo/runs/" + run["campaign_id"] + "/subscription", {"user_id": second_user, "status": status}, DEMO_TOKEN)
        assert expected_text in action_reply(run, "check", expected_text, second_user)["text"]
    request("POST", "/_demo/runs/" + run["campaign_id"] + "/subscription", {"user_id": second_user, "status": "yes"}, DEMO_TOKEN)
    assert "Заявка зарегистрирована" in action_reply(run, "check", "Вы в лайнапе", second_user)["text"]
    print("PASS standard: menu, code replay, inbox replay, one daily chance, subscription failures")

    empty = create("exhausted")
    assert "закончились" in action_reply(empty, "promo", "Промокоды партнёра закончились")["text"]
    assert "Заявка зарегистрирована" in action_reply(empty, "check", "Вы в лайнапе")["text"]
    print("PASS exhausted: no code does not prevent participation")

    drawable = create("drawable")
    draw_path = "/draws/" + drawable["draw_id"]
    result = admin(drawable, draw_path + "/run", "POST")
    replay = admin(drawable, draw_path + "/run", "POST")
    assert result["selection"] == replay["selection"]
    assert len(result["selection"]["winners"]) == 1
    assert not [message for message in all_messages(drawable) if message.get("offer_id")]
    admin(drawable, draw_path + "/approve", "POST")
    prize = wait_message(drawable, lambda message: bool(message.get("offer_id")))
    user = prize["user_id"]
    action_reply(drawable, "claim_begin:" + prize["offer_id"], "Введите ваше имя и фамилию", user)
    previous = {message["id"] for message in all_messages(drawable)}
    request("POST", "/webhooks/max", {
        "update_type": "message_created", "timestamp": int(time.time() * 1000),
        "message": {"sender": {"user_id": user, "is_bot": False}, "recipient": {"chat_type": "dialog"}, "body": {"mid": str(uuid.uuid4()), "text": "Иван Иванов"}},
    }, headers={"X-Max-Bot-Api-Secret": WEBHOOK_SECRET, "X-Demo-Campaign": drawable["campaign_id"]})
    consent = wait_message(drawable, lambda message: any(button.get("payload") == "claim:" + prize["offer_id"] for button in message.get("buttons", [])), previous)
    assert consent
    action_reply(drawable, "claim:" + prize["offer_id"], "Получение приза подтверждено", user)
    offer = request("GET", "/api/admin/offers/" + prize["offer_id"], token=ADMIN_TOKEN)
    assert offer["status"] == "claimed" and offer["name"] == "Иван Иванов" and offer["consent_at"]
    csv = admin(drawable, "/export.csv")
    assert "winner" in csv and "reserve" in csv and "participant" in csv and "offer" in csv
    print("PASS daily draw: saved result, approval, consent conversation, claimed offer, CSV")

    final = create("final")
    result = admin(final, "/draws/" + final["final_draw_id"] + "/run", "POST")
    selected = result["selection"]["winners"] + result["selection"]["reserve"]
    assert len(result["selection"]["winners"]) == 5 and len(set(selected)) == len(selected)
    assert len({candidate["weight"] for candidate in result["candidates"]}) > 1
    print("PASS final: five unique winners, weighted snapshot, unique reserve")

    expired = create("expired")
    request("POST", "/api/admin/offers/" + expired["offer_id"] + "/expire", token=ADMIN_TOKEN)
    reserve = wait_message(expired, lambda message: bool(message.get("offer_id")) and message["offer_id"] != expired["offer_id"])
    assert reserve["user_id"] != expired["user_ids"][0]
    offer = request("GET", "/api/admin/offers/" + expired["offer_id"], token=ADMIN_TOKEN)
    assert offer["status"] == "expired"
    print("PASS reserve: overdue offer replaced without changing the saved draw result")
    print("HTTP smoke passed")


if __name__ == "__main__":
    main()
