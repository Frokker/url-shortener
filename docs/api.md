# HTTP API

[← К оглавлению](./README.md) · Связанные: [architecture.md](./architecture.md) · [data-model.md](./data-model.md) · [configuration.md](./configuration.md)

Базовый префикс API: `/api/v1`. Редирект и health-чеки живут в корне.
Все тела запросов/ответов — `application/json; charset=utf-8`, кроме редиректа.

## Содержание

- [Сводка эндпоинтов](#сводка-эндпоинтов)
- [Формат ошибки](#формат-ошибки)
- [POST /api/v1/links — создать](#post-apiv1links--создать)
- [GET /{code} — редирект](#get-code--редирект)
- [GET /api/v1/links/{code} — метаданные](#get-apiv1linkscode--метаданные)
- [DELETE /api/v1/links/{code} — удалить](#delete-apiv1linkscode--удалить)
- [GET /api/v1/links/{code}/stats — аналитика (Этап 2)](#get-apiv1linkscodestats--аналитика-этап-2)
- [GET /healthz и /readyz](#get-healthz-и-readyz)
- [GET /metrics (Этап 2)](#get-metrics-этап-2)
- [Rate limiting (Этап 2)](#rate-limiting-этап-2)
- [Идемпотентность](#идемпотентность)
- [Безопасность и известные риски](#безопасность-и-известные-риски)

## Сводка эндпоинтов

| Метод | Путь | Этап | Назначение |
|---|---|---|---|
| POST | `/api/v1/links` | 1 | Создать короткую ссылку |
| GET | `/{code}` | 1 | Редирект (302) на оригинал, учёт клика |
| GET | `/api/v1/links/{code}` | 1 | Метаданные ссылки |
| DELETE | `/api/v1/links/{code}` | 1 | Удалить ссылку |
| GET | `/healthz` | 1 | Liveness |
| GET | `/readyz` | 1 | Readiness (Postgres критичен; Redis — `degraded`, не валит `503`) |
| GET | `/api/v1/links/{code}/stats` | 2 | Расширенная аналитика по времени |
| GET | `/metrics` | 2 | Метрики Prometheus |

## Формат ошибки

Единый JSON для всех ошибок:

```json
{
  "error": {
    "code": "ALIAS_TAKEN",
    "message": "custom alias is already in use"
  }
}
```

| HTTP | `code` | Когда |
|---|---|---|
| 400 | `INVALID_REQUEST` | битый JSON, пустой/невалидный URL, плохой алиас, отрицательный ttl |
| 404 | `NOT_FOUND` | кода нет в БД |
| 409 | `ALIAS_TAKEN` | кастомный алиас занят |
| 410 | `GONE` | ссылка просрочена (`expires_at` в прошлом) |
| 429 | `RATE_LIMITED` | превышен лимит запросов (Этап 2) |
| 500 | `INTERNAL` | непредвиденная ошибка |
| 503 | `NOT_READY` | `/readyz`: **Postgres** недоступен (Redis — мягкая деградация, не валит готовность) |

Маппинг доменных ошибок (`ErrNotFound`, `ErrExpired`, `ErrAliasTaken`, ...) на эти коды
делает **только handler** через `errors.Is`.

## POST /api/v1/links — создать

Запрос:

```json
{
  "url": "https://example.com/very/long/path?a=1",
  "custom_alias": "promo",
  "ttl_seconds": 3600
}
```

| Поле | Тип | Обязательность | Правила |
|---|---|---|---|
| `url` | string | да | непустой, схема **только** `http`/`https` (схемы `javascript:`/`data:`/`file:` отклоняются) |
| `custom_alias` | string | нет | `^[A-Za-z0-9_-]{3,32}$` **и не из reserved-списка** (см. ниже) |
| `ttl_seconds` | integer | нет | `>= 0`; `0`/отсутствует = бессрочно |

**Reserved-алиасы.** Редирект живёт в корне (`GET /{code}`), там же — служебные маршруты.
Кастомный алиас, совпадающий с именем корневого маршрута, либо затенит маршрут, либо будет
затенён им (зависит от порядка монтирования в chi) — это реальный баг роутинга, а не просто
хардненинг. Поэтому `custom_alias` из множества `{healthz, readyz, metrics, api, favicon.ico,
robots.txt}` отклоняется с `400 INVALID_REQUEST`. Сгенерированные коды этих значений не
принимают по построению.

Ответ `201 Created` (новая ссылка):

```json
{
  "code": "promo",
  "short_url": "http://localhost:8080/promo",
  "expires_at": "2026-06-04T13:00:00Z"
}
```

Ответ `200 OK` (идемпотентный повтор того же `url` без алиаса и без TTL — вернулась существующая):

```json
{
  "code": "aZ3xQ",
  "short_url": "http://localhost:8080/aZ3xQ",
  "expires_at": null
}
```

| Статус | Когда |
|---|---|
| 201 | создана новая ссылка (в т.ч. всегда — для запросов с `custom_alias` или `ttl_seconds > 0`) |
| 200 | тот же URL **без алиаса и без TTL** уже существовал → возвращён существующий код (идемпотентность) |
| 400 | `INVALID_REQUEST` (в т.ч. reserved-алиас, не-`http(s)` схема) |
| 409 | `ALIAS_TAKEN` — кастомный алиас занят |
| 429 | `RATE_LIMITED` (Этап 2) |

curl:

```bash
curl -s -X POST http://localhost:8080/api/v1/links \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com","custom_alias":"promo","ttl_seconds":3600}'
```

## GET /{code} — редирект

Главный горячий путь. Возвращает **302 Found** (см. обоснование 301 vs 302 в
[architecture.md](./architecture.md#решение-301-vs-302)).

```
HTTP/1.1 302 Found
Location: https://example.com/very/long/path?a=1
```

| Статус | Когда |
|---|---|
| 302 | найдено и не просрочено → `Location: original_url` |
| 404 | `NOT_FOUND` — кода нет |
| 410 | `GONE` — ссылка просрочена |
| 429 | `RATE_LIMITED` (Этап 2) |

Поведение по этапам:
- **Этап 1**: перед ответом синхронно делается `UPDATE click_count + 1` (блокирует).
- **Этап 2**: событие клика кладётся в буферизованный канал, ответ отдаётся сразу; при
  полном буфере событие дропается (метрика), редирект всё равно мгновенный.

curl (не следовать редиректу, показать заголовки):

```bash
curl -s -i http://localhost:8080/promo
```

## GET /api/v1/links/{code} — метаданные

Ответ `200 OK`:

```json
{
  "code": "promo",
  "original_url": "https://example.com",
  "clicks": 142,
  "is_custom": true,
  "created_at": "2026-06-04T12:00:00Z",
  "expires_at": "2026-06-04T13:00:00Z"
}
```

| Статус | Когда |
|---|---|
| 200 | найдено (даже если просрочено — метаданные отдаём) |
| 404 | `NOT_FOUND` |

> `clicks` — это денормализованный `click_count`. На Этапе 2 он может отставать на величину
> до одного окна флаша (`FLUSH_INTERVAL_T`) — это ожидаемое следствие батчинга.

curl:

```bash
curl -s http://localhost:8080/api/v1/links/promo | jq
```

## DELETE /api/v1/links/{code} — удалить

| Статус | Когда |
|---|---|
| 204 | удалено |
| 404 | `NOT_FOUND` |

Удаление инвалидирует запись в Redis-кэше. На Этапе 2 `ON DELETE CASCADE` чистит и
`click_aggregates`.

curl:

```bash
curl -s -i -X DELETE http://localhost:8080/api/v1/links/promo
```

## GET /api/v1/links/{code}/stats — аналитика (Этап 2)

Агрегаты по времени из `click_aggregates` (питается Kafka-конвейером).
Параметры запроса: `from`, `to` (RFC3339), `bucket` (`hour`|`minute`, по умолчанию `hour`).

```bash
curl -s 'http://localhost:8080/api/v1/links/promo/stats?from=2026-06-04T00:00:00Z&to=2026-06-04T23:59:59Z&bucket=hour' | jq
```

Ответ `200 OK`:

```json
{
  "code": "promo",
  "total": 142,
  "bucket": "hour",
  "series": [
    { "ts": "2026-06-04T12:00:00Z", "clicks": 90 },
    { "ts": "2026-06-04T13:00:00Z", "clicks": 52 }
  ]
}
```

| Статус | Когда |
|---|---|
| 200 | найдено |
| 400 | `INVALID_REQUEST` — кривые `from`/`to`/`bucket` |
| 404 | `NOT_FOUND` |

## GET /healthz и /readyz

| Эндпоинт | Назначение | Ответ |
|---|---|---|
| `GET /healthz` | liveness — процесс жив | `200 {"status":"ok"}` всегда, без проверки зависимостей |
| `GET /readyz` | readiness — готов принимать трафик | `200`, если отвечает **Postgres** (критичная зависимость); Redis проверяется, но его отказ → `degraded`, не `503` |

```json
// 200 /readyz — всё ок
{ "status": "ready", "checks": { "postgres": "ok", "redis": "ok" } }
```

```json
// 200 /readyz — Redis лёг, но сервис готов (cache-miss → Postgres)
{ "status": "ready", "checks": { "postgres": "ok", "redis": "degraded" } }
```

```json
// 503 /readyz — Postgres недоступен
{ "status": "not_ready", "checks": { "postgres": "fail", "redis": "ok" } }
```

**Почему Redis не валит готовность.** Redis здесь — read-through *кэш*: при его недоступности
запрос деградирует к Postgres (cache-miss → БД), сервис продолжает обслуживать всё. Если
гейтить `/readyz` на Redis, кратковременный сбой кэша выкинул бы все поды из балансировщика,
хотя они полностью работоспособны через Postgres — это анти-паттерн. Поэтому критичная
зависимость готовности — только Postgres; Redis отражается как `degraded`. `/healthz` не
дёргает зависимости вовсе, чтобы временный сбой БД не вызывал рестарт пода.

## GET /metrics (Этап 2)

Экспозиция Prometheus в text-формате. Список метрик — в [operations.md](./operations.md#метрики-prometheus).

```bash
curl -s http://localhost:8080/metrics | grep urlshort_
```

## Rate limiting (Этап 2)

Token-bucket middleware на **всём роутере** (отдельной ручки нет). Ключ — IP или API-ключ
(см. [configuration.md](./configuration.md)). При превышении — `429`.

Заголовки на ответе:

| Заголовок | Пример | Смысл |
|---|---|---|
| `X-RateLimit-Limit` | `200` | размер бакета (burst) |
| `Retry-After` | `1` | (только при 429) через сколько секунд повторить |

> `X-RateLimit-Remaining`/`X-RateLimit-Reset` **намеренно не отдаются**: реализация на
> `golang.org/x/time/rate` использует непрерывный token-bucket, который не выдаёт ни «остаток
> токенов», ни единый момент «reset» — эти заголовки были бы вычислены неверно. Отдаём только
> то, что определено корректно (`Limit` = burst, `Retry-After` при отказе). Подробнее —
> [stage-2.md](./stage-2.md#фича-3-rate-limiting).

```json
// 429
{ "error": { "code": "RATE_LIMITED", "message": "too many requests" } }
```

## Идемпотентность

- **POST /api/v1/links без `custom_alias` и без TTL**: повтор с тем же `url` возвращает
  **тот же `code`** (через частичный `UNIQUE(url_hash) WHERE is_custom=false AND
  expires_at IS NULL`), статус `200` вместо `201`. См.
  [data-model.md](./data-model.md#идемпотентность-создания).
- **POST с `custom_alias` или `ttl_seconds > 0`**: идемпотентность по URL **не**
  применяется — всегда создаётся новая строка. Это сделано осознанно: иначе один URL не мог
  бы иметь и кастомный алиас, и разные сроки жизни. Полное правило приоритета — в
  [data-model.md](./data-model.md#идемпотентность-создания).
- **POST с `custom_alias`**: повтор с занятым алиасом → `409 ALIAS_TAKEN` (это не
  идемпотентность, а конфликт уникального ключа).
- **DELETE**: повторное удаление отсутствующего кода → `404` (не `204`); операция не
  «идемпотентна» в смысле возврата того же кода, но безопасна для повторов.
- **GET /{code}**: на Этапе 1 каждый успешный редирект меняет состояние (счётчик), поэтому
  он не безопасен как «pure GET» — это часть учебной боли.

## Безопасность и известные риски

Сокращатель ссылок по своей природе несёт несколько рисков. Часть принимается осознанно,
часть — митигируется. Фиксируем явно, чтобы это были решения, а не упущения.

| Риск | Статус | Что делаем |
|---|---|---|
| **Open redirect** | принят by design | сокращатель *обязан* редиректить на произвольный внешний URL — это его функция. Митигируем поверхность: схемы только `http(s)` (никаких `javascript:`/`data:`), редирект отдаётся как `302` без проксирования контента. Опционально — денидист доменов. |
| **SSRF** | ограничен | сервис **сам не ходит** по сохранённому URL (не делает preview/fetch), поэтому серверного SSRF нет. Если в будущем добавишь подтягивание title/preview — это станет вектором: тогда нужен запрет приватных диапазонов (`127.0.0.0/8`, `10/8`, `169.254.169.254`, `::1` и т.п.). Сейчас — non-goal, но помни. |
| **Alias squatting / затенение маршрутов** | митигирован | reserved-денидист для `custom_alias` (`healthz`, `readyz`, `metrics`, `api`, `favicon.ico`, `robots.txt`) — см. [POST /api/v1/links](#post-apiv1links--создать). Без него алиас `metrics` затенял бы `/metrics`. |
| **Перебор/enumeration кодов** | принят by design | короткие коды угадываемы; для публичного сокращателя это норма. Если ссылки приватные — нужен несеквенциальный генератор и более длинный код (`CODE_LENGTH`). |
| **Абьюз создания (флуд)** | митигирован | rate limiting на роутере (Этап 2). Для прод-сценария — ещё и капча/ауth на `POST`. |

> Это учебный проект, и список — не «прод-grade threat model», а честная фиксация границ.
> Главное: open redirect и угадываемость кодов **приняты осознанно** (свойства сокращателя),
> а затенение маршрутов и схемы URL **закрыты**, потому что это баги, а не свойства.
