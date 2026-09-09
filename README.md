# OpenMark Core 🚀

![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)
![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8.svg?style=flat&logo=go)
![Docker](https://img.shields.io/badge/Docker-Ready-2496ED.svg?style=flat&logo=docker)

**OpenMark** — это высоконагруженное промышленное ПО промежуточного слоя (Industrial Middleware) на Go, предназначенное для прямой интеграции маркировочного и весоизмерительного оборудования с ERP/MES-системами (1С и др.) и Национальной системой маркировки («Честный Знак» / ГИС МТ).

Сервис исключает необходимость закупки дорогостоящих вендорских лицензий и обеспечивает отказоустойчивую работу конвейера в режиме 24/7.

---

## 🏗 Архитектура и возможности

Проект построен по принципам гексагональной архитектуры (Clean Architecture), что позволяет легко изолировать ядро бизнес-логики от драйверов физических устройств:

* **Прямой сокетный обмен (Direct Socket I/O):** Драйверы общаются с принтерами и весовыми комплексами напрямую по сетевым протоколам без стороннего тяжёлого ПО.
* **Драйверная архитектура (`/internal/drivers/`):** Единый интерфейс для интеграции оборудования. Из коробки поддерживаются:
  * **Savema** (SPPL)
  * **TSC** (TSPL)
  * **Videojet** (Zypher)
  * **CarlValentine/GEA** (CVPL)
  * **MARKEM Imaje** 
* **Буферизация и локальная очередь:** При отвале центральной ERP или сети сервис продолжает потоковую печать и регистрацию кодов из локального буфера.
* **Observability из коробки:** Интеграция с **Promtail / Grafana Loki** для централизованного сбора логов и мониторинга событий на линии.
* **Web UI Dashboard:** Встроенный легкий интерфейс для операторов КИПиА и контроля состояния оборудования на линии.


🏗 Архитектурная структура RovnoMark / AstraPrint

1. **Ядро (internal/core)**
- **Line & Task Orchestration:** Управление топологией «Линия» (координация четных/нечетных принтеров, конвейерный насос Pumper).
- **TaskProcessor:** Фоновая асинхронная прокачка партий кодов из SQLite в буферы оборудования с контролем свободного места (GetBufferFreeSpace) и подтверждением через одометры.
- **PrinterManager:** Потокобезопасный реестр активных соединений (sync.RWMutex) и сборщик телеметрии.
- **Интерфейс core.Printer:** Аппаратный контракт с методами управления сессиями (InitSession, PrintBatchIndexed, ClearQueue, GetTotalPrints).
- **Marking (core/marking):** Парсинг и верификация структуры кодов маркировки GS1 DataMatrix.

2. **Драйверы оборудования (internal/drivers)**
- **videojet**: Протокол Zipher/CLARiTY (команды GST, SHO, SID, SLR).
- **markem:** Протокол SOAP/XML поверх TCP в кодировке UTF-16LE (SmartDate X60/X40).
- **valentine:** Драйверы CVPL Native и NiceLabel Direct .
- **savema:** Бинарный протокол SPPL.
- **tsc:** Генератор и шаблонизатор команд TSPL.
- **extserver:** Виртуальный HTTP/XML шлюз для внешних контроллеров печати.

3. **Хранилище (internal/storage)**
- **Master DB (openMark_master.db):** Конфигурации линий, привязки, задачи, журнал аудита и телеметрия.
- **Monthly Sharding:** Изолированные помесячные базы кодов (codes_YYYY_MM.db) с защитой от разрастания WAL и B-Tree индексов.
- **Миграции:** Управление схемой данных строго через PRAGMA user_version.
- **Надежность:** Режим WAL, синхронизация NORMAL, busy_timeout 5000 мс, принудительный wal_checkpoint(TRUNCATE) при остановке.

4. **Транспорт и интерфейс (internal/api & UI)**
- **API Server:** Изолированный роутер без глобального ServeMux.
- **1C/MES Gateway:** Эндпоинты /api/task/create, /api/task/append, /api/task/stop, /api/system/info.
- **Quality Control & Live:** Эндпоинты /api/code/info (ОКК) и /api/dashboard/live.
- **Embedded UI:** Три автономных SPA-интерфейса в embed.FS (Инженерная панель, Диспетчер линий /frontend2, Терминал ОКК /okk).

5. **Метаданные и White-label (internal/version, internal/brand)**
- Сквозной контроль версий бинарника (Version, GitCommit, BuildDate) через -ldflags.
- Конфигурация торговой марки шлюза на лету через GATEWAY_BRAND_NAME или при сборке.

6. **Сервисный контур Windows**
- Нативная служба Windows (Windows Service) с поддержкой SCM-команд.
- Интеграция с Windows Event Log при авариях связи и сбоях сокетов.
---

## 📁 Структура проекта

```text
├── internal/
│   ├── core/         # Оркестратор пайплайна маркировки и менеджер потоков
│   ├── drivers/      # Драйверы физического оборудования (Savema, TSC, Videojet...)
│   ├── models/       # Структуры данных DataMatrix, весовых диапазонов и команд
│   └── storage/      # Буферизация очереди и локальное хранилище состояний
├── ui/               # Веб-интерфейс оператора
├── Dockerfile        # Контейнеризация приложения
├── docker-compose.yml# Готовый стек для разворачивания в цеху
└── promtail-config.yaml # Конфигурация логирования
```
---
# 🚀 Быстрый запуск
**Запуск через Docker Compose **
```text
Bash
# Клонировать репозиторий
git clone [https://github.com/your-username/openmark.git](https://github.com/your-username/openmark.git)
cd openmark
```
# Запустить сервис и обвязку логирования
docker-compose up -d --build
После запуска Web Dashboard доступен по адресу: http://localhost:8080

**🧩 Руководство для разработчиков (How to add a Driver)**
Для добавления поддержки нового оборудования (например, Bizerba, Markem-Imaje или Carl Valentin):

Создайте новый пакет в директории internal/drivers/<vendor_name>/.

Реализуйте интерфейс драйвера (метод инициализации сокета, отправку управляющих последовательностей, парсинг ответов).

Зарегистрируйте новый драйвер в internal/core/manager.go.

**📄 Лицензирование
Copyright (c) 2026 [Aleksandr K.](zmichok@gmail.com)**

Проект распространяется под лицензией GNU Affero General Public License v3.0 (AGPLv3).

Open Source: Вы можете свободно использовать, запускать и модифицировать данный софт на производственных площадках.

Ограничения: Запрещена закрытая коммерческая перепродажа кода или передача третьим лицам без открытия исходного кода ваших доработок.

**Commercial Licensing: По вопросам получения закрытой коммерческой лицензии обращаться к автору проекта.**
