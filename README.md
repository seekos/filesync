# FileSync

一个用 Go 编写的双端文件同步程序：

- 发送端：提供 Web 页面，管理多个本地同步目录。
- 接收端：不提供配置页面，只接收并存储发送端推送过来的文件。
- **本机目录镜像**：可将本地目录增量同步到本机另一绝对路径，不经过 HTTP 接收端（与接收端共用比对、排除、清理多余文件等逻辑）。

发送端会先把本地目录清单发给接收端比对，接收端只返回缺失或变化的文件。随后发送端逐个流式上传这些文件，因此适合小文件批量同步，也适合大文件传输，不会把文件整体读入内存。本机镜像则在本地比对后直接复制，大文件同样采用流式 `io.Copy` 与 `.uploading` 临时文件再 `rename` 的原子替换方式。

## 功能

- 发送端 Web 管理同步目录
- 接收端纯 API 接收文件，无配置页面
- 多目录支持
- 增量备份：按文件大小和修改时间比对，只上传变化文件
- 小文件友好：发送端支持多文件并发上传
- 大文件友好：HTTP 模式下大文件默认按 64MB 分片并发上传，接收端先写临时文件再原子替换；本机镜像下大文件为单文件流式复制，同样先写 `.uploading` 再替换
- 可选删除接收端多余文件
- 定时调度
- 进程守护：默认双模式后台运行可直接添加 `-d`，单独运行可用 `sender -d` 或 `receiver -d`
- MVC 分层：`model`、`controller`、`view`，同步和守护逻辑在 `service`

## 接收端运行

运行参数放在当前目录的 `config.toml`。配置文件中的路径统一使用 `/`，Windows 盘符也写成 `E:/filesync`，不要写需要转义的 `\\`。首次启动如果文件不存在，程序会生成默认配置；如果 `receiver.token` 为空，也会自动生成并写回配置。接收端示例：

```toml
[receiver]
listen = ":1987"
storage_root = "C:/Users/amov/.filesync/storage"
token = "your-secret-token"
allowed_dirs = ["E:/filesync"]

[daemon]
pid_path = "C:/Users/amov/.filesync/filesync-receiver.pid"
log_path = "C:/Users/amov/.filesync/filesync-receiver.log"
```

前台运行：

```bash
go build -o filesync .
./filesync receiver -c config.toml
```

后台守护运行：

```bash
./filesync receiver -c config.toml -d
```

接收端路径规则：

- 发送端 `destination_path` 是相对路径时，会写入 `storage_root` 下。
- 发送端 `destination_path` 是绝对路径时，必须位于 `allowed_dirs` 配置的目录内，否则返回 400。
- 文件自身路径仍然只能是相对路径，不能通过 `../` 逃逸目标目录。

例如 Windows 接收端允许写入 E 盘：

```toml
[receiver]
storage_root = "C:/Users/amov/.filesync/storage"
allowed_dirs = ["E:/filesync"]
```

发送端任务可以写：

```toml
destination_path = "E:/filesync/mac/rust-web-learning"
```

## 本机目录镜像（不经过网络）

将源目录同步到本机另一盘或路径下的镜像目录（`destination_path` 必须为**绝对路径**）。比对规则与 HTTP 接收端一致：普通文件按大小与修改时间（约 1 秒容差）；非普通文件（如符号链接）仍会跳过，与扫描端行为一致。

**命令行一次性执行**（不写长期配置）：

```bash
go build -o filesync .
./filesync local -from D:/data/project -to E:/backup/project -workers 32
./filesync local -from D:/data -to E:/mirror/data -delete-extraneous -excludes "*.tmp,.git/,node_modules/"
```

`mirror` 与 `local` 等价。`-excludes` 为英文逗号分隔，规则与任务里的 `excludes` 相同。

**发送端 Web**：在任务中设置同步方式为“本机目录”，`receiver_url` 可留空，`destination_path` 填写本机绝对路径。其余字段（`local_path`、`excludes`、`delete_extraneous`、`upload_workers` 等）与 HTTP 任务相同。

限制简述：`destination_path` 须为绝对路径且不得与 `local_path` 重叠或互为子目录；符号链接等非普通文件不复制；路径与权限行为随操作系统而定。

## 发送端运行

在需要备份的服务器上运行：

发送端配置示例：

```toml
[sender]
listen = "127.0.0.1:1985"
web_token = ""
db_path = "filesync.db"
scheduler_seconds = 30

[daemon]
pid_path = "/Users/zy/.filesync/filesync-sender.pid"
log_path = "/Users/zy/.filesync/filesync-sender.log"
```

前台运行：

```bash
go build -o filesync .
./filesync sender -c config.toml
```

打开：

```text
http://localhost:1985
```

如果要让 Web 控制台监听非本机地址，必须设置 Web 密码：

```toml
[sender]
listen = ":1985"
web_token = "your-web-password"
```

浏览器会弹出 Basic Auth 登录框，用户名可任意填写，密码为 `web_token` 的值。

后台守护运行：

```bash
./filesync sender -c config.toml -d
```

## 同时运行发送端和接收端

默认无子命令就是双模式，同一进程会同时运行 sender 和 receiver。配置文件中同时设置 `[sender]` 和 `[receiver]` 即可：

```toml
[sender]
listen = "127.0.0.1:1985"
web_token = ""
db_path = "filesync.db"
scheduler_seconds = 30

[receiver]
listen = ":1987"
storage_root = "E:/filesync"
token = "your-secret-token"
allowed_dirs = ["E:/filesync"]

[daemon]
pid_path = "C:/Users/amov/.filesync/filesync.pid"
log_path = "C:/Users/amov/.filesync/filesync.log"
```

