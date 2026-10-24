# 2026-10-21 五个业务镜像更新

目标：`https://192.168.102.68:6443`，namespace `ani-system`。

用户确认范围：Model、模型导入 Worker、Inference、Governance、Console；平台组件保持版本。

批次标签：`release-20261021-1141`。

| 组件 | 镜像 | Registry digest |
|---|---|---|
| model | `docker.changqingyun.cn/ani/model-service:release-20261021-1141` | `sha256:0e734efe73a54963c17a5491d334009e0a21eb7717150dfa17c7fa1cdbf9c092` |
| import | `docker.changqingyun.cn/ani/model-import-worker:release-20261021-1141` | `sha256:399bce06e827490c253457b07fd95d207421e60e4646f0ab0fcae194b1467968` |
| inference | `docker.changqingyun.cn/ani/inference-service:release-20261021-1141` | `sha256:da29110177591bfa6a3ab01a096b7c3d0f2a0356c2e415429a69a970e6251932` |
| governance | `docker.changqingyun.cn/ani/governance:release-20261021-1141` | `sha256:0777883c43e2eb8328017e2bf1bbb2120fb2337e11666767fb32adcf964edfbc` |
| console | `docker.changqingyun.cn/ani/ani-console:release-20261021-1141` | `sha256:cbe9fc6c47793a7cdc10cea7dc65123dfb89964dbadf068555815bb0e8f1dc3f` |

## 构建与验证

- 从当前工作区源码重新编译 Model、导入 Worker、Inference、Governance；在独立临时目录构建 Console。
- 后端使用 `CGO_ENABLED=0`、`-trimpath`。临时 runtime Dockerfile 保留仓库原有的二进制路径、配置路径、用户及入口。
- Model/Governance 使用固定 digest 的 distroless nonroot 基础镜像；Inference 保留 scratch + UID/GID 65534。
- 导入 Worker 基于集群 Model 正在使用的 `aggregate-a6c9c4d-20261017` 镜像 digest 更新 Go 程序，保留 Python/provider 依赖；非 root 探针显示 UID/GID 65534，modelscope 1.40.2、huggingface_hub 2.2.0。
- Model 的工作区许可证文件原先已删除。本次仅在临时构建目录通过 `git show HEAD:THIRD_PARTY_NOTICES.go-kratos-layout.txt` 补入镜像，未恢复或覆盖用户的删除。
- `make verify`（Model）：通过。
- Governance 的 server/service/data 包 Model/Inference 相关测试与编译：通过。
- Inference 使用本轮已通过全量测试/vet/build 的 Volcano 与非 root 源码，重新编译并打包。
- Console：Corepack 按 packageManager 选择 pnpm 8.6.11；lint、typecheck、production build 通过。通过 `COREPACK_DEFAULT_TO_LATEST=0` 避免旧 Corepack 无关的 latest 元数据签名问题，未关闭签名校验。未修改前端应用源码。
- 三个 Chart 的 Helm lint/template 通过。

## 部署方式

- 升级前保存 Helm values/manifest 与 Deployment 快照。三个后端通过 `helm upgrade --atomic --wait` 升级，Console 只更新现有 Deployment 镜像，保留当前节点与代理配置。
- 保留各 Certificate 的实际集群 duration/renewBefore；未顺带应用工作区的其他证书策略更改。
- 已有 Governance 数据库关闭 `bootstrap.adminInit.enabled`，避免在升级时重复首次管理员初始化；数据库迁移按已有版本化 hook 执行。
- Model 的 `ANI_IMPORT_JOB_IMAGE` 与 Inference 的 `ANI_MODEL_MATERIALIZER_IMAGE` 同时更新为本批次 Worker。

原始日志、release 快照及联调证据：`/tmp/ani-business-images-20261021/`。认证材料仅保存在受限临时目录，不加入仓库。

## 集群验收

- Model Helm revision 18、Inference revision 38、Governance revision 33 均为 `deployed`。
- Console 使用现有 Deployment 完成镜像滚动更新；四个常驻 Deployment 均 `1/1 Ready`，镜像标签全部为本批次。
- 已检查 Model 的 `ANI_IMPORT_JOB_IMAGE` 与 Inference 的 `ANI_MODEL_MATERIALIZER_IMAGE`，均引用本批次 Worker。
- Console NodePort `192.168.102.68:30081` 首页与三个入口静态资源均 HTTP 200；通过其 `/api/` 代理执行 CSRF、登录刷新、模型列表、推理列表均成功。
- 通过 Console → Governance 创建真实服务 `release-cpu-20261021-1141`，HTTP 202。
  服务 ID `7e2667c8-8553-4bf9-8b8f-6a24dd2f6901`，创建 operation `24c6495e-c1f2-4367-9396-a12292a41bc9`。
- 物化 Job `ani-model-19d58891c50234f91d4f17d2-fetch` 使用新 Worker 镜像，39 秒内完成，实际 UID/GID 65534。
- predictor `release-cpu-20261021-1141-predictor-69bcc8fc7f-5vb4f` 的 `schedulerName: volcano`，
  Scheduled 事件来源 `volcano`，分配到 `ani-k8s-worker-2`。
  容器内实际 UID/GID 为 65534、`CapEff=0`、`NoNewPrivs=1`、`Seccomp=2`，未挂载 ServiceAccount token。
- 创建 operation `succeeded`，服务 `ready/published`，predictor `1/1 Running`、零重启。
- Higress `192.168.102.68:30090/v1/completions` 使用既有 API key，按 JSON model 自动路由，
  返回 HTTP 200，completion ID `cmpl-f7cd9cee-faf0-48f9-96b7-c8bbee67db2e`，响应模型名为 `release-cpu-20261021-1141`，
  文本为 `Paris. The capital of Italy is Rome. The capital of Spain is Madrid.`。
- 通过 Console/Governance 查询推理列表 HTTP 200；携带有效 API key 请求 Higress 的 `/api/v1/inference/services` 返回 404。管理与推理入口分别验证。
- 验收服务删除 operation：`d156fa35-5078-4fff-9a1b-d35450f84c32`。
  最终 `succeeded/complete`；按 service-id 查询 CR、Deployment、ReplicaSet、Pod、Service、Job、PVC、下载 Secret 均无残留，
  Volcano PodGroup 列表为空，模型专属 Higress route/Service Source 已清理，Governance 推理列表恢复为空。
- 最终四个业务 Deployment 均 `1/1 Ready`；新镜像保持部署，验收临时服务已删除。

## 验证边界

本轮验证四个常驻服务部署、Worker 实际物化任务、Console 首页与入口资源及其 API 代理、
CPU 推理端到端链路。未逐页进行前端交互验收，也未执行新的外部 provider 模型导入；
Worker Go 程序已重新编译，实际集群任务验证的是 Inference 物化用途。
集群没有 GPU，未做 GPU 实跑；原有 `health=unknown` 观测语义仍保留。
