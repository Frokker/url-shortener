# Структура проекта

[← К оглавлению](./README.md) · Связанные: [architecture.md](./architecture.md) · [stage-1.md](./stage-1.md) · [stage-2.md](./stage-2.md)

## Содержание

- [Раскладка папок](#раскладка-папок)
- [Ответственность пакетов](#ответственность-пакетов)
- [Порядок написания слоёв](#порядок-написания-слоёв)
- [Конвенции именования](#конвенции-именования)
- [Граф зависимостей](#граф-зависимостей)

## Раскладка папок

Стандартная для Go идиома: `cmd/` — точки входа, `internal/` — приватный код,
не импортируемый извне.

```
url-shortener/
├── cmd/
│   └── server/
│       └── main.go                 # сборка зависимостей, graceful shutdown, запуск
├── internal/
│   ├── config/
│   │   └── config.go               # чтение env → структура Config
│   ├── domain/
│   │   ├── link.go                 # доменные типы (Link, ClickEvent)
│   │   └── errors.go               # сентинел-ошибки: ErrNotFound, ErrExpired, ...
│   ├── service/
│   │   ├── link.go                 # бизнес-логика + интерфейсы Repository/Cache
│   │   └── link_test.go            # table-driven unit-тесты (моки)
│   ├── repository/
│   │   ├── postgres/
│   │   │   ├── links.go            # реализация LinkRepository на pgxpool
│   │   │   └── links_test.go       # интеграционные тесты (testcontainers)
│   │   └── redis/
│   │       └── cache.go            # реализация Cache
│   ├── transport/
│   │   └── http/
│   │       ├── router.go           # сборка chi-роутера + middleware
│   │       ├── handlers.go         # HTTP-обработчики
│   │       ├── dto.go              # request/response JSON-структуры
│   │       ├── errors.go           # маппинг доменных ошибок → HTTP-коды
│   │       └── middleware.go       # request-id, logging, recover, (Эт.2) rate limit
│   ├── pipeline/                   # ── ЭТАП 2 ──
│   │   ├── pipeline.go             # буферизованный канал + worker pool + батч-флаш
│   │   ├── worker.go               # логика воркера (накопление, флаш по N/T)
│   │   └── pipeline_test.go        # -race тесты конвейера
│   ├── kafka/                      # ── ЭТАП 2 ──
│   │   └── producer.go             # producer в топик clicks
│   ├── sweeper/                    # ── ЭТАП 2 ──
│   │   └── sweeper.go              # time.Ticker-горутина чистки протухших ссылок
│   └── metrics/                    # ── ЭТАП 2 ──
│       └── metrics.go              # регистрация Prometheus-коллекторов
├── migrations/
│   ├── 00001_create_links.sql
│   └── 00002_create_click_aggregates.sql
├── deployments/
│   └── docker-compose.yml          # app + postgres + redis (+ kafka на Эт.2)
├── Dockerfile
├── Makefile                        # up, test, test-race, migrate, load-test
├── go.mod
├── go.sum
├── README.md                       # НЕ ТРОГАТЬ — исходная спека
└── docs/                           # эта документация
```

## Ответственность пакетов

| Пакет | Знает про | НЕ знает про | Ответственность |
|---|---|---|---|
| `cmd/server` | всё (composition root) | — | собрать зависимости, запустить сервер, graceful shutdown |
| `config` | env | домен | распарсить окружение в `Config`, валидировать |
| `domain` | ничего внешнего | HTTP, SQL, pgx | чистые типы и сентинел-ошибки |
| `service` | `domain`, интерфейсы repo/cache | `http.*`, `pgx.*` | бизнес-правила, оркестрация |
| `repository/postgres` | `domain`, `pgxpool` | `http.*`, бизнес-правила | SQL, маппинг строк |
| `repository/redis` | `domain`, redis client | бизнес-правила | кэш read-through |
| `transport/http` | `service`, `domain`, `chi` | `pgx.*` | парсинг/валидация HTTP, маппинг ошибок |
| `pipeline` (Эт.2) | `domain`, repo-интерфейс | `http.*` | канал, worker pool, батч-флаш, backpressure |
| `kafka` (Эт.2) | `domain` | бизнес-правила | продьюс событий в топик |
| `sweeper` (Эт.2) | repo-интерфейс | `http.*` | периодическая чистка по тикеру |
| `metrics` (Эт.2) | prometheus | домен | определение и регистрация метрик |

Ключевое правило: **интерфейсы объявляет потребитель**. Интерфейс `LinkRepository` живёт в
`service`, а не в `repository` — так репозиторий не диктует контракт, а зависимость
направлена внутрь.

## Порядок написания слоёв

> Рекомендация: **снизу вверх — repository → service → handler.**

Обоснование:
1. **Контракты рождаются из реальных нужд.** Сначала пишешь `service` и его интерфейс
   `LinkRepository` (на стороне потребителя), затем реализуешь его в `repository/postgres`.
   То есть фактически: определи `domain` → объяви интерфейсы в `service` → реализуй
   `repository` под них → допиши бизнес-логику `service` → надень `handler`.
2. **Тестируемость по ходу.** Репозиторий проверяешь интеграционно (`testcontainers`) сразу,
   сервис — юнит-тестами на моках интерфейса. К моменту handler'а ядро уже зелёное.
3. **Handler — тонкий.** Когда service готов, handler сводится к декоду/валидации/маппингу
   ошибок, и его легко довести.

Практический порядок шагов (детально — в [stage-1.md](./stage-1.md)):

```
1. domain/link.go, domain/errors.go         (типы и ошибки)
2. config/config.go                          (env → Config)
3. service/link.go: интерфейсы + бизнес-логика
4. repository/postgres/links.go              (под интерфейсы)
5. repository/redis/cache.go
6. service/link_test.go                      (table-driven на моках)
7. repository/postgres/links_test.go         (testcontainers)
8. transport/http: dto, handlers, errors, router
9. cmd/server/main.go                        (сборка + graceful shutdown)
```

Альтернатива «сверху вниз» (handler → service → repo) удобна, когда API фиксирован раньше
данных. Здесь API уже задан README, а интересна **доменная и инфраструктурная** часть,
поэтому снизу вверх практичнее.

## Конвенции именования

| Что | Конвенция | Пример |
|---|---|---|
| Пакеты | короткие, в нижнем регистре, без подчёркиваний | `pipeline`, `repository` |
| Интерфейсы | по роли, часто с суффиксом `-er` или доменно | `Cache`, `LinkRepository` |
| Реализации | конкретно, без дублирования имени пакета | `postgres.LinkRepo`, не `postgres.PostgresLinkRepo` |
| Сентинел-ошибки | `Err` + суть | `ErrNotFound`, `ErrAliasTaken` |
| Конструкторы | `New` + тип | `service.NewLinkService(...)` |
| DTO | суффикс назначения | `CreateLinkRequest`, `CreateLinkResponse` |
| Env-переменные | `UPPER_SNAKE_CASE` | `CLICK_CHANNEL_BUFFER` |
| Тест-функции | `TestXxx`, подтесты `t.Run("case", ...)` | `TestCreate_Idempotent` |

- Не «штампуй» имя пакета в тип: `postgres.LinkRepo`, а не `postgres.PostgresLinkRepo` —
  вызывается как `postgres.LinkRepo`, дубль лишний.
- Контекст всегда первый аргумент: `func (s *Service) Create(ctx context.Context, ...)`.
- Ошибки оборачивай с контекстом: `fmt.Errorf("create link: %w", err)`.

## Граф зависимостей

Зависимости направлены внутрь к `domain`; внешние пакеты не зависят от транспорта.

```
            cmd/server  (composition root, знает всех)
                 │ собирает
   ┌─────────────┼───────────────┬───────────────┐
   ▼             ▼               ▼               ▼
transport/http  pipeline(Эт.2)  sweeper(Эт.2)   kafka(Эт.2)
   │             │               │
   └──────┬──────┴───────┬───────┘
          ▼              ▼
       service ──▶ интерфейсы (Repository, Cache)
          │              ▲
          ▼              │ реализуют
       domain     repository/{postgres,redis}
          ▲              │
          └──────────────┘ (репозиторий маппит в domain-типы)
```

Никаких циклов: `transport` → `service` → `domain`; `repository` → `domain`. `domain` не
импортирует никого.
