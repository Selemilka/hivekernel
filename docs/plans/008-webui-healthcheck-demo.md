# Plan 008: WebUI + HealthChecker + Demo Config

## Context

HiveKernel -- ядро для оркестрации LLM-агентов. Текущий WebUI показывает дерево процессов, позволяет spawn/kill/execute, но имеет ряд недостатков:
- Нет истории задач (какие промпты отправлялись узлам, что они возвращали)
- "Send Task" спрятан за неудобным "Execute" модалом с JSON-полями
- CRON-задачи видны только в sidebar чата, нет отдельной страницы
- HealthChecker пингует виртуальные процессы (Client=nil), считает их unhealthy, и убивает

---

## Plan

### 1. HealthChecker: пропуск виртуальных процессов

**Проблема:** `ListRuntimes()` возвращает ВСЕ рантаймы, включая виртуальные (`virtual://pid-N`, `Client=nil`). Метод `ping()` возвращает `false` для них, и после `maxFailures` HealthMonitor вызывает `onUnhealthy` -- убивая виртуальные процессы.

**Примечание:** доступ ко всем потомкам уже работает. `ListRuntimes()` -- плоская карта всех зарегистрированных рантаймов, включая глубоких потомков. ACL не задействован при пинге.

**Файл:** `internal/runtime/health.go` (метод `check`, строка 65)

**Изменение:** фильтровать рантаймы с `Client == nil` перед пингом:

```go
func (h *HealthMonitor) check(ctx context.Context) {
    runtimes := h.manager.ListRuntimes()

    // Skip virtual runtimes (no gRPC client to ping).
    var real []*AgentRuntime
    for _, rt := range runtimes {
        if rt.Client != nil {
            real = append(real, rt)
        }
    }

    results := make(chan pingResult, len(real))
    for _, rt := range real {
        // ... existing ping logic ...
    }
    for range real {
        // ... existing result handling ...
    }
}
```

---

### 2. Task History: история задач в dashboard

**Проблема:** нет механизма просмотра, какие задачи отправлялись узлам и какие ответы вернулись.

**Решение:** хранить историю на стороне Python dashboard (в памяти). Без изменений в proto/Go-ядре.

#### 2a. Backend: `sdk/python/dashboard/app.py`

Добавить глобальную структуру `task_history: list[dict]` (макс. 500 записей).

Функция `record_task(pid, name, description, result, success, error, duration_ms)` -- вызывать в конце `api_execute()`, `api_run_task()`, `api_chat()`.

Новый эндпоинт:
```python
@app.get("/api/task-history")
async def api_task_history(limit: int = 50, pid: int = 0):
    # фильтрация по pid, возврат последних limit записей
```

#### 2b. Frontend: панель "Task History" в `index.html` + `app.js`

Добавить коллапсибельную панель "Task History" в right-panel (после result-panel, перед log-panel).

В `app.js`: при `selectNode()` -- загружать `/api/task-history?pid=<pid>` и показывать список записей. Каждая запись:
- Время (относительное: "2m ago")
- Описание (сокращённое)
- Длительность
- Успех/ошибка (цветовой индикатор)
- Раскрываемый вывод

#### Файлы:
- `sdk/python/dashboard/app.py` -- task_history store + endpoint + record_task() в 3 эндпоинтах
- `sdk/python/dashboard/static/index.html` -- новая секция Task History
- `sdk/python/dashboard/static/app.js` -- loadTaskHistory(), рендеринг
- `sdk/python/dashboard/static/style.css` -- стили для history панели

---

### 3. Direct Node Messaging: улучшение Execute модала

**Проблема:** "Execute" модал требует JSON params, неудобен. "Run Task" работает только через Queen.

**Решение:** переименовать "Execute" в "Send Task", упростить модал.

#### 3a. `index.html`: переработанный Execute модал

```html
<h2>Send Task to <span id="exec-target-name"></span> (PID <span id="exec-target-pid"></span>)</h2>
<textarea id="exec-description" rows="4" placeholder="Describe what you want..."></textarea>
<input type="number" id="exec-timeout" value="120"> <!-- timeout -->
<!-- params JSON -- скрытый/опциональный -->
<div id="exec-progress" style="display:none"> <!-- прогресс-бар --> </div>
```

Переставить кнопки в `detail-actions`: "Send Task" первая (primary), "Run Task (Queen)" вторая.

#### 3b. `app.js`: улучшённый `doExecute()`

- Показать имя узла в заголовке модала
- Прогресс-бар при выполнении
- Результат показывать в Task Result панели (reuse `showTaskResult()`)
- Не закрывать модал сразу -- показать результат

