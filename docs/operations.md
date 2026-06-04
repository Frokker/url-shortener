# Эксплуатация

[← К оглавлению](./README.md) · Связанные: [stage-1.md](./stage-1.md) · [stage-2.md](./stage-2.md) · [configuration.md](./configuration.md) · [api.md](./api.md)

## Содержание

- [Запуск локально](#запуск-локально)
- [Миграции](#миграции)
- [Health и readiness](#health-и-readiness)
- [Метрики Prometheus](#метрики-prometheus)
- [Нагрузочное тестирование](#нагрузочное-тестирование)
- [Graceful shutdown](#graceful-shutdown)
- [Типовые проблемы](#типовые-проблемы)

## Запуск локально

### Через docker-compose (рекомендуется)

```bash
# Этап 1: app + postgres + redis
docker compose -f deployments/docker-compose.yml up --build

# Этап 2: добавь kafka в compose (профиль или отдельный файл) и подними с её сервисами
docker compose -f deployments/docker-compose.yml --profile stage2 up --build
```

Полный compose для Этапа 1 — в [stage-1.md](./stage-1.md#docker-compose). Для Этапа 2
добавляются сервисы Kafka (например `redpanda` как лёгкая Kafka-совместимая замена) и,
опционально, Prometheus.

### Локально без контейнера приложения

```bash
# поднять только зависимости
docker compose -f deployments/docker-compose.yml up postgres redis -d

# применить миграции и запустить
export POSTGRES_DSN="postgres://app:app@localhost:5432/urlshort?sslmode=disable"
export REDIS_ADDR="localhost:6379"
go run ./cmd/server
```

Удобные цели Makefile:

```makefile
up:         ## поднять стек
	docker compose -f deployments/docker-compose.yml up --build
test:       ## unit + integration
	go test ./...
test-race:  ## гонки (Этап 2 — обязательно)
	go test -race ./...
migrate:    ## применить миграции
	goose -dir migrations postgres "$(POSTGRES_DSN)" up
load:       ## нагрузка на редирект
	k6 run scripts/redirect_load.js
```

## Миграции

goose (см. обоснование в [data-model.md](./data-model.md#стратегия-миграций)).

```bash
# применить все
goose -dir migrations postgres "$POSTGRES_DSN" up
# откатить последнюю
goose -dir migrations postgres "$POSTGRES_DSN" down
# статус
goose -dir migrations postgres "$POSTGRES_DSN" status
```

Для учебного проекта проще всего применять миграции встроенно при старте `main`
(`goose.Up(db, "migrations")` с `//go:embed migrations/*.sql`), чтобы `docker compose up`
поднимал всё одной командой без отдельного шага.

## Health и readiness

| Эндпоинт | Назначение | Использование |
|---|---|---|
| `GET /healthz` | процесс жив, без проверки зависимостей | liveness-проба оркестратора |
| `GET /readyz` | Postgres **и** Redis отвечают на ping | readiness-проба; 503 → трафик не слать |

```bash
curl -s http://localhost:8080/healthz   # {"status":"ok"}
curl -s http://localhost:8080/readyz     # {"status":"ready","checks":{...}}
```

Подробные тела ответов — в [api.md](./api.md#get-healthz-и-readyz). `/healthz` намеренно не
дёргает БД, чтобы временная недоступность зависимости не вызывала рестарт пода.

## Метрики Prometheus

Эндпоинт `GET /metrics` (Этап 2). Префикс всех метрик — `urlshort_`.

| Метрика | Тип | Лейблы | Описание |
|---|---|---|---|
| `urlshort_http_request_duration_seconds` | Histogram | `method`, `route`, `status` | латентность запросов; p50/p99 — главная цифра до/после |
| `urlshort_http_requests_total` | Counter | `method`, `route`, `status` | счётчик запросов по кодам |
| `urlshort_click_queue_depth` | Gauge | — | текущая глубина канала кликов (`len(clickCh)`) |
| `urlshort_click_batch_size` | Histogram | — | размер флашируемых батчей |
| `urlshort_clicks_dropped_total` | Counter | — | события, отброшенные при полном буфере (backpressure) |
| `urlshort_clicks_flushed_total` | Counter | — | успешно записанные в БД клики |
| `urlshort_flush_errors_total` | Counter | — | ошибки батчевого флаша |
| `urlshort_cache_hits_total` | Counter | — | попадания в Redis |
| `urlshort_cache_misses_total` | Counter | — | промахи Redis |
| `urlshort_kafka_produce_errors_total` | Counter | — | ошибки продьюса в топик `clicks` |
| `urlshort_links_swept_total` | Counter | — | удалено протухших ссылок sweeper'ом |
| `urlshort_rate_limited_total` | Counter | `route` | запросы, отклонённые rate-limit'ом (429) |

Что смотреть под нагрузкой:
- `urlshort_click_queue_depth` — растёт при всплеске, должен спадать; уперся в потолок буфера
  → появляются `clicks_dropped_total`.
- `urlshort_http_request_duration_seconds{route="/{code}"}` — p99 редиректа; на Этапе 2
  должен быть плоским и низким.
- `urlshort_click_batch_size` — подтверждает, что батчинг работает (флаши по N, а не по 1).

PromQL-примеры:

```promql
# p99 редиректа
histogram_quantile(0.99,
  sum(rate(urlshort_http_request_duration_seconds_bucket{route="/{code}"}[1m])) by (le))

# доля дропов
rate(urlshort_clicks_dropped_total[1m])
  / rate(urlshort_http_requests_total{route="/{code}"}[1m])
```

```bash
curl -s http://localhost:8080/metrics | grep urlshort_
```

## Нагрузочное тестирование

Цель — снять p50/p99 редиректа и сравнить Этап 1 с Этапом 2. Полная методика и шаблон
таблицы результатов — в [stage-2.md](./stage-2.md#методика-замера-p99-допосле).

### Подготовка

```bash
# создать ссылку и прогреть кэш
CODE=$(curl -s -X POST http://localhost:8080/api/v1/links \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com"}' | jq -r .code)
curl -s -o /dev/null "http://localhost:8080/$CODE"   # прогрев
echo "code=$CODE"
```

### vegeta

```bash
echo "GET http://localhost:8080/$CODE" > target.txt
vegeta attack -targets=target.txt -rate=2000 -duration=30s \
  | vegeta report -type=json | jq '{p50:.latencies["50th"], p99:.latencies["99th"]}'
```

### k6

```bash
k6 run scripts/redirect_load.js   # см. пример в stage-2.md
```

Во время прогона держи открытым `/metrics` (или Prometheus/Grafana) и наблюдай
`urlshort_click_queue_depth` — живая глубина очереди под нагрузкой это и есть доказательство
работающего конвейера.

## Graceful shutdown

По `SIGINT`/`SIGTERM` приложение завершается, **не теряя принятых событий кликов**.
Точная последовательность и обоснование порядка — в
[architecture.md](./architecture.md#graceful-shutdown-и-drain).

Кратко:
1. сигнал → `http.Server.Shutdown` (перестаём принимать новые запросы, даём текущим доиграть);
2. `close(clickCh)` (только после Shutdown — иначе паника send в закрытый канал);
3. воркеры дочитывают канал, делают **финальный flush** остатка батча, `wg.Wait()`;
4. останавливаем sweeper (отмена ctx) и Kafka producer (flush + close);
5. закрываем pgxpool и redis client.

Проверить вручную:

```bash
# под нагрузкой отправь SIGTERM и убедись по логам/метрикам,
# что queue_depth дренировался в 0 и был финальный flush
docker compose kill -s SIGTERM app   # или Ctrl+C при go run
```

В логах должно быть видно: `http server stopped` → `click channel closed` →
`workers drained, final flush N events` → `shutdown complete`. `clicks_flushed_total` после
рестарта должен соответствовать числу принятых кликов (минус осознанные дропы при перегрузке).

## Типовые проблемы

| Симптом | Вероятная причина | Что делать |
|---|---|---|
| `/readyz` → 503 | Postgres/Redis не подняты | проверь `docker compose ps`, healthcheck'и |
| Паника `send on closed channel` на shutdown | `close(clickCh)` до `http.Shutdown` | исправь порядок (см. graceful drain) |
| `go test -race` падает | общий батч между воркерами | сделай батч локальным для воркера |
| `clicks_dropped_total` высокий | буфер мал / воркеров мало / медленный флаш | подними `CLICK_CHANNEL_BUFFER`, `WORKER_POOL_SIZE`, проверь БД |
| p99 редиректа высокий на Этапе 2 | случайно остался синхронный `IncrementClicks` | убедись, что `Redirect` только шлёт в канал |
| Счётчик `clicks` отстаёт | это норма: до `FLUSH_INTERVAL_T` | ожидаемое следствие батчинга |
| Идемпотентность не работает | нет `UNIQUE(url_hash)` или разная нормализация | проверь миграцию и правила нормализации URL |
