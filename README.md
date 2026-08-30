# Price Tracker Bot

Телеграм-бот, который следит за ценами на товары и присылает уведомление, когда
цена упала ниже заданной, товар вернулся в наличие или установлен новый минимум.
Цены приводятся к валюте, удобной пользователю.

Под капотом — микросервисы на Go: gRPC между сервисами, PostgreSQL как источник
правды, Redis как кэш и очередь задач, пул воркеров для обхода страниц с
ограничением скорости по хосту.

> **Статус:** в разработке. Готовы два слоя:
>
> - `cmd/bot` — Telegram-бот с in-memory хранилищем, запускается без базы и без
>   остальных сервисов;
> - `cmd/core` — сервис-владелец данных: `ItemService` и `PricingService` по
>   gRPC, PostgreSQL как источник правды, Redis как кэш статистики.
>
> Бот умеет работать против core: `BOT_STORAGE=core` переключает его с
> in-memory хранилища на gRPC-клиент, интерфейс `bot.Store` при этом не меняется.
>
> Следующие шаги: `scraper`, `currency` и `notifier`.

## Возможности бота

| Команда | Что делает |
| --- | --- |
| `/start` | Регистрирует пользователя, показывает подсказку |
| `/add` | Диалог добавления: ссылка → желаемая цена → интервал проверки |
| `/add <url>` | То же, но ссылка передана сразу |
| `/list` | Список отслеживаемых товаров с текущей ценой |
| `/stats` | Мин / макс / среднее за 14 дней, тренд, спарклайн |
| `/settings` | Валюта по умолчанию, интервал проверки товара |
| `/remove` | Удалить товар |
| `/cancel` | Прервать текущий диалог |

## Архитектура

```
                    ┌──────────────┐
   Telegram ◀──────▶│     bot      │  FSM диалогов, рендер сообщений
                    └──────┬───────┘
                           │ gRPC
                    ┌──────▼───────┐
                    │     core     │  источник правды: товары, история, статистика
                    └──┬────────┬──┘
              PostgreSQL        Redis (кэш + очереди)
                                 ▲          ▲
                       queue:scrape    queue:notify
                                 │          │
                    ┌────────────┴──┐   ┌───┴──────────┐
                    │    scraper    │   │   notifier   │
                    │ пул воркеров, │   │  доставка    │
                    │ rate limit,   │   │  алертов,    │
                    │ курсы валют   │   │  retry       │
                    └───────────────┘   └──────────────┘
```

Почему так:

- **bot** ничего не знает о базе — только gRPC-контракт `core`. Поэтому его можно
  запустить на in-memory реализации того же интерфейса `bot.Store` и отлаживать UX
  отдельно от инфраструктуры.
- **core** единственный владеет схемой БД. Остальные сервисы ходят к данным только
  через него.
- **scraper** и **notifier** общаются через очереди в Redis, а не напрямую: обход
  страниц и доставка сообщений — независимо масштабируемые и отказоустойчивые части.

### Ключевые решения

- **Деньги — `int64` в минорных единицах**, никогда `float64`. Сравнение сумм в
  разных валютах возвращает ошибку, а не молча неверный ответ.
- **Неоднозначный ввод отклоняется.** `1.234` — это 1234 по-турецки и 1.23
  по-английски, поэтому парсер просит уточнить вместо догадки: иначе порог алерта
  мог бы уехать в 1000 раз.
- **Нормализация URL — это и защита от SSRF.** Обрезаются utm-метки и креды, а
  localhost, приватные и link-local адреса отклоняются на входе, потому что по
  ссылке потом ходит сервер.
- **callback_data версионирована** (`v1:<action>:<arg>`) и укладывается в лимит
  Telegram в 64 байта; при переполнении кнопка деградирует в no-op, а не ломает
  сообщение.
- **Пользовательский текст всегда экранируется** перед отправкой в HTML parse mode.

## Быстрый старт

