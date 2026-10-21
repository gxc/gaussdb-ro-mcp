#!/usr/bin/env bash
# 双语 README 结构同步校验：两份文件的章节（##/###）数量与顺序必须一致。
# 标题文字是翻译关系无法直接比对，这里比对标题级别序列——章节增删或
# 顺序调整只落在其中一份文件时，级别序列几乎必然出现差异。
# 跳过围栏代码块（``` 开头的行成对开关），避免示例中的 "#"/"##" 注释
# 或 Markdown 示例被误认为标题；标题级别模式只在 awk 中出现一次。
set -euo pipefail
cd "$(dirname "$0")/.."

levels() { awk '/^```/{f = !f; next} !f && /^#{2,3} /{ sub(/ .*/, ""); print }' "$1"; }

if diff <(levels README.md) <(levels README_en.md) >/dev/null; then
  echo "README 结构同步校验通过：README.md 与 README_en.md 章节结构一致"
else
  {
    echo "::error::README.md 与 README_en.md 的章节结构不一致（##/### 数量或顺序存在漂移），请同步修改两份文件"
    echo "--- README.md 标题 ---"
    grep -nE '^#{2,3} ' README.md || true
    echo "--- README_en.md 标题 ---"
    grep -nE '^#{2,3} ' README_en.md || true
  } >&2
  exit 1
fi
