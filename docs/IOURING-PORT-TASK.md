# ЗАДАЧА: io_uring send-путь для quic-go (продолжение)

> Документ-бриф для открытия в новой сессии. Ветка `feat/io-uring-send` форка
> `nagual2/quic-go` (клон: `C:\Git\project\quic-go`, upstream remote добавлен).
> Материнский контекст: `nagual2/ssh3`, Stage 1.5 «data-path performance»
> (см. `CTO_TASK.md` §6a и `docs/PROFILE-2026-09-29.md` в репо ssh3).

## Контекст и цель

R3-бенчмарки показали: bulk-throughput ssh3 118–131 MB/s против OpenSSH 365–488
(loopback, WSL). Профилирование выявило доминацию системных вызовов per-packet
UDP I/O. Единственный нетронутый syscall-рычаг — **io_uring**: батчинг
SENDMSG-операций, один `io_uring_enter` на батч. x/sys/unix биндингов
io_uring не содержит — написаны сырые (`internal/io_uring/io_uring_raw.go`).

## Текущее состояние (сессия 2026-09-29, ветка feat/io-uring-send)

**Всё оживлено и интегрировано:**

- `internal/io_uring`: setup + mmap колец, адаптивный резолв CQ-layout
  (`resolveCqLayout`), WRITE-тесты (`TestRingWritePipe/File/Stdout`) зелёные;
- SENDMSG+GSO end-to-end (`TestSenderUDPLoopbackGSO/Batch`): кольцо + cmsg
  UDP_SEGMENT, sockaddr-упаковка v4/v6, per-slot control-буферы;
- `send_queue_uring.go` (package quic): `uringSendQueue` за env-флагом
  **`QUIC_GO_IO_URING_SEND=1`** — батч до 64 пакетов, один enter на батч;
  деградация до `conn.Write` при любой нефатальной для QUIC ошибке (ring
  закрывается, батч пересылается fallback-путём; EMSGSIZE/PMTU — как раньше);
- кросс-компиляция windows/darwin зелёная (stub `io_uring_stub.go`,
  `send_queue_uring_other.go`);
- полный suite корневого пакета зелёный.

**Бенчи (loopback, WSL, Ryzen 5 4600H, 3 прогона):**

| Путь | MB/s |
|------|------|
| Sendto (1 syscall/датаграмма) | 200–213 |
| SendtoJumbo (64 KiB датаграммы) | 2708–3655 |
| WriteBatch+GSO (текущий путь quic-go) | 3100–4479 (шумно) |
| IOURingGSO (Submit 64 + 1 enter) | 3670–4350 |

Паритет с WriteBatch+GSO на loopback — выигрыш от io_uring ожидается не в
микробенче, а в B1 (сквозной ssh3 push, один батчер на send-очередь).

## Блокеры, снятые в этой сессии (были: EFAULT + «NOP-пробы»)

1. **Опкод**: 5 — это `WRITE_FIXED` (требует registered buffers → EFAULT без
   них). `IORING_OP_WRITE` = **23** (UAPI-enum). «NOP-пробы» прошлой сессии —
   зеро-SQE = opcode 0.
2. **CQ-layout ядра 6.18.33.2-MS-WSL2**: mmap-содержимое CQ-кольца НЕ следует
   reported `cq_off` — фактическая единая layout `io_rings`:
   `sq head@0, tail@4; cq head@8, tail@12; sq_mask@16, cq_mask@20;
   sq_entries@24, cq_entries@28; cqes@64; sq_array@320 (для sq=8)`.
   Reported `cq_off {0,8,12,20,44,...}` — устаревший шаблон. Кросс-проверка:
   reported `sq_off.array = 64 + cq_entries*16`, т.е. ядро само считает
   cqes@64. Резолв адаптивный: по совпадению значений entries/mask.

## TRAP-лист

