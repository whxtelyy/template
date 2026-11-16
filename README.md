# Trip Service

HTTP-сервис управления поездками на Go. ЛР3 развивает архитектуру ЛР2: сохраняет координаты и маршрут, находит устаревшие поездки и запрашивает координаты через Push Service. API поездок, идемпотентность и телеметрия ЛР1/ЛР2 сохранены.

## Возможности

| Метод | Маршрут | Назначение |
| --- | --- | --- |
| `POST` | `/api/v1/trips` | Создать поездку (`201`) |
| `GET` | `/api/v1/trips/{tripId}` | Получить поездку (`200` или `404`) |
| `POST` | `/api/v1/trips/{tripId}/finish` | Завершить поездку (`200` или `409`) |
| `POST` | `/api/v1/trips/{tripId}/positions` | Сохранить координату (`201`; ошибки `400/404/409`) |
| `GET` | `/api/v1/trips/{tripId}/positions` | Все точки маршрута (`200` или `404`) |
| `GET` | `/health` | Проверить работу HTTP-сервера |
| `GET` | `/ready` | Проверить доступность PostgreSQL |

Создание поддерживает необязательный заголовок `Idempotency-Key` (UUID). Повтор с тем же ключом и телом возвращает ту же поездку (`200`), с другим телом - `409 idempotency_conflict`. Ключи хранятся 24 часа. Ошибки API представлены в формате RFC 9457 (`application/problem+json`).

**Стек:** Go 1.27, `net/http`, chi, PostgreSQL, pgx/pgxpool, Squirrel, Goose, oapi-codegen, OpenTelemetry, `slog`. Для тестов - `gomock` и `httptest`.

## Быстрый запуск

Команды выполняются из корня проекта **в Linux/WSL**. Нужны Go, Docker и `tripgoctl`.

```bash
# PostgreSQL, Push Service, OTel Collector, Jaeger, Prometheus и Grafana (lab = 3)
tripgoctl cluster start
tripgoctl environment start
tripgoctl connect

# Подготовка базы и запуск сервера
make migrate
make run
```

HTTP API будет доступен на `http://localhost:8080`. `tripgoctl connect` показывает адрес базы и ссылки на интерфейсы наблюдаемости.

**Настройка OTLP.** В `.env.example` указан стандартный `http://localhost:4318`, но локальный порт коллектора, выданный `tripgoctl connect`, может отличаться. Если в `.env` отсутствует `OTEL_EXPORTER_OTLP_ENDPOINT`, добавьте туда актуальный HTTP-адрес OTLP из вывода `connect`, например:

```dotenv
OTEL_EXPORTER_OTLP_ENDPOINT=http://<адрес-OTel-HTTP-из-connect>
```

`make run` последовательно загружает `.env.example` и `.env`, поэтому локальное значение имеет приоритет. `.env` не коммитится; после пересоздания окружения адрес следует проверить заново.

## Проверка и сборка

```bash
make generate           # OpenAPI-код и моки через go:generate
make test               # все unit/HTTP-тесты с race detector; покрытие domain/usecase
make test-integration   # интеграционные тесты с PostgreSQL
make lint               # golangci-lint по .golangci.yml
make build              # сборка исполняемого файла
make docker-build       # многостадийный Docker-образ
make build VERSION=1.2.3 # service.version через -ldflags (по умолчанию dev)
make docker-build VERSION=1.2.3
python3 scripts/smoke_lab3.py # после make build; реальные PostgreSQL/Push/OTel
```

Для `make test-integration` необходимы доступная PostgreSQL и корректный `DATABASE_URL` в `.env`; можно переопределить его через `TEST_DATABASE_URL`. Все интеграционные тесты, включая унаследованные, создают отдельную схему на тест, прогоняют настоящие Goose-миграции и удаляют только эту схему. Smoke создаёт отдельную временную БД, удаляет только её и сбрасывает настройки Push в `finally`; нужны права CREATE DATABASE. Не запускайте smoke одновременно с чужими проверками заглушки. Интеграционные тесты используют тег `integration`; unit-тестам БД не нужна. Makefile использует POSIX shell. Для воспроизводимости в Windows/WSL Makefile, `.env.example`, Go и shell-файлы закреплены с LF.

## Архитектура

