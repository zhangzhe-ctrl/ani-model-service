# Model 与 Inference API 模块边界

更新日期：2026-10-21。本文件区分当前源码、消费者依赖和已发布版本，
不能用工作区目录结构推断消费者已经获得了相同代码。

## 协议归属

| 协议 | 唯一维护方 | 消费方式 |
|------|------------|----------|
| ModelService、StorageService | Model API 模块 | Inference、Governance 等依赖 Model API 的不可变版本 |
| Inference 管理协议、ModelReferenceService | Inference API 模块 | Model、Governance 等依赖 Inference API 的不可变版本 |

两个服务主模块可以依赖对方 API；这两个 API 模块不需要为上述 RPC
相互依赖。复制 proto 并修改 `go_package` 无法隔离 protobuf 名称空间：
相同文件名、消息名或服务名在同一进程注册时可能直接 panic。

本次工作区修改删除 Model 的 `api/inference/v1` 副本，将客户端、组合根
和引用查询测试统一改为使用 Inference 已发布的 `api v0.1.1`。
RPC 路径、字段编号、租户元数据和 mTLS 配置均保持原有契约。

## 源码与发布版本

检查时的状态：

- Model 主模块依赖 Model API `v0.0.0-20261020102840-e6b26b580a9f`。
  该版本仍有旧引用协议；本次删除尚未发布。主模块已经不再导入旧包。
- Inference 主模块依赖自身 API `v0.1.1`。该发布版本仍含复制的
  `model/v1`，当前 Inference 源码已删除它，需发布后才能传递给消费者。
- Inference 使用的 Model API `v0.0.0-20261020112655-0b687e3fceea`
  与 Model/Governance 使用的上述版本，在 `api/` 内容上没有差异；
  不能仅凭伪版本不同判定协议不兼容。
- Governance 工作区已改为依赖独立 Model API，但该迁移尚未提交。
  其提交版本仍依赖整个 Model 服务模块。

因此，本地修复不等于 API 发布或集群升级完成。后续需要发布清理后的
API 版本、更新消费者，再构建部署和联调；本次修改不执行这些发布操作。

## 校验与实现的边界

两边都有 API README 和 Buf 生成配置。旧版对比中“Inference 没有文档、
没有 Buf”的判断不再成立。

公共契约可以包含调用双方必须共享的纯校验、规范化和摘要算法。
Governance 与 Inference 共用的请求哈希必须保持一致，不能因为它们
是手写 Go 就全部移到服务私有的 `internal`。

存储访问、调度器实现及业务用例应由服务内部负责。Model API 目前仍含
`Bootstrap` 配置和校验；Inference API 仍依赖 Kubernetes 类型与整个
Accelerator 模块。这些依赖边界需要单独评估，不应宣称两边都已成为
只含 protobuf 的最小模块，也不在本次协议去重中一并重构。

## 验证入口

从 Model 根目录运行：

```bash
make test                      # 显式测试 api 子模块及主模块
make verify-api                # 单独检查 api 子模块
make verify-api-compatibility  # 检查本地 API 与服务、Inference 协议的兼容性
make verify                    # 完整本地检查
```

日常构建使用已发布的固定依赖，禁用工作区覆盖。兼容检查仅在临时
modfile 中将 Model API 指向本地源码，不改写仓库依赖；它将全部本地
API 包与 Inference 协议加载到一个进程，检查重复注册，再运行主模块测试。
发布后仍需在实际消费版本上检查，临时兼容检查不能代替发布验证。

真实联调继续分别验证 Governance 的 `/api/v1/...` 管理入口和 Higress 的
`/v1/...` 推理入口，并覆盖模型引用存在时禁止删除、下游故障时拒绝删除。
