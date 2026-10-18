# terminal ของเครื่องที่รัน engine ผ่าน WebSocket บน tunnel เดิม — design

- วันที่: 2026-10-18
- สถานะ: รออนุมัติ
- ขอบเขต: **`Tulipskun/ai-engine` เท่านั้น** — ฝั่งแอพ (`aixodia-app`) ผู้ใช้ทำเอง: terminal view + WS client ตามสัญญาใน §3
- ที่มา: ผู้ใช้ขอ (18 ต.ค. 2026) "ทำให้ engine เปิด ssh ผ่าน ws ด้วย url tunnel เดียวกันได้มั้ย? แบบที่ทำใน notebook kaggle ที่ชื่อ opencode" → ดูของเดิมแล้ว notebook `kyomusen/opencode` ทำ `sshd:2222` → `websockify:8080` → cloudflared อีก tunnel หนึ่ง และต่อด้วย `ssh -o ProxyCommand="websocat --binary wss://..."`; ต่อมาผู้ใช้ตัดสิน "ถ้าทำได้ก็ไม่ต้องเปิด ssh" (อยากได้ terminal ในแอพคล้าย Termux) และยืนยันรูปร่างที่เล่นกลับว่า "ตามนั้นเลย"
- ของเดิมที่ใช้ต่อ: tunnel เดียว (`cloudflared` ชี้ `127.0.0.1:8787`) และ WS hand-rolled ใน `io/mobile/ws.go` (handshake + frame + `maxFrame` 1 MiB)

## 1. Intent

engine รันอยู่ในเครื่องที่ผู้ใช้อยากคุมอยู่แล้ว — เปิด shell ของเครื่องนั้นให้แอพใช้ได้เหมือน Termux ผ่าน WebSocket บน tunnel เดิม ไม่มี ssh ไม่มี sshd ไม่มี websockify ไม่มีรหัสผ่านเพิ่ม ไม่มี tunnel ที่สอง และไม่แตะ `/etc/ssh`

## 2. Goals / Non-goals / Anti-goals

**Goals**
- `GET /terminal` (WS upgrade) บนพอร์ต 8787 → ออกทาง tunnel เดิม
- auth: bearer token ตัวเดียวกับที่ `/ws` ใช้ → ไม่มี/ผิด = 401, ไม่ใช่ upgrade = 400
- shell จริงบน pty: `/bin/bash -l` (ไม่มีค่อย `/bin/sh`), env สืบทอดทั้งหมด + `TERM=xterm-256color`; cwd = `/kaggle/working` ถ้ามีจริง ไม่งั้น cwd ของ engine (จริง ๆ คือ `$AIXODIA_HOME` เพราะ `aixodia:65` `cd` เข้าไปก่อนรัน — ใช้กฎเดียวกับ `AIXODIA_DB_PATH` ใน `localdb`)
- byte ผ่านตรงสองทาง (UTF-8/ANSI ไม่แกะ ไม่ strip) — แอพเอาไปเข้า terminal emulator ของตัวเอง
- resize ได้ทั้งตอนเปิด (`?cols=&rows=`) และระหว่างใช้ (control frame) → `TIOCSWINSZ`
- อยู่ได้ตอน terminal ว่าง: engine ส่ง WS ping เองทุก 30 วิ (Cloudflare ตัด WS ที่ไม่มีข้อมูล ~100 วิ; ฝั่งแชทวันนี้รอดเพราะแอพตั้ง `pingInterval` 20 วิ ที่ `AiDirectSocket.kt:50`)
- ปิด WS = shell ตาย (SIGHUP จาก kernel + kill process group สำรอง)
- หลาย connection = หลาย shell (pty แยกกัน) แต่จำกัดพร้อมกัน 8 → เกิน = 503
- **ไม่เพิ่ม dependency** — WS primitives ที่มีอยู่ + `golang.org/x/sys/unix` (มีใน go.mod แล้ว: ยืนยัน `TIOCSPTLCK`/`TIOCGPTN`/`TIOCGPTPEER`/`TIOCSWINSZ` + `IoctlSetWinsize`/`IoctlGetInt`/`IoctlSetPointerInt` มีจริงใน x/sys v0.48.0)