```text
.
├── cmd/trip-service/              запуск и graceful shutdown
├── internal/
│   ├── config/                    конфигурация приложения
│   ├── container/                 сборка зависимостей
│   ├── domain/trip/               модель, правила и ошибки
│   ├── usecase/trip/
│   │   ├── create/                создание поездки
│   │   ├── get/                   получение поездки
│   │   ├── finish/                завершение поездки
│   │   └── positions/             сохранение координат и чтение маршрута
│   ├── http/
│   │   ├── trip/{create,get,finish,positions}/ HTTP-обработчики
│   │   ├── domainerror/           преобразование ошибок в RFC 9457
│   │   └── middleware/            Recover → Tracing → Logging → Metrics
│   ├── repository/trip/           запросы PostgreSQL
│   ├── worker/positions/          scanner, bounded priority queue, worker pool
│   ├── gateway/push/              HTTP-клиент Push Service
│   ├── infra/
│   │   ├── postgres/              трассировка SQL
│   │   │   └── transaction/       управление транзакциями
│   │   └── telemetry/             OpenTelemetry и JSON-логи
│   └── generated/                 код из OpenAPI (не редактировать вручную)
├── contracts/openapi/             исходный контракт API
└── migrations/                    схема базы данных
```

**Зависимости направлены внутрь.** Домен не зависит от HTTP, PostgreSQL и OTel. Usecase использует доменную модель и узкие интерфейсы из собственных `contract.go`; HTTP-обработчики также описывают свои зависимости на стороне потребителя. Конкретные реализации связываются в `internal/container`, без DI-фреймворка. Границы слоёв дополнительно контролирует `depguard`.

**Почему нет отдельного `model`.** Между слоями используется `domain.Trip`: модель поездки пока едина, поэтому промежуточная структура не добавляет полезной изоляции. При появлении различных контрактов слоёв будет оправдано выделение отдельной модели.

**Транзакции и конкурентность.** Менеджер в `internal/infra/postgres/transaction` выполняет `COMMIT` или `ROLLBACK`. Создание поездки и записи в истории статусов атомарно. Частичный уникальный индекс не позволяет водителю иметь более одной активной поездки; завершение выполняется условным `UPDATE`. Идемпотентность основана на резервировании ключа в транзакции и сравнении SHA-256 хэша тела запроса. Контракт OpenAPI и миграции в ЛР2 не изменялись.

Usecase проверяет допустимость завершения через `CanFinish`, а PostgreSQL атомарно меняет статус и задаёт `finished_at`. `now()` внутри одной транзакции даёт согласованное время поездки и истории. `COMMIT` использует контекст запроса с таймаутом; `ROLLBACK` - `context.WithoutCancel` с таймаутом. Неудачный откат при panic логируется, исходная panic передаётся дальше.

## Тестирование

- **Домен:** table-driven тесты правил и переходов статусов.
- **Usecase:** unit-тесты с `gomock`; моки генерируются рядом с интерфейсами в `mocks_test.go`.
- **HTTP:** `httptest`, проверка валидации, ошибок и middleware.
- **Репозиторий:** интеграционные тесты на реальном PostgreSQL, включая откат транзакции, идемпотентность и конкурентное создание поездок.

Целевой порог покрытия `internal/domain/...` и `internal/usecase/...` - не ниже 50%. Запуск тестов и линтера выполняется в Linux/WSL.

`make test` запускает тесты всех пакетов, а `-coverpkg` ограничивает измерение покрытия пакетами domain/usecase. Итог выводится из `coverage.out`.

## Логи, метрики и трейсы

На каждый HTTP-запрос пишется структурированный JSON-лог через `slog`: метод, шаблон маршрута, статус, длительность, `trace_id` и `span_id`. Тела запросов и персональные идентификаторы не используются как метки метрик. Порядок middleware: **Recover → Tracing → Logging → Metrics → Router**.

Трейсы HTTP и запросов PostgreSQL отправляются по OTLP в коллектор; для pgx используется `otelpgx`. Данные можно просматривать в **Jaeger**, а метрики - в **Prometheus/Grafana**. Адреса выводит `tripgoctl connect`. Недоступность коллектора не должна останавливать HTTP API.