前台运行：

```bash
./filesync -c config.toml
```

后台运行：

```bash
./filesync -c config.toml -d
```

新增任务时填写：

- 同步方式：HTTP 接收端，或本机目录（目标为绝对路径）
- 本地目录：例如 `/data/app`
- 接收端地址：例如 `http://10.0.0.8:1987`（本机目录模式可留空）
- 接收端存储目录 / 本机目标目录：例如 `server-a/app`，或本机模式下 `E:/backup/app` 等绝对路径
- 认证令牌：与接收端 `receiver.token` 一致（本机目录模式可留空）
- 上传并发数：默认 `32`，小文件很多时可以适当调大

## 查看同步状态

发送端 Web 页面会显示每个任务的实时状态：

- 当前阶段：准备同步、扫描文件、比对接收端（本机任务为「比对目标目录」）、上传文件（本机任务为「复制文件」）、清理多余文件、同步完成
- 当前文件：正在上传或刚上传完成的文件路径
- 进度：已上传文件数 / 需要上传文件数
- 数据量：已上传容量 / 需要上传容量
- 上传网速：按本次任务已经发送的数据量估算
- 预计剩余时间：按当前平均上传速度估算
- 已扫描文件数
- 最近状态更新时间
- 失败原因

有任务正在运行时，页面会每 3 秒自动刷新一次。任务配置和运行状态会写入 SQLite 数据库 `sender.db_path`，例如 `current_step`、`current_file`、`uploaded_files`、`uploaded_bytes`、`upload_speed_bps`、`eta_seconds` 等字段。

## 磁盘写满时的行为

接收端写入目标磁盘失败时不会退出进程，会返回 HTTP 500 给发送端，发送端任务会标记失败并显示错误原因。整文件上传、分片上传和分片合并失败时都会尝试删除对应的 `.uploading` 临时文件；分片上传失败后，发送端还会调用接收端的中止接口再次清理临时文件。

程序不会主动扩容或暂停等待磁盘释放空间，生产环境仍应给接收端存储目录配置磁盘容量监控和告警。

## 守护进程

发送端：

```bash
./filesync sender -c config.toml -d
./filesync status -c config.toml
./filesync stop -c config.toml
```

接收端：

```bash
./filesync receiver -c config.toml -d
./filesync status -c config.toml
./filesync stop -c config.toml
```

默认双模式：

```bash
./filesync -c config.toml -d
./filesync status -c config.toml
./filesync stop -c config.toml
```

不传 `-c` 时，默认加载程序启动时工作目录下的 `config.toml`，并在守护进程启动时固定为绝对路径。后台启动双模式直接使用 `-d`；只想单独运行一端时使用 `sender -d` 或 `receiver -d`。

## TOML 配置方法

`config.toml` 只保存运行参数，不保存 Web 页面里的同步任务。页面新增、编辑、运行状态都保存在 SQLite 数据库 `sender.db_path` 中，默认是当前目录下的 `filesync.db`。

启动并指定配置文件：

```bash
./filesync sender -c /etc/filesync/config.toml
```

完整示例：

```toml
[sender]
listen = "127.0.0.1:1985"
web_token = ""
db_path = "filesync.db"
scheduler_seconds = 30

[receiver]
listen = ":1987"
storage_root = "/backup/filesync"
token = "your-secret-token"
allowed_dirs = ["/backup/filesync", "/mnt/backup"]

[daemon]
pid_path = "/var/run/filesync.pid"
log_path = "/var/log/filesync.log"
```

字段说明：

- `sender.listen`：发送端 Web 监听地址。
- `sender.web_token`：Web 控制台密码。监听公网地址时必须设置。
- `sender.db_path`：SQLite 数据库路径，保存页面任务和运行状态。
- `sender.scheduler_seconds`：调度器检查间隔。
- `receiver.listen`：接收端 API 监听地址。
- `receiver.storage_root`：相对 `destination_path` 的默认存储根目录。
- `receiver.token`：接收端 Bearer Token。留空时程序会自动生成并写回配置；发送端任务里的认证令牌必须与它一致。
- `receiver.allowed_dirs`：允许绝对 `destination_path` 写入的目录列表。
- `daemon.pid_path`：守护进程 PID 文件。
- `daemon.log_path`：守护进程日志文件。

## 跨平台编译

编译 Linux x64、Windows x64 和当前 Mac 平台：

```bash
./scripts/build_cross.sh
```

输出文件：

```text
dist/filesync-linux-amd64
dist/filesync-windows-amd64.exe
dist/filesync-darwin-当前架构
```

## 参数简写

- `local` / `mirror`：本机目录镜像子命令，使用独立参数 `-from`、`-to` 等（见上文）。
- `-c config.toml`：运行配置文件；不指定时默认使用启动时工作目录下的 `config.toml`。
- `-d`：以守护进程方式启动，默认双模式示例为 `./filesync -c config.toml -d`。

监听端口、token、Web 密码、存储目录、数据库路径、PID 和日志路径都改为配置文件字段。

## API

接收端提供以下接口：

- `GET /health`
- `POST /api/v1/manifest`：比对文件清单，返回需要上传的文件
- `PUT /api/v1/files`：接收单个文件流
- `PUT /api/v1/chunks`：接收分片
- `POST /api/v1/complete`：合并分片
- `POST /api/v1/abort`：中止并清理临时分片
- `POST /api/v1/prune`：删除接收端多余文件

接收端会使用 `config.toml` 里的 `receiver.token` 校验请求；如果该字段为空，启动时会自动生成。发送端请求会使用 `Authorization: Bearer <token>`。