#### 3c. Контекстное меню: "Send Task" как primary вместо "Run Task"

#### Файлы:
- `sdk/python/dashboard/static/index.html` -- модал + кнопки + контекстное меню
- `sdk/python/dashboard/static/app.js` -- openExecuteModal(), doExecute()

---

### 4. CRON Page: отдельная страница расписания

#### 4a. Новая страница `sdk/python/dashboard/static/cron.html`

Структура:
- Header с навигацией (Tree | Chat | Cron)
- Таблица CRON-задач: Name, Expression, Action, Target, Description, Enabled, Last Run, Actions (Delete)
- Модал "Add Cron Job": name, expression, action type, target PID, description
- Toast-нотификации

#### 4b. Новый скрипт `sdk/python/dashboard/static/cron.js`

Функции:
- `loadCronEntries()` -- GET /api/cron + GET /api/tree (для имён процессов)
- `openAddCronModal()` -- показать модал
- `doAddCron()` -- POST /api/cron
- `removeCron(id)` -- DELETE /api/cron/{id}
- Рендеринг таблицы с человекочитаемыми описаниями cron-выражений

#### 4c. Навигация: добавить ссылку "Cron" в header на всех страницах

В `index.html` и `chat.html` добавить `<a href="/cron.html" class="nav-link">Cron</a>`.

#### 4d. Redirect в `app.py`

```python
@app.get("/cron")
async def redirect_cron():
    return RedirectResponse("/cron.html")
```

#### 4e. Добавить last_run/next_run в proto

В `core.proto` добавить в `CronEntryProto` (строка 193):
```protobuf
int64 last_run_ms = 9;
int64 next_run_ms = 10;
```

В `internal/scheduler/cron.go` добавить хелпер `NextRunAfter(sched CronSchedule, after time.Time) time.Time` -- перебирает минуты вперёд (до 1440 = 24ч) пока `Matches()` не вернёт true.

В `internal/kernel/grpc_core.go` метод `ListCron()` (строка 524) -- заполнять `LastRunMs` и `NextRunMs`.

Перегенерировать proto (Go + Python), поправить импорты в Python.

#### Файлы:
- `sdk/python/dashboard/static/cron.html` -- НОВЫЙ
- `sdk/python/dashboard/static/cron.js` -- НОВЫЙ
- `sdk/python/dashboard/static/style.css` -- стили таблицы, badge
- `sdk/python/dashboard/static/index.html` -- nav link
- `sdk/python/dashboard/static/chat.html` -- nav link
- `sdk/python/dashboard/app.py` -- redirect /cron + last_run/next_run в api_cron_list
- `api/proto/core.proto` -- last_run_ms, next_run_ms в CronEntryProto
- `internal/kernel/grpc_core.go` -- ListCron заполнение
- `internal/scheduler/cron.go` -- NextRunAfter helper

---

### 5. Demo Config: showcase-конфиг

Создать `configs/startup-showcase.json` -- 5 агентов с разными ролями, cron-задачами, system prompt'ами:

- **queen** (daemon, tactical, sonnet) -- координатор задач
- **assistant** (daemon, tactical, sonnet) -- чат-интерфейс с system prompt
- **maid@local** (daemon, operational) -- мониторинг здоровья
- **coder** (daemon, tactical, sonnet) -- написание кода + cron: code-quality-scan каждые 6ч
- **researcher** (daemon, tactical, sonnet) -- исследования + cron: daily-briefing в 8:00

---

## Порядок реализации

1. **HealthChecker fix** -- 1 файл, ~10 строк
2. **Demo config** -- 1 JSON файл
3. **Direct Node Messaging** -- index.html + app.js (UX улучшение)
4. **Task History** -- app.py (backend) + index.html + app.js + style.css
5. **CRON Page** -- 2 новых файла (cron.html, cron.js) + правки в 4 файлах

## Verification

1. **Build:** `"C:\Program Files\Go\bin\go.exe" build -o bin/hivekernel.exe ./cmd/hivekernel`
2. **Tests:** `"C:\Program Files\Go\bin\go.exe" test ./internal/... -v`
3. **Manual check:**
   - Запустить kernel + dashboard
   - Открыть Tree -- убедиться, что навигация (Tree|Chat|Cron) работает
   - Кликнуть узел -- "Send Task" кнопка видна и работает
   - После выполнения задачи -- панель Task History показывает запись
   - Открыть /cron -- таблица с CRON-задачами, кнопка добавления
   - HealthChecker не убивает виртуальные процессы
