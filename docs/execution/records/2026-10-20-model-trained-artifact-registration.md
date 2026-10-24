# Model 训练产物注册实现记录

日期：2026-10-20

## 范围

本次只修改 `ani-model-service`。没有修改 `ani-modeldev-service`、Governance、
Inference 或前端代码。

## 已完成

- 增加 `PrepareTrainedArtifact`、`CompleteTrainedArtifact`、
  `AbortTrainedArtifact` 三个 Model Service gRPC 方法。
- 新增训练产物注册会话和 `000008_trained_artifact_registration.sql`。
- 上传完成后重新校验对象实际大小和 SHA256。
- 新建 Model 或已有 Model 下创建版本均在同一个 PostgreSQL 事务中完成。
- 同租户幂等键重放返回同一个注册结果；请求摘要变化返回冲突。
- 完成、失败、终止状态可恢复记录，ready 结果不会重复创建版本。
- 运行时的 `engine_type`、`engine_image`、`startup_command` 必须完整传入，
  或从 ready 的 `source_model_version_id` 原样继承；Model 不拼接命令。
- 仅向已有 Governance mTLS 身份开放三个新方法，Inference 权限不变。
- 增加 ModelDev 后续对接说明：
  `docs/modeldev-trained-artifact-integration.md`。

## 验证

已执行并通过：

```text
go test ./...
make verify
PG_TEST_CONTAINER=recycling-postgres PG_TEST_USER=recycling_user \
PG_TEST_PASSWORD=change-me-before-development \
scripts/test-model-migrations.sh
MODEL_DATABASE_URL=<临时迁移数据库> \
go test ./internal/data/postgres \
  -run TestTrainedArtifactStoreCompletesAggregateIdempotently -count=1
```

验证覆盖了运行时参数、租户身份、幂等重放、对象大小/SHA 不匹配、对象校验
适配器、注册事务和迁移重复执行。

## 尚未完成

ModelDev 还没有调用这些 RPC、复制训练对象或保存最终回执；专用 ModelDev mTLS
身份和大文件分片上传也未在本次实现。

