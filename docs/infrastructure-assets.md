# 服务器资产版部署与升级

后端基于哪吒 Dashboard v2.6.2，管理界面基于 admin-frontend v2.6.1。资产独立存储在 Dashboard 的 SQLite，使用现有 Agent、监控快照、通知组和鉴权服务。现有 Agent 可以继续使用原连接地址和凭据。

## 构建并启动 Docker 镜像

将两个 Fork 的对应开发分支放在相邻目录中。镜像构建器会编译管理前端、下载上游的访客主题，并把它们与 Dashboard 一起嵌入。Docker Buildx 需要能访问 npm、Go 模块代理、Docker Hub 和 GitHub Releases。

```bash
cd ~/Project/nezha
docker buildx build --load \
  --build-context admin_source=../admin-frontend \
  -f Dockerfile.infrastructure \
  -t nezha-infrastructure:v2.6.2 .
```

Linux 构建机默认生成自身架构的镜像。跨架构时加 `--platform linux/amd64` 或 `--platform linux/arm64`，构建机需要支持相应的执行环境。可用 `docker save` 与 `docker load` 将本地镜像搬到目标机器。和上游 Release 一样，可用 BuildKit secret 嵌入 IPInfo 国家数据库。在主机上设置 `IPINFO_TOKEN` 后追加 `--secret id=ipinfo_token,env=IPINFO_TOKEN`，或者在构建前将已有的兼容 MMDB 放到 `pkg/geoip/geoip.db`。未提供数据库时源码附带占位文件，GeoIP 国家归属不可用。资产的云地域和可用区取自台账与云 API。

新实例的 Compose 示例。先在本机完成初始化和修改默认管理员密码，再接入现有 HTTPS 反代或开放监听。

```yaml
services:
  dashboard:
    image: nezha-infrastructure:v2.6.2
    restart: unless-stopped
    ports:
      - "127.0.0.1:8008:8008"
    volumes:
      - ./data:/dashboard/data
    # 配置阿里云同步时启用下列两项，凭据文件挂载放在同一个 volumes 列表下。
    # environment:
    #   NZ_ASSET_CLOUD_CONFIG: /run/secrets/nezha-cloud.yaml
    # volumes 条目
    #   - ./secrets/nezha-cloud.yaml:/run/secrets/nezha-cloud.yaml:ro
```

`docker compose up -d` 后，管理前端路径为 `/dashboard/assets`。对接生产流量时复用上游 Dashboard 的反代配置和 Agent 所需的 gRPC 转发。

## 现有安装升级与回退

1. 构建新镜像，记录旧镜像标签或 digest。暂停旧 Dashboard，完整备份它已有的 `/dashboard/data`，包括 SQLite、配置、时序库及已安装主题。如果配置或数据库设在其他挂载中，一并备份。云凭据文件单独备份，保留文件权限。例如现有数据是当前目录的绑定挂载，可在停止容器后执行 `tar -czf nezha-data-before-assets.tar.gz data`。
2. 在原 Compose 服务中将 `image` 换成新镜像，保留原挂载、服务端口、反代和运行参数。用 `docker compose up -d --no-deps dashboard` 重建该服务，并检查容器日志。首次启动由 GORM 在原 SQLite 中添加资产、续费、账单、提醒和同步状态表。上游可能随版本执行原有迁移。
3. 在后台保留或选回内置 `OfficialAdmin`（`admin-dist`），然后确认原服务器仍在线、原服务探测和告警可用，并打开“服务器资产”。自定义管理主题会使用其自身前端，需要切回内置主题才能看到资产页面。
4. 需要回退时停止新服务，将当前数据目录另存以保留升级后的资产和账单，再恢复升级前的完整备份和旧镜像，启动旧服务。恢复点之后的写入仍留在另存的数据目录。避免多个 Dashboard 同时写同一个 SQLite/时序库。

## 台账和历史数据

- 在资产页面录入云实例、裸金属、ESXi 宿主机或虚拟机。可从原服务器列表的“服务器资产”操作打开对应台账、关联 Agent，或将旧公开备注中的已知采购与网络套餐字段复制到私有台账。旧公开备注依旧按上游规则展示。若此前填过敏感字段，请在检查导入结果后编辑相应的公开备注。
- 一个 Agent 可关联一条资产，一条资产可单独存在。分组使用现有服务器组，台账筛选也识别 Agent 原有组。用上级资产将物理机、ESXi 和虚拟机关联。成员访问需要同时拥有资产及其关联节点。云同步默认将新增资产交给第一个管理员，也可在账号配置中指定资产所有者。已有资产匹配同步时保留它原有的所有者。
- 厂商下发的到期时间优先用于包年包月实例。开启自动续费只表示厂商的续费设置，实际到期时间延长后才反映在到期列表。人工续费记录可按付款日期查询，并向后更新人工记录的到期时间。
- 在资产中选择其所有者名下的哪吒通知组，并设置阈值，例如 `30, 15, 7, 0`。Dashboard 每小时第 10 分钟检查。每个到期时间、通知组与阈值的成功记录保存在 SQLite，重启后继续去重。错过多个阈值会合并发送，失败的发送在后续周期重试。
- 费用按厂商、项目、币种生成独立行。实际云账单列显示阿里云的原价 `PretaxGrossAmount` 和应付 `PretaxAmount`，金额精确到小数点后六位。人工采购和续费依时间范围汇总。不同来源可能描述同一笔费用，用对应来源做核算。月付、年付的估算按录入的周期归一化，日、周按 365 天估算，估算反映当前配置。人工月年边界按 UTC，阿里云账期沿用厂商账期。历史账单根据资产当前项目归组。

