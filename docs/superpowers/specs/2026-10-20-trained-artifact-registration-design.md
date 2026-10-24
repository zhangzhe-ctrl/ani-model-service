# 训练产物注册到模型服务设计

日期：2026-10-20

## 目标

训练服务完成微调后会产出一份标准 HF 模型归档 `model.tar`。模型服务负责接收、校验、登记这份产物，并将它变成可以被 Inference 使用的 `ModelVersion`。

本阶段只交付 `ani-model-service`：注册协议、对象完整性校验、注册会话和聚合提交。
不修改 `ani-modeldev-service` 的训练流程，也不在本阶段输出跨仓库接入代码。ModelDev
当前状态和未来调用边界只作为契约约束记录；模型服务完成并验证后，再单独输出
ModelDev 对接文档。

## 当前约束

模型服务已有两个底层能力：

- `GetUploadURL`：为一个对象生成短期上传地址；
- `CreateModelVersion`：校验对象存在和 SHA256，创建版本及 Artifact。

这两个接口不能完整覆盖训练产物发布：

- 上传地址当前依赖 `model_id`，无法自然覆盖自动创建新 Model 的场景；
- 训练产物是 tar 归档，`format` 不能填写 `tar`；
- 上传和版本登记是两个独立步骤，失败重试和幂等关系不够清晰；
- 训练产物本身不能推断 vLLM、SGLang 或其他推理运行时；
- ModelDev 的发布对象和 Model Service 的对象存储是不同边界，不能直接把对方的对象 key 写入 `storage_path`。

## 交叉审查结论

这份方案需要和两个仓库当前的实现边界对齐，不能把现有的占位流程当成已经接通：

- ModelDev 的 `register` 步骤会先确认 `PUBLISHED` 产物，然后进入
  `ManagedRuntime.RegisterModel`。当前实现只持久化
  `MODEL_DEPENDENCY_UNAVAILABLE` 登记意图，不会直接调用 Model Service。真正的
  Model 调用必须放在 ModelDev 控制面运行时适配器中；训练 Pod 和 `register` Pod
  都不能持有 Model 数据库或对象存储的长期凭据。
- ModelDev 快照已经有两种目标归属：`target_model_id` 表示写入已有 Model，
  `new_model_name` 表示创建新 Model；`base_model_version_id` 位于基础模型引用中，
  它是训练血缘来源，不能用目标 Model ID 替代。新 Model 模式目前没有
  `display_name`、`description`、`capabilities`，所以这些字段必须在训练准入时加入
  冻结快照，或明确要求调用方先创建 Model，不能从名称推断。
- Model Service 的版本写入要求完整的 `engine_type`、`engine_image`、
  `startup_command`。ModelDev 当前快照没有这三个字段。第一阶段必须在注册请求中
  提供完整三元组，或从明确指定的 `base_model_version_id` 原样复制；不能把训练镜像、
  训练命令或模型文件推断成推理配置，也不能拼接启动命令。
- 当前 Storage 合同的 `VerifyChecksum` 只返回 SHA 校验结果，没有实际对象大小；
  MinIO 适配器也没有把大小纳入校验。本阶段只改 Model Service：增加内部
  `ObjectVerifier` 端口，MinIO 直接 `StatObject` 并计算摘要，现有 Storage gRPC
  适配器通过已有下载地址读取并计数。外部 Storage protobuf 不在本阶段修改；如果
  后续需要避免整对象下载，再单独扩展 Storage 合同。未完成适配器实际大小校验前，
  不得声称固定大小校验已经实现。
- ModelDev 的发布引用包含它自己的 storage connection、bucket、key 和对象版本；
  Model Service 只接受自己的租户对象引用。控制面必须读取已验证的 ModelDev 对象，
  再通过 Model Service 签发的上传地址流式复制，并记录复制状态和重试信息。
- 注册完成后必须返回 Model Service 生成的 `model_id`、`model_version_id` 和
  `registration_id`（或等价的持久化回执）。ModelDev 现有的
  `VerifiedPublication` 只能证明训练产物已发布，不能作为 ModelVersion 回执。

## 业务模型

注册请求中的 `target_model_id` 是可选的：

### 已有 Model

请求提供 `target_model_id`，模型服务校验该 Model 属于当前租户，然后在其下创建新的 `ModelVersion`。

训练任务还应记录 `source_model_version_id`，表示本次微调使用的基础版本。`target_model_id` 只表示产物归属，不能替代基础版本血缘。

### 新建 Model

请求不提供 `target_model_id`，但必须提供 `name`、`display_name`、`description` 和 `capabilities`。模型服务在完成产物校验后创建 Model 和第一个 ModelVersion。

Model 创建和版本创建必须由注册会话拥有的同一持久化事务完成。当前 Model Service
已有的普通 Model/ModelVersion store 是分开的，不能直接宣称现有写入已经满足这个
原子性；实现注册协议时必须增加聚合写入事务或等价的可恢复提交记录。模型服务不能
留下只有 Model 没有可用版本的半成品记录；注册失败时只保留注册会话的失败记录和
审计信息。

