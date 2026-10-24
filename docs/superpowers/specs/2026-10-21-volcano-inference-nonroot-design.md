# Volcano 与推理非 root 运行时设计

**日期：** 2026-10-21  
**范围：** `/root/kubercon/ani-inference-service`

本文件为最初设计副本。实施与验证以 Inference 仓库的
`docs/superpowers/specs/2026-10-21-volcano-inference-nonroot-design.md` 和
`docs/execution/records/2026-10-21-volcano-inference-nonroot.md` 为准。

## 目标

在当前新集群中使用独立安装的 Volcano，并让 Inference 创建的所有运行工作负载以非 root 身份运行。完成后通过真实的 CPU 推理链路验证模型物化、KServe 资源、Pod 启动、Higress OpenAI 调用和删除清理。

## 当前状态

- 集群上下文是 `kubernetes-admin@kubekey`，API Server 为 `https://192.168.102.68:6443`。
- Volcano Helm release `volcano` 已安装到 `volcano-system`，版本为 `1.12.1`。
- Volcano 镜像使用 `docker.changqingyun.cn/mirror/volcanosh/*:v1.12.1`，三个核心 Deployment 已就绪。
- 集群目前没有 GPU 节点；GPU 只能验证计划渲染和 Volcano 控制面，不能宣称真实 GPU 调度通过。
- 现有代码依赖 `volcano.sh/apis v1.12.1`。

## 运行时与调度边界

根据用户在 2026-10-21 的明确要求，CPU 和 GPU 推理统一使用 `schedulerName: volcano`。
CPU 使用内置 `default` 队列；GPU 必须保留 Accelerator 有效执行计划的队列、资源和设备注解。
模型下载 Job 与控制面继续默认调度。实施与限制详见 Inference 仓库同名设计及执行记录。

Volcano 作为独立平台组件管理，不加入 `ani-inference` 业务 Chart。Chart 与控制面只做前置检查和使用已有运行时能力。Volcano chart 当前生成的配置名是 `volcano-scheduler-configmap`；Governance 约定的 `ani-volcano-scheduler-configmap` 属于后续治理配置兼容项，本次不通过安装脚本或隐式 ConfigMap 别名处理。

## 非 root 安全合同

Inference 创建的下列工作负载统一使用同一安全策略：

1. KServe `InferenceService` predictor；
2. KServe `LLMInferenceService` 的 leader 和 worker；
3. 旧 Deployment 兼容渲染路径；
4. 旧 LeaderWorkerSet 兼容渲染路径；
5. 模型物化下载 Job；
6. Inference 控制面 Deployment 的容器级安全上下文。

Pod 级安全上下文：

```yaml
runAsUser: 65534
runAsGroup: 65534
runAsNonRoot: true
fsGroup: 65534
seccompProfile:
  type: RuntimeDefault
```

容器级安全上下文：

```yaml
allowPrivilegeEscalation: false
capabilities:
  drop:
    - ALL
```

不设置 `readOnlyRootFilesystem`。vLLM/sglang 需要写入缓存和临时目录；渲染器保留 `/tmp` 和 `/dev/shm` 可写。模型 PVC 仍按模型物化合同挂载，物化 Job 使用 `fsGroup` 获得写入权限。完整启动命令保持请求原值，代码不解析、不拼接、不注入引擎参数。

推理 Pod 和物化 Job 显式关闭 ServiceAccount token 自动挂载。控制面继续使用自己的 ServiceAccount 访问 Kubernetes API；runtime ServiceAccount 仍由 Chart 创建并通过配置传给运行时。

## 实现位置

- `internal/data/kubernetes/renderer.go`：Deployment/LWS 的 Pod 和容器安全上下文。
- `internal/data/kubernetes/kserve.go`：InferenceService predictor、LLMInferenceService template/worker 的安全上下文传播。
- `internal/data/model/materializer.go`：模型物化 Job 的安全上下文和临时目录。
- `charts/ani-inference/templates/deployment.yaml` 与 `values.yaml`：控制面容器级安全默认值。
- 对应 renderer、KServe、materializer 测试：断言非 root、禁止权限提升、capabilities 丢弃和 token 不自动挂载。

## 验证

代码验证：

- `go test ./internal/data/kubernetes ./internal/data/model`
- `helm lint charts/ani-inference`
- `helm template` 检查控制面安全上下文和 runtime ServiceAccount。

集群验证：

1. 用真实 CPU vLLM 镜像以 UID/GID `65534`（镜像内的 `nobody`）启动探针，确认 Python 用户解析和缓存目录可写。
2. 用真实模型物化镜像以 UID/GID `65534` 启动探针，确认导入脚本可运行。
3. 通过 Governance/Console 导入或选择模型并创建推理服务。
4. 检查 KServe CR、Deployment、Pod 的 `runAsNonRoot/runAsUser/allowPrivilegeEscalation`。
5. 通过 Higress 的 `/v1/completions` 或 `/v1/chat/completions` 调用 `served_model_name`。
6. 删除推理服务，确认 KServe、Pod、物化资源和 Higress 路由清理，并确认列表接口恢复。
7. 记录 Volcano 控制面和调度字段结果；不把无 GPU 集群上的 GPU 渲染验证写成 GPU 端到端通过。

## 不在本次范围

- 不改旧 `/root/kubercon/ANI` 仓库。
- 不修改 Model、Governance、Accelerator 或 Console 代码。
- 不改变 OpenAI 启动命令合同，不把环境变量转换成命令行参数。
- 不新增 GPU 节点，不伪造 GPU 调度成功。
