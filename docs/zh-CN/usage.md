# 模型网关中文测试环境使用说明

本文说明如何用 PostgreSQL 部署网关、启用中文测试控制台、用 `admin` / `admin` 登录、创建渠道并验证重启后数据仍然存在。所有示例都使用占位符；请勿把真实的 `DATABASE_URL`、密码或上游密钥提交到仓库。

## 1. 适用范围

- **PostgreSQL**：设置 `DATABASE_URL` 后启用，建议使用独立的 `exp_xxxx` 数据库。不设置时继续使用原来的 SQLite 单文件模式，已有实例无需修改。
- **SQLite**：默认模式，数据库文件为 `$DATA_DIR/gateway.db`。
- **中文测试控制台**：设置 `TEST_MODE=1` 时显式启用。控制台默认中文，初始管理员账号为 `admin` / `admin`。登录、概览、渠道、模型、密钥、日志、语音合成、音色设计、音乐生成等页面均提供中文界面与提示。
- `TEST_MODE=1` 仅用于本地或验收测试，**不要在生产环境启用**。生产环境仍应设置强随机的 `SECRET_KEY` 和 `ADMIN_PASSWORD`。

`admin` / `admin` 只在数据库第一次创建管理员时生效。如果数据库中已经存在管理员，请使用已有账号，或换一个空数据库重新验收。

## 2. 准备 PostgreSQL 数据库

在测试数据库服务器上创建一个独立数据库（示例名称 `exp_model_relay`）：

```bash
createdb -h 127.0.0.1 -p 5432 -U relay exp_model_relay
```

也可以使用已有的 `exp_xxxx` 数据库。记下连接串，但不要写进仓库：

```bash
read -rsp '数据库密码: ' PGPASSWORD; echo
export PGPASSWORD
export DATABASE_URL="postgres://relay@127.0.0.1:5432/exp_model_relay?sslmode=disable"
```

网关首次启动时会自动建表，之后每次启动只做增量迁移，不需要手工执行 SQL。

## 3. 启动网关

### 方式 A：本机二进制

```bash
# 构建前端并编译内嵌控制台的二进制
make build

read -rsp '数据库密码: ' PGPASSWORD; echo
export PGPASSWORD
export DATABASE_URL="postgres://relay@127.0.0.1:5432/exp_model_relay?sslmode=disable"
export TEST_MODE=1
export SECRET_KEY="$(openssl rand -hex 32)"
export DATA_DIR='./data'
./bin/gateway
```

启动日志会输出 `database=postgres`，并提示已启用本地测试环境。

### 方式 B：Docker Compose

```bash
cp .env.example .env
# 编辑 .env：
#   SECRET_KEY=强随机字符串
#   POSTGRES_PASSWORD=数据库密码（使用 URL 安全字符）
#   PGPASSWORD=同一个数据库密码
#   DATABASE_URL=postgres://relay@postgres:5432/exp_model_relay?sslmode=disable
#   TEST_MODE=1            # 仅测试环境
docker compose -f docker-compose.postgres.yml up --build -d
docker compose -f docker-compose.postgres.yml logs -f gateway
```

该 compose 文件会启动 PostgreSQL，并把 `DATABASE_URL` 指向容器内的 `postgres:5432`。默认的 `docker-compose.yml` 仍保持原来的 SQLite 用法。

如果需要在容器里启用 `TEST_MODE=1`，注意监听地址默认是容器内的 `127.0.0.1`，端口映射无法访问；确需从宿主机访问时在容器内显式设置 `HOST=0.0.0.0`，并只把宿主机端口绑定到回环地址（例如 `127.0.0.1:8080:8080`）。

## 4. SQLite 路径与监听地址（本次评测环境）

本次统一评测使用 SQLite，可以用两个环境变量固定数据库位置和监听地址：

```bash
export SQLITE_PATH='/srv/model-relay/state/gateway.db'
export DATA_DIR='/srv/model-relay/data'
export HOST=127.0.0.1
export TEST_MODE=1
export SECRET_KEY="$(openssl rand -hex 32)"
./bin/gateway
```

- `SQLITE_PATH`：指定 SQLite 文件的完整路径。网关会在该路径创建或复用数据库文件，并自动创建父目录；重启时使用同一个 `SQLITE_PATH` 即可保留渠道、密钥和日志。未设置时保持原来的 `$DATA_DIR/gateway.db`，已有实例无需修改。
- `HOST`：HTTP 监听地址，控制管理控制台、管理 API 和中继 API 共用的监听端口。正常部署不设置 `HOST` 时仍监听所有网卡，与之前一致；`TEST_MODE=1` 且未设置 `HOST` 时默认只监听 `127.0.0.1`，避免 `admin/admin` 暴露到其他网卡。显式设置 `HOST` 时以显式值为准；如果在 `TEST_MODE=1` 下绑定非回环地址，启动日志会给出警告。
- 同时设置 `DATABASE_URL` 时仍优先使用 PostgreSQL，`SQLITE_PATH` 不生效。