**Non-goals**
- ไม่มี sshd / sftp / scp / port-forward / ssh key / รหัสผ่าน — ตัดทั้งไอเดีย SSH-over-WS
- ไม่มีอัป/ดาวน์โหลดไฟล์ผ่าน terminal (แชทมี attachments อยู่แล้ว)
- AI ไม่แชร์ pty นี้ — แยกจาก tool `shell` ของ agent (คนละอย่าง ไม่รวมกันรอบนี้)
- ไม่ทำ persistence ของ shell ข้ามการตัดเน็ต/ข้าม engine restart — ถ้าอยากได้ให้รัน `tmux` ในเทอร์มินัลเอง แล้ว `tmux attach` หลังต่อใหม่
- ไม่แตะ `/ws` (แชท), API/สัญญาเดิม, tunnel, snapshot, restart, log DB

**Anti-goals**
- terminal ทำให้แชท/สตรีมสะดุด (แยก goroutine ต่อ connection, chunk จำกัด, cap จำนวน)
- log เนื้อหาที่ผู้ใช้พิมพ์หรือ output (log แค่ เปิด/ปิด + remote addr + pid)
- เปิด shell โดยไม่ตรวจ token หรือเผลอเปิด path/พฤติกรรมอื่น

## 3. สัญญากับแอพ (สิ่งที่ผู้ใช้ต้องทำฝั่งแอพ)

| เรื่อง | ค่า |
|---|---|
| URL | `wss://<tunnel-host>/terminal` (tunnel เดิมของ engine) |
| header | `Authorization: Bearer <CF_TOKEN>` — ตัวเดียวกับที่ `/ws` ใช้ |
| query (ไม่บังคับ) | `?cols=100&rows=30` ขนาดเริ่มต้น (default 80x24) |
| binary frame | byte ของ terminal ทั้งสองทาง: แอพ → engine = stdin, engine → แอพ = output |
| text frame (แอพ → engine) | `{"type":"resize","cols":100,"rows":30}` |
| text frame (engine → แอพ) | `{"type":"ready","shell":"/bin/bash","cwd":"/kaggle/working","cols":80,"rows":24}` แล้วตอนจบ `{"type":"exit","code":0}` (แล้วปิด WS); ถ้าเปิด pty/shell ไม่สำเร็จหลัง handshake → `{"type":"error","error":"..."}` แล้วปิด (`cwd` คือค่าที่ใช้จริงตาม §4.2) |
| ping | engine ส่ง WS ping ทุก 30 วิ และตอบ ping ของแอพอยู่แล้ว — แอพตอบ pong ตามมาตรฐาน WS อัตโนมัติ; จะตั้ง `pingInterval` แบบ `/ws` (20 วิ) ไว้ด้วยก็ได้ ไม่ชนกัน |
| Ctrl-C / Ctrl-D / Ctrl-Z | ส่งเป็น byte `0x03` / `0x04` / `0x1a` ทาง binary ปกติ (pty line discipline จัดการเอง ไม่ต้องมี control frame) |
| ข้อควรระวัง | input ใส่ทีละ frame เล็ก ๆ (keystroke) — paste ยาวให้แบ่ง frame ละ ≤ 32 KiB |

## 4. รูปร่างใหม่ (engine)

### 4.1 handler
- ไฟล์ใหม่ 1–2 ไฟล์ + เทสต์ (ชื่อ/ที่อยู่ให้ผู้สร้างเลือก) — ใช้ handshake/frame เดิม ไม่ import WS library ใหม่
- route: `mux.Handle("/terminal", ...)` — handler **ตรวจ bearer เอง** เหมือน `hub.Handler` (catch-all `"/"` ที่ห่อ `RequireToken` ไม่ช่วย เพราะ pattern เจาะจงกว่าชนะ)
- ตรวจ upgrade: ต้องมี `Upgrade: websocket` + `Sec-WebSocket-Version: 13` + `Sec-WebSocket-Key` ไม่งั้น 400 (แบบเดียวกับ hub)