OTel SDK читает `OTEL_TRACES_SAMPLER` и `OTEL_TRACES_SAMPLER_ARG`; например, `parentbased_traceidratio` и `0.1` включают выборку 10% корневых трейсов. `OTEL_METRIC_EXPORT_INTERVAL` задаётся в миллисекундах (`10000` в `.env.example`). Ошибки OTel направляются в JSON-лог с уровнем Warn. Гистограммы длительности используют секунды и границы от `0.005` до `10`. Шаблоны маршрутов берутся из chi, поэтому UUID не попадают в метки.

Основные метрики:

| OpenTelemetry | В Prometheus |
| --- | --- |
| `http.server.request.duration` | `http_server_request_duration_seconds_*` |
| `http.server.active_requests` | `http_server_active_requests` |
| `db.client.operation.duration` | `db_client_operation_duration_seconds_*` |
| `tripgo.trips.created` | `tripgo_trips_created_total` |
| `tripgo.trips.completed` | `tripgo_trips_completed_total` |

Для быстрой проверки отправьте `POST /api/v1/trips`, затем откройте в Jaeger трейс запроса и убедитесь, что в нём есть вложенные SQL-спаны. В Grafana → Explore → Prometheus проверьте:

```promql
http_server_request_duration_seconds_count
tripgo_trips_created_total
tripgo_trips_completed_total
```

Исторические скриншоты **ЛР2**: [трейсы Jaeger](docs/screenshots/Jaeger/) и [метрики Grafana](docs/screenshots/Grafana/). Реальные метрики **ЛР3** под ограниченной нагрузкой: [снимки и исходные данные](docs/screenshots/Lab3/README.md). Результаты проверок и план SQL: [отчёт](docs/lab3-verification.md). Заключительный аудит очереди и безопасный порядок PR с учётом истории ЛР1: [подготовка к PR](docs/lab3-pr-finalization.md).

## Конфигурация ЛР3

`make run` загружает `.env.example`, затем `.env`. Приложение требует все перечисленные ниже значения; отсутствующие и неположительные размеры/таймауты отклоняются на старте. Адреса PostgreSQL, Push и OTLP берутся из текущего `tripgoctl connect`, примерные порты не заменяют эту команду.

| Переменные | Значения в `.env.example` / назначение |
| --- | --- |
| `HTTP_ADDR` | `:8080` |
| `HTTP_READ_TIMEOUT`, `HTTP_READ_HEADER_TIMEOUT` | `10s`, `5s` |
| `HTTP_WRITE_TIMEOUT`, `HTTP_IDLE_TIMEOUT` | `10s`, `60s` |
| `LOG_LEVEL`, `SHUTDOWN_TIMEOUT` | `info`, `10s`; единый бюджет остановки |
| `DATABASE_URL` | Строка PostgreSQL из `.env` |
| `DATABASE_MAX_CONNS`, `DATABASE_MIN_CONNS` | `10`, `2` |
| `DATABASE_MAX_CONN_LIFETIME` | `30m` |
| `DATABASE_CONNECT_TIMEOUT`, `DATABASE_QUERY_TIMEOUT` | `5s`, `3s` |
| `POSITION_CHECK_INTERVAL` | `30s`; частота обхода и повторного запроса без ответа |
| `POSITION_STALE_AFTER` | `60s`; порог отсутствия координат |
| `POSITION_WORKERS` | `4`; постоянных воркера |
| `POSITION_QUEUE_SIZE` | `100`; максимум ожидающих задач, без in-flight |
| `POSITION_BATCH_SIZE` | `100`; размер SQL-батча, бонус 1 |
| `PUSH_HTTP_URL` | Базовый HTTP URL из `.env`; гейтвей добавляет `/api/v1/push` |
| `PUSH_REQUEST_TIMEOUT` | `1s`; одна попытка, включая соединение и чтение ответа |
| `OTEL_SERVICE_NAME` | `trip-service` |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL` | Endpoint из `.env`, `http/protobuf` (также поддерживается `grpc`) |
| `OTEL_RESOURCE_ATTRIBUTES` | `service.namespace=tripgo,deployment.environment=local` |
| `OTEL_TRACES_SAMPLER`, опционально `OTEL_TRACES_SAMPLER_ARG` | `parentbased_always_on`; аргумент для sampler с вероятностью |
| `OTEL_METRIC_EXPORT_INTERVAL` | `10000` мс; читает OTel SDK |
| Опционально `OTEL_EXPORTER_OTLP_TIMEOUT` | Таймаут экспорта в мс, стандартная настройка OTel SDK |

`config/trip-service.env.example` - исходный справочник курса, включая будущие ЛР4/ЛР5; сервис ЛР3 использует только переменные своей работы из корневого `.env.example`. Новые frameworks, gRPC, broker, retries и rate limiter не добавлены.

## Координаты и транзакция

```http
POST /api/v1/trips/{tripId}/positions
Content-Type: application/json