## 阿里云 ECS 和费用同步

建立专用 RAM 用户或使用带会话令牌的 RAM 角色凭据。每个主账号可配置自己的地域和账单开关。云配置由进程从私有 YAML 文件加载，配置变更后重启 Dashboard。文件权限使用 `0600` 或 `0400`，进程须可读。容器中只读挂载到 `NZ_ASSET_CLOUD_CONFIG` 指向的位置。该文件与 Dashboard 配置和 SQLite 分开存放。

```yaml
accounts:
  - provider: aliyun
    account: "1234567890123456"
    label: production
    regions: [cn-hangzhou, cn-shanghai]
    # owner_user_id: 1
    interval_minutes: 360
    billing_site: china
    billing_months: 12
    access_key_id: "<RAM AccessKey ID>"
    access_key_secret: "<RAM AccessKey Secret>"
    # security_token: "<STS token>"
```

RAM 自定义策略示例。账单关闭时可以移除第二条。可按 RAM 官方资源类型对地域、实例和账号进一步约束。

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ecs:DescribeInstances",
        "ecs:DescribeDisks",
        "ecs:DescribeInstanceAutoRenewAttribute"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": "bssapi:DescribeInstanceBill",
      "Resource": "*"
    }
  ]
}
```

首次启动会尝试同步。ECS 通过 STS 校验实际主账号，随后按配置地域分页读取实例、挂载盘和自动续费设置。资产身份由厂商、主账号 ID、地域与实例 ID 组成。可提前在同一所有者的人工资产填写完整身份以便合并。若对应的人工资产由其他用户持有，同步状态会报告冲突，暂不挂载云配置或账单。同步只更新云快照。云清单查不到的已同步实例会标记缺失，保留台账记录。删除云资产时会记住该实例的删除，以免下次同步重新创建。

`billing_months: 0` 关闭费用同步，1–18 配置回溯账期。首次和手动同步获取设定范围，日常刷新当月和上月。账单只归属到该账号下能明确匹配的 ECS 实例。独立 EIP、独立云盘、共享产品和没有匹配资产的账单请在阿里云费用中心查看。账单接口存在延迟，当月应付以结算后的账单为准。“云同步”页面可按账号查看资产和账单最后成功时间、失败原因、运行状态，并手动触发同步。

- 阿里云官方接口。[ECS](https://api.aliyun.com/document/Ecs/2014-05-26/overview) · [实例账单](https://help.aliyun.com/en/user-center/developer-reference/api-bssopenapi-2017-12-14-describeinstancebill)
- 莱卡云公开支持页面提供控制台操作和工单指引。台账支持人工录入及续费记录。后续自动接入需要厂商提供的实例和账单 API 文档及授权。[莱卡云服务与支持](https://www.lcayun.com/support.html)
- ESXi 目前使用人工关联。接入 vSphere 自动发现时可沿用上级资产关系。

## REST、MCP 与 MausBot

REST 路径在 `/api/v1/asset` 下。通用响应为 `{ "success": true, "data": ... }`，业务错误使用现有的 `success: false` 与 `error`。浏览器使用原有登录和 CSRF。程序使用 `Authorization: Bearer <PAT>`。接口请求和返回结构可在构建时生成的 Swagger 中查看，面板开启 Debug 后可通过 `/swagger/index.html` 访问。

| REST | PAT scope |
| --- | --- |
| GET `/asset`、`/asset/{id}`、`/asset/{id}/history`、`/asset/costs` | `nezha:asset:read` |
| POST `/asset`、PUT `/asset/{id}`、POST `/asset/{id}/renewals`、POST `/asset/import/{serverID}` | `nezha:asset:write` |
| DELETE `/asset/{id}` | `nezha:asset:delete` |
| GET `/asset/cloud` | `nezha:cloud:read` |
| POST `/asset/cloud/{provider}/{account}/sync` | `nezha:cloud:sync` |

列表支持 `q`、`provider`、`region`、`purpose`、`project`、`status`、`billing`、`tag`、`group_id`、`server_id`、`expiry_days`、`issue`、`sort`、`limit` 和 `offset`。例如查未来 15 天到期的资产，用 `GET /api/v1/asset?expiry_days=15&sort=expiry`。费用接口接受 `period=2026-11` 或 `period=2026`，以及可选的 `provider` 与 `project`。

在设置中启用 MCP，将 MausBot 指向 `https://<dashboard-host>/mcp`。给日常问答 Bot 单独创建带有效期的 `nezha:asset:read` PAT。其工具包括 `asset.list`、`asset.get`、`asset.expiring`、`asset.costs`。用 `asset.list` 的 `issue=offline`、`issue=alert` 或 `issue=expired` 查询 Agent 掉线、现有规则判定的活动告警或过期资产。旧 MCP 的 `server.list` 可继续枚举监控节点，包括尚未建立资产的节点，需要原有 `nezha:inventory:read`。

设置了服务器白名单时，PAT 只能访问白名单内 Agent 关联且拥有访问权的资产。要读取无 Agent 的资产，使用相应资产所有者的未设服务器白名单的 PAT。用 `cloud.status` 查看同步状态需管理员的 `nezha:cloud:read`，用 `cloud.sync` 手动同步另需 `nezha:cloud:sync`。云账号工具要求管理员且无服务器白名单。保留原有终端、文件和远程命令的单独 Scope。OAuth 客户端也可在用户同意后请求 `nezha:asset:read`。