### 4.2 pty
- เปิด `/dev/ptmx` → ปลดล็อกด้วย `TIOCSPTLCK` → เอาเลขด้วย `TIOCGPTN` → เปิด `/dev/pts/N` ด้วย `O_RDWR|O_NOCTTY` (ทางเดียวกับ creack/pty; `TIOCGPTPEER` ก็มีถ้าผู้สร้างสะดวกกว่า) — ไม่ต้องมี dependency
- child: `Setsid` + `Setctty` ให้ slave เป็น controlling terminal แล้ว exec `/bin/bash -l` (ไม่มี → `/bin/sh`); stdin/stdout/stderr = slave
- ขนาดเริ่มต้นจาก query (`cols`/`rows`, default 80x24) ผ่าน `TIOCSWINSZ`; resize ภายหลังก็ `TIOCSWINSZ` (kernel ส่ง `SIGWINCH` ให้ foreground process group เอง)
- env สืบทอดทั้งหมด (มี `CF_TOKEN`, `PATH`, `HF_TOKEN` ฯลฯ ครบเหมือนตอนผู้ใช้รันคำสั่งเอง) + `TERM=xterm-256color`
- cwd: `/kaggle/working` ถ้าเป็น dir จริง ไม่งั้น cwd ของ engine (`os.Getwd()`) — กฎเดียวกับที่ `localdb` เลือกไฟล์ DB; `ready.cwd` รายงานค่าที่ใช้จริง
- `bash -l` เป็น login + interactive → profile (`/etc/profile`, `~/.bashrc`) ทำงาน (ได้ PATH ของ conda/CUDA ตาม image) และถ้า profile `cd` เองก็ปล่อยตามนั้นเหมือน shell ปกติ — เราไม่ re-assert env/cwd ทับ

### 4.3 ชีวิตของ shell
- อ่าน master → ส่งเป็น binary frame (chunk ≤ 32 KiB)
- รับ binary frame → เขียนลง master
- shell ออก → ส่ง `{"type":"exit","code":N}` แล้วปิด WS
- WS ปิด/พัง → ปิด master (kernel ส่ง SIGHUP เอง) แล้ว kill process group สำรอง กันลูกค้าง (pattern เดียวกับ `exec.CommandContext` + process group ใน `tools/tools.go`)
- เปิด pty/shell ไม่สำเร็จ (หลัง handshake ไปแล้ว) → ส่ง `{"type":"error","error":"..."}` แล้วปิด (ไม่ทิ้งให้แอพเห็นแค่ socket ปิด)
- ping ทุก 30 วิ ต่อ connection (อ่าน pong ผ่าน loop เดิม); เขียน ping ไม่ผ่าน = ถือว่าตาย แล้วเก็บกวาดตามปกติ

### 4.4 ความปลอดภัย
- token คือประตูเดียว — เท่ากับวันนี้ (ใครถือ token ก็ได้ root shell อยู่แล้วผ่าน `/restart` และ tool `shell` ของ agent) ระดับความเสี่ยงไม่เปลี่ยน
- จำกัด 8 connection พร้อมกัน (ตัวนับ global) → เกิน = 503
- ไม่เขียน `/etc/ssh`, ไม่มี daemon เพิ่ม, ไม่มีไฟล์ config เพิ่ม, ไม่เพิ่มคีย์ D1

## 5. ฝั่งแอพ (ของผู้ใช้ — engine ไม่บังคับวิธี)

- เปิด WS ไป `/terminal` + ส่ง token, ส่ง `resize` (หรือใส่ `?cols=&rows=`), แสดง byte ที่ได้ด้วย terminal emulator (xterm.js ใน WebView หรือ widget ของแอพ)
- ปุ่มพิเศษ (Ctrl/Tab/ลูกศร/Esc) ส่งเป็น byte ปกติ
- **ข้อสังเกต:** WebView ตั้ง header `Authorization` บน WS ไม่ได้ — ถ้าใช้ xterm.js ใน WebView ให้เปิด WS จากฝั่ง native (OkHttp แบบเดียวกับ `/ws` วันนี้) แล้ว bridge byte เข้า JS; ถ้าใช้ WS จาก native อยู่แล้วก็ตรง ๆ

## 6. การทดสอบ

- unit: เปิด pty → `stty size` เห็นค่าที่ตั้ง, `echo hi` ออก `hi`
- unit: shell path ที่ไม่มี → ได้ `{"type":"error","error":...}` แล้วปิด
- ระดับ handler ด้วย token จริง: ส่ง byte เห็น output กลับ, ส่ง `resize` แล้ว shell เห็นขนาดใหม่, `exit 3` → ได้ `{"type":"exit","code":3}`
- keepalive: ping ticker ทดสอบด้วย interval ที่ย่อได้ (ผู้สร้างเลือกวิธี ไม่ต้องรอ 30 วิจริง)
- ประตู: ไม่มี token → 401; `GET /terminal` ไม่ upgrade → 400; เกิน cap → 503
- ของเดิมต้องเขียว: `go clean -testcache && go build ./... && go vet ./... && gofmt -l . && go test ./...`

