import json
import os
import re
import time
import urllib.error
import urllib.request


BASE_URL = os.environ.get("SIMULATOR_URL", "http://127.0.0.1:" + os.environ.get("SIMULATOR_PORT", "4010")).rstrip("/")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def request(path, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE_URL + path, data=data, headers={"Content-Type": "application/json"})
    with HTTP.open(req, timeout=10) as response:
        require(response.status in (200, 201, 202), "Unexpected simulator response status")
        return json.load(response)


def await_result(check, description):
    deadline = time.monotonic() + 15
    while True:
        result = check()
        if result:
            return result
        if time.monotonic() >= deadline:
            raise RuntimeError("Timed out waiting for " + description)
        time.sleep(0.1)


def user_state(user):
    return request(f"/api/users/{user}")


def bot_messages(user):
    return [message for message in user_state(user)["messages"] if message["source"] == "bot"]


def buttons(message):
    return [
        button
        for attachment in message.get("attachments") or []
        if attachment["type"] == "inline_keyboard"
        for row in attachment["payload"]["buttons"]
        for button in row
    ]


def callback(user, payload):
    before = len(bot_messages(user))
    message = next(
        (message for message in reversed(bot_messages(user)) if any(button.get("payload") == payload for button in buttons(message))),
        None,
    )
    require(message is not None, "Bot did not provide the expected callback button: " + payload)
    request(f"/api/users/{user}/callbacks", {"mid": message["mid"], "payload": payload})
    return await_reply(user, before)


def await_reply(user, before):
    def latest_reply():
        messages = bot_messages(user)
        return messages[-1] if len(messages) > before else None

    return await_result(latest_reply, "bot reply")


def new_run(scenario):
    run = request("/api/runs", {"scenario": scenario})
    require(len(run["user_ids"]) == 8, "Scenario did not create eight participants")
    return run


def run_state(run):
    return request("/api/runs/" + run["campaign_id"])


def standard_scenario():
    run = new_run("standard")
    user = run["user_ids"][0]
    request(f"/api/users/{user}/start", {})
    menu = await_reply(user, 0)
    require(len(buttons(menu)) == 5, "Welcome message has an incomplete menu")
    images = [attachment for attachment in menu.get("attachments", []) if attachment.get("type") == "image"]
    require(len(images) == 1, "Welcome message must include the campaign banner")
    require(images[0].get("payload", {}).get("url") == "https://simulator:4011/assets/banner.png", "Welcome banner does not use the simulator HTTPS asset")
    with HTTP.open(BASE_URL + "/assets/banner.png", timeout=10) as response:
        require(response.status == 200 and response.headers.get_content_type() == "image/png", "Banner is not served as a PNG image")
        image = response.read(1 << 20)
    require(image[:8] == b"\x89PNG\r\n\x1a\n" and len(image) > 1000, "Banner response is not a complete PNG image")
    require(int.from_bytes(image[16:20], "big") == 1200 and int.from_bytes(image[20:24], "big") == 480, "Banner dimensions differ from the campaign image")
    first = callback(user, "promo")
    second = callback(user, "promo")
    code = lambda message: re.search(r"DEMO-[A-Za-z0-9-]+", message["text"])
    require(code(first) is not None and code(second) is not None, "Promo response has no code")
    require(code(first).group() == code(second).group(), "Repeated promo request issued a different code")
    callback(user, "participate")
    request(f"/api/users/{user}/subscription", {"status": "no"})
    unsubscribed = callback(user, "check")
    require("Пока не вижу подписку" in unsubscribed["text"], "Unsubscribed participant was not rejected")
    request(f"/api/users/{user}/subscription", {"status": "error"})
    unavailable = callback(user, "check")
    require("Не удалось проверить подписку" in unavailable["text"], "Membership failure was treated as a valid subscription")
    require(run_state(run)["stats"]["entries"].get(run["draw_id"], 0) == 0, "Rejected subscription created an entry")
    request(f"/api/users/{user}/subscription", {"status": "yes"})
    accepted = callback(user, "check")
    require("Накоплено шансов: 1 из 10" in accepted["text"], "Promo and registration produced multiple daily chances")
    state = run_state(run)
    require(state["stats"]["promo_issued"] == 1, "Repeated request consumed more than one promo")
    require(state["stats"]["entries"].get(run["draw_id"]) == 1, "Subscription did not register exactly one entry")
    before = len(bot_messages(user))
    request(f"/api/users/{user}/replay", {})
    require(user_state(user)["last_webhook"]["body"]["duplicate"] is True, "Replayed webhook was executed as a fresh event")
    require(len(bot_messages(user)) == before, "Replayed event produced a second reply")
    print("PASS campaign banner, participation, membership errors, daily uniqueness and webhook replay")


