# ЗАДАЧА: io_uring send-путь для quic-go (продолжение)

> Документ-бриф для открытия в новой сессии. Ветка `feat/io-uring-send` форка
> `nagual2/quic-go` (клон: `C:\Git\project\quic-go`, upstream remote добавлен).
> Материнский контекст: `nagual2/ssh3`, Stage 1.5 «data-path performance»
> (см. `CTO_TASK.md` §6a и `docs/PROFILE-2026-09-29.md` в репо ssh3).

## Контекст и цель

R3-бенчмарки показали: bulk-throughput ssh3 118–131 MB/s против OpenSSH 365–488
(loopback, WSL). Профилирование (docs/PROFILE-2026-09-29.md в ssh3) выявило
доминацию системных вызовов per-packet UDP I/O (39% клиента, 25% сервера);
AES ~3% (AES-NI работает, невиновен). quic-go обновлён до v0.59.1 — throughput
не изменился: стена фундаментальная (userspace QUIC, ядерного offload нет).

Единственный нетронутый syscall-рычаг — **io_uring**: батчинг SENDMSG-операций,
один `io_uring_enter` на кольцо вместо syscall на batch. x/sys/unix биндингов
io_uring не содержит — написаны сырые (`internal/io_uring/io_uring_raw.go`).

## Текущее состояние (коммиты f960da7a → fecb456a, ветка feat/io-uring-send)

Работает:

- `io_uring_setup` (params 104 Б: 40 первый блок + sq_off 36 Б (9 полей) +
  cq_off 32 Б (8 полей)) — сверено hexdump'ом;
- mmap колец: SQ@off 0, CQ@0x8000000, SQE-массив@0x10000000; размеры из params;
- **заголовки колец на ядре 6.18 паддингованы под атомики**:
  SQ: head@0, tail@4, mask@16, entries@24, flags@32, dropped@36, array@320;
  CQ: head@0, tail@8, mask@12, entries@20, overflow@28, cqes@44, flags@64;
- маски и entries читаются как ЗНАЧЕНИЯ из mmap по этим офсетам
  (sqMask=7 ✓; cq ядро удваивает: sq=8 → cq=16);
- SQE-плейсмент байт-в-байт верифицирован (opcode/fd/len/userdata в памяти);
- ядро потребляет SQE (sqHead 0→1) и доставляет CQE с нашим userdata (0x1234).

Сломано (последний блокер):

- **IORING_OP_WRITE возвращает EFAULT (-14)** — ядро не смогло прочитать
  payload по переданному адресу. SQE долетает (userdata совпадает), значит
  проблема в самом адресе данных или в особенностях окружения go test
  (stdout перехвачен go test'ом — pipe; проверять через `os.Pipe` и
  временный файл).

## TRAP-лист (набито кровью этой сессии)

| № | Ловушка |
|---|---------|
| 1 | `params.SqOff.*` — это **байтовые офсеты в mmap с паддингом** (mask@16, entries@24, array@320), а не компактные `4*i`; значения (маска, entries) читаются из mmap по офсетам |
| 2 | CQ-кольцо ядро удваивает: sq=8 → cq=16; маска = entries-1 |
| 3 | `io_sqring_offsets` в UAPI — 9 полей (36 Б, включая Resv2); `io_cqring_offsets` — 8 полей (32 Б); первый блок params — 10 u32 (40 Б); итого 108 |
| 4 | SQ-индексный массив инициализирует приложение: identity 0..n-1 |
| 5 | CQE-массив читается по офсету `CqOff.Cqes` (44), НЕ с нуля (там head/tail-заголовок) |
| 6 | WSL: go1.26.0 (GOTOOLCHAIN=auto); sum.golang.org/прокси флапают точечно — ретраи; `GOSUMDB=off` блокирует скачивание toolchain; strace отсутствует; инлайн `wsl bash -lc '...$var...'` у агента съедается — только файловые скрипты |

## Следующие шаги

1. Прочитать UAPI ядра 6.18 (`include/uapi/linux/io_uring.h`) и путь mmap в
   `io_uring.c` — понять, где на 6.18 лежит SQE-массив (сменился ли
   `IORING_OFF_SQES`, появилась ли single-region схема).
2. Оживить WRITE: тест через `os.Pipe` (гарантированно валидный fd), затем
   файл; при EFAULT — проверить `runtime.Pinner` на payload и владельца памяти.
3. После оживления: GSO-cmsg (UDP_SEGMENT) в SENDMSG, затем интеграция в
   send_queue за env-флагом.
4. PR-материал: бенчмарки A/B уже есть (`io_uring_send_test.go`),
   доехать до `B1` через весь стек ssh3.

## Критерий готовности

- `TestRingWriteStdout` / `TestRingWritePipe` зелёные (res == len);
- B1 push через io_uring-путь vs база 118–131 MB/s — разница зафиксирована;
- остальные сьюты зелёные, Rust-интероп не тронут (`vendor/h3` не менять).

## Не потерять

- Бенчмарк-факты: jumbo-датаграммы ×16 на loopback (2547 MB/s), WriteBatch+GSO
  4076 MB/s — уже в `io_uring_send_test.go`;
- сырой пакет не пушить в main: только PR-подмножество (internal/io_uring
  без бенч-мусора) после оживления.
