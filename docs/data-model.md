# Модель данных

[← К оглавлению](./README.md) · Связанные: [architecture.md](./architecture.md) · [stage-1.md](./stage-1.md) · [stage-2.md](./stage-2.md)

## Содержание

- [Обзор](#обзор)
- [Таблица `links`](#таблица-links)
- [Идемпотентность создания](#идемпотентность-создания)
- [TTL и истечение](#ttl-и-истечение)
- [Этап 2: `click_events` и агрегаты](#этап-2-click_events-и-агрегаты)
- [Индексы — сводка](#индексы--сводка)
- [Стратегия миграций](#стратегия-миграций)
- [Полный DDL](#полный-ddl)

## Обзор

Postgres, доступ через `pgx`/`pgxpool`. Одна основная таблица `links` на Этапе 1; на Этапе 2
добавляется `click_events` (сырой поток) и/или `click_aggregates` (агрегаты по времени).
Счётчик `click_count` денормализован в `links` для быстрого `GET .../links/{code}`.

```
Этап 1:   links (click_count инкрементится синхронно)

Этап 2:   links (click_count обновляется батчами из воркер-пула)
            ▲
            │ батчевый UPDATE
          worker pool ◀── click_events (сырьё, опционально) / Kafka topic clicks
                              │
                              ▼
                        click_aggregates (по-часовые/по-минутные бакеты для /stats)
```

## Таблица `links`

| Колонка | Тип | Ограничения | Назначение |
|---|---|---|---|
| `code` | `text` | **PRIMARY KEY**, `CHECK (char_length(code) BETWEEN 3 AND 32)` | короткий код (он же алиас); ключ редиректа |
| `original_url` | `text` | `NOT NULL`, `CHECK (original_url <> '')` | целевой URL |
| `url_hash` | `bytea` | `NOT NULL`, **partial UNIQUE** (см. ниже) | SHA-256 от нормализованного URL — для идемпотентности «простых» ссылок |
| `is_custom` | `boolean` | `NOT NULL DEFAULT false` | задан ли алиас пользователем |
| `created_at` | `timestamptz` | `NOT NULL DEFAULT now()` | момент создания |
| `expires_at` | `timestamptz` | `NULL` | срок жизни; `NULL` = бессрочно |
| `click_count` | `bigint` | `NOT NULL DEFAULT 0`, `CHECK (click_count >= 0)` | денормализованный счётчик кликов |

Почему `code` — это `PRIMARY KEY` (а не суррогатный `id`):
- редирект всегда ищет по `code` → PK-индекс используется напрямую;
- кастомный алиас естественно ложится в тот же столбец (`is_custom = true`);
- коллизия алиаса = нарушение PK → чистый `ErrAliasTaken` (409).

Доменная структура в Go:

```go
type Link struct {
    Code        string
    OriginalURL string
    URLHash     []byte
    IsCustom    bool
    CreatedAt   time.Time
    ExpiresAt   *time.Time // nil = бессрочно
    ClickCount  int64
}
```

## Идемпотентность создания

Требование README: «одинаковый URL → тот же код (через unique-констрейнт)». Реализуется
**на уровне БД**, потому что только так корректно при гонке двух одновременных вставок.

**Идемпотентность по URL применяется только к «простым» ссылкам** — без кастомного алиаса
и без TTL. Иначе один глобальный `UNIQUE(url_hash)` ломает остальную модель:

- кастомный алиас для уже сокращённого URL стал бы невозможен (конфликт по `url_hash`
  раньше, чем создастся алиас);
- два запроса с разным `ttl_seconds` на один URL не ужились бы — второй молча унаследовал
  бы чужой `expires_at`;
- пересоздание протухшего URL вернуло бы старый мёртвый код с `200`.

Поэтому констрейнт **частичный**: уникальность `url_hash` действует только там, где
`is_custom = false AND expires_at IS NULL`. Кастомные и срочные ссылки всегда создают
новую строку с новым `code`.

```sql
-- частичный уникальный индекс: дедуп только «простых» ссылок
CREATE UNIQUE INDEX links_url_hash_plain_key ON links (url_hash)
WHERE is_custom = false AND expires_at IS NULL;
```

Создание простой ссылки (без алиаса и TTL) — идемпотентно:

```sql
INSERT INTO links (code, original_url, url_hash, is_custom, created_at, expires_at)
VALUES ($1, $2, $3, false, now(), NULL)
ON CONFLICT (url_hash) WHERE is_custom = false AND expires_at IS NULL DO NOTHING
RETURNING code, original_url, is_custom, created_at, expires_at, click_count;
```

- Если вставка прошла → вернётся новая строка.
- Если конфликт по частичному индексу (этот URL уже сокращён «просто») → `RETURNING` ничего
  не вернёт; делаем добор `SELECT ... WHERE url_hash = $3 AND is_custom = false AND
  expires_at IS NULL` и возвращаем существующий код. Клиент получает тот же `code`.

Нормализация URL (минимум): нижний регистр схемы и хоста, убрать дефолтный порт, убрать
завершающий слэш у пути-корня. Важно зафиксировать правила нормализации один раз — от них
зависит, что считается «тем же URL».

**Правило приоритета при создании** (фиксируем однозначно):

| Запрос | Поведение |
|---|---|
| `url`, без `custom_alias`, без/`0` `ttl` | идемпотентно: тот же URL → тот же `code` (`200` на повторе) |
| `url` + `custom_alias` | всегда новая строка; `code = alias`; конфликт по `PRIMARY KEY(code)` → `ErrAliasTaken` (409). Дедуп по URL **не** применяется |
| `url` + `ttl_seconds > 0` | всегда новая строка с `expires_at`; дедуп по URL **не** применяется |

Отдельный случай — **кастомный алиас**: тут `code` задаёт пользователь, конфликт ловится по
`PRIMARY KEY (code)` и трактуется как `ErrAliasTaken` (409), а не как идемпотентный повтор.

## TTL и истечение

- `ttl_seconds` из запроса → `expires_at = now() + ttl_seconds`. Если `ttl_seconds` не задан
  или `0` → `expires_at = NULL` (бессрочно).
- **Проверка при редиректе** (lazy expiration): в обработчике `/{code}` сравниваем
  `expires_at` с текущим временем; если просрочено → `410 Gone` и ссылка не редиректит.
- **Активная чистка** (Этап 2, sweeper): фоновая горутина на `time.Ticker` периодически
  удаляет просроченные строки, чтобы таблица не пухла и кэш не держал мусор.
- **Согласование с кэшем.** TTL записи в Redis и TTL ссылки — разные вещи, и их нужно
  связать. При кэшировании ссылки с `expires_at` ставь TTL ключа
  `min(CACHE_TTL, время_до_истечения)` — иначе протухшая (а после sweep'а и удалённая)
  ссылка будет жить в кэше до `CACHE_TTL` (по умолчанию 10m) и отдаваться из него. Плюс
  sweeper должен инвалидировать удалённые коды в Redis (`cache.Del`), а не полагаться только
  на TTL. Подробнее — в [stage-2.md](./stage-2.md#фича-4-background-expiry-sweeper).

```sql
-- ленивая проверка делается в коде по выбранной строке;
-- активная чистка (sweeper, Этап 2):
DELETE FROM links
WHERE expires_at IS NOT NULL AND expires_at < now();
```

Частичный индекс по `expires_at` ускоряет работу sweeper'а (см. ниже).

## Этап 2: `click_events` и агрегаты

На Этапе 2 счётчик уезжает в асинхронный конвейер. Здесь два уровня хранения кликов.

### Денормализованный счётчик (остаётся в `links`)

Воркер-пул обновляет `click_count` батчами одним запросом. Это «оперативное» число для
`GET /api/v1/links/{code}`.

```sql
-- батчевый инкремент из воркера: дельты на несколько кодов за один round-trip
UPDATE links AS l
SET click_count = l.click_count + b.delta
FROM (VALUES ($1::text, $2::bigint), ($3, $4) /* ... */) AS b(code, delta)
WHERE l.code = b.code;
```

В коде это собирается из `map[string]int` через `pgx.Batch` или построенный `VALUES`.

### Агрегаты по времени для `/stats`

Для эндпоинта `GET /api/v1/links/{code}/stats` нужны бакеты по времени. Источник —
Kafka-поток `clicks` (consumer пишет агрегаты) или прямая запись из воркера.

| Колонка | Тип | Ограничения | Назначение |
|---|---|---|---|
| `code` | `text` | `NOT NULL`, FK → `links(code)` ON DELETE CASCADE | ссылка |
| `bucket` | `timestamptz` | `NOT NULL` | начало временно́го бакета (час/минута) |
| `clicks` | `bigint` | `NOT NULL DEFAULT 0` | кликов в бакете |
| | | **PRIMARY KEY (code, bucket)** | upsert-агрегация |

```sql
-- агрегирующий upsert (truncate-гранулярность должна совпадать с той, что спросит /stats)
INSERT INTO click_aggregates (code, bucket, clicks)
VALUES ($1, date_trunc($2 /* 'hour' | 'minute' */, $3::timestamptz), 1)
ON CONFLICT (code, bucket) DO UPDATE
SET clicks = click_aggregates.clicks + EXCLUDED.clicks;
```

> Гранулярность `bucket` на стороне consumer'а должна совпадать с той, что принимает
> `/stats` (`bucket=hour|minute`, см. [api.md](./api.md)). Если consumer пишет только
> часовые бакеты, запрос `bucket=minute` не найдёт данных. Выбери: либо хранить самую мелкую
> гранулярность (минуты) и доагрегировать в час на чтении, либо вести оба уровня. Для
> учебного проекта проще хранить минуты и сворачивать в час запросом.
>
> **Важно (at-least-once):** этот upsert **аддитивный** и потому **не** идемпотентен против
> дублей доставки — повторное событие удвоит счётчик. Для точности consumer должен дедупить
> по `ClickID` до инкремента (см.
> [architecture.md](./architecture.md#kafka-семантика-доставки)).

Сырая таблица `click_events` (необязательна — нужна, если хочешь хранить каждое событие):

| Колонка | Тип | Назначение |
|---|---|---|
| `id` | `bigint GENERATED ALWAYS AS IDENTITY` | PK |
| `code` | `text NOT NULL` | ссылка |
| `clicked_at` | `timestamptz NOT NULL DEFAULT now()` | время клика |
| `ip` | `inet NULL` | источник (если нужно) |

> Рекомендация: для учебного проекта храни **агрегаты** (`click_aggregates`), а не каждое
> сырое событие — это и есть смысл батчинга. `click_events` заводи только если специально
> хочешь показать сырьё в Kafka-consumer'е.

## Индексы — сводка

| Индекс | Таблица | Назначение |
|---|---|---|
| `links_pkey` (PK по `code`) | `links` | редирект и lookup по коду |
| `links_url_hash_plain_key` (partial UNIQUE) | `links` | идемпотентность создания «простых» ссылок (`is_custom=false AND expires_at IS NULL`) |
| `idx_links_expires_at` (partial) | `links` | ускорение sweeper'а и проверок TTL |
| `click_aggregates_pkey` (PK по `code, bucket`) | `click_aggregates` | upsert и выборка для /stats |
| `idx_click_aggregates_code_bucket` | `click_aggregates` | диапазонные запросы по времени |

```sql
-- частичный индекс: индексируем только строки со сроком жизни
CREATE INDEX idx_links_expires_at ON links (expires_at)
WHERE expires_at IS NOT NULL;
```

## Стратегия миграций

> Решение: **goose**. Обоснование:
> - простые SQL-файлы `-- +goose Up` / `-- +goose Down`, читаемые без знания инструмента;
> - можно гонять как CLI и как встроенную библиотеку (`pressly/goose/v3`) — удобно для
>   `testcontainers` (применить миграции к свежей БД из теста);
> - не требует отдельного формата нумерации, версия = timestamp в имени файла.
>
> `golang-migrate` тоже хорош (up/down в отдельных файлах, много драйверов). Если предпочитаешь
> строгое разделение up/down-файлов — бери его; схема БД от выбора не зависит.

Файлы миграций лежат в `migrations/` (см. [project-structure.md](./project-structure.md)):

```
migrations/
  00001_create_links.sql            # Этап 1
  00002_create_click_aggregates.sql # Этап 2
```

Применение в тестах (`testcontainers`):

```go
import "github.com/pressly/goose/v3"

goose.SetBaseFS(embedMigrations) // //go:embed migrations/*.sql
if err := goose.Up(db, "migrations"); err != nil { t.Fatal(err) }
```

## Полный DDL

### `00001_create_links.sql` (Этап 1)

```sql
-- +goose Up
CREATE TABLE links (
    code         text        PRIMARY KEY
                             CHECK (char_length(code) BETWEEN 3 AND 32),
    original_url text        NOT NULL CHECK (original_url <> ''),
    url_hash     bytea       NOT NULL,
    is_custom    boolean     NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NULL,
    click_count  bigint      NOT NULL DEFAULT 0 CHECK (click_count >= 0)
);

-- идемпотентность только для «простых» ссылок (без алиаса и без TTL)
CREATE UNIQUE INDEX links_url_hash_plain_key ON links (url_hash)
WHERE is_custom = false AND expires_at IS NULL;

CREATE INDEX idx_links_expires_at ON links (expires_at)
WHERE expires_at IS NOT NULL;

-- +goose Down
DROP TABLE links;
```

### `00002_create_click_aggregates.sql` (Этап 2)

```sql
-- +goose Up
CREATE TABLE click_aggregates (
    code   text        NOT NULL REFERENCES links(code) ON DELETE CASCADE,
    bucket timestamptz NOT NULL,
    clicks bigint      NOT NULL DEFAULT 0 CHECK (clicks >= 0),

    PRIMARY KEY (code, bucket)
);

CREATE INDEX idx_click_aggregates_code_bucket
    ON click_aggregates (code, bucket DESC);

-- Необязательно: сырьё событий, если хочешь хранить каждый клик
-- CREATE TABLE click_events (
--     id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
--     code       text        NOT NULL,
--     clicked_at timestamptz NOT NULL DEFAULT now(),
--     ip         inet        NULL
-- );

-- +goose Down
DROP TABLE click_aggregates;
```
