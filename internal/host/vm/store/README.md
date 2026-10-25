# VM SQLite 存储

本包保存自己的记录类型，不导入父包 `vm`，避免 Manager 导入存储时形成包循环。使用 `modernc.org/sqlite`，不要求 SQLite CGO 驱动；race 仍需要宿主 C 工具链。

- `vms`：配置、后端、最近观察状态、当前 epoch、未决操作。
- `runs`：每次运行和可空真实退出结果，VM 删除时通过外键级联删除。
- `epochs` 和 SQLite 自增序列：分配跨资源销毁和数据库重开的 epoch。
- `metadata`：数据库不可变的后端资源身份绑定。

数据库单连接，启用 foreign_keys、WAL 和 synchronous=FULL。数据库主文件使用非阻塞独占 flock；拒绝符号链接及硬链接数据库入口，不支持复制运行中的数据库或删除运行中的数据库文件。数据库目录由 Host 管理，不提供多活租约或远程存储。
