# 第三方许可证与分发说明

ARTEX 项目源码依据 GNU AGPL v3.0 提供，同时使用 Go 模块、npm 包、前端组件及其他第三方材料。AGPL-3.0 不会替代这些材料各自的许可证，也不能删除其要求保留的版权、许可和免责声明。

## 仓库内的许可证入口

| 范围 | 核验入口 | 说明 |
| --- | --- | --- |
| ARTEX 项目源码 | [`LICENSE`](LICENSE) | GNU Affero General Public License v3.0（AGPL-3.0） |
| 前端目录内保留的 MIT 声明 | [`web/LICENSE`](web/LICENSE) | 保留原始版权与 MIT 条款；该文件不代表整个 ARTEX 项目采用 MIT 许可证 |
| Go 依赖 | [`go.mod`](go.mod)、[`go.sum`](go.sum) | 依赖版本清单；实际分发前还应从对应模块源码或发布包收集完整许可证文本 |
| Web 依赖 | [`web/package.json`](web/package.json)、[`web/package-lock.json`](web/package-lock.json) | 锁定 npm 依赖及部分 SPDX 许可证元数据 |
| 技能脚本依赖 | [`skills/api-recon/scripts/package.json`](skills/api-recon/scripts/package.json)、[`skills/api-recon/scripts/package-lock.json`](skills/api-recon/scripts/package-lock.json) | 独立脚本的 npm 依赖清单 |

## 分发前检查

对外提供源代码、二进制、容器镜像、安装包或托管服务前，至少应完成以下检查：

1. 基于实际发布产物生成软件物料清单（SBOM），不要只按仓库中的声明推断最终包含内容。
2. 收集发布产物中每个第三方组件对应版本的许可证、版权声明和 NOTICE 文件。
3. 核对许可证是否要求提供源码、修改说明、归属声明、许可证副本、专利声明或安装信息。
4. 保留 `web/LICENSE` 及第三方材料自带的文件头和权利标识。
5. 对 GPL、AGPL、LGPL、MPL、SSPL、BUSL 等可能影响分发或网络服务义务的许可证进行专项审查。
6. 检查字体、图标、截图、模型输出、测试数据、容器基础镜像及系统工具；它们可能不出现在语言包管理器的依赖清单中。

## 范围限制

本文档提供许可证核验入口和操作清单，不是完整的第三方许可证汇编，也不构成法律意见。依赖可能随构建标签、操作系统、目标架构和安装方式发生变化，应以实际发布产物和对应版本的上游许可证文本为准。