{"coordinates":{"latitude":59.935,"longitude":30.335},"recorded_at":"2026-11-12T00:00:00Z"}
```

Обязательны оба числа и RFC3339 timestamp; `null`, неизвестные поля, неверные типы и несколько JSON-значений отвергаются. Домен проверяет диапазоны и время не более минуты в будущем. Ошибки преобразуются единственным `http/domainerror`: `invalid_request`, `trip_not_found`, `trip_completed`. GET возвращает `{"positions":[]}` для существующей поездки без точек; порядок - `recorded_at ASC, id ASC`.

Usecase вызывает `TxManager`: условный `UPDATE trips ... WHERE status='active'` блокирует строку и обновляет `last_position_at=GREATEST(last_position_at, recorded_at)`, затем вставляется `trip_positions`. В PostgreSQL `GREATEST` игнорирует NULL. Завершение блокирует ту же строку: после ожидания PostgreSQL перепроверяет `active`. Ошибка вставки откатывает timestamp. Поздние координаты сохраняются в маршруте, но timestamp не уменьшают. Только после успешного COMMIT отменяется ожидающий запрос и увеличивается `tripgo.positions.saved`.

## Опрос устаревших поездок

```text
time.Ticker → scanner → bounded priority queue → fixed workers → HTTP Push Service
                            ↑ cancel после COMMIT координаты