验证监听地址：

```bash
lsof -nP -iTCP:8080 -sTCP:LISTEN
# TEST_MODE=1 且未设置 HOST 时应看到 127.0.0.1:8080
```

用于真实厂商凭证的本地试用实例时，建议显式设置 `HOST=127.0.0.1`，不要设置 `HOST=0.0.0.0`；只在本机浏览器访问测试控制台。

## 5. 登录测试控制台

浏览器打开 <http://localhost:8080>，使用：

| 用户名 | 密码 |
| --- | --- |
| `admin` | `admin` |

登录页会显示“本地测试环境”提示；登录后顶部会显示当前数据库后端（PostgreSQL 或 SQLite）。如果仍看到英文界面，请确认启动环境里确实设置了 `TEST_MODE=1`，并清除浏览器中该站点的 `mr-lang` 本地存储后刷新。

## 6. 创建渠道

1. 打开左侧“渠道”，点击“添加渠道”。
2. 填写：
   - **名称**：例如 `本地 mock`
   - **协议**：`OpenAI 兼容`
   - **Base URL**：本地 mock 使用 `http://127.0.0.1:19090/v1`
   - **API 密钥**：mock 可用任意非空值，例如 `test-key`
   - **模型**：`gpt-4o`（或点击“拉取模型”后勾选）
3. 点击“创建渠道”，再在渠道行点击“测试连接”，应提示连接成功。

没有上游服务时，可先启动仓库自带的 mock：

```bash
python3 scripts/mock-upstream.py 19090
```

## 7. 创建 API 密钥并验证请求

1. 打开“API 密钥”，点击“创建密钥”。
2. 名称填 `验收测试`，额度可勾选“不限额度”，点击“创建密钥”。
3. 复制只显示一次的 `sk-...` 明文，调用：

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-替换为你的密钥" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"你好"}]}'
```

也可以在“概览”和“请求日志”页面看到这次请求的令牌数、费用和延迟。

## 8. 语音与音乐新入口（可选验收）

- **语音合成**：添加带 `speech-` 模型的 MiniMax 渠道后，可试听短文本的流式朗读；超过同步阈值的文本会作为一个异步 MiniMax 任务提交。任务 ID 保存在数据库和 `DATA_DIR`，刷新页面或重启网关后仍会继续跟踪。
- **音乐生成**：添加 `minimax-music` 渠道（例如模型 `music-3.0`）后，在“音乐”页填写风格描述和歌词即可生成歌曲；完成后可试听和下载 MP3/WAV。
- 两种任务的记录分别保存在 `speech_clips` 和 `music_tasks` 表中；PostgreSQL 与 SQLite 首次启动时都会自动建表。

## 9. 重启验证

1. 记下已创建的渠道名称和 API 密钥名称。
2. 停止网关（本机 `Ctrl+C`；Compose 使用 `docker compose -f docker-compose.postgres.yml restart gateway`）。
3. 用相同的 `DATABASE_URL`、`TEST_MODE=1`、`SECRET_KEY` 重新启动。
4. 重新登录后确认：
   - 渠道列表仍有刚才的渠道；
   - API 密钥仍在，历史请求日志仍可查询；
   - 如已生成语音或音乐，任务记录与可下载音频仍在；
   - `/api/status` 返回的 `database` 仍为 `postgres`。

本机验证：

```bash
curl -s http://localhost:8080/api/status
# {"name":"Model Relay",...,"database":"postgres","test_mode":true}

psql "$DATABASE_URL" -c 'select count(*) from channels;'
```

SQLite 模式的重启验证方式相同：使用同一个 `SQLITE_PATH` 重启即可；未设置 `SQLITE_PATH` 时数据仍保存在 `$DATA_DIR/gateway.db`。

## 10. 从 SQLite 切换到 PostgreSQL

两种后端的数据不会自动互相迁移。切到 PostgreSQL 会从一个空数据库开始；如需保留原 SQLite 数据用于回滚，请不要删除 `$DATA_DIR/gateway.db`。切换前建议先备份数据库和 `DATA_DIR`。

## 11. 安全要求

- 生产环境不要设置 `TEST_MODE=1`，并始终设置强随机的 `SECRET_KEY` 和 `ADMIN_PASSWORD`。
- `DATABASE_URL` 含有数据库密码，只放在环境变量或未提交的 `.env` 中。
- 上游渠道密钥由 `SECRET_KEY` 加密后存入数据库；更换 `SECRET_KEY` 会导致已有渠道密钥无法解密，需要重新填写。
- 对公网暴露前请配置 TLS 和访问控制。
