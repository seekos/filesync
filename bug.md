---

## 安全问题

**1. Receiver 端允许绝对路径写入（高危）**

[fsmirror.go:32](internal/fsmirror/fsmirror.go:32) — `ReceiverDestinationBase` 当 destination 是绝对路径时直接返回，意味着远程 sender 可以指定任意绝对路径（如 `/etc`、`/root/.ssh`）作为写入目标。只要 token 泄露或未配置 token，攻击者可以覆盖系统任意文件。

**2. Token 认证是可选的且明文传输**

[receiver_controller.go:254](internal/controller/receiver_controller.go:254) — token 为空时直接放行所有请求。没有 TLS 强制要求，token 在 HTTP header 中明文传输。配置文件中 `auth_token` 也是明文存储。

**3. Web 控制台无任何认证**

[web_controller.go:39](internal/controller/web_controller.go:39) — sender 的 Web 管理界面没有任何认证机制，默认监听 `:8080`，任何能访问该端口的人都可以创建/删除/触发同步任务。

**4. 无请求体大小限制**

[receiver_controller.go:107](internal/controller/receiver_controller.go:107) — upload 接口直接 `io.CopyBuffer(out, r.Body, ...)` 没有限制 body 大小，可被用于磁盘耗尽攻击。

---

## 并发与数据一致性问题

**5. 配置文件频繁写入导致 I/O 瓶颈**

[toml_store.go:91](internal/repository/toml_store.go:91) — 每次 `UpdateTask` 都会序列化整个配置并写磁盘。上传进度更新（每 1.5 秒一次 × 多个 worker）会产生大量磁盘写入。高并发时这是性能瓶颈。

**6. Scheduler 可能重复触发任务**

[scheduler.go:48](internal/service/scheduler.go:48) — `runDue` 遍历任务列表后用 goroutine 启动任务，但 `LastRunAt` 的更新发生在 `Run` 内部。如果 scheduler tick 间隔（30s）内任务还没来得及更新 `LastRunAt`，下一次 tick 可能再次触发同一任务。虽然 `running.LoadOrStore` 能防止真正的重复执行，但会产生不必要的错误日志。

**7. 分块上传无原子性保证**

[receiver_controller.go:129](internal/controller/receiver_controller.go:129) — 多个 chunk 并发写入同一个 `.uploading` 临时文件，使用 `Seek` 定位偏移。如果某个 chunk 失败，文件处于半写状态，没有清理机制。重试时也没有校验已写入 chunk 的完整性。

---

## 功能缺陷

**8. 没有校验和验证**

文件比对仅依赖 size + mtime。如果文件内容变了但 size 和 mtime 恰好相同（某些工具会保留 mtime），变更会被漏掉。没有 checksum 机制。

**9. 符号链接处理不完整**

[sync_runner.go:439](internal/service/sync_runner.go:439) — `scanFiles` 只处理 `IsRegular()` 的文件，符号链接被静默跳过。如果源目录包含重要的符号链接，同步后目标端会缺失这些文件，且没有任何警告。

**10. 排除规则不支持目录递归匹配**

[sync_runner.go:461](internal/service/sync_runner.go:461) — `excluded` 函数只做简单的 `filepath.Match`，不支持 `**` 通配符（如 `**/node_modules`）。用户可能期望 gitignore 风格的匹配但实际不生效。

**11. go.mod 没有依赖管理**

[go.mod](go.mod) — 只有 module 声明和 go 版本，没有任何第三方依赖。这意味着所有功能都是手写的（包括 TOML 解析器），增加了 bug 风险。自定义 TOML 解析器不支持标准 TOML 的很多特性（嵌套表、多行字符串等）。

---

## 可靠性问题

**12. 错误被静默忽略**

多处 `_ = os.Chmod(...)`, `_ = os.Chtimes(...)`, `_ = r.store.UpdateTask(...)` 忽略了错误。特别是 `UpdateTask` 失败意味着进度信息丢失，用户在 Web 界面看到的状态可能是过时的。

**13. Daemon 进程管理粗糙**

[daemon_unix.go:77](internal/service/daemon_unix.go:77) — Stop 只发 SIGTERM 后立即删除 PID 文件，不等待进程实际退出。Restart 只 sleep 500ms 就启动新进程，如果旧进程还没释放端口会导致启动失败。

**14. HTTP server 缺少超时配置**

[main.go:203](cmd/filesync/main.go:203) — 只设置了 `ReadHeaderTimeout`，没有 `ReadTimeout`、`WriteTimeout`。大文件上传时虽然不能设太短，但完全没有 write timeout 意味着慢客户端可以无限占用连接。

---

## 总结

最需要优先修复的是安全问题：receiver 端的绝对路径写入和 Web 控制台无认证。这两个问题组合起来意味着，如果 filesync 暴露在网络上，任何人都可以向目标机器写入任意文件。