```

Одна постоянная горутина сканера синхронно читает батчи; новые тики не создают параллельных обходов. SQL вычисляет `status='active' AND COALESCE(last_position_at,started_at) < now() - POSITION_STALE_AFTER`. Таймаут есть у SQL и у каждого Push-вызова. Ошибка БД не останавливает последующие тики. Повторная отправка возможна на следующем тике, если поездка осталась stale; отдельного retry, cooldown или хранения времени последней команды нет.

**Бонус 1: keyset.** Курсор - точная пара `(COALESCE(last_position_at,started_at), id)` последней прочитанной строки. WHERE-продолжение и ORDER BY используют один ключ; LIMIT параметризован, OFFSET отсутствует. При переполнении разбирается уже загруженный батч, дальнейшие батчи не читаются; следующий тик начинает с начала. Тест на 250 поездках с batch=100 доказывает три запроса без пропусков. Это READ COMMITTED, а не snapshot: конкурентные изменения могут перемещать строки между страницами; следующий тик повторяет обход. Обновлённые координаты делают поездку свежей и исключают её из дальнейшей stale-выборки. Временная карта отмен в текущем обходе также предотвращает постановку устаревшего SQL-результата после COMMIT.

**Бонус 2: приоритет.** Min-heap владеет ожидающими заданиями, порядок - timestamp, затем UUID. Обязательный буферизованный канал `slots` ёмкостью `POSITION_QUEUE_SIZE` хранит ровно один токен на элемент heap. Канал и heap меняются под одним mutex; канал не является второй очередью. Воркеры ждут `sync.Cond`, проверяя heap под тем же mutex: сигнал не теряется, токены не читаются вне блокировки. Канал не закрывается; закрытое состояние и broadcast исключают send-on-closed-channel. Mutex никогда не держится во время SQL/HTTP.

`pending` охватывает queued и in-flight. При полной очереди более старая задача вытесняет самую свежую ожидающую; иначе дропается новая. Поиск вытесняемой задачи занимает O(queue size), извлечение/отмена - O(log queue size); для небольшого ограниченного буфера это проще второго heap. При COMMIT координаты queued-задача удаляется сразу вместе с токеном; in-flight получает отмену контекста, но сохраняет дедупликацию до выхода вызова. Отмену уже принятого удалённым сервисом пуша гарантировать нельзя. Вытесненная задача освобождает запись дедупликации и вновь обнаруживается БД.

Семантика метрик: новая принятая задача даёт `stale_checks{result=queued}`; повторная или инвалидированная в текущем обходе - `duplicate`; отклонённая новая - `dropped`. При вытеснении новая считается queued, вытесненная даёт один `position_requests{result=dropped}`. Отклонённая новая также даёт ровно один такой dropped; отмена координатой не считается переполнением.

Гейтвей отправляет `kind=REQUEST_POSITION`, `recipient_id=driver_id`, `data.trip_id` и новый UUID `request_id`. Принимает только HTTP 202. `StatusError.Status` различает 400/429/503 и другие ответы; транспортные ошибки сохраняют цепочку `%w`. Ответ закрывается и ограниченно вычитывается; редиректы не создают дополнительных попыток. Transport принадлежит приложению.

### Индекс и миграции

Миграция 00004 добавляет колонку и официальную таблицу с FK, CHECK и индексом маршрута. Миграция 00005 с `NO TRANSACTION` создаёт/удаляет `CONCURRENTLY` частичный expression index:

```sql
CREATE INDEX CONCURRENTLY trips_active_position_idx
ON trips (COALESCE(last_position_at, started_at), id) WHERE status = 'active';
```

Предикат, временное выражение и порядок совпадают с запросом. На 30 000 строках, из которых 250 stale, `EXPLAIN (ANALYZE, BUFFERS)` показал `Index Scan using trips_active_position_idx`; `enable_seqscan` не менялся. Полный план и условия замера - в [отчёте](docs/lab3-verification.md). На очень маленькой таблице PostgreSQL может обоснованно предпочесть seq scan.

### Остановка

После SIGINT/SIGTERM HTTP перестаёт принимать запросы и дожидается активных. Затем `Worker.Shutdown` отменяет сканер, ждёт завершения SQL, закрывает постановку и дренирует heap постоянными воркерами. Их контекст независим от уже отменённого signal context. При истечении общего дедлайна контексты вызовов отменяются, ожидающий буфер очищается. Далее закрываются Push Transport, PostgreSQL и OTel. `main` ограничивает ожидание очистки тем же общим бюджетом и при превышении пишет ошибку и завершает процесс. При принудительном выходе принятые задачи могут остаться неотправленными; следующий процесс снова обнаружит stale-поездки в PostgreSQL. Подтверждения доставки и exactly-once эта ЛР не реализует.

### Метрики ЛР3

| OTel | Тип / атрибуты | Имя в реальном Prometheus |
| --- | --- | --- |
| `tripgo.positions.saved` | counter | `tripgo_positions_saved_total` |
| `tripgo.stale_checks` | counter, result queued/duplicate/dropped | `tripgo_stale_checks_total` |
| `tripgo.position_requests.queue.size` | gauge, ожидающие | `tripgo_position_requests_queue_size` |
| `tripgo.position_requests.workers.active` | gauge, in-flight | `tripgo_position_requests_workers_active` |
| `tripgo.position_requests` | counter, result success/error/dropped | `tripgo_position_requests_total` |
| `tripgo.position_requests.duration` | histogram, секунды, result success/error | `tripgo_position_requests_duration_seconds_*` |
| `tripgo.stale_checks.rows` | histogram, число прочитанных строк на тик, без атрибутов | `tripgo_stale_checks_rows_*` |

В `Grafana → Explore → Prometheus` можно использовать `sum by(result)(rate(tripgo_position_requests_total[1m]))` и gauges из таблицы. Worker создаёт новый корневой `positions.request` со Span Link на `positions.check`; Jaeger показывает его как `FOLLOWS_FROM`. Вложенный HTTP client span распространяет W3C TraceContext. UUID не попадают в metric labels. Все gauges считываются согласованно под mutex. OTel экспортеры работают асинхронно; отказ коллектора логируется и не отменяет бизнес-операции.

Проверки гонок и утечек: `go test -race -count=2 ./...`, `goleak.VerifyTestMain` в worker и gateway. В unit-тестах зависимости только mockgen; управляемые каналы заменяют ожидание реального тикера.

## Дополнительно

- Docker-образ собирается в несколько стадий; итоговый контейнер основан на `distroless` и работает от `nonroot`.
- При `SIGINT` или `SIGTERM` сервер завершает приём запросов, выполняет graceful shutdown и закрывает пул соединений PostgreSQL.
