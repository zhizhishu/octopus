# tokenizer_bank.json — 来源与许可

逐字节复制自 **lm-detector** `data/tokenizer_bank.json`，未做任何修改。

- Source: <https://github.com/Ikaleio/lm-detector>
- Revision: `175355e8939c2ee7aaeeac2b22186fa5940412c3`（2026-10-30）
- sha256: `6dbf15a693deabfc36c4dfab9da64e9cdeee518f54eecf9cc69567ff5af26723`
- 文件自述：schema `tokenizer-bank-v1`，generated_at `2026-09-30`，由 `llm-tokenizer-fingerprint`
  对 290 个公开分词器（280 个候选，1 个排除）计算，archive manifest sha256
  `54577e9701c1c9f15c1b5bc838027b568b066d9fee8f6a463bfa785ba5257f03`。
- Copyright (c) 2026 xqy2006，MIT License（全文见 `../../NOTICE.md`）。

## 内容

- 72 道探测文本、62 个分词器类（每类 72 个"P t S − P S"差分计数）、8 条厂商 API 型号映射。
- 计数是参考项目离线用开源分词器算出来的，**不是本项目测的，也不是编的**。

## 能力边界（照参考实现文档）

- 测出来的是"上游用的哪套分词器"，不是"哪个模型"：同一分词器常被多个模型共用。
- 厂商未公开分词器的型号（Claude、Grok）在库里映射为 `class: null`，只能判断"像不像库里某个
  开源分词器"。要正向核对它们，需要在**可信渠道**上用「录制分词器基线」探针录本地基线
  （存在 `tokenizer_baselines` 表，本仓库不附带任何本地基线数据）。
- 中转站若用 tiktoken 本地估算用量，会测成 o200k / cl100k。
- 扩词表的变体（在某个库内分词器上加词）与原版在多数题上相同，只能判成"近亲"。

## 更新

替换此文件后必须同步重生成 `../testdata/reference_golden.json`（`../testdata/gen_golden.mjs`，
用参考实现的 TS 源码打包后跑），并更新上面的 revision 与 sha256。库版本（generated_at）变化后，
旧版本录的本地基线会被自动跳过并给出警告，需要重录。
