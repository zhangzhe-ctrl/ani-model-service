# Inference → Model.GetModelVersion 实际联调

2026-09-19，C1–C8 最终全部 **pass**。基线为
`a7437c954d9ed92367471b0f2cae49031482ca7e`，开始时工作区干净。

本仓库变更：

- `internal/server/governance.go`：在现有 mTLS 边界增加固定 Inference 身份，仅允许
  GetModelVersion。Governance 仍只允许 ListModels；其他身份、业务方法及流式 RPC 拒绝。
  从验证证书确定身份；Inference Actor 固定 `service:ani-inference-service`，不采信外部 Actor。
  tenant/request ID 必须单值、规范、非零 UUID，请求租户必须与 metadata 一致。
- `cmd/ani-model-service/catalog.go`：复用 PostgreSQL VersionStore，保持只读装配，不启动
  Storage、MinIO、导入或下载 worker。独立查询账号仅三表 SELECT。
- `internal/service/model.go`：GetModelVersion 的存储故障返回 Unavailable，保留原始日志；
  不再把数据库连接超时误报为 NotFound。不存在资源仍返回 NotFound。
- 增补接收规则、catalog 装配和存储错误分类的定向测试；更新 catalog 文档。

Fedora 任务：`/home/chabking/ani-inference-runs/mtls-20260919-01`。
集群 `.10/.11/.12`，namespace `inf-model-mtls-20260919`，Model/PG 服务与 PVC 均保留。
既有 Governance 资源未更改。没有推送、合并、tag 或生产切换。

| 验收 | 状态 | 结果 |
| --- | --- | --- |
| C1 | pass | cert-manager v1.21.2 实际签发证书，链/SAN/EKU 正确并成功握手 |
| C2 | pass | Inference 正式客户端 A/B 读取，响应与独立 PostgreSQL 导出全字段一致 |
| C3 | pass | 跨租户/不存在版本无数据，请求租户不一致拒绝 |
| C4 | pass | 无证书、外部 CA、错误服务身份分别拒绝 |
| C5 | pass | Inference 非授权读写方法拒绝；Governance ListModels 成功，GetModelVersion 拒绝 |
| C6 | pass | tenant/request ID 缺失、重复、非法拒绝；伪造 Actor 不能改变权限/身份 |
| C7 | pass | Model 停止约 3 秒、PG 停止约 1 秒内失败；恢复后原 PVC 数据仍可读取 |
| C8 | pass | 两端叶证书重签，Secret/序列号改变；RollingUpdate 后新握手和读取成功 |

定向验证：`TestGovernanceMTLSBoundary`、`TestInferenceReceiverMatrix`、
`TestCatalogReadinessLiveProbe`、`TestGetModelVersion*`、`TestCatalogListContract`、
`TestCatalogStartupRequiresIdentityConfiguration`、`TestCatalogWiresVersionReader` 均 pass。
Model 实际入口构建 pass。所有操作均在 Fedora、私有 Go 缓存、`GOWORK=off` 下完成。
根据本次用户明确指令覆盖 AGENTS.md 的全量门禁：未运行 make verify/make test/go test ./...、
全量 race/lint 或前端测试，均 **not_verified**。

Model 二进制 SHA-256：`d1aefc27048b813d3e0515946ea88f511d1dbd7cd3ad465e2046f8237e9c7b52`。
镜像：`ani-model:inf-model-mtls-20260919-d1aefc27048b`，
摘要 `sha256:daadd73c899b444c8b21be50e3da4c9b58f6ac323b298d709e8ef8d91953e2b1`。
源码摘要、最终 Git 提交及完整执行日志在任务 `evidence/`。

复现命令和完整脱敏交付记录位于 Inference 仓库：
`docs/inference-model-mtls-20260919.md`、`scripts/model-mtls/README.md`、
`docs/evidence/model-mtls-20260919/`。Fedora 中对应 `work/inference/`。

中间失败（构造器类型、Podman 归档选项、短 DNS 首次超时、PG 错误分类）均保留原记录，
修复后执行对应重验；最终无 C1–C8 遗留失败。旧失败 Job 未删除。

Model 明确信任平台 Inference 断言租户，不重验人的 Membership。跨租户负例只证明
租户查询边界，不抵抗持有合法 Inference 凭据的恶意服务。本批仅证明服务身份出站调用，
不代表 Inference 用户入口、创建或推理运行已完成。自然续期、CA 轮换、HA、压测未验证。