Нужны Go 1.25+, Docker и токен бота от [@BotFather](https://t.me/BotFather).

```bash
git clone https://github.com/NE-smirnov/price-tracker-bot.git
cd price-tracker-bot

make hooks                 # включить git-хуки из .githooks
make tools                 # локальные линтеры и генераторы в ./.tools

cp .env.example .env
$EDITOR .env               # вписать TELEGRAM_BOT_TOKEN

make run-bot               # BOT_STORAGE=memory — база не нужна
```

Полный стек (когда появятся остальные сервисы):

```bash
make up-all                # postgres, redis и все сервисы в docker compose
make logs
make down
```

## Разработка

```bash
make check            # fmt-check + vet + lint + test — то же, что в CI
make fix              # gofmt, goimports, go mod tidy, golangci-lint --fix
make test             # go test -race ./...  (тесты с БД пропускаются)
make test-integration # то же + тесты против настоящего PostgreSQL
make cover            # покрытие
make build            # бинарники в ./bin
```

### Тесты, которым нужна база

Логика репозитория — это в основном SQL: аренда задач через
`FOR UPDATE SKIP LOCKED`, дедупликация алертов через `ON CONFLICT`, решение
«новый ли это минимум» внутри одной транзакции. Мокать здесь нечего, поэтому
эти тесты идут против настоящего PostgreSQL и пропускаются, если не задан
`TEST_DATABASE_URL`:

```bash
make up                # postgres + redis
make test-db-create    # отдельная база price_tracker_test
make test-integration
```

Схема применяется из `migrations/` перед каждым тестом, так что миграция
проверяется вместе с кодом. В CI то же самое делает сервис-контейнер
`postgres:18-alpine`.

### Запуск core локально

```bash
make up                                    # postgres + redis
make migrate-up                            # применить схему
POSTGRES_DSN=... make run-core              # сервис на :9090
grpcurl -plaintext localhost:9090 list      # gRPC reflection включён
```

Git-хуки (включаются через `make hooks`):

| Хук | Что проверяет | Как обойти |
| --- | --- | --- |
| `pre-commit` | секреты и мусорные файлы, файлы >1 MiB, автоформат staged `.go`, `go build`, `go vet` и `golangci-lint` по затронутым пакетам, `go mod tidy`, `buf format`/`buf lint` для `.proto` | `HOOK_NO_FIX=1`, `HOOK_SKIP_LINT=1`, `git commit -n` |
| `commit-msg` | Conventional Commits (`feat:`, `fix:`, `refactor:` …) | `git commit -n` |
| `pre-push` | `go test -race ./...` | `HOOK_SKIP_TESTS=1` |

Хук сам форматирует staged-файлы и добавляет их обратно в индекс, так что
`gofmt`-замечание не превращается в отдельный коммит-фикс.

## Структура

```
cmd/            точки входа сервисов (bot, core, scraper, notifier)
internal/
  domain/       модель предметной области: Money, TrackedItem, Stats, нормализация URL
  bot/          Telegram-слой: FSM, хендлеры, рендер, in-memory Store
  core/         core-сервис: репозиторий, движок алертов, gRPC-обработчики
  scraper/      обход страниц и извлечение цены
  currency/     клиент курсов валют с кэшем
  platform/     config, logger, postgres, redis
api/proto/      gRPC-контракты
migrations/     миграции схемы PostgreSQL
deploy/         Dockerfile
```

## Конфигурация

Все переменные и значения по умолчанию — в [`.env.example`](.env.example).
Основные:

| Переменная | Назначение |
| --- | --- |
| `TELEGRAM_BOT_TOKEN` | токен от BotFather (обязательна) |
| `BOT_STORAGE` | `memory` или `core` |
| `BOT_ALLOWED_USERS` | список Telegram ID через запятую; пусто — доступ всем |
| `CORE_GRPC_ADDR` | адрес core-сервиса |
| `POSTGRES_DSN`, `REDIS_ADDR` | подключения к хранилищам |
| `CORE_GRPC_LISTEN` | адрес, на котором слушает core (по умолчанию `:9090`) |
| `CORE_STATS_TTL` | сколько живёт кэш `/stats` в Redis |
| `TEST_DATABASE_URL` | база для `make test-integration`; пусто — тесты с БД пропускаются |
| `CURRENCY_PROVIDER` | провайдер курсов валют |

## Стек

Go · gRPC (+ streaming) · PostgreSQL · Redis · Docker Compose ·
[go-telegram/bot](https://github.com/go-telegram/bot) ·
[golangci-lint](https://golangci-lint.run/) · [buf](https://buf.build/) ·
[golang-migrate](https://github.com/golang-migrate/migrate) ·
[Frankfurter](https://frankfurter.dev/) для курсов валют