| № | Ловушка |
|---|---------|
| 1 | `params.SqOff.*` — байтовые офсеты в mmap с паддингом; значения (mask, entries) читаются из mmap по офсетам |
| 2 | CQ-кольцо ядро удваивает: sq=8 → cq=16; маска = entries-1 |
| 3 | SQE-массив — классический mmap `IORING_OFF_SQES` (0x10000000); внутри SQ-mmap его нет (sqRing на 6.18 = 352 Б для sq=8: заголовок+индексный массив) |
| 4 | SQ-индексный массив инициализирует приложение: identity 0..n-1 |
| 5 | **Опкоды**: `WRITE_FIXED`=5 (registered buffers, иначе EFAULT), `WRITE`=23, `SENDMSG`=9 |
| 6 | **6.18.33.2-MS**: CQ-content по единой layout `io_rings` (head@8, tail@12, mask@20, entries@28, cqes@64), reported `cq_off` не совпадает; резолвить адаптивно (`resolveCqLayout`) |
| 7 | **`net.UDPConn.File()` → `*os.File` с финализатором**: если не держать ссылку (defer Close), GC закрывает дублированный fd → EBADF в середине батча (маскировался под «глюк ядра») |
| 8 | Control-буферы (UDP_SEGMENT/ECN) для кольца — **per-slot**: общий oob-буфер sconn перезаписывается до `io_uring_enter`; `Sender` требует живости data+control до возврата `Flush` |
| 9 | WSL: go1.26.0 (GOTOOLCHAIN=auto); inline `wsl bash -lc '...$var...'` у агента съедается — файловые скрипты |

## Следующие шаги

1. [ВЫПОЛНЕНО 2026-09-29] **B1 через весь стек ssh3** — вердикт: **паритет,
   выигрыша нет**. Детали: `ssh3/docs/B1-IOURING-2026-09-29.md`.
   - Медианы (512 MiB push, S1 loopback, 10 прогонов): stock v0.49 137.2 /
     fork-off 129.0 / fork-on 151.3 MB/s;
   - арбитраж строгим ABBA (8 пар): средняя попарная Δ ≈ −1.9 MB/s (−1.5%) —
     в пределах шума;
   - корректность: 512 MiB через кольцо бит-в-бит (cmp OK), ring-fd в клиенте
     подтверждён;
   - причина: v0.49-путь (WriteBatch+GSO) уже коалесцирует send'ы — число
     sendmsg не является узким местом полного ssh3 (сходится с микробенчем).
2. Порт на v0.49: worktree `project/quic-go-v049`, ветка `bench/io-uring-v049`
   (cherry-pick af8a7816 c адаптацией: нет SendProbe/WriteTo, sconn хранит
   packetInfoOOB/remoteAddr плоскими полями). База под PR в ssh3, пока ssh3
   сидит на v0.49.
3. PR-подмножество в форк v0.59.1: `internal/io_uring` без бенч-мусора и
   debug-логов (`TestRingWriteStdout` пишет в stdout сырой `go test`-вывод).
4. Опционально: `io_uring_register_buffers` (FIXED-опкоды), multishot send и
   ЗА-соединение кольцо — следующий рычаг, если когда-нибудь вернёмся к
   syscall-направлению; приоритет сместился на слой ssh3 и апгрейд quic-go.

## Критерий готовности

- [x] `TestRingWriteStdout` / `TestRingWritePipe` зелёные (res == len);
- [x] SENDMSG+GSO end-to-end зелёные;
- [x] интеграция в send_queue за env-флагом, деградация безопасная;
- [x] `go test .` и `go vet` зелёные, кросс-компиляция windows/darwin зелёная;
- [x] B1 push через io_uring-путь vs база — **зафиксирован: дельта отсутствует
  (паритет, −1.5% в ABBA)**;
- [x] остальные сьюты зелёные, Rust-интероп не тронут (`vendor/h3` не менять).

## Не потерять

- Бенчмарк-факты: jumbo-датаграммы ×16 на loopback, WriteBatch+GSO и
  IOURingGSO ~4 GB/s-класс — в `io_uring_send_test.go`;
- сырой пакет не пушить в main: только PR-подмножество после B1.