## 7. ลำดับปฏิบัติ

1. engine: WS primitives ที่ใช้ร่วม + handler + pty + route + เทสต์
2. เทสต์ผ่าน + ของเดิมเขียว
3. release rolling `engine-latest` → `POST /restart` ให้ session ติดตั้งเอง
4. verify จริงผ่าน tunnel: ต่อ `/terminal` ด้วย token → `echo`, `stty size` ตรงกับ resize, เปิด `nano`/`top` ได้, ปิดแล้วไม่มี process ค้าง, แชทเดิมยังทำงาน
5. ส่ง URL + ตัวอย่าง frame ให้ผู้ใช้ทำฝั่งแอพ

## 8. Calls ของผม (ให้ตรวจ)

- **framing ผสม:** binary = byte ล้วน, text = JSON control (ประหยัดกว่า base64 ทั้งหมด และฝั่งแอพ parse ง่าย)
- **ไม่แตะ chat WS:** แยก handshake/frame ออกเป็น package เล็ก (`wsock`) ให้ทั้ง chat และ terminal ใช้ตัวเดียวกัน — ถ้าไม่อยากแตะ chat เลย ผมลอกในไฟล์ใหม่ได้ (บอกได้)
- **ไม่มี subprotocol/version** ใน handshake — path `/terminal` คือสัญญา (engine กับแอพเป็นของเราทั้งคู่)
- **control มีแค่ `resize`** (แอพ→engine) และ `ready`/`exit`/`error` (engine→แอพ); Ctrl-* เป็น byte ธรรมดา
- **`TERM=xterm-256color`** — ถ้า emulator ฝั่งแอพไม่ใช่ xterm-256color บอกได้ เปลี่ยนเป็น `xterm`
- **cwd เริ่มที่ `/kaggle/working` ถ้ามีจริง ไม่งั้น cwd ของ engine** — ถ้าอยากให้เริ่มที่ `$AIXODIA_HOME` หรือ `$HOME` บอกได้ (กฎเดิมของ repo: ใช้ `/kaggle/working` เมื่อมี)
- **`bash -l` แบบ login+interactive ปล่อยให้ profile ทำงาน** — ไม่ re-assert env/cwd ทับ (ได้ PATH conda/CUDA ของ image)
- **ping 30 วิ ทำที่ `/terminal` เท่านั้น** — ไม่แตะ ping/พฤติกรรมของ chat hub; `pingInterval` 20 วิ ของแอพใช้ร่วมได้ไม่ชนกัน
- **`error` frame** เพิ่มเข้ามา (หลัง handshake ล้มเหลว) แทนที่จะปิดเงียบ ๆ
- **cap 8 connection, chunk 32 KiB, frame input ≤ 1 MiB** (เพดานเดิมของ `maxFrame`)
- **ไม่ log เนื้อหา; ไม่ทำ persistence** (ให้รัน `tmux` เอง); engine restart = shell ตาย
- **ไม่รวม terminal กับ tool `shell` ของ agent** ในรอบนี้

## 9. ความเสี่ยง / ค้าง

- คำสั่งหนัก (เช่น `yes`, `cat ไฟล์ยักษ์`) ทำให้ tunnel/แอพแน่นได้ — เท่ากับผู้ใช้รันในเครื่องตัวเอง (ไม่มี rate limit เพิ่ม; chunk + cap ช่วยจำกัด)
- Cloudflare ตัด WS ที่ว่างเกิน ~100 วิ — ping 30 วิ ใน §4.3 กันไว้ (เราแค่ต้องเขียน ping ได้ ไม่ได้บังคับว่าต้องได้ pong; WS client มาตรฐานตอบ pong เอง)
- token หลุด = root shell — เท่ากับ `/restart` วันนี้
- ถ้าแอพเลือกทาง WebView/xterm.js ต้องเปิด WS จาก native (ดู §5) และต้องเช็คว่า `wss` ผ่านจากมือถือได้ (วันนี้ `/ws` ผ่านอยู่ → น่าจะผ่าน)
- ปิดแอพแล้ว shell ตาย (ตั้งใจ) — ไม่มีทาง "กู้" session ที่ค้าง; ตอบด้วย `tmux`
- revert ง่าย: ไม่มี migration/ข้อมูล — ถอย commit + republish release