def exhausted_scenario():
    run = new_run("exhausted")
    user = run["user_ids"][0]
    request(f"/api/users/{user}/start", {})
    await_reply(user, 0)
    reply = callback(user, "promo")
    require("Промокоды партнёра закончились" in reply["text"], "Empty promo pool did not produce an explicit response")
    callback(user, "participate")
    request(f"/api/users/{user}/subscription", {"status": "yes"})
    callback(user, "check")
    stats = run_state(run)["stats"]
    require(stats["promo_issued"] == 0 and stats["entries"][run["draw_id"]] == 1, "Empty promo pool blocked participation")
    print("PASS participation with an empty promo pool")


def approve_draw(run, draw):
    path = f"/api/runs/{run['campaign_id']}/draws/{draw}"
    result = request(path + "/run", {})
    request(path + "/approve", {})
    return result


def prize_scenario():
    run = new_run("drawable")
    approve_draw(run, run["draw_id"])
    offer = await_result(
        lambda: next((offer for offer in run_state(run)["offers"] if offer["status"] == "notified"), None),
        "delivered prize notification",
    )
    user = offer["user_id"]
    reply = callback(user, "claim_begin:" + offer["id"])
    require("имя и фамилию" in reply["text"], "Prize did not request a full name")
    before = len(bot_messages(user))
    request(f"/api/users/{user}/messages", {"text": "Ada"})
    invalid = await_reply(user, before)
    require("Введите имя и фамилию" in invalid["text"], "Incomplete name was accepted")
    before = len(bot_messages(user))
    request(f"/api/users/{user}/messages", {"text": "Ada Lovelace"})
    consent = await_reply(user, before)
    require(any(button.get("payload") == "claim:" + offer["id"] for button in buttons(consent)), "Name was accepted without requesting consent")
    pending = next(item for item in run_state(run)["offers"] if item["id"] == offer["id"])
    require(pending["status"] == "notified", "Prize was claimed before consent")
    callback(user, "claim:" + offer["id"])
    claimed = next(item for item in run_state(run)["offers"] if item["id"] == offer["id"])
    require(claimed["status"] == "claimed" and claimed["name"] == "Ada Lovelace", "Prize claim was not persisted")
    require(claimed.get("consent_at") not in (None, "0001-01-01T00:00:00Z"), "Prize claim has no consent timestamp")
    print("PASS draw approval, prize delivery, name validation and consent")


def final_scenario():
    run = new_run("final")
    result = approve_draw(run, run["final_draw_id"])
    winners = result["selection"]["winners"]
    require(len(winners) == 5 and len(set(winners)) == 5, "Final draw did not select five distinct winners")
    require(sorted(candidate["weight"] for candidate in result["candidates"]) == list(range(1, 9)), "Final weights differ from accumulated chances")
    await_result(
        lambda: len([offer for offer in run_state(run)["offers"] if offer["draw_id"] == run["final_draw_id"] and offer["status"] == "notified"]) == 5,
        "five final notifications",
    )
    print("PASS weighted final and five distinct prize notifications")


def reserve_scenario():
    run = new_run("expired")

    def reserve_notification():
        state = run_state(run)
        if any(offer["id"] != run["offer_id"] and offer["status"] == "notified" for offer in state["offers"]):
            return state
        return None

    state = await_result(reserve_notification, "reserve notification")
    expired = next(offer for offer in state["offers"] if offer["id"] == run["offer_id"])
    replacement = next(offer for offer in state["offers"] if offer["id"] != run["offer_id"] and offer["status"] == "notified")
    require(expired["status"] == "expired", "Original offer was not expired")
    require(replacement["user_id"] != expired["user_id"] and replacement["slot_id"] == expired["slot_id"], "Reserve did not replace the original prize recipient")
    print("PASS expired offer and reserve promotion")


def main():
    require(request("/health").get("ok") is True, "Simulator is not healthy")
    standard_scenario()
    exhausted_scenario()
    prize_scenario()
    final_scenario()
    reserve_scenario()
    print("Simulator smoke passed: MAX API, webhook, PostgreSQL and prize lifecycle.")


if __name__ == "__main__":
    main()
