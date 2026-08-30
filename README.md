# Price Tracker Bot

Телеграм-бот, который следит за ценами на товары и присылает уведомление, когда
цена упала ниже заданной, товар вернулся в наличие или установлен новый минимум.
Цены приводятся к валюте, удобной пользователю.

Под капотом — микросервисы на Go: gRPC между сервисами, PostgreSQL как источник
правды, Redis как кэш и очередь задач, пул воркеров для обхода страниц с
ограничением скорости по хосту.

> **Статус:** в разработке. Готов слой бота (`cmd/bot`) с in-memory хранилищем —
> его можно запустить и полностью пройти сценарии в Telegram без базы и без
> остальных сервисов. Следующий шаг — сервис `core`.

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
make check       # fmt-check + tidy-check + vet + lint + test — то же, что в CI
make fix         # gofmt, goimports, go mod tidy, golangci-lint --fix
make test        # go test -race ./...
make cover       # покрытие в coverage.html
make build       # бинарники в ./bin
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
  core/         бизнес-логика core-сервиса
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
| `CURRENCY_PROVIDER` | провайдер курсов валют |

## Стек

Go · gRPC (+ streaming) · PostgreSQL · Redis · Docker Compose ·
[go-telegram/bot](https://github.com/go-telegram/bot) ·
[golangci-lint](https://golangci-lint.run/) · [buf](https://buf.build/) ·
[golang-migrate](https://github.com/golang-migrate/migrate) ·
[Frankfurter](https://frankfurter.dev/) для курсов валют
