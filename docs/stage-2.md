# Этап 2 — конкурентность

[← К оглавлению](./README.md) · Связанные: [architecture.md](./architecture.md) · [operations.md](./operations.md) · [configuration.md](./configuration.md) · [stage-1.md](./stage-1.md)

## Цель этапа

Убрать синхронный инкремент счётчика с горячего пути и заменить его асинхронным конвейером
на конкурентных примитивах Go. Доказать рефакторинг цифрами: p99 редиректа **до и после**.
Никаких новых внешних зависимостей ради зависимостей — добавляем только Kafka и Prometheus.

## Главный рефактор

```
БЫЛО (Этап 1):   redirect ──▶ UPDATE click_count+1 (блокирует) ──▶ 302
СТАЛО (Этап 2):  redirect ──▶ clickCh <- event (не блокирует) ──▶ 302
                                   │
                       worker pool ─┴─▶ батч ──▶ один UPDATE на N кодов
```

Полная архитектура конвейера, backpressure и graceful drain описаны в
[architecture.md](./architecture.md#async-click-pipeline). Здесь — план реализации по фичам.

## Фича 1: Async click pipeline

### Шаги

1. **Канал и событие.**

   ```go
   type ClickEvent struct {
       ClickID string    // уникальный id для дедупа Kafka-аналитики (UUID или code:unixnano:rand)
       Code    string    // он же ключ партиции Kafka
       TS      time.Time
   }
   clickCh := make(chan ClickEvent, cfg.ClickChannelBuffer) // буфер, напр. 10000
   ```

   `ClickID` нужен не внутреннему счётчику (там инкремент-дельты), а Kafka-ветке:
   at-least-once может прислать дубль, и consumer дедупит по `ClickID` (см.
   [architecture.md](./architecture.md#kafka-семантика-доставки)).

2. **Неблокирующий send из сервиса** (backpressure = drop + метрика):

   ```go
   func (s *LinkService) recordClick(ev ClickEvent) {
       select {
       case s.clickCh <- ev:
       default:
           s.metrics.ClicksDropped.Inc() // канал полон → дропаем, латентность защищена
       }
   }
   ```

   `Redirect` теперь **не** вызывает `IncrementClicks`, а зовёт `recordClick` и сразу
   возвращает URL.

3. **Worker pool** на `WORKER_POOL_SIZE` горутин, каждая копит локальный батч `map[code]int`,
   флашит по `BATCH_SIZE_N` **или** по `time.Ticker(FLUSH_INTERVAL_T)` — что раньше. Полный
   псевдокод воркера — в [architecture.md](./architecture.md#async-click-pipeline).

4. **Батчевый флаш** одним запросом (см. [data-model.md](./data-model.md#этап-2-click_events-и-агрегаты)):

   ```go
   func (r *LinkRepo) BatchIncrement(ctx context.Context, deltas map[string]int) error {
       // собрать VALUES ($code,$delta),... и сделать один UPDATE ... FROM (VALUES ...)
   }
   ```

5. **Graceful drain**: `http.Shutdown` → `close(clickCh)` → воркеры дочитывают `range`,
   делают финальный `flush`, `wg.Wait()` под потолком `SHUTDOWN_TIMEOUT`. Воркер
   **не** селектит на отмену root-контекста (иначе по SIGTERM бросит непрочитанный буфер) —
   сигнал завершения только через закрытие канала. Порядок и обоснование обязательны (см.
   [architecture.md](./architecture.md#graceful-shutdown-и-drain)).

### Backpressure: принятое решение

**Drop + метрика** (`clicks_dropped_total`). Блокирующий send вернул бы блокировку на горячий
путь — ровно ту боль, которую убираем. Полное обоснование с таблицей альтернатив —
[architecture.md](./architecture.md#анализ-backpressure).

## Фича 2: Kafka producer

Каждое событие клика дополнительно уходит в топик `clicks` для внешней аналитики.
Продьюс делает воркер (или отдельная горутина) — **не** горячий путь редиректа.

- Начни с **fire-and-forget** (async, без ожидания ack) — видно неблокирующую отправку.
- Затем переключи конфигом на **at-least-once** (`acks=all`, idempotent producer, ретраи) и
  обсуди дубли → идемпотентный consumer (upsert в `click_aggregates` по `(code, bucket)`).

Тонкости семантики доставки и выбор Kafka vs NATS — в
[architecture.md](./architecture.md#kafka-семантика-доставки).

Consumer (отдельный процесс или горутина) читает топик и наполняет `click_aggregates`,
откуда отвечает `GET /api/v1/links/{code}/stats`.

## Фича 3: Rate limiting

Token-bucket middleware на **всём** chi-роутере (отдельной ручки нет).

```go
func RateLimit(cfg RateLimitCfg) func(http.Handler) http.Handler {
    limiters := newKeyedLimiterStore(cfg) // map[key]*rate.Limiter + TTL-эвикция
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            key := clientKey(r, cfg)            // IP или API-ключ из заголовка
            lim := limiters.get(key)            // rate.NewLimiter(rps, burst)
            w.Header().Set("X-RateLimit-Limit", strconv.Itoa(cfg.Burst))
            if !lim.Allow() {                   // reject без «бронирования» токена
                w.Header().Set("Retry-After", "1")
                writeError(w, 429, "RATE_LIMITED", "too many requests")
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

- База — `golang.org/x/time/rate` (часть расширенной stdlib, не «зависимость ради зависимости»).
- **`Allow()`, а не `Reserve()`.** `Allow()` — это чистый «пропустить/отклонить» без побочного
  «бронирования» токена; `Reserve()` резервирует токен и требует обязательного `Cancel()` на
  каждом пути отказа, иначе подъедает бакет. Для middleware reject-семантики `Allow()` идиоматичнее.
- **Заголовки только те, что реально вычислимы.** `golang.org/x/time/rate` **не** отдаёт
  «остаток токенов», поэтому `X-RateLimit-Remaining`/`-Reset` для непрерывного token-bucket
  не определены корректно и в спеке убраны (см. [api.md](./api.md#rate-limiting-этап-2)).
  Отдаём `X-RateLimit-Limit` (= burst) всегда и `Retry-After` при `429`. Если очень нужен
  `Remaining` — придётся вести счётчик токенов вручную, мимо `rate.Limiter`.
- Ключ — IP (по умолчанию) или API-ключ (`RATE_LIMIT_KEY=api_key`).
- Эвиктируй неактивные лимитеры по TTL, чтобы map не рос бесконечно.

## Фича 4: Background expiry sweeper

Горутина на `time.Ticker`, периодически удаляющая просроченные ссылки.

```go
func (s *Sweeper) Run(ctx context.Context) {
    t := time.NewTicker(s.interval) // SWEEP_INTERVAL, напр. 1m
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return // graceful stop
        case <-t.C:
            // DELETE ... WHERE expires_at < now() RETURNING code
            codes, err := s.repo.DeleteExpired(ctx)
            if err != nil {
                s.log.Error("sweep failed", "err", err)
                continue
            }
            for _, code := range codes {
                _ = s.cache.Del(ctx, code) // инвалидируем кэш удалённых ссылок
            }
            s.metrics.SweptTotal.Add(float64(len(codes)))
        }
    }
}
```

- Запускается в `main`, останавливается по отмене `ctx` в graceful shutdown.
- Использует частичный индекс `idx_links_expires_at` (см. [data-model.md](./data-model.md#индексы--сводка)).
- **`DeleteExpired` возвращает `RETURNING code`**, и sweeper делает `cache.Del` по каждому —
  иначе удалённая ссылка останется в Redis до `CACHE_TTL` (по умолчанию 10m) и будет
  отдаваться из кэша уже после удаления строки. Полагаться только на TTL кэша здесь
  недостаточно (см. [data-model.md](./data-model.md#ttl-и-истечение)).
- Аналогично: при кэшировании ссылки с `expires_at` ставь TTL ключа
  `min(CACHE_TTL, время_до_истечения)`, чтобы протухшая ссылка не пережила свой срок в кэше.

## Фича 5: Метрики Prometheus

`/metrics` отдаёт коллекторы; полный список с именами/типами/лейблами —
[operations.md](./operations.md#метрики-prometheus). Минимум для Этапа 2:

| Метрика | Тип | Зачем |
|---|---|---|
| `urlshort_http_request_duration_seconds` | Histogram | p50/p99 латентности (главная цифра) |
| `urlshort_click_queue_depth` | Gauge | живая глубина канала под нагрузкой |
| `urlshort_click_batch_size` | Histogram | число **событий** во флаше (`pending`, не `len(map)`) |
| `urlshort_clicks_dropped_total` | Counter | backpressure-дропы |

`queue_depth` обновляй как `len(clickCh)` периодически или при каждом send.

> `batch_size` наблюдает счётчик событий `pending`, а не количество уникальных кодов
> `len(batch)`. Это тот же `pending`, по которому срабатывает порог `BATCH_SIZE_N` — иначе
> на перекошенном трафике метрика покажет единицы, а порог размера не сработает (см.
> [architecture.md](./architecture.md#async-click-pipeline)).

## Чеклист корректности конкурентности

- [ ] `go test -race ./...` зелёный.
- [ ] `context.Context` пробрасывается везде: handler → service → repo → pgx; sweeper, воркеры,
      producer слушают отмену.
- [ ] Нет гонок по разделяемому состоянию (батчи **локальны** для воркера, не общие).
- [ ] `clickCh` закрывается ровно один раз и **после** `http.Shutdown` (иначе паника send в
      закрытый канал).
- [ ] Воркер завершается по **закрытию канала** (`ok == false`), а не по `ctx.Done()`;
      принудительный потолок — отдельный `SHUTDOWN_TIMEOUT` в `main`, а не внутри воркера.
- [ ] На shutdown событие не теряется **при дренаже в пределах `SHUTDOWN_TIMEOUT`**: воркеры
      доделывают финальный flush, `sync.WaitGroup` дожидается их. За потолком — осознанная
      потеря остатка (метрика), это граница graceful-периода.
- [ ] Send в канал неблокирующий (drop-политика) — горячий путь не блокируется.
- [ ] `keyed limiter store` потокобезопасен (мьютекс/`sync.Map`) и эвиктит старые ключи.
- [ ] Метрики `queue_depth`/`batch_size`/`dropped` обновляются и видны под нагрузкой.
- [ ] Kafka producer не сидит на горячем пути; ошибки продьюса → метрика, не блок.

## Методика замера p99 «до/после»

Главная цель этапа — показать цифрой, что редирект больше не ждёт запись в БД.

### Что и как мерить

> **Сначала выключи rate limit для замера.** Лимитер висит на всём роутере (по умолчанию
> `RATE_LIMIT_ENABLED=true`, `RATE_LIMIT_RPS=100`/burst `200` **на один IP**). Нагрузка
> идёт с одного хоста на `-rate=2000`, поэтому с включённым лимитером почти всё уйдёт в
> `429`, а p99 (главная цифра проекта) станет бессмысленным. Для прогона выставь
> `RATE_LIMIT_ENABLED=false` (или подними лимит существенно выше целевого RPS). Сам лимитер
> тестируй отдельным сценарием, а не во время замера латентности конвейера.

1. Подними **Этап 1**, прогрей кэш (создай ссылку, кликни пару раз).
2. Запусти нагрузку **только на редирект** `/{code}` (горячий путь) с выключенным лимитером.
3. Сними p50/p99 латентности.
4. Подними **Этап 2** (так же без лимитера), повтори ту же нагрузку на тот же эндпоинт.
5. Положи цифры рядом.

Ожидание: на Этапе 1 p99 редиректа тащит за собой латентность `UPDATE` + контеншн по строке
счётчика; на Этапе 2 p99 редиректа становится плоским (send в канал — это наносекунды),
независимым от записи в БД. `queue_depth` под нагрузкой растёт, дропы появляются только при
перегрузке.

### vegeta

```bash
# создать ссылку, получить code (например promo)
echo "GET http://localhost:8080/promo" > target.txt

vegeta attack -targets=target.txt -rate=2000 -duration=30s \
  | tee results.bin \
  | vegeta report -type='hist[0,1ms,5ms,10ms,25ms,50ms,100ms,250ms]'

vegeta report -type=json results.bin | jq '.latencies | {p50:.["50th"], p99:.["99th"]}'
```

### k6

```javascript
// redirect_load.js
import http from 'k6/http';
export const options = {
  scenarios: { redirect: { executor: 'constant-arrival-rate',
    rate: 2000, timeUnit: '1s', duration: '30s',
    preAllocatedVUs: 200, maxVUs: 1000 } },
  thresholds: { http_req_duration: ['p(99)<50'] }, // ожидание для Этапа 2
};
export default function () {
  http.get('http://localhost:8080/promo', { redirects: 0 }); // не следовать за 302
}
```

```bash
k6 run redirect_load.js
```

### Шаблон таблицы результатов

| Метрика | Этап 1 (синхронный) | Этап 2 (асинхронный) |
|---|---|---|
| p50 редиректа | _напр._ 8 ms | _напр._ 0.7 ms |
| p99 редиректа | _напр._ 95 ms | _напр._ 3 ms |
| RPS устойчиво | ... | ... |
| `clicks_dropped_total` | n/a | ... под перегрузкой |
| max `queue_depth` | n/a | ... |

> Это и есть доказательство, что разница понята. Подробности по нагрузке и наблюдаемости —
> в [operations.md](./operations.md).

## Definition of done — чеклист

- [ ] Редирект больше не ждёт запись в БД (подтверждено замером p99 до/после).
- [ ] `go test -race ./...` зелёный.
- [ ] На shutdown событие из канала не потеряно при дренаже в пределах `SHUTDOWN_TIMEOUT`
      (drain через закрытие канала + финальный flush; потолок по времени — в `main`).
- [ ] Метрики показывают живой `queue_depth` под нагрузкой; видны `batch_size` и `dropped`.
- [ ] Async pipeline: канал → worker pool → батчевый флаш одним запросом.
- [ ] Backpressure: drop + метрика, обоснованно.
- [ ] Kafka producer в топик `clicks`; опробованы fire-and-forget и at-least-once.
- [ ] Rate-limit middleware на всём роутере, заголовки `X-RateLimit-*`, 429 при превышении.
- [ ] Expiry sweeper на `time.Ticker` чистит протухшие ссылки, останавливается по `ctx`.
- [ ] Нагрузочный тест прогнан (vegeta/k6), p50/p99 обоих этапов зафиксированы рядом.