两种业务共用同一个产物上传和版本登记流程，区别只有是否需要创建目标 Model。

## 注册协议

新增模型服务内部 RPC，不改变现有公开 Model API。本阶段只接入已有的 Governance
控制面身份，且只放行下面三个注册方法；不新增 ModelDev 身份，也不让训练 Pod 直接
调用。ModelDev 的专用身份和跨服务代理方式留到后续对接文档：

```protobuf
rpc PrepareTrainedArtifact(PrepareTrainedArtifactRequest)
    returns (PrepareTrainedArtifactResponse);
rpc CompleteTrainedArtifact(CompleteTrainedArtifactRequest)
    returns (CompleteTrainedArtifactResponse);
rpc AbortTrainedArtifact(AbortTrainedArtifactRequest)
    returns (google.protobuf.Empty);
```

### PrepareTrainedArtifact

请求包括：

- `target_model_id`，可选；
- 新 Model 元数据，只有 `target_model_id` 为空时必填；
- `source_model_version_id`，可选；
- `version`；
- 产物文件名，第一阶段固定为 `model.tar`；
- `format`，第一阶段固定使用 `safetensors`；
- 整个 tar 的 `size_bytes` 和 `checksum_sha256`；
- `execution_id` 和 `idempotency_key`；
- 推理运行时配置：显式提供完整三元组，或提供可继承的
  `source_model_version_id`。两者都没有时不能进入完成阶段。

响应包括：

- `registration_id`；
- 短期 `upload_url`；
- `storage_path`；
- 过期时间和上传请求头。

上传对象使用注册会话路径，例如
`<tenant_id>/trained-artifacts/<registration_id>/model.tar`，不依赖最终是否创建
Model。完成注册后 ModelVersion 直接引用这个不可变对象路径，避免为了创建 Model
再复制一次大文件。

模型服务创建一条注册会话，但此时不创建可部署的 ready 版本。会话记录必须保存
对象引用、大小、摘要、租户、执行 ID、运行时三元组和目标归属，便于 Complete 重试
时重新校验同一份输入。

### CompleteTrainedArtifact

完成请求只携带 `registration_id` 和幂等信息。模型服务依次执行：

1. 校验注册会话属于当前租户且没有被终止；
2. 检查对象存在；
3. 通过 Storage 的实际对象校验接口校验大小和 SHA256；
4. 校验运行时配置是否完整；
5. 按 `target_model_id` 分支使用已有 Model 或创建新 Model；
6. 创建 Artifact 和 ModelVersion；
7. 将 ModelVersion 标记为 `ready`；
8. 复用现有逻辑同步父 Model 状态；
9. 记录执行任务、来源版本和审计信息。

完成响应必须包含 `registration_id`、最终 `model_id`、`model_version_id`、
`model_version_status` 和不可变产物引用。ModelDev 的注册步骤还要把这份 Model
回执持久化到执行记录，并在重试时原样返回；`VerifiedPublication` 不能替代这份回执。

重复完成请求必须返回同一个结果。相同幂等键但产物摘要不同，返回冲突，不得创建第二个版本。

### AbortTrainedArtifact

上传失败、校验失败或训练任务取消时，控制面调用终止接口。注册会话进入 `failed` 或 `aborted`，后续不能再完成。对象清理由存储生命周期或受控清理任务处理，不能由训练 Pod 直接删除任意对象。

## 产物格式

第一阶段保持现有跨服务兼容约定：

```text
file_name: model.tar
format: safetensors
size_bytes: 整个 model.tar 的字节数
checksum_sha256: 整个 model.tar 的 SHA256
```

`format` 表示归档内部的模型权重格式，不能写成 `tar`。tar 是封装方式，当前由 `.tar` 文件名和 Inference 物化协议表示。

Model Service 在第一阶段至少校验对象存在、实际大小和 SHA256。Model Service 内部
`ObjectVerifier` 由 MinIO 和现有 Storage gRPC 适配器实现；后者通过短期下载地址读取
对象并计算实际大小与摘要。现有仅按 SHA 校验的 `VerifyChecksum` 不足以单独满足本方案。
ModelDev
已经校验 HF 必需文件和 tar 安全性，Inference 物化时还会独立校验并安全解包到
`/models`。后续如果支持非 tar 产物，再新增显式 `packaging` 字段，并同步 Model、
Inference 和 Governance 的生成代码。

## 运行时配置

HF 模型文件不包含完整推理启动配置。以下字段必须来自调用方或已授权的基础版本：

```text
engine_type
engine_image
startup_command
```

模型服务只保存、校验和原样传递，不拼接、不补参数、不从训练镜像推断命令。

处理规则：

- 如果请求显式提供完整三元组，使用请求值；
- 如果请求指定 `source_model_version_id` 且未提供运行时，允许逐字段原样继承基础版本的三元组；
- 没有来源版本且三元组不完整时，完成注册失败；
- 训练服务的 Export 镜像和训练命令不能当作推理引擎配置。

## 存储和身份边界

训练 Pod 不直接访问 Model 数据库，也不持有 Model Storage 的长期凭据。

后续 ModelDev 的控制面注册 worker 负责：

