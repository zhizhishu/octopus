# 数据来源与许可

本目录下的数据文件来自 **hlwy**（hlwy-ai-checker，https://github.com/hlwy/ai-checker，
上游代号 `hlwy`），用于「1–355 单数字采样分布」基准对照。

## 固定版本

- 上游提交：`7bddead107da09ddec459522df4b74eb0c7f8ac6`
- 复制范围：`baselines/index.json` 与 7 份基准 JSON 的**原始字节**（不重写、不压缩、不改字段）。
- 取数日期：2026-11-06。
- 本目录不 vendor 整个上游仓库，也不运行其代理服务或前端页面。

## 许可

上游为 **GNU Lesser General Public License v2.1**（LGPL-2.1），完整许可正文见本目录
`LICENSE`。本项目按「保留上游数据与声明」的方式复用这 8 份 JSON；算法实现为本项目
按上游公开公式独立重写（见 `../parse.go`、`../score.go`），不作为上游代码的再分发。

## 文件清单（逐文件 sha256，启动时由加载器校验）

| 文件 | sha256 | 说明 |
| --- | --- | --- |
| `index.json` | `f73ef6c3f5cfeaf7ba217c0a50aeaf49d90338d1b6a70c09fc4311db5e0c8afe` | 基准顺序清单 |
| `claude-fable-5-1.json` | `000f238d644c4316a6f7126ff396f9bc8f3a90d700186aecab77eb6ad799be41` | 200 次 |
| `claude-opus-5-5.json` | `15a405211dd3ea18b8090486f6929beee4f9443d87e8366ed27d45821b17bc42` | 198 次 |
| `claude-opus-5.json` | `8d7bc644412b0f282869930300afe9243d73185901ea03ebd7779b868e8914e6` | 100 次 |
| `deepseek-v4.1-flash-1017.json` | `18713b9605ead32ee58eae43fd1408b4657a12b8ddf0f1f343f1cc4ad20bbd5d` | 200 次 |
| `deepseek-v4.1-flash.json` | `75662fa602b5e85e20915116379d345bf225762a22b4a01404642a7709a78aa9` | 100 次 |
| `gpt-5.6-sol.json` | `60fc9745bd141faff6a193ed062dd9d01ed5be579eb9e1530c5389eefb58ce3d` | 200 次 |
| `gpt-6-astra.json` | `db64ce89111b3e15c7a004dfcd5cf274019e450f2f06d11b4b4c4474f56167ab` | 100 次 |

两份 DeepSeek 基准 `model` 字段相同（`deepseek-flash`），但日期、哈希、样本数不同，按
文件/哈希分别保留，不按 model 覆盖。

## 能力边界（务必随数据一起保留）

- 旧文件未记录采样参数、题目版本与出站 shape；`Conditions` 一律记为 **unknown**，
  不能凭文件名或用户口述升级为「官方同条件」。
- 相似度只是**基准库内的描述性对照**，不是「真品率」、不是准确率校准、也不代表
  两批请求实际落到同一后端。
- 显示分数保留两位；计算与排序用全精度。

## 如何更新

换基准时必须整目录替换 7 份 JSON + `index.json`，同步更新本表 sha256 与固定提交，
并重新核对 `internal/modelverify/numberdist/testdata` 中由独立脚本产出的 pairwise 金标。
