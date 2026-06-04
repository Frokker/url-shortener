# Этап 1 — синхронный скелет

[← К оглавлению](./README.md) · Связанные: [architecture.md](./architecture.md) · [api.md](./api.md) · [data-model.md](./data-model.md) · [project-structure.md](./project-structure.md) · [stage-2.md](./stage-2.md)

## Цель этапа

Собрать рабочий сокращатель ссылок с **намеренно синхронным** инкрементом счётчика кликов
на горячем пути. Это «плохо для прода» осознанно: на Этапе 2 у тебя будет конкретная боль
для рефакторинга и цифра p99 для замера «до/после». Только HTTP, никакого gRPC.

## Scope

В рамках Этапа 1:
- Создание ссылки (с опциональным `custom_alias` и `ttl_seconds`), идемпотентность по URL.
- Редирект по коду с **синхронным** `+1` к счётчику и проверкой TTL.
- Метаданные ссылки, удаление.
- `/healthz` (без зависимостей), `/readyz` (Postgres — критичен → 503; Redis — мягкая
  деградация, не валит готовность; обоснование в [api.md](./api.md#get-healthz-и-readyz)).
- Redis как **синхронный** read-through кэш (это не конкурентность, просто кэш).
- Чистые слои с интерфейсами, валидация, идиоматичные ошибки, graceful shutdown.
- Table-driven unit-тесты на service, интеграционные на repository через testcontainers.
- Поднятие одной командой `docker compose up`.

Вне Этапа 1 (это Этап 2): каналы, worker pool, Kafka, Prometheus, rate limit, sweeper.

## Порядок реализации

Снизу вверх (обоснование — [project-structure.md](./project-structure.md#порядок-написания-слоёв)).

### 1. Домен и ошибки

```go
// internal/domain/errors.go
var (
    ErrNotFound   = errors.New("link not found")
    ErrExpired    = errors.New("link expired")
    ErrAliasTaken = errors.New("alias already taken")
    ErrInvalid    = errors.New("invalid input")
)
```

```go
// internal/domain/link.go
type Link struct {
    Code        string
    OriginalURL string
    URLHash     []byte
    IsCustom    bool
    CreatedAt   time.Time
    ExpiresAt   *time.Time
    ClickCount  int64
}
```

### 2. Конфиг

Читаем env в структуру (см. [configuration.md](./configuration.md)). Для Этапа 1 нужны:
`HTTP_ADDR`, `POSTGRES_DSN`, `REDIS_ADDR`, `BASE_URL`, `SHUTDOWN_TIMEOUT`, `CACHE_TTL`.

### 3. Сервис и интерфейсы

Объяви интерфейсы `LinkRepository` и `Cache` в пакете `service` (на стороне потребителя) и
напиши бизнес-логику: генерация кода (base62), нормализация URL + `url_hash` (sha256),
проверка алиаса регуляркой **и по reserved-списку** (`metrics`/`api`/`healthz`/... → 400,
иначе алиас затенит маршрут), вычисление `expires_at`, проверка TTL при редиректе.

```go
func (s *LinkService) Redirect(ctx context.Context, code string) (string, error) {
    // 1. cache.Get → miss → repo.GetByCode → cache.Set
    l, err := s.lookup(ctx, code)
    if err != nil {
        return "", err // ErrNotFound пробросится наверх
    }
    if l.ExpiresAt != nil && l.ExpiresAt.Before(time.Now()) {
        return "", domain.ErrExpired
    }
    // 3. ЭТАП 1: СИНХРОННЫЙ инкремент — намеренно блокирует
    if err := s.repo.IncrementClicks(ctx, code); err != nil {
        return "", fmt.Errorf("increment clicks: %w", err)
    }
    return l.OriginalURL, nil
}
```

### 4. Репозиторий Postgres (под интерфейсы)

`pgxpool`, запросы из [data-model.md](./data-model.md): `Create` с `ON CONFLICT (url_hash)`,
`GetByCode`, `GetByURL`, `IncrementClicks`, `Delete`. Маппинг ошибок pgx в доменные:
нарушение PK по `code` → `ErrAliasTaken`, нет строк → `ErrNotFound`.

### 5. Репозиторий Redis

Read-through кэш: `Get(code)`, `Set(link, ttl)`, `Del(code)`. Ключ — `link:{code}`,
значение — JSON. Инвалидация при `Delete`.

### 6–7. Тесты

- `service/link_test.go` — table-driven на моках интерфейсов (см. ниже).
- `repository/postgres/links_test.go` — testcontainers (см. ниже).

### 8. Транспорт HTTP

chi-роутер, DTO, обработчики, маппинг доменных ошибок → HTTP (см. [api.md](./api.md)).
Базовые middleware: request-id, structured logging (`slog`), recover.

```go
func mapError(err error) (int, errBody) {
    switch {
    case errors.Is(err, domain.ErrNotFound):   return 404, body("NOT_FOUND", err)
    case errors.Is(err, domain.ErrExpired):    return 410, body("GONE", err)
    case errors.Is(err, domain.ErrAliasTaken): return 409, body("ALIAS_TAKEN", err)
    case errors.Is(err, domain.ErrInvalid):    return 400, body("INVALID_REQUEST", err)
    default:                                    return 500, body("INTERNAL", err)
    }
}
```

### 9. main + graceful shutdown

```go
func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    cfg := config.Load()
    pool := mustPgxPool(ctx, cfg)
    defer pool.Close()                     // ресурсы закрываются на выходе
    rdb := mustRedis(cfg)
    defer rdb.Close()
    svc := service.NewLinkService(postgres.New(pool), redis.New(rdb), cfg)
    srv := &http.Server{Addr: cfg.HTTPAddr, Handler: httptransport.NewRouter(svc, cfg)}

    go func() {
        if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            log.Fatal(err)
        }
    }()

    <-ctx.Done() // сигнал
    sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
    defer cancel()
    _ = srv.Shutdown(sctx) // дать запросам доиграть; затем сработают defer'ы выше
}
```

## Definition of done — чеклист

- [ ] Слои `handler → service → repository` разделены, интерфейсы на границах.
- [ ] Service не импортирует `net/http`; repository не содержит бизнес-логики.
- [ ] Валидация входа: пустой/невалидный URL → 400, плохой алиас → 400, `ttl < 0` → 400.
- [ ] Идемпотентность: тот же URL **без алиаса и без TTL** → тот же `code` (через частичный
      `UNIQUE(url_hash) WHERE is_custom=false AND expires_at IS NULL`); алиас/TTL → новая строка.
- [ ] Кастомный алиас занят → `409 ALIAS_TAKEN`.
- [ ] Редирект отдаёт **302**, синхронно инкрементит `click_count`, проверяет TTL (просрочено → 410).
- [ ] Read-through Redis-кэш: miss → Postgres → populate; инвалидация при delete.
- [ ] Идиоматичные ошибки: сентинелы, `%w`-обёртка, `errors.Is/As` в маппинге.
- [ ] `/healthz` без зависимостей; `/readyz` → `503` только при недоступном Postgres; Redis
      проверяется, но его отказ = `degraded`, не `503` (Redis — всего лишь кэш).
- [ ] Graceful shutdown по `SIGINT/SIGTERM` с таймаутом, закрытие pool/redis.
- [ ] Миграции применяются (goose), таблица `links` с индексами создаётся.
- [ ] Table-driven unit-тесты на service зелёные.
- [ ] Интеграционные тесты репозитория на testcontainers зелёные.
- [ ] `docker compose up` поднимает app + postgres + redis одной командой.

## Подход к тестированию

### Table-driven unit-тесты на service (моки)

Мокаем интерфейсы `LinkRepository`/`Cache` (ручные fake-структуры или `testify/mock`).

```go
func TestRedirect(t *testing.T) {
    past := time.Now().Add(-time.Hour)
    tests := []struct {
        name     string
        link     domain.Link
        repoErr  error
        wantURL  string
        wantErr  error
    }{
        {"ok", domain.Link{Code: "a", OriginalURL: "https://x"}, nil, "https://x", nil},
        {"expired", domain.Link{Code: "a", ExpiresAt: &past}, nil, "", domain.ErrExpired},
        {"not found", domain.Link{}, domain.ErrNotFound, "", domain.ErrNotFound},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            svc := service.NewLinkService(&fakeRepo{link: tt.link, err: tt.repoErr}, &fakeCache{}, cfg)
            url, err := svc.Redirect(context.Background(), "a")
            require.ErrorIs(t, err, tt.wantErr)
            assert.Equal(t, tt.wantURL, url)
        })
    }
}
```

### Интеграционные тесты репозитория (testcontainers)

Поднимаем настоящий Postgres в контейнере, применяем миграции, гоняем CRUD по-настоящему.

```go
func setupPostgres(t *testing.T) *pgxpool.Pool {
    ctx := context.Background()
    pgC, err := postgrescontainer.Run(ctx, "postgres:16-alpine",
        postgrescontainer.WithDatabase("test"),
        postgrescontainer.WithUsername("test"),
        postgrescontainer.WithPassword("test"),
        testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
    )
    require.NoError(t, err)
    t.Cleanup(func() { _ = pgC.Terminate(ctx) })

    dsn, _ := pgC.ConnectionString(ctx, "sslmode=disable")
    pool, err := pgxpool.New(ctx, dsn)
    require.NoError(t, err)
    // применить миграции goose к pool/db
    return pool
}

func TestCreate_Idempotent(t *testing.T) {
    pool := setupPostgres(t)
    repo := postgres.New(pool)
    l1, _ := repo.Create(ctx, linkFor("https://example.com"))
    l2, _ := repo.Create(ctx, linkFor("https://example.com")) // тот же URL
    assert.Equal(t, l1.Code, l2.Code) // идемпотентность через UNIQUE(url_hash)
}
```

Проверь: `Create` + повтор (идемпотентность), занятый алиас → `ErrAliasTaken`, `GetByCode`
miss → `ErrNotFound`, `IncrementClicks` действительно растит счётчик, `Delete`.

## Docker Compose

```yaml
# deployments/docker-compose.yml
services:
  app:
    build: ..
    environment:
      HTTP_ADDR: ":8080"
      POSTGRES_DSN: "postgres://app:app@postgres:5432/urlshort?sslmode=disable"
      REDIS_ADDR: "redis:6379"
      BASE_URL: "http://localhost:8080"
    ports: ["8080:8080"]
    depends_on:
      postgres: { condition: service_healthy }
      redis:    { condition: service_healthy }

  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: app
      POSTGRES_DB: urlshort
    ports: ["5432:5432"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U app -d urlshort"]
      interval: 2s
      timeout: 3s
      retries: 10

  redis:
    image: redis:7-alpine
    ports: ["6379:6379"]
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 2s
      timeout: 3s
      retries: 10
```

Поднять:

```bash
docker compose -f deployments/docker-compose.yml up --build
```

> Перед стартом приложения примени миграции (через goose CLI, init-контейнер или внутри
> `main` на старте). Для учебного проекта проще всего — `goose.Up` при старте `main`.

## Что замерить перед переходом на Этап 2

Зафиксируй **p50/p99 редиректа** под нагрузкой (см. [operations.md](./operations.md#нагрузочное-тестирование)).
Это «до» — главная цифра, с которой будешь сравнивать после рефакторинга на Этапе 2.
