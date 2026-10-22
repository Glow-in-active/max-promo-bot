# API и Postman

[OpenAPI](../api/openapi.yaml)
[Коллекция Postman](../postman/promo.postman_collection.json)
[Окружение](../postman/local.postman_environment.json)

## Контракт

`api/openapi.yaml` задаёт типы и маршруты. Закреплённый `oapi-codegen` генерирует интерфейс обработчиков и DTO; middleware проверяет запросы, контрактные тесты проверяют ответы.

```sh
make generate
make check-generated
```

## Доступ

| Маршрут | Авторизация |
| --- | --- |
| `/healthz`, `/readyz` | Без токена |
| `/api/admin/*` | `Authorization: Bearer <ADMIN_TOKEN>` |
| `/_demo/*` | `Authorization: Bearer <DEMO_TOKEN>`, demo и simulator |
| `/webhooks/max` | `X-Max-Bot-Api-Secret` |

В production акция задаётся через `CAMPAIGN_ID`; в demo и simulator webhook получает `X-Demo-Campaign`. Внутреннее изменение подписки доступно только в demo; simulator проверяет её через внешний MAX API. Локальное окружение Postman уже содержит demo-токены.

## Основные операции

| Метод и путь | Результат |
| --- | --- |
| `POST /_demo/runs` | Создаёт `standard`, `exhausted`, `drawable`, `final` или `expired` |
| `POST /webhooks/max` | Сохраняет событие бота |
| `GET /_demo/runs/{campaign}/messages` | Возвращает исходящие сообщения и состояние доставки |
| `POST /_demo/runs/{campaign}/subscription` | Задаёт demo-подписку: `yes`, `no`, `error` |
| `POST /api/admin/campaigns/{campaign}/draws/{draw}/run` | Сохраняет результат розыгрыша |
| `POST /api/admin/campaigns/{campaign}/draws/{draw}/approve` | Подтверждает результат и создаёт уведомления |
| `GET /api/admin/campaigns/{campaign}/stats` | Возвращает статистику, включая остатки по партнёрам |
| `GET /api/admin/campaigns/{campaign}/export.csv` | Выгружает участников, выбор и получение призов |

Все операции и схемы описаны в OpenAPI. Порядок ручной проверки совпадает с [папками коллекции](index.md).

## Когда появляется результат

HTTP `200` от webhook подтверждает получение. Поддерживаемые действия сохраняются в inbox; бизнес-сценарий и ответ выполняются через inbox/outbox. Фото без подписи, пустой текст и пересылка с `body: null` получают `ignored: true` и не меняют шансы или данные для получения приза. Непустая подпись обрабатывается как текст. В demo дождитесь результата в `messages`; `after` принимает ID последнего сообщения. Collection Runner делает это автоматически.

Callback из личного диалога подтверждается через MAX `/answers` после сохранения inbox. Ошибка ACK не отменяет принятое событие; ACK не имеет долговечной очереди повторов.

Callback с `message: null` или без поля `message` получает HTTP `200` и `ignored: true`. Без исходного сообщения бот не определяет диалог и не выполняет действие. Inbox, outbox и данные пользователя сохраняются без изменений; `/answers` не вызывается.
