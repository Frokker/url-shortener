# Конфигурация

[← К оглавлению](./README.md) · Связанные: [stage-1.md](./stage-1.md) · [stage-2.md](./stage-2.md) · [operations.md](./operations.md)

Конфиг читается из переменных окружения в структуру `Config` при старте. Никаких файлов
конфигурации — только env (12-factor). Невалидный или отсутствующий обязательный параметр →
приложение падает на старте с понятной ошибкой (fail-fast).

## Содержание

- [Переменные окружения](#переменные-окружения)
- [Пример .env](#пример-env)
- [Структура Config](#структура-config)
- [Загрузка и валидация](#загрузка-и-валидация)

## Переменные окружения

### Этап 1

| Переменная | Тип | По умолчанию | Обяз. | Описание |
|---|---|---|---|---|
| `HTTP_ADDR` | string | `:8080` | нет | адрес/порт HTTP-сервера |
| `BASE_URL` | string | `http://localhost:8080` | нет | база для построения `short_url` в ответах |
| `POSTGRES_DSN` | string | — | **да** | DSN Postgres, напр. `postgres://app:app@localhost:5432/urlshort?sslmode=disable` |
| `POSTGRES_MAX_CONNS` | int | `10` | нет | размер пула pgxpool |
| `REDIS_ADDR` | string | `localhost:6379` | нет | адрес Redis |
| `REDIS_PASSWORD` | string | `` | нет | пароль Redis (если есть) |
| `REDIS_DB` | int | `0` | нет | номер БД Redis |
| `CACHE_TTL` | duration | `10m` | нет | TTL записей в read-through кэше |
| `CODE_LENGTH` | int | `7` | нет | длина генерируемого кода (base62) |
| `SHUTDOWN_TIMEOUT` | duration | `15s` | нет | таймаут graceful shutdown |
| `LOG_LEVEL` | string | `info` | нет | уровень slog: `debug`/`info`/`warn`/`error` |
| `LOG_FORMAT` | string | `json` | нет | формат slog: `json`/`text` |

### Этап 2 (добавляются)

| Переменная | Тип | По умолчанию | Обяз. | Описание |
|---|---|---|---|---|
| `CLICK_CHANNEL_BUFFER` | int | `10000` | нет | размер буфера канала кликов; при переполнении — drop |
| `WORKER_POOL_SIZE` | int | `4` | нет | число воркеров, читающих канал и флашащих батчи |
| `BATCH_SIZE_N` | int | `500` | нет | флаш батча по достижении N событий |
| `FLUSH_INTERVAL_T` | duration | `1s` | нет | флаш батча по таймеру (что раньше — N или T) |
| `KAFKA_BROKERS` | string (csv) | `localhost:9092` | нет* | список брокеров; пусто → Kafka отключена |
| `KAFKA_TOPIC` | string | `clicks` | нет | топик для событий кликов |
| `KAFKA_ACKS` | string | `none` | нет | `none`=acks=0 (fire-and-forget), `leader`=acks=1, `all`=acks=all (at-least-once) |
| `KAFKA_ENABLED` | bool | `false` | нет | включить producer |
| `RATE_LIMIT_ENABLED` | bool | `true` | нет | включить token-bucket middleware |
| `RATE_LIMIT_RPS` | float | `100` | нет | пополнение токенов в секунду на ключ |
| `RATE_LIMIT_BURST` | int | `200` | нет | размер бакета (всплеск) |
| `RATE_LIMIT_KEY` | string | `ip` | нет | ключ лимитера: `ip` или `api_key` |
| `SWEEP_INTERVAL` | duration | `1m` | нет | период работы expiry sweeper'а |
| `SWEEP_ENABLED` | bool | `true` | нет | включить фоновую чистку протухших ссылок |
| `METRICS_ENABLED` | bool | `true` | нет | включить эндпоинт `/metrics` |

\* `KAFKA_BROKERS` обязателен только если `KAFKA_ENABLED=true`.

### Подбор значений конвейера

- `CLICK_CHANNEL_BUFFER` — буфер должен переживать обычные всплески, чтобы дропы случались
  только в реальной перегрузке. Слишком большой → больше потенциальная потеря на жёстком
  kill; разумный старт — 10k.
- `BATCH_SIZE_N` vs `FLUSH_INTERVAL_T` — баланс «свежесть счётчика против числа round-trip».
  N=500/T=1s означает: при высоком трафике флашим по 500 **событиям** (эффективно), при
  низком — раз в секунду (счётчик не «зависает» дольше секунды). Важно: `N` считается по
  числу накопленных кликов (`pending`), а **не** по числу уникальных кодов в батче
  (`len(map)`) — иначе на перекошенном трафике порог почти не срабатывает и флаш висит только
  на таймере (см. [architecture.md](./architecture.md#async-click-pipeline)).
- `WORKER_POOL_SIZE` — обычно небольшое число (равно/около числу ядер БД-пула). Больше
  воркеров → больше параллельных `UPDATE`, но и больше контеншн в Postgres.

## Пример .env

```bash
# --- Этап 1 ---
HTTP_ADDR=:8080
BASE_URL=http://localhost:8080
POSTGRES_DSN=postgres://app:app@localhost:5432/urlshort?sslmode=disable
POSTGRES_MAX_CONNS=10
REDIS_ADDR=localhost:6379
CACHE_TTL=10m
CODE_LENGTH=7
SHUTDOWN_TIMEOUT=15s
LOG_LEVEL=info
LOG_FORMAT=json

# --- Этап 2 ---
CLICK_CHANNEL_BUFFER=10000
WORKER_POOL_SIZE=4
BATCH_SIZE_N=500
FLUSH_INTERVAL_T=1s

KAFKA_ENABLED=true
KAFKA_BROKERS=localhost:9092
KAFKA_TOPIC=clicks
KAFKA_ACKS=none

RATE_LIMIT_ENABLED=true
RATE_LIMIT_RPS=100
RATE_LIMIT_BURST=200
RATE_LIMIT_KEY=ip

SWEEP_ENABLED=true
SWEEP_INTERVAL=1m
METRICS_ENABLED=true
```

## Структура Config

```go
// internal/config/config.go
package config

import "time"

type Config struct {
    HTTPAddr        string
    BaseURL         string
    ShutdownTimeout time.Duration

    Postgres PostgresConfig
    Redis    RedisConfig
    Cache    CacheConfig

    // --- Этап 2 ---
    Pipeline  PipelineConfig
    Kafka     KafkaConfig
    RateLimit RateLimitConfig
    Sweep     SweepConfig

    Log LogConfig
}

type PostgresConfig struct {
    DSN      string
    MaxConns int32
}

type RedisConfig struct {
    Addr     string
    Password string
    DB       int
}

type CacheConfig struct {
    TTL        time.Duration
    CodeLength int
}

type PipelineConfig struct {
    ChannelBuffer int
    WorkerPool    int
    BatchSize     int
    FlushInterval time.Duration
}

type KafkaConfig struct {
    Enabled bool
    Brokers []string
    Topic   string
    Acks    string // "none" (acks=0) | "leader" (acks=1) | "all" (acks=all)
}

type RateLimitConfig struct {
    Enabled bool
    RPS     float64
    Burst   int
    Key     string // "ip" | "api_key"
}

type SweepConfig struct {
    Enabled  bool
    Interval time.Duration
}

type LogConfig struct {
    Level  string
    Format string
}
```

## Загрузка и валидация

Минималистичный загрузчик на stdlib (без внешних библиотек конфига — в духе README):

```go
func Load() (Config, error) {
    cfg := Config{
        HTTPAddr:        env("HTTP_ADDR", ":8080"),
        BaseURL:         env("BASE_URL", "http://localhost:8080"),
        ShutdownTimeout: envDuration("SHUTDOWN_TIMEOUT", 15*time.Second),
        Postgres: PostgresConfig{
            DSN:      env("POSTGRES_DSN", ""),
            MaxConns: int32(envInt("POSTGRES_MAX_CONNS", 10)),
        },
        Redis: RedisConfig{
            Addr:     env("REDIS_ADDR", "localhost:6379"),
            Password: env("REDIS_PASSWORD", ""),
            DB:       envInt("REDIS_DB", 0),
        },
        Cache: CacheConfig{
            TTL:        envDuration("CACHE_TTL", 10*time.Minute),
            CodeLength: envInt("CODE_LENGTH", 7),
        },
        Pipeline: PipelineConfig{
            ChannelBuffer: envInt("CLICK_CHANNEL_BUFFER", 10000),
            WorkerPool:    envInt("WORKER_POOL_SIZE", 4),
            BatchSize:     envInt("BATCH_SIZE_N", 500),
            FlushInterval: envDuration("FLUSH_INTERVAL_T", time.Second),
        },
        // ... Kafka, RateLimit, Sweep, Log аналогично
    }

    if cfg.Postgres.DSN == "" {
        return Config{}, fmt.Errorf("POSTGRES_DSN is required")
    }
    if cfg.Kafka.Enabled && len(cfg.Kafka.Brokers) == 0 {
        return Config{}, fmt.Errorf("KAFKA_BROKERS required when KAFKA_ENABLED=true")
    }
    return cfg, nil
}
```

Хелперы `env`, `envInt`, `envDuration`, `envBool`, `envFloat`, `envCSV` — тонкие обёртки над
`os.Getenv` с дефолтом и парсингом. Принцип: **fail-fast** — обязательные параметры без
значения валят старт сразу, а не падают позже в рантайме.