1. 从 ModelDev 发布存储读取已验证的 `bundle.tar`；
2. 使用注册身份获得 Model Service 上传地址；
3. 流式上传到 Model Service Storage；
4. 完成注册会话。

该 worker 后续应使用专用服务身份，只允许调用训练产物注册 RPC。现有的公开 Model 管理 RPC 不直接开放给训练 Pod。本阶段由 Governance 身份承载这条控制面调用，具体的 ModelDev 身份、租户授权和对象复制链路不在本次 Model 改动内。

## 状态和幂等

注册会话状态：

```text
pending → uploaded → ready
                   ↘ failed
pending → aborted
```

ModelVersion 仍使用现有的 `pending → ready/error` 状态，注册会话负责记录上传和校验过程，避免为了训练发布扩大现有版本状态枚举。只有聚合事务提交成功后才允许变为
`ready`；对象校验或 Model/Version 写入失败时，注册会话进入终态并保留可重试或审计
信息，不能返回一个没有真实回执的成功状态。

幂等键建议使用：

```text
training:{execution_id}:model-version
```

同一租户、同一幂等键只能对应一个注册结果。

## 大文件处理

当前 `GetUploadURL` 是单对象单次 PUT，只作为第一阶段的小型验证通道。大模型发布前需要在 Storage/Model 边界增加上传会话、分片上传、完成和中止协议，不能把大文件通过 gRPC 传输，也不能假设单次 PUT 永远不会超时。

## 兼容范围

本阶段保留：

- 现有 `GetUploadURL`；
- 现有 `CreateModelVersion`；
- 现有远程 Hugging Face/ModelScope 导入接口；
- 现有前端模型导入流程。

新增注册协议只服务训练产物，不改变 Higress 路由，也不把 Model 管理 API 放入 AI 网关。

## 验证要求

模型服务实现后需要验证：

- 有 `target_model_id` 时能在已有 Model 下生成新版本；
- 没有 `target_model_id` 时能原子创建 Model 和 ModelVersion；
- 跨租户 Model ID 被拒绝；
- 上传对象不存在、大小不符、SHA 不符时注册失败；
- 同一幂等键重试返回同一结果；
- 同一幂等键使用不同 SHA 时返回冲突；
- ModelDev 的 `target_model_id`、`new_model_name`、`base_model_version_id` 映射正确，
  新 Model 所需的展示信息和能力字段缺失时明确失败；
- ModelDev 注册适配器使用专用 mTLS 身份，训练 Pod 无法调用注册 RPC；
- ModelDev 发布对象复制到 Model Storage 后，复制失败可重试且不会生成半成品 ready 版本；
- Complete 响应中的 Model/ModelVersion 回执能被 ModelDev 执行记录读取；
- 基础版本运行时继承是原样复制，不修改启动命令；
- 注册完成后 Inference 能下载并解包 `model.tar`；
- 训练服务尚未接入时，不宣称训练到推理的真实链路已通过。

## 当前实现状态（2026-10-20）

### Model Service 已完成

- ✅ 三个注册 RPC 方法实现并测试通过（PrepareTrainedArtifact、CompleteTrainedArtifact、AbortTrainedArtifact）
- ✅ 注册会话数据库表和迁移（`000008_trained_artifact_registration.sql`）
- ✅ 对象完整性校验（MinIO 和 gRPC Storage 适配器实现 `ObjectVerifier` 接口）
- ✅ 原子事务创建 Model + ModelVersion + Artifact，确保不会留下半成品记录
- ✅ 幂等重放和请求指纹冲突检测
- ✅ 运行时三元组显式传入或从基础版本原样继承
- ✅ RPC 策略配置，仅允许 Governance mTLS 身份（`ani-governance`）调用
- ✅ 单元测试和集成测试覆盖核心场景

### 尚未完成

- ModelDev 的注册调用适配器（`RegisterModel` 仍返回 `MODEL_DEPENDENCY_UNAVAILABLE`）
- ModelDev 发布对象到 Model Storage 的复制逻辑
- ModelDev 专用 mTLS 身份和对应 RPC 策略（当前借用 Governance 身份）
- Storage 分片上传协议（当前只支持单次 PUT，大文件需要后续扩展）
- 端到端的训练到推理链路验证

因此训练产物注册的 Model Service 端已完成，但跨服务集成和端到端链路尚未打通。

## 后续训练服务改动

模型服务协议稳定后，ModelDev 只需要：

1. 保留现有 HF bundle 生成、固定大小和 SHA256；
2. 在控制面而不是训练 Pod 中调用 Prepare/Complete；
3. 传递 `target_model_id` 或新 Model 元数据；
4. 传递 `source_model_version_id` 和显式运行时配置；
5. 使用 `execution_id` 作为幂等依据；
6. 根据注册会话状态更新训练发布结果，并保存 Model Service 的 Model/ModelVersion
   回执；
7. 不在训练 Pod 中直接实现注册，不把 ModelDev 对象存储引用冒充成 Model Storage
   路径。

ModelDev 不应直接写 Model 数据库，不应使用训练镜像推断推理命令，也不应把自己的对象存储 key 直接写入 ModelVersion。
