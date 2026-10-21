#!/usr/bin/env bash
# 双语 README 结构同步校验：两份文件的章节（##/###）数量与顺序必须一致。
# 标题文字是翻译关系无法直接比对，这里比对标题级别序列——章节增删或
# 顺序调整只落在其中一份文件时，级别序列几乎必然出现差异。
set -euo pipefail
cd "$(dirname "$0")/.."

levels() { grep -E '^#{2,3} ' "$1" | sed -E 's/^(#{2,3}).*/\1/'; }

if diff <(levels README.md) <(levels README_en.md) >/dev/null; then
  echo "README 结构同步校验通过：README.md 与 README_en.md 章节结构一致"
else
  {
    echo "::error::README.md 与 README_en.md 的章节结构不一致（##/### 数量或顺序存在漂移），请同步修改两份文件"
    echo "--- 级别序列差异（左 README.md / 右 README_en.md）---"
    diff <(levels README.md) <(levels README_en.md) || true
  } >&2
  exit 1
fi